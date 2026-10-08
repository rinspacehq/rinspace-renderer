package typstpdfexecutor

import (
	"crypto/sha256"
	"encoding/hex"
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
	// ResultSchemaVersion is the cross-module result contract for Typst PDF
	// preview and committed export. It is a strict alternative to the LaTeX
	// preview schema; a Typst job must never be reported as latexmk output.
	ResultSchemaVersion = "rin-typst-pdf/v1"
	// PreviewContractVersion is the job submit contract a draft Typst PDF
	// preview must declare. It is distinct from the stored result schema and
	// must never be satisfied by the LaTeX preview contract.
	PreviewContractVersion = "rin-typst-pdf-preview/v1"
	PreviewOutputKind      = "typst-pdf-preview"
	ExportOutputKind       = "typst-pdf-export"
	ResourceClass          = "typst-pdf"
	WorkspaceRoot          = "/workspace"
	// PolicyID is the fixed isolation and resource policy the manager and
	// Control Plane may request; the worker never accepts a policy from source.
	PolicyID = "rinspace-typst-pdf-safe-v1"

	maxPreviewArtifacts  = 8
	maxTotalArtifactSize = 48 << 20
	pdfArtifactMaxBytes  = 32 << 20
	logArtifactMaxBytes  = 2 << 20
	artifactTTL          = 24 * time.Hour
)

var (
	sha256Pattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	imageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	sessionPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$`)
	profilePattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9._@-]{2,127}$`)
	commitPattern      = regexp.MustCompile(`^[a-f0-9]{40}(?:[a-f0-9]{24})?$`)
	typstArtifactPath  = regexp.MustCompile(`^(?:[^/]+/)*\.rinspace/typst/[^/]+\.(?:pdf|log)$`)
	jobIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{2,127}$`)
	driveLetterPattern = regexp.MustCompile(`^[A-Za-z]:`)
)

// Artifact is one private Typst PDF build product. Unlike the LaTeX preview
// contract, only the PDF and a bounded log are part of the result; TeX aux,
// fls and synctex never appear here.
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Kind       string `json:"kind"`
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	MediaType  string `json:"mediaType"`
	Visibility string `json:"visibility"`
	ExpiresAt  string `json:"expiresAt"`
}

// Cache summarizes whether this result reused an earlier Typst build.
type Cache struct {
	Hit         bool   `json:"hit"`
	ReusedJobID string `json:"reusedJobId,omitempty"`
}

// Versions records the exact renderer, compiler and compiler image that
// produced the result so renderProfileId upgrades never reuse a stale artifact.
type Versions struct {
	RinRenderer   string `json:"rinRenderer"`
	Typst         string `json:"typst"`
	CompilerImage string `json:"compilerImage"`
}

// Result is the Typst PDF preview/export result. Preview results carry a
// session draft identity; export results carry an exact project commit
// identity. The two branches are mutually exclusive.
type Result struct {
	SchemaVersion      string                 `json:"schemaVersion"`
	JobID              string                 `json:"jobId"`
	RequestID          string                 `json:"requestId"`
	ContentKind        string                 `json:"contentKind"`
	OutputKind         string                 `json:"outputKind"`
	DocumentEngine     string                 `json:"documentEngine"`
	ResourceClass      string                 `json:"resourceClass"`
	PriorityClass      string                 `json:"priorityClass"`
	IdempotencyKey     string                 `json:"idempotencyKey"`
	RenderProfileID    string                 `json:"renderProfileId"`
	Entrypoint         string                 `json:"entrypoint"`
	WorkspaceRoot      string                 `json:"workspaceRoot"`
	SnapshotHash       string                 `json:"snapshotHash,omitempty"`
	SessionID          string                 `json:"sessionId,omitempty"`
	DraftRevision      *int64                 `json:"draftRevision,omitempty"`
	EnginePolicyID     string                 `json:"enginePolicyId,omitempty"`
	ImageDigest        string                 `json:"imageDigest,omitempty"`
	ControlProjectID   string                 `json:"controlProjectId,omitempty"`
	SourceCommit       string                 `json:"sourceCommit,omitempty"`
	ControlProjectHash string                 `json:"controlProjectHash,omitempty"`
	Artifacts          []Artifact             `json:"artifacts"`
	TotalArtifactBytes int64                  `json:"totalArtifactBytes"`
	Diagnostics        []renderapi.Diagnostic `json:"diagnostics"`
	Cache              Cache                  `json:"cache"`
	Versions           Versions               `json:"versions"`
}

// PreviewIdempotencyKey derives the Idempotency-Key for a Typst PDF preview
// from its session snapshot identity, matching the protocol validator.
func PreviewIdempotencyKey(sessionID, snapshotHash, entrypoint, enginePolicyID, imageDigest string) string {
	digest := sha256.Sum256([]byte(sessionID + snapshotHash + entrypoint + enginePolicyID + imageDigest))
	return hex.EncodeToString(digest[:])
}

// ExportIdempotencyKey derives the Idempotency-Key for a committed Typst PDF
// export from its published commit identity and pinned render profile. The same
// commit rebuilt under a different profile therefore produces a new key, while
// a repeated request for the same pair stays idempotent.
func ExportIdempotencyKey(controlProjectID, sourceCommit, entrypoint, enginePolicyID, imageDigest string) string {
	digest := sha256.Sum256([]byte(controlProjectID + "\x00" + sourceCommit + "\x00" + entrypoint + "\x00" + enginePolicyID + "\x00" + imageDigest))
	return hex.EncodeToString(digest[:])
}

// Validate enforces the full rin-typst-pdf/v1 contract.
func (result Result) Validate() error {
	if result.SchemaVersion != ResultSchemaVersion || result.ContentKind != "typst" ||
		result.DocumentEngine != "typst" || result.ResourceClass != ResourceClass ||
		result.WorkspaceRoot != WorkspaceRoot {
		return errors.New("Typst PDF result contract identity is invalid")
	}
	if result.OutputKind != PreviewOutputKind && result.OutputKind != ExportOutputKind {
		return errors.New("Typst PDF result output kind is invalid")
	}
	if !jobIDPattern.MatchString(result.JobID) || strings.TrimSpace(result.RequestID) == "" ||
		!sha256Pattern.MatchString(result.IdempotencyKey) || !profilePattern.MatchString(result.RenderProfileID) ||
		!canonicalTypstEntrypoint(result.Entrypoint) {
		return errors.New("Typst PDF result identity is invalid")
	}
	if result.OutputKind == PreviewOutputKind {
		if err := result.validatePreviewIdentity(); err != nil {
			return err
		}
	} else if err := result.validateExportIdentity(); err != nil {
		return err
	}
	if result.Versions.RinRenderer == "" || result.Versions.Typst == "" ||
		!imageDigestPattern.MatchString(result.Versions.CompilerImage) {
		return errors.New("Typst PDF result versions are invalid")
	}
	if result.Artifacts == nil || len(result.Artifacts) < 1 || len(result.Artifacts) > maxPreviewArtifacts ||
		result.Diagnostics == nil {
		return errors.New("Typst PDF result collections are invalid")
	}
	seen := map[string]bool{}
	var total int64
	for _, artifact := range result.Artifacts {
		if err := artifact.validate(result.JobID); err != nil {
			return err
		}
		if seen[artifact.Kind] {
			return fmt.Errorf("duplicate Typst PDF artifact kind %q", artifact.Kind)
		}
		seen[artifact.Kind] = true
		total += artifact.Bytes
		if total > maxTotalArtifactSize {
			return errors.New("Typst PDF artifacts exceed 48 MiB")
		}
	}
	if !seen["pdf"] {
		return errors.New("Typst PDF result is missing the pdf artifact")
	}
	if result.TotalArtifactBytes != total || total < 1 || total > maxTotalArtifactSize {
		return errors.New("Typst PDF total artifact bytes are invalid")
	}
	return nil
}

func (result Result) validatePreviewIdentity() error {
	if result.PriorityClass != "preview" || !sha256Pattern.MatchString(result.SnapshotHash) ||
		!sessionPattern.MatchString(result.SessionID) ||
		result.DraftRevision == nil || *result.DraftRevision < 0 || result.EnginePolicyID != PolicyID ||
		!imageDigestPattern.MatchString(result.ImageDigest) {
		return errors.New("Typst PDF preview identity is invalid")
	}
	if result.ControlProjectID != "" || result.SourceCommit != "" || result.ControlProjectHash != "" {
		return errors.New("Typst PDF preview must not carry a project commit identity")
	}
	if result.IdempotencyKey != PreviewIdempotencyKey(result.SessionID, result.SnapshotHash, result.Entrypoint, result.EnginePolicyID, result.ImageDigest) {
		return errors.New("Typst PDF preview idempotency key does not match its identity")
	}
	return nil
}

func (result Result) validateExportIdentity() error {
	if result.PriorityClass != "publish" && result.PriorityClass != "rebuild" {
		return errors.New("Typst PDF export priority must be publish or rebuild")
	}
	if !canonicalControlProjectID(result.ControlProjectID) || !commitPattern.MatchString(result.SourceCommit) ||
		strings.Trim(result.SourceCommit, "0") == "" || !sha256Pattern.MatchString(result.ControlProjectHash) {
		return errors.New("Typst PDF export commit identity is invalid")
	}
	if result.SessionID != "" || result.SnapshotHash != "" || result.DraftRevision != nil {
		return errors.New("Typst PDF export must not carry a session identity")
	}
	if result.EnginePolicyID != PolicyID || !imageDigestPattern.MatchString(result.ImageDigest) {
		return errors.New("Typst PDF export policy identity is invalid")
	}
	if result.IdempotencyKey != ExportIdempotencyKey(result.ControlProjectID, result.SourceCommit, result.Entrypoint, result.EnginePolicyID, result.ImageDigest) {
		return errors.New("Typst PDF export idempotency key does not match its identity")
	}
	return nil
}

func (artifact Artifact) validate(jobID string) error {
	if strings.TrimSpace(artifact.ArtifactID) == "" || !isCanonicalPath(artifact.ArtifactID) ||
		!strings.HasPrefix(artifact.ArtifactID, "render-jobs/v1/typst/"+jobID+"/") {
		return fmt.Errorf("invalid Typst PDF artifact identity %q", artifact.Kind)
	}
	if artifact.Visibility != "private" || !sha256Pattern.MatchString(artifact.SHA256) || artifact.Bytes <= 0 {
		return fmt.Errorf("invalid Typst PDF artifact %q", artifact.Kind)
	}
	extension, mediaType, limit, ok := artifactPolicy(artifact.Kind)
	if !ok || artifact.MediaType != mediaType || artifact.Bytes > limit {
		return fmt.Errorf("invalid Typst PDF artifact policy %q", artifact.Kind)
	}
	if !isCanonicalPath(artifact.Path) || !typstArtifactPath.MatchString(artifact.Path) ||
		!strings.HasSuffix(artifact.Path, extension) {
		return fmt.Errorf("invalid Typst PDF artifact path %q", artifact.Path)
	}
	if reference := (contracts.ArtifactReference{
		ArtifactID: artifact.ArtifactID, SHA256: artifact.SHA256, Bytes: artifact.Bytes,
		MediaType: artifact.MediaType, Visibility: artifact.Visibility, ExpiresAt: artifact.ExpiresAt,
	}); reference.Validate() != nil {
		return fmt.Errorf("invalid Typst PDF artifact %q", artifact.Kind)
	}
	expiresAt, err := time.Parse(time.RFC3339, artifact.ExpiresAt)
	if err != nil || expiresAt.IsZero() {
		return fmt.Errorf("invalid Typst PDF artifact expiry %q", artifact.Kind)
	}
	return nil
}

func artifactPolicy(kind string) (extension string, mediaType string, maximum int64, ok bool) {
	switch kind {
	case "pdf":
		return ".pdf", "application/pdf", pdfArtifactMaxBytes, true
	case "log":
		return ".log", "text/plain; charset=utf-8", logArtifactMaxBytes, true
	default:
		return "", "", 0, false
	}
}

func canonicalTypstEntrypoint(value string) bool {
	return isCanonicalPath(value) && strings.HasSuffix(strings.ToLower(value), ".typ")
}

// controlProjectPattern is the Control Plane project identity form
// (article:42, book:7, pdf:8, tag-wiki:91). It matches the Renderer
// submission gate so a result can never claim a project the API would not
// have admitted.
var controlProjectPattern = regexp.MustCompile(`^(article|book|tag-wiki|pdf):[1-9][0-9]*$`)

func canonicalControlProjectID(value string) bool {
	return controlProjectPattern.MatchString(value)
}

func isCanonicalPath(value string) bool {
	if value == "" || len([]byte(value)) > 1024 || strings.HasPrefix(value, "/") ||
		strings.Contains(value, `\`) || strings.ContainsAny(value, "\x00\r\n") ||
		strings.Contains(value, "//") || path.Clean(value) != value {
		return false
	}
	if driveLetterPattern.MatchString(value) {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
