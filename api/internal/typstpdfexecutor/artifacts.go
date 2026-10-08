package typstpdfexecutor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
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

const (
	artifactSchemaVersion = "rin-typst-pdf-artifact/v1"
	outputLogFilename     = "compile.log"
)

var (
	errArtifactTooLarge = errors.New("Typst PDF artifact exceeds its size limit")
	// Typst prints compiler locations as either a boxed source frame
	// (`┌─ /workspace/main.typ:3:5`) or an arrow annotation. Only these two
	// canonical forms are parsed; everything else stays a plain message.
	typstLocationPattern = regexp.MustCompile(`^(?:┌─|-->)\s*(.+):([0-9]+):([0-9]+)$`)
)

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
) ([]Artifact, []renderapi.Diagnostic, *Failure) {
	stem := strings.TrimSuffix(path.Base(metadata.Entrypoint), path.Ext(metadata.Entrypoint))
	root := path.Dir(metadata.Entrypoint)
	if root == "." {
		root = ""
	}
	logicalRoot := path.Join(root, ".rinspace", "typst")
	specifications := []struct {
		kind     string
		filename string
		logical  string
		required bool
	}{
		{kind: "pdf", filename: stem + ".pdf", logical: path.Join(logicalRoot, stem+".pdf"), required: true},
		{kind: "log", filename: outputLogFilename, logical: path.Join(logicalRoot, stem+".log"), required: true},
	}
	if failure := validateOutputDirectory(outputDirectory); failure != nil {
		return nil, []renderapi.Diagnostic{}, failure
	}
	var total int64
	entries, err := os.ReadDir(outputDirectory)
	if err != nil {
		return nil, []renderapi.Diagnostic{}, retryableFailure("typst_artifact_storage_failed", "Typst compiler output is unavailable")
	}
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 {
			return nil, []renderapi.Diagnostic{}, invalidFailure("invalid_typst_artifact", "Typst compiler emitted an unsafe artifact")
		}
		if info.Size() > maxTotalArtifactSize-total {
			return nil, []renderapi.Diagnostic{}, &Failure{Code: "typst_artifact_quota_exceeded", Status: 413, Message: "Typst artifacts exceed 48 MiB", Diagnostics: []renderapi.Diagnostic{}}
		}
		total += info.Size()
	}

	candidates := make([]artifactCandidate, 0, len(specifications))
	var compileLog []byte
	for _, specification := range specifications {
		_, mediaType, maximum, _ := artifactPolicy(specification.kind)
		body, present, readErr := readOutputFile(outputDirectory, specification.filename, maximum)
		if readErr != nil {
			failure := invalidFailure("invalid_typst_artifact", "Typst compiler artifact is invalid")
			if errors.Is(readErr, errArtifactTooLarge) {
				failure = &Failure{Code: "typst_artifact_quota_exceeded", Status: 413, Message: "A Typst artifact exceeds its size limit", Diagnostics: []renderapi.Diagnostic{}}
			}
			return nil, parseTypstDiagnostics(compileLog), failure
		}
		if !present || len(body) == 0 {
			if requireComplete && specification.required {
				return nil, parseTypstDiagnostics(compileLog), invalidFailure("invalid_typst_artifact", "Typst compiler omitted a required artifact")
			}
			continue
		}
		if specification.kind == "log" {
			compileLog = body
		}
		candidates = append(candidates, artifactCandidate{kind: specification.kind, logical: specification.logical, mediaType: mediaType, body: body})
	}
	diagnostics := parseTypstDiagnostics(compileLog)
	validated := make([]artifactCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		switch candidate.kind {
		case "pdf":
			if _, err := pdfinspect.Inspect(candidate.body); err != nil {
				if errors.Is(err, pdfinspect.ErrQuarantined) {
					return nil, diagnostics, &Failure{Code: "unsafe_typst_pdf_quarantined", Status: 422, Message: "Typst PDF contains active or embedded content", Diagnostics: diagnostics}
				}
				return nil, diagnostics, invalidFailure("invalid_typst_artifact", "Typst compiler output is not a valid PDF")
			}
		case "log":
			if !utf8.Valid(candidate.body) || containsUnsafeText(candidate.body) {
				return nil, diagnostics, invalidFailure("invalid_typst_artifact", "Typst compiler log is invalid")
			}
		}
		validated = append(validated, candidate)
	}
	artifacts := make([]Artifact, 0, len(validated))
	for _, candidate := range validated {
		artifact, err := executor.storeArtifact(ctx, job.ID, candidate)
		if err != nil {
			return nil, diagnostics, retryableFailure("typst_artifact_storage_failed", "Typst PDF artifacts could not be stored")
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, diagnostics, nil
}

func readOutputFile(directory, name string, maximum int64) ([]byte, bool, error) {
	if name != path.Base(name) || strings.ContainsAny(name, `/\`) {
		return nil, false, errors.New("artifact name is not canonical")
	}
	target := path.Join(directory, name)
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > maximum {
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
		return retryableFailure("typst_artifact_storage_failed", "Typst compiler output directory is unavailable")
	}
	return nil
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

func (executor *Executor) storeArtifact(ctx context.Context, jobID string, candidate artifactCandidate) (Artifact, error) {
	digest := sha256.Sum256(candidate.body)
	hash := hex.EncodeToString(digest[:])
	extension, _, _, _ := artifactPolicy(candidate.kind)
	artifactID := fmt.Sprintf("render-jobs/v1/typst/%s/%s-%s%s", jobID, candidate.kind, hash, extension)
	expiresAt := executor.config.Now().UTC().Truncate(time.Second).Add(artifactTTL)
	reference, err := executor.store.PutPrivate(ctx, orchestration.ArtifactWrite{
		ArtifactID: artifactID, Body: candidate.body, MediaType: candidate.mediaType,
		SchemaVersion: artifactSchemaVersion, ExpiresAt: &expiresAt,
	})
	if err != nil {
		return Artifact{}, err
	}
	artifact := jobpostgres.Artifact{
		ID: reference.ArtifactID, SHA256: reference.SHA256, Kind: "debug", Visibility: reference.Visibility,
		StorageKey: reference.ArtifactID, ByteSize: reference.Bytes, MediaType: reference.MediaType,
		SchemaVersion: artifactSchemaVersion, ExpiresAt: &expiresAt,
	}
	if _, err := executor.repository.CreateArtifact(ctx, artifact); err != nil {
		existing, lookupErr := executor.repository.Artifact(ctx, artifact.ID)
		if lookupErr != nil || !sameArtifact(existing, artifact) {
			return Artifact{}, err
		}
	}
	return Artifact{
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

func parseTypstDiagnostics(body []byte) []renderapi.Diagnostic {
	diagnostics := make([]renderapi.Diagnostic, 0)
	seen := map[string]bool{}
	last := -1
	for _, rawLine := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if match := typstLocationPattern.FindStringSubmatch(line); len(match) == 4 {
			file := strings.TrimPrefix(strings.TrimSpace(match[1]), "/workspace/")
			if last >= 0 && diagnostics[last].Source == nil && canonicalSnapshotPath(file) {
				diagnostics[last].Source = map[string]string{"path": file, "line": match[2], "column": match[3]}
			}
			continue
		}
		severity := ""
		code := "typst.message"
		message := line
		switch {
		case strings.HasPrefix(line, "error:"):
			severity, code, message = "error", "typst.compile_error", strings.TrimSpace(strings.TrimPrefix(line, "error:"))
		case strings.HasPrefix(line, "warning:"):
			severity, code, message = "warning", "typst.warning", strings.TrimSpace(strings.TrimPrefix(line, "warning:"))
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
		diagnostics = append(diagnostics, renderapi.Diagnostic{Severity: severity, Code: code, Message: message, Engine: "typst"})
		last = len(diagnostics) - 1
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
