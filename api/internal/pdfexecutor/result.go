package pdfexecutor

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
)

const (
	PreviewSchemaVersion  = "rin-latex-pdf-preview/v1"
	SnapshotSchemaVersion = "rinspace-snapshot/v1"
	PolicyID              = "rinspace-latex-pdf-safe-v1"
	LatexmkVersion        = "4.79"
	TeXLiveVersion        = "TeX Live 2022/Debian"
	artifactTTL           = 24 * time.Hour
)

var (
	sha256Pattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	imageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	sessionPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
)

type PreviewArtifact struct {
	ArtifactID string `json:"artifactId"`
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	MediaType  string `json:"mediaType"`
	Visibility string `json:"visibility"`
	ExpiresAt  string `json:"expiresAt"`
}

type PreviewCache struct {
	Hit         bool   `json:"hit"`
	ReusedJobID string `json:"reusedJobId,omitempty"`
}

type PreviewVersions struct {
	RinRenderer   string `json:"rinRenderer"`
	Latexmk       string `json:"latexmk"`
	TeXLive       string `json:"texlive"`
	CompilerImage string `json:"compilerImage"`
}

type PreviewResult struct {
	SchemaVersion      string                 `json:"schemaVersion"`
	JobID              string                 `json:"jobId"`
	RequestID          string                 `json:"requestId"`
	ContentKind        string                 `json:"contentKind"`
	OutputKind         string                 `json:"outputKind"`
	DocumentEngine     string                 `json:"documentEngine"`
	ResourceClass      string                 `json:"resourceClass"`
	PriorityClass      string                 `json:"priorityClass"`
	SnapshotHash       string                 `json:"snapshotHash"`
	SessionID          string                 `json:"sessionId"`
	DraftRevision      int64                  `json:"draftRevision"`
	EnginePolicyID     string                 `json:"enginePolicyId"`
	ImageDigest        string                 `json:"imageDigest"`
	IdempotencyKey     string                 `json:"idempotencyKey"`
	Entrypoint         string                 `json:"entrypoint"`
	WorkspaceRoot      string                 `json:"workspaceRoot"`
	Artifacts          []PreviewArtifact      `json:"artifacts"`
	TotalArtifactBytes int64                  `json:"totalArtifactBytes"`
	Diagnostics        []renderapi.Diagnostic `json:"diagnostics"`
	Cache              PreviewCache           `json:"cache"`
	Versions           PreviewVersions        `json:"versions"`
}

func (result PreviewResult) Validate() error {
	if result.SchemaVersion != PreviewSchemaVersion || result.ContentKind != "latex" ||
		result.OutputKind != "latex-pdf-preview" || result.DocumentEngine != "latexmk" ||
		result.ResourceClass != "latex-pdf" || result.PriorityClass != "preview" ||
		result.WorkspaceRoot != "/workspace" {
		return errors.New("PDF preview result contract identity is invalid")
	}
	if !jobIDPattern.MatchString(result.JobID) || strings.TrimSpace(result.RequestID) == "" ||
		!sha256Pattern.MatchString(result.SnapshotHash) || !sessionPattern.MatchString(result.SessionID) ||
		result.DraftRevision < 0 || result.EnginePolicyID != PolicyID ||
		!imageDigestPattern.MatchString(result.ImageDigest) || !sha256Pattern.MatchString(result.IdempotencyKey) ||
		!canonicalEntrypoint(result.Entrypoint) {
		return errors.New("PDF preview result identity is invalid")
	}
	if result.Versions.RinRenderer == "" || result.Versions.Latexmk != LatexmkVersion ||
		result.Versions.TeXLive != TeXLiveVersion || result.Versions.CompilerImage != result.ImageDigest {
		return errors.New("PDF preview result versions are invalid")
	}
	if result.Artifacts == nil || len(result.Artifacts) < 5 || len(result.Artifacts) > 16 || result.Diagnostics == nil {
		return errors.New("PDF preview result collections are invalid")
	}
	required := map[string]bool{"pdf": false, "synctex": false, "log": false, "aux": false, "fls": false}
	seen := map[string]bool{}
	var total int64
	for _, artifact := range result.Artifacts {
		if err := artifact.validate(result.JobID, result.Entrypoint); err != nil {
			return err
		}
		if seen[artifact.Kind] {
			return fmt.Errorf("duplicate PDF preview artifact kind %q", artifact.Kind)
		}
		seen[artifact.Kind] = true
		if _, ok := required[artifact.Kind]; ok {
			required[artifact.Kind] = true
		}
		total += artifact.Bytes
		if total > 48<<20 {
			return errors.New("PDF preview artifacts exceed 48 MiB")
		}
	}
	for kind, present := range required {
		if !present {
			return fmt.Errorf("PDF preview result is missing %s artifact", kind)
		}
	}
	if result.TotalArtifactBytes != total || total <= 0 {
		return errors.New("PDF preview total artifact bytes are invalid")
	}
	return nil
}

func (artifact PreviewArtifact) validate(jobID, entrypoint string) error {
	reference := contracts.ArtifactReference{
		ArtifactID: artifact.ArtifactID, SHA256: artifact.SHA256, Bytes: artifact.Bytes,
		MediaType: artifact.MediaType, Visibility: artifact.Visibility, ExpiresAt: artifact.ExpiresAt,
	}
	if err := reference.Validate(); err != nil || artifact.Visibility != "private" || artifact.Bytes <= 0 ||
		!strings.HasPrefix(artifact.ArtifactID, "render-jobs/v1/debug/"+jobID+"/") {
		return fmt.Errorf("invalid PDF preview artifact %q", artifact.Kind)
	}
	expiresAt, err := time.Parse(time.RFC3339, artifact.ExpiresAt)
	if err != nil || expiresAt.IsZero() {
		return fmt.Errorf("invalid PDF preview artifact expiry %q", artifact.Kind)
	}
	extension, mediaType, limit, ok := artifactPolicy(artifact.Kind)
	if !ok || artifact.MediaType != mediaType || artifact.Bytes > limit {
		return fmt.Errorf("invalid PDF preview artifact policy %q", artifact.Kind)
	}
	rootDirectory := path.Dir(entrypoint)
	if rootDirectory == "." {
		rootDirectory = ""
	}
	expectedDirectory := path.Join(rootDirectory, ".rinspace")
	stem := strings.TrimSuffix(path.Base(entrypoint), path.Ext(entrypoint))
	if path.Clean(artifact.Path) != artifact.Path || path.Dir(artifact.Path) != expectedDirectory ||
		path.Base(artifact.Path) != stem+extension {
		return fmt.Errorf("invalid PDF preview artifact path %q", artifact.Path)
	}
	return nil
}

func artifactPolicy(kind string) (extension string, mediaType string, maximum int64, ok bool) {
	switch kind {
	case "pdf":
		return ".pdf", "application/pdf", 32 << 20, true
	case "synctex":
		return ".synctex.gz", "application/gzip", 32 << 20, true
	case "log":
		return ".log", "text/plain; charset=utf-8", 2 << 20, true
	case "aux", "fls", "bbl", "blg", "toc", "out":
		return "." + kind, "text/plain; charset=utf-8", 8 << 20, true
	default:
		return "", "", 0, false
	}
}

func canonicalEntrypoint(value string) bool {
	if value == "" || len([]byte(value)) > 255 || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "-") ||
		strings.Contains(value, `\`) || strings.Contains(value, "..") || strings.ContainsAny(value, "\x00\r\n") || path.Clean(value) != value ||
		!strings.HasSuffix(strings.ToLower(value), ".tex") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
