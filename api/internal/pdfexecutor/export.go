package pdfexecutor

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
)

const (
	ExportOutputKind    = "latex-pdf-export"
	ExportSchemaVersion = "rin-latex-pdf-export/v1"
)

var (
	exportProjectPattern = regexp.MustCompile(`^(article|book|tag-wiki):[1-9][0-9]*$`)
	exportCommitPattern  = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

type exportSource struct {
	ControlProjectID   string `json:"controlProjectId"`
	SourceCommit       string `json:"sourceCommit"`
	ControlProjectHash string `json:"controlProjectHash"`
}

func (source exportSource) valid() bool {
	return exportProjectPattern.MatchString(source.ControlProjectID) && exportCommitPattern.MatchString(source.SourceCommit) &&
		strings.Trim(source.SourceCommit, "0") != "" && sha256Pattern.MatchString(source.ControlProjectHash)
}

func (source exportSource) key(entrypoint, policy, image string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{source.ControlProjectID, source.SourceCommit, entrypoint, policy, image}, "\x00")))
	return hex.EncodeToString(digest[:])
}

// CanonicalExportEntrypoint accepts the committed LaTeX entrypoint forms
// supported by the fixed compiler and workload broker.
func CanonicalExportEntrypoint(value string) bool {
	extension := strings.ToLower(path.Ext(value))
	first, _, _ := strings.Cut(value, "/")
	return (extension == ".tex" || extension == ".ltx") && canonicalSnapshotPath(value) &&
		!strings.HasPrefix(value, "-") && !strings.Contains(value, "..") && !strings.Contains(first, ":") &&
		!strings.HasSuffix(value, "~") && strings.IndexFunc(value, unicode.IsControl) < 0
}

// ExportResult is the verified LaTeX artifact of a committed project archive.
type ExportResult struct {
	exportSource
	SchemaVersion      string                 `json:"schemaVersion"`
	JobID              string                 `json:"jobId"`
	ContentKind        string                 `json:"contentKind"`
	OutputKind         string                 `json:"outputKind"`
	DocumentEngine     string                 `json:"documentEngine"`
	ResourceClass      string                 `json:"resourceClass"`
	PriorityClass      string                 `json:"priorityClass"`
	EnginePolicyID     string                 `json:"enginePolicyId"`
	ImageDigest        string                 `json:"imageDigest"`
	IdempotencyKey     string                 `json:"idempotencyKey"`
	Entrypoint         string                 `json:"entrypoint"`
	Artifacts          []PreviewArtifact      `json:"artifacts"`
	TotalArtifactBytes int64                  `json:"totalArtifactBytes"`
	Diagnostics        []renderapi.Diagnostic `json:"diagnostics"`
	Versions           PreviewVersions        `json:"versions"`
}

func (result ExportResult) Validate() error {
	if result.SchemaVersion != ExportSchemaVersion || result.OutputKind != ExportOutputKind ||
		result.ContentKind != "latex" || result.DocumentEngine != "latexmk" || result.ResourceClass != "latex-pdf" ||
		(result.PriorityClass != "publish" && result.PriorityClass != "rebuild") ||
		!jobIDPattern.MatchString(result.JobID) || !result.exportSource.valid() ||
		!CanonicalExportEntrypoint(result.Entrypoint) || result.EnginePolicyID != PolicyID ||
		!imageDigestPattern.MatchString(result.ImageDigest) ||
		result.IdempotencyKey != result.exportSource.key(result.Entrypoint, result.EnginePolicyID, result.ImageDigest) {
		return errors.New("LaTeX PDF export identity is invalid")
	}
	if result.Versions.RinRenderer == "" || result.Versions.Latexmk != LatexmkVersion ||
		result.Versions.TeXLive != TeXLiveVersion || result.Versions.CompilerImage != result.ImageDigest ||
		result.Diagnostics == nil || len(result.Artifacts) < 1 || len(result.Artifacts) > 2 {
		return errors.New("LaTeX PDF export versions or collections are invalid")
	}
	seen := make(map[string]bool, len(result.Artifacts))
	var size int64
	for _, artifact := range result.Artifacts {
		if (artifact.Kind != "pdf" && artifact.Kind != "log") || seen[artifact.Kind] {
			return errors.New("LaTeX PDF export artifact kind is invalid")
		}
		if err := artifact.validate(result.JobID, result.Entrypoint); err != nil {
			return err
		}
		seen[artifact.Kind] = true
		size += artifact.Bytes
	}
	if !seen["pdf"] || size != result.TotalArtifactBytes {
		return errors.New("LaTeX PDF export does not have the verified PDF bytes")
	}
	return nil
}
