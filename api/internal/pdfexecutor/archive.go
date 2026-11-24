package pdfexecutor

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

const (
	snapshotFileLimit  = 700
	snapshotFileBytes  = 8 << 20
	snapshotTotalBytes = 64 << 20
)

var jobIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type snapshotEntry struct {
	path string
	body []byte
	hash string
}

type preparedSpool struct {
	directory string
	workspace string
	output    string
}

func prepareSpool(root, jobID, entrypoint string, archive []byte, expectedSnapshotHash string) (preparedSpool, error) {
	if !jobIDPattern.MatchString(jobID) || !canonicalEntrypoint(entrypoint) || !sha256Pattern.MatchString(expectedSnapshotHash) {
		return preparedSpool{}, errors.New("PDF spool identity is invalid")
	}
	absoluteRoot, err := secureSpoolRoot(root)
	if err != nil {
		return preparedSpool{}, err
	}
	entries, snapshotHash, err := decodeSnapshotArchive(archive)
	if err != nil {
		return preparedSpool{}, err
	}
	if snapshotHash != expectedSnapshotHash {
		return preparedSpool{}, errors.New("snapshot hash does not match the queued PDF job")
	}
	return writeSpool(absoluteRoot, jobID, entrypoint, entries)
}

// prepareExportSpool applies the LaTeX project importer's file policy to an
// exact commit archive while enforcing the compiler's cumulative limits.
func prepareExportSpool(root, jobID, entrypoint string, archive []byte) (preparedSpool, error) {
	if !jobIDPattern.MatchString(jobID) || !CanonicalExportEntrypoint(entrypoint) ||
		len(archive) == 0 || len(archive) > snapshotTotalBytes {
		return preparedSpool{}, errors.New("LaTeX PDF export spool identity is invalid")
	}
	zipped, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(zipped.File) > snapshotFileLimit {
		return preparedSpool{}, errors.New("LaTeX project archive is invalid or contains too many entries")
	}
	seen := make(map[string]bool, len(zipped.File))
	total := uint64(0)
	for _, file := range zipped.File {
		if file.FileInfo().IsDir() {
			continue
		}
		name, canonical := projectcore.CleanProjectPath(file.Name)
		if !file.Mode().IsRegular() || !canonical || name != file.Name || seen[name] ||
			file.UncompressedSize64 > snapshotFileBytes || file.UncompressedSize64 > snapshotTotalBytes-total {
			return preparedSpool{}, errors.New("LaTeX project archive contains an invalid or oversized file")
		}
		seen[name] = true
		total += file.UncompressedSize64
	}
	files, _, err := projectcore.ImportArchive("project.zip", archive, projectcore.Limits{
		ArchiveMaxBytes: snapshotTotalBytes, FileMaxCount: snapshotFileLimit, FileMaxBytes: snapshotFileBytes,
	})
	if err != nil {
		return preparedSpool{}, err
	}
	entries := make(map[string]snapshotEntry, len(files))
	for _, file := range files {
		if !canonicalSnapshotPath(file.Path) {
			return preparedSpool{}, errors.New("LaTeX project file path is not canonical")
		}
		body := []byte(file.Body)
		if file.Encoding == "base64" {
			body, err = base64.StdEncoding.DecodeString(file.Body)
			if err != nil {
				return preparedSpool{}, err
			}
		}
		entries[file.Path] = snapshotEntry{path: file.Path, body: body}
	}
	absoluteRoot, err := secureSpoolRoot(root)
	if err != nil {
		return preparedSpool{}, err
	}
	return writeSpool(absoluteRoot, jobID, entrypoint, entries)
}

func writeSpool(absoluteRoot, jobID, entrypoint string, entries map[string]snapshotEntry) (preparedSpool, error) {
	if _, ok := entries[entrypoint]; !ok {
		return preparedSpool{}, errors.New("PDF entrypoint is absent from the source archive")
	}
	finalDirectory := filepath.Join(absoluteRoot, jobID)
	if err := removeExactSpoolPath(absoluteRoot, finalDirectory); err != nil {
		return preparedSpool{}, err
	}
	temporary, err := os.MkdirTemp(absoluteRoot, "."+jobID+"-")
	if err != nil {
		return preparedSpool{}, fmt.Errorf("create PDF spool: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temporary)
		}
	}()
	workspace := filepath.Join(temporary, "workspace")
	output := filepath.Join(temporary, "output")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		return preparedSpool{}, fmt.Errorf("create PDF workspace: %w", err)
	}
	if err := os.Mkdir(output, 0o777); err != nil {
		return preparedSpool{}, fmt.Errorf("create PDF output: %w", err)
	}
	if err := os.Chmod(output, 0o777); err != nil {
		return preparedSpool{}, fmt.Errorf("set PDF output permissions: %w", err)
	}
	ordered := make([]string, 0, len(entries))
	for name := range entries {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		entry := entries[name]
		target := filepath.Join(workspace, filepath.FromSlash(name))
		relative, relErr := filepath.Rel(workspace, target)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
			return preparedSpool{}, errors.New("snapshot extraction path escaped the PDF workspace")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return preparedSpool{}, fmt.Errorf("create snapshot directory: %w", err)
		}
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
		if err != nil {
			return preparedSpool{}, fmt.Errorf("create snapshot file: %w", err)
		}
		if _, err := file.Write(entry.body); err != nil {
			file.Close()
			return preparedSpool{}, fmt.Errorf("write snapshot file: %w", err)
		}
		if err := file.Close(); err != nil {
			return preparedSpool{}, fmt.Errorf("close snapshot file: %w", err)
		}
	}
	state, err := json.Marshal(struct {
		ID         string `json:"id"`
		Entrypoint string `json:"entrypoint"`
	}{ID: jobID, Entrypoint: entrypoint})
	if err != nil {
		return preparedSpool{}, err
	}
	if err := os.WriteFile(filepath.Join(temporary, "state.json"), append(state, '\n'), 0o440); err != nil {
		return preparedSpool{}, fmt.Errorf("write PDF spool state: %w", err)
	}
	if err := os.Chmod(workspace, 0o555); err != nil {
		return preparedSpool{}, fmt.Errorf("seal PDF workspace: %w", err)
	}
	if err := os.Chmod(temporary, 0o750); err != nil {
		return preparedSpool{}, fmt.Errorf("seal PDF spool: %w", err)
	}
	if err := os.Rename(temporary, finalDirectory); err != nil {
		return preparedSpool{}, fmt.Errorf("commit PDF spool: %w", err)
	}
	committed = true
	return preparedSpool{
		directory: finalDirectory,
		workspace: filepath.Join(finalDirectory, "workspace"),
		output:    filepath.Join(finalDirectory, "output"),
	}, nil
}

func cleanupSpool(root, jobID string) error {
	if !jobIDPattern.MatchString(jobID) {
		return errors.New("PDF cleanup job ID is invalid")
	}
	absoluteRoot, err := secureSpoolRoot(root)
	if err != nil {
		return err
	}
	return removeExactSpoolPath(absoluteRoot, filepath.Join(absoluteRoot, jobID))
}

func secureSpoolRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return "", errors.New("PDF spool root must be absolute")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve PDF spool root: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o750); err != nil {
		return "", fmt.Errorf("create PDF spool root: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o002 != 0 {
		return "", errors.New("PDF spool root is unsafe")
	}
	return absolute, nil
}

func removeExactSpoolPath(root, target string) error {
	relative, err := filepath.Rel(root, target)
	if err != nil || !jobIDPattern.MatchString(relative) || filepath.Dir(relative) != "." {
		return errors.New("refusing to remove an unexpected PDF spool path")
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect PDF spool before cleanup: %w", err)
	}
	if !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
		return errors.New("PDF spool target is not a directory")
	}
	if info.IsDir() {
		// The extracted source tree is deliberately sealed read-only while the
		// compiler runs. Restore owner access on directories only so an
		// unprivileged worker can remove the exact, already-validated spool path.
		// WalkDir does not follow symlinks, so this cannot chmod outside target.
		if err := filepath.WalkDir(target, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return os.Chmod(current, 0o700)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("unseal PDF spool before cleanup: %w", err)
		}
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove PDF spool: %w", err)
	}
	return nil
}

func decodeSnapshotArchive(body []byte) (map[string]snapshotEntry, string, error) {
	if len(body) < 1024 || len(body)%512 != 0 || !allZero(body[len(body)-1024:]) || len(body) > snapshotTotalBytes+snapshotFileLimit*512+1024 {
		return nil, "", errors.New("snapshot tar framing is invalid")
	}
	if err := validateSnapshotTarEnd(body); err != nil {
		return nil, "", err
	}
	entries := map[string]snapshotEntry{}
	source := bytes.NewReader(body)
	reader := tar.NewReader(source)
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read snapshot tar: %w", err)
		}
		if header.Format != tar.FormatUSTAR || (header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA) ||
			header.Size < 0 || header.Size > snapshotFileBytes || !canonicalSnapshotPath(header.Name) {
			return nil, "", errors.New("snapshot tar contains an unsafe entry")
		}
		if len(entries) >= snapshotFileLimit {
			return nil, "", errors.New("snapshot tar exceeds the file-count limit")
		}
		if _, exists := entries[header.Name]; exists {
			return nil, "", errors.New("snapshot tar contains a duplicate path")
		}
		fileBody, err := io.ReadAll(io.LimitReader(reader, snapshotFileBytes+1))
		if err != nil || int64(len(fileBody)) != header.Size {
			return nil, "", errors.New("snapshot tar file size is invalid")
		}
		total += int64(len(fileBody))
		if total > snapshotTotalBytes {
			return nil, "", errors.New("snapshot tar exceeds the total byte limit")
		}
		digest := sha256.Sum256(fileBody)
		entries[header.Name] = snapshotEntry{path: header.Name, body: fileBody, hash: hex.EncodeToString(digest[:])}
	}
	if len(entries) == 0 {
		return nil, "", errors.New("snapshot tar contains no files")
	}
	ordered := make([]snapshotEntry, 0, len(entries))
	for _, entry := range entries {
		ordered = append(ordered, entry)
	}
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].path < ordered[right].path })
	digest := sha256.New()
	_, _ = digest.Write([]byte(SnapshotSchemaVersion + "\x00"))
	for _, entry := range ordered {
		_, _ = fmt.Fprintf(digest, "%s\x00%d\x00%s\x00", entry.path, len(entry.body), entry.hash)
	}
	return entries, hex.EncodeToString(digest.Sum(nil)), nil
}

func validateSnapshotTarEnd(body []byte) error {
	for offset := 0; offset+512 <= len(body); {
		header := body[offset : offset+512]
		if allZero(header) {
			if offset+1024 > len(body) || !allZero(body[offset:]) {
				return errors.New("snapshot tar contains data after its end marker")
			}
			return nil
		}
		rawSize := strings.Trim(string(header[124:136]), "\x00 ")
		size := int64(0)
		var err error
		if rawSize != "" {
			size, err = strconv.ParseInt(rawSize, 8, 64)
		}
		if err != nil || size < 0 {
			return errors.New("snapshot tar size field is invalid")
		}
		blocks := (size + 511) / 512
		if blocks > int64((len(body)-offset-512)/512) {
			return errors.New("snapshot tar entry exceeds its framing")
		}
		offset += 512 + int(blocks)*512
	}
	return errors.New("snapshot tar end marker is missing")
}

func canonicalSnapshotPath(value string) bool {
	if value == "" || len([]byte(value)) > 255 || strings.HasPrefix(value, "/") || strings.Contains(value, `\`) ||
		strings.ContainsAny(value, "\x00\r\n") || path.Clean(value) != value {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." || segment == ".git" || segment == ".rinspace" {
			return false
		}
	}
	return true
}

func allZero(body []byte) bool {
	for _, value := range body {
		if value != 0 {
			return false
		}
	}
	return true
}
