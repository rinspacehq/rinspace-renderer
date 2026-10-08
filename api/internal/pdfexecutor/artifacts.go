package pdfexecutor

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfinspect"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
)

const pdfArtifactSchemaVersion = "rin-latex-pdf-artifact/v1"

var latexFileLinePattern = regexp.MustCompile(`^(.+?\.tex):([0-9]+):\s*(.+)$`)

type artifactCandidate struct {
	kind      string
	logical   string
	mediaType string
	body      []byte
}

func (executor *Executor) collectAndStore(
	ctx context.Context,
	job jobpostgres.Job,
	metadata requestMetadata,
	outputDirectory string,
	requireComplete bool,
) ([]PreviewArtifact, []renderapi.Diagnostic, *Failure) {
	stem := strings.TrimSuffix(path.Base(metadata.Entrypoint), path.Ext(metadata.Entrypoint))
	root := path.Dir(metadata.Entrypoint)
	if root == "." {
		root = ""
	}
	logicalRoot := path.Join(root, ".rinspace")
	specifications := []struct {
		kind     string
		filename string
		logical  string
		required bool
	}{
		{kind: "pdf", filename: stem + ".pdf", logical: path.Join(logicalRoot, stem+".pdf"), required: true},
		{kind: "synctex", filename: stem + ".synctex.gz", logical: path.Join(logicalRoot, stem+".synctex.gz"), required: true},
		{kind: "log", filename: "compile.log", logical: path.Join(logicalRoot, stem+".log"), required: true},
		{kind: "aux", filename: stem + ".aux", logical: path.Join(logicalRoot, stem+".aux"), required: true},
		{kind: "fls", filename: stem + ".fls", logical: path.Join(logicalRoot, stem+".fls"), required: true},
		{kind: "bbl", filename: stem + ".bbl", logical: path.Join(logicalRoot, stem+".bbl")},
		{kind: "blg", filename: stem + ".blg", logical: path.Join(logicalRoot, stem+".blg")},
		{kind: "toc", filename: stem + ".toc", logical: path.Join(logicalRoot, stem+".toc")},
		{kind: "out", filename: stem + ".out", logical: path.Join(logicalRoot, stem+".out")},
	}
	if failure := validateOutputDirectory(outputDirectory); failure != nil {
		return nil, []renderapi.Diagnostic{}, failure
	}
	var total int64
	entries, err := os.ReadDir(outputDirectory)
	if err != nil {
		return nil, []renderapi.Diagnostic{}, retryableFailure("pdf_artifact_storage_failed", "PDF compiler output is unavailable")
	}
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 {
			return nil, []renderapi.Diagnostic{}, invalidFailure("invalid_pdf_artifact", "PDF compiler emitted an unsafe artifact")
		}
		if info.Size() > (48<<20)-total {
			return nil, []renderapi.Diagnostic{}, &Failure{Code: "pdf_artifact_quota_exceeded", Status: 413, Message: "PDF artifacts exceed 48 MiB", Diagnostics: []renderapi.Diagnostic{}}
		}
		total += info.Size()
	}

	candidates := make([]artifactCandidate, 0, len(specifications))
	var compileLog []byte
	for _, specification := range specifications {
		_, mediaType, maximum, _ := artifactPolicy(specification.kind)
		body, present, readErr := readOutputFile(outputDirectory, specification.filename, maximum)
		if readErr != nil {
			failure := invalidFailure("invalid_pdf_artifact", "PDF compiler artifact is invalid")
			if errors.Is(readErr, errArtifactTooLarge) {
				failure = &Failure{Code: "pdf_artifact_quota_exceeded", Status: 413, Message: "A PDF artifact exceeds its size limit", Diagnostics: []renderapi.Diagnostic{}}
			}
			return nil, parseLatexDiagnostics(compileLog), failure
		}
		if !present || len(body) == 0 {
			if requireComplete && specification.required {
				return nil, parseLatexDiagnostics(compileLog), invalidFailure("invalid_pdf_artifact", "PDF compiler omitted a required artifact")
			}
			continue
		}
		if specification.kind == "log" {
			compileLog = body
		}
		candidates = append(candidates, artifactCandidate{kind: specification.kind, logical: specification.logical, mediaType: mediaType, body: body})
	}
	diagnostics := parseLatexDiagnostics(compileLog)
	validated := make([]artifactCandidate, 0, len(candidates))
	var quarantineFailure *Failure
	for _, candidate := range candidates {
		switch candidate.kind {
		case "pdf":
			if _, err := pdfinspect.Inspect(candidate.body); err != nil {
				if errors.Is(err, pdfinspect.ErrQuarantined) {
					quarantineFailure = &Failure{Code: "unsafe_pdf_quarantined", Status: 422, Message: "PDF contains active or embedded content", Diagnostics: diagnostics}
					continue
				}
				return nil, diagnostics, invalidFailure("invalid_pdf_artifact", "PDF compiler output is not a valid PDF")
			}
		case "synctex":
			if err := validateSyncTeX(candidate.body); err != nil {
				return nil, diagnostics, invalidFailure("invalid_pdf_artifact", "PDF SyncTeX output is invalid")
			}
		case "log", "aux", "fls", "bbl", "blg", "toc", "out":
			if !utf8.Valid(candidate.body) || containsUnsafeText(candidate.body) {
				return nil, diagnostics, invalidFailure("invalid_pdf_artifact", "PDF text artifact is invalid")
			}
		}
		validated = append(validated, candidate)
	}

	artifacts := make([]PreviewArtifact, 0, len(validated))
	for _, candidate := range validated {
		artifact, err := executor.storeArtifact(ctx, job.ID, candidate)
		if err != nil {
			return artifacts, diagnostics, retryableFailure("pdf_artifact_storage_failed", "PDF artifact could not be stored")
		}
		artifacts = append(artifacts, artifact)
	}
	if quarantineFailure != nil {
		return artifacts, diagnostics, quarantineFailure
	}
	return artifacts, diagnostics, nil
}

var errArtifactTooLarge = errors.New("PDF artifact exceeds its limit")

func readOutputFile(directory, filename string, maximum int64) ([]byte, bool, error) {
	target := filepath.Join(directory, filename)
	relative, err := filepath.Rel(directory, target)
	if err != nil || filepath.Dir(relative) != "." || filepath.Base(relative) != filename {
		return nil, false, errors.New("PDF artifact path escaped output")
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("PDF artifact is not a regular file")
	}
	if info.Size() > maximum {
		return nil, true, errArtifactTooLarge
	}
	file, err := os.Open(target)
	if err != nil {
		return nil, true, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(body)) != info.Size() || int64(len(body)) > maximum {
		return nil, true, errArtifactTooLarge
	}
	return body, true, nil
}

func validateOutputDirectory(directory string) *Failure {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return retryableFailure("pdf_artifact_storage_failed", "PDF compiler output directory is unavailable")
	}
	return nil
}

func validateSyncTeX(body []byte) error {
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer reader.Close()
	written, err := io.Copy(io.Discard, io.LimitReader(reader, (64<<20)+1))
	if err != nil || written > 64<<20 {
		return errors.New("SyncTeX payload is invalid")
	}
	return reader.Close()
}

func containsUnsafeText(body []byte) bool {
	for _, value := range string(body) {
		if value == '\n' || value == '\r' || value == '\t' {
			continue
		}
		if unicode.IsControl(value) {
			return true
		}
	}
	return false
}

func (executor *Executor) storeArtifact(ctx context.Context, jobID string, candidate artifactCandidate) (PreviewArtifact, error) {
	digest := sha256.Sum256(candidate.body)
	hash := hex.EncodeToString(digest[:])
	extension, _, _, _ := artifactPolicy(candidate.kind)
	artifactID := fmt.Sprintf("render-jobs/v1/debug/%s/%s-%s%s", jobID, candidate.kind, hash, extension)
	expiresAt := executor.config.Now().UTC().Truncate(time.Second).Add(artifactTTL)
	reference, err := executor.store.PutPrivate(ctx, orchestration.ArtifactWrite{
		ArtifactID: artifactID, Body: candidate.body, MediaType: candidate.mediaType,
		SchemaVersion: pdfArtifactSchemaVersion, ExpiresAt: &expiresAt,
	})
	if err != nil {
		return PreviewArtifact{}, err
	}
	artifact := jobpostgres.Artifact{
		ID: reference.ArtifactID, SHA256: reference.SHA256, Kind: "debug", Visibility: reference.Visibility,
		StorageKey: reference.ArtifactID, ByteSize: reference.Bytes, MediaType: reference.MediaType,
		SchemaVersion: pdfArtifactSchemaVersion, ExpiresAt: &expiresAt,
	}
	if _, err := executor.repository.CreateArtifact(ctx, artifact); err != nil {
		existing, lookupErr := executor.repository.Artifact(ctx, artifact.ID)
		if lookupErr != nil || !sameArtifact(existing, artifact) {
			return PreviewArtifact{}, err
		}
	}
	return PreviewArtifact{
		ArtifactID: reference.ArtifactID, Kind: candidate.kind, Path: candidate.logical,
		SHA256: reference.SHA256, Bytes: reference.Bytes, MediaType: reference.MediaType,
		Visibility: reference.Visibility, ExpiresAt: reference.ExpiresAt,
	}, nil
}

func sameArtifact(left, right jobpostgres.Artifact) bool {
	return left.ID == right.ID && left.SHA256 == right.SHA256 && left.Kind == right.Kind &&
		left.Visibility == right.Visibility && left.StorageKey == right.StorageKey && left.ByteSize == right.ByteSize &&
		left.MediaType == right.MediaType && left.SchemaVersion == right.SchemaVersion && left.ExpiresAt != nil &&
		right.ExpiresAt != nil && left.ExpiresAt.Equal(*right.ExpiresAt)
}

func parseLatexDiagnostics(body []byte) []renderapi.Diagnostic {
	diagnostics := make([]renderapi.Diagnostic, 0)
	seen := map[string]bool{}
	for _, rawLine := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		severity := ""
		code := "latex.message"
		message := line
		source := map[string]string(nil)
		if match := latexFileLinePattern.FindStringSubmatch(line); len(match) == 4 {
			severity, code, message = "error", "latex.compile_error", strings.TrimSpace(match[3])
			file := strings.TrimPrefix(strings.TrimSpace(match[1]), "/workspace/")
			if canonicalSnapshotPath(file) {
				source = map[string]string{"path": file, "line": match[2]}
			}
		} else if strings.HasPrefix(line, "!") {
			severity, code, message = "error", "latex.compile_error", strings.TrimSpace(strings.TrimPrefix(line, "!"))
		} else if strings.Contains(strings.ToLower(line), "warning") {
			severity, code = "warning", "latex.warning"
		}
		if severity == "" {
			continue
		}
		message = boundedPrintable(message, 1024)
		key := severity + "\x00" + message
		if message == "" || seen[key] {
			continue
		}
		seen[key] = true
		diagnostics = append(diagnostics, renderapi.Diagnostic{Severity: severity, Code: code, Message: message, Engine: "latexmk", Source: source})
		if len(diagnostics) == 80 {
			break
		}
	}
	return diagnostics
}

func boundedPrintable(value string, maximum int) string {
	value = strings.Map(func(item rune) rune {
		if item == '\t' {
			return ' '
		}
		if unicode.IsControl(item) {
			return -1
		}
		return item
	}, value)
	if len(value) <= maximum {
		return strings.TrimSpace(value)
	}
	for len(value) > maximum {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return strings.TrimSpace(value)
}
