package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

type DocumentCapabilities struct {
	Engine       string
	ContentKinds []contracts.ContentKind
	Features     []string
}

type RenderOptions struct {
	Mode   string
	Status string
	Values map[string]any
}

type Analysis struct {
	Entrypoints      []string
	DependencyHashes []string
	WorkloadFeatures map[string]int64
	Diagnostics      []contracts.Diagnostic
}

type ProjectFileReader interface {
	ReadFile(context.Context, string, int64) ([]byte, error)
}

// ProjectSnapshot joins the public, hash-only graph with a private bounded source reader. Source
// bytes remain outside the graph and are verified against the admitted graph before adapter use.
type ProjectSnapshot struct {
	Graph contracts.ProjectGraph
	Files ProjectFileReader
}

func (snapshot ProjectSnapshot) ReadFileVerified(ctx context.Context, projectPath string, maxBytes int64) ([]byte, error) {
	if err := snapshot.Graph.Validate(); err != nil {
		return nil, fmt.Errorf("project graph: %w", err)
	}
	if snapshot.Files == nil {
		return nil, errors.New("project snapshot file reader is nil")
	}
	if maxBytes <= 0 {
		return nil, errors.New("project snapshot max bytes must be positive")
	}
	cleaned, ok := contracts.CleanProjectPath(projectPath)
	if !ok || cleaned != projectPath {
		return nil, fmt.Errorf("project path must be canonical: %q", projectPath)
	}
	var declared *contracts.ProjectGraphFile
	for index := range snapshot.Graph.Files {
		if snapshot.Graph.Files[index].Path == projectPath {
			declared = &snapshot.Graph.Files[index]
			break
		}
	}
	if declared == nil {
		return nil, fmt.Errorf("project file is not declared in graph: %q", projectPath)
	}
	if declared.Bytes > maxBytes {
		return nil, fmt.Errorf("project file %q exceeds read limit", projectPath)
	}
	body, err := snapshot.Files.ReadFile(ctx, projectPath, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("read project file %q: %w", projectPath, err)
	}
	if int64(len(body)) != declared.Bytes {
		return nil, fmt.Errorf("project file %q size mismatch: got %d want %d", projectPath, len(body), declared.Bytes)
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != declared.SHA256 {
		return nil, fmt.Errorf("project file %q hash mismatch", projectPath)
	}
	return body, nil
}

func (snapshot ProjectSnapshot) ReadAllVerified(ctx context.Context, maxFileBytes int64, maxTotalBytes int64) (map[string][]byte, error) {
	if maxFileBytes <= 0 || maxTotalBytes <= 0 {
		return nil, errors.New("project snapshot read limits must be positive")
	}
	if err := snapshot.Graph.Validate(); err != nil {
		return nil, fmt.Errorf("project graph: %w", err)
	}
	var declaredTotal int64
	for _, file := range snapshot.Graph.Files {
		if file.Bytes > maxFileBytes || file.Bytes > maxTotalBytes-declaredTotal {
			return nil, errors.New("project snapshot exceeds read limits")
		}
		declaredTotal += file.Bytes
	}
	files := make(map[string][]byte, len(snapshot.Graph.Files))
	for _, file := range snapshot.Graph.Files {
		body, err := snapshot.ReadFileVerified(ctx, file.Path, maxFileBytes)
		if err != nil {
			return nil, err
		}
		files[file.Path] = body
	}
	return files, nil
}

type ResolvedWork struct {
	Units map[string]ResolvedWorkUnit
}

type ResolvedWorkUnit struct {
	ID          string
	Kind        contracts.WorkUnitKind
	HTML        string
	CSS         []string
	Artifact    *contracts.ArtifactReference
	Diagnostics []contracts.Diagnostic
}

type DocumentAdapter interface {
	Capabilities() DocumentCapabilities
	Analyze(context.Context, ProjectSnapshot, RenderOptions) (Analysis, error)
	Compile(context.Context, ProjectSnapshot, Analysis) (contracts.DocumentBundle, error)
	Finalize(context.Context, contracts.DocumentBundle, ResolvedWork) (contracts.DocumentBundle, error)
}

type CodeService interface {
	ResolveCode(context.Context, CodeBatch) (CodeBatchResult, error)
}

type ArtifactWrite struct {
	ArtifactID    string
	Body          []byte
	MediaType     string
	SchemaVersion string
	ExpiresAt     *time.Time
}

type ArtifactStore interface {
	PutPrivate(context.Context, ArtifactWrite) (contracts.ArtifactReference, error)
	GetPrivate(context.Context, contracts.ArtifactReference) ([]byte, error)
	DeletePrivate(context.Context, contracts.ArtifactReference) error
	PutPublicIfMissing(context.Context, ArtifactWrite) (contracts.ArtifactReference, error)
}

type ArtifactExpectation struct {
	Reference     contracts.ArtifactReference
	MediaType     string
	SchemaVersion string
	MaxBytes      int64
}

type VerifiedArtifactStore interface {
	ArtifactStore
	GetVerified(context.Context, ArtifactExpectation) ([]byte, error)
}

type WorkerHealth struct {
	Name          string
	Ready         bool
	Version       string
	Active        int
	Capacity      int
	LastErrorCode string
}

type WorkerDiagnostics interface {
	Snapshot(context.Context) ([]WorkerHealth, error)
}
