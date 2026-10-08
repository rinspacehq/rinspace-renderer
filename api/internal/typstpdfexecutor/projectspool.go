package typstpdfexecutor

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

const (
	// ExportArchiveSchemaVersion is the Control Plane project archive schema a
	// committed Typst PDF export consumes. It is a different framing from the
	// preview snapshot and must never satisfy the preview branch.
	ExportArchiveSchemaVersion = "rin-project-archive/v1"
	// ExportArchiveMediaType is the only media type a committed export accepts.
	ExportArchiveMediaType = "application/zip"
	// exportArchiveName is the synthetic archive name handed to the shared
	// project importer; the admitted schema, never the client filename,
	// selects the zip branch.
	exportArchiveName = "project.zip"
)

// prepareExportSpool extracts a published project archive into the Typst
// workspace. It reuses the shared project importer so path traversal,
// duplicate, oversized and unsupported entries are rejected exactly as they
// are by the publishing pipeline, and the entrypoint must survive that
// normalization before the compiler is allowed to run.
func prepareExportSpool(root, jobID, entrypoint string, archive []byte, expectedArchiveHash string) (preparedSpool, error) {
	if !jobIDPattern.MatchString(jobID) || !canonicalTypstEntrypoint(entrypoint) ||
		!sha256Pattern.MatchString(expectedArchiveHash) || len(archive) == 0 || len(archive) > snapshotTotalBytes {
		return preparedSpool{}, errors.New("Typst PDF export spool identity is invalid")
	}
	absoluteRoot, err := secureSpoolRoot(root)
	if err != nil {
		return preparedSpool{}, err
	}
	files, _, err := projectcore.ImportTypstArchive(exportArchiveName, archive, projectcore.Limits{
		ArchiveMaxBytes: snapshotTotalBytes, FileMaxCount: snapshotFileLimit, FileMaxBytes: snapshotFileBytes,
	})
	if err != nil {
		return preparedSpool{}, fmt.Errorf("import Typst PDF project archive: %w", err)
	}
	found := false
	for _, file := range files {
		if file.Path == entrypoint {
			found = true
			break
		}
	}
	if !found {
		return preparedSpool{}, errors.New("Typst PDF entrypoint is absent from the project archive")
	}
	finalDirectory, temporary, workspace, _, err := createSpoolLayout(absoluteRoot, jobID)
	if err != nil {
		return preparedSpool{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temporary)
		}
	}()
	for _, file := range files {
		body, err := projectFileBody(file)
		if err != nil {
			return preparedSpool{}, err
		}
		target := filepath.Join(workspace, filepath.FromSlash(file.Path))
		relative, relErr := filepath.Rel(workspace, target)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return preparedSpool{}, errors.New("Typst PDF project path escaped the workspace")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return preparedSpool{}, fmt.Errorf("create Typst project directory: %w", err)
		}
		handle, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
		if err != nil {
			return preparedSpool{}, fmt.Errorf("create Typst project file: %w", err)
		}
		if _, err := handle.Write(body); err != nil {
			handle.Close()
			return preparedSpool{}, fmt.Errorf("write Typst project file: %w", err)
		}
		if err := handle.Close(); err != nil {
			return preparedSpool{}, fmt.Errorf("close Typst project file: %w", err)
		}
	}
	prepared, err := sealSpool(temporary, finalDirectory, jobID, entrypoint)
	if err != nil {
		return preparedSpool{}, err
	}
	committed = true
	return prepared, nil
}

// projectFileBody reconstructs the on-disk bytes of one imported project file.
// Binary assets are base64 in the shared manifest and must decode cleanly; the
// shared importer already enforced the per-file and total byte limits.
func projectFileBody(file projectcore.File) ([]byte, error) {
	body := []byte(file.Body)
	switch file.Encoding {
	case "":
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(file.Body)
		if err != nil {
			return nil, errors.New("Typst PDF project file encoding is invalid")
		}
		body = decoded
	default:
		return nil, errors.New("Typst PDF project file encoding is unsupported")
	}
	if int64(len(body)) > snapshotFileBytes {
		return nil, errors.New("Typst PDF project file is too large")
	}
	return body, nil
}
