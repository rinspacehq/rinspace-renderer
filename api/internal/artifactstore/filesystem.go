package artifactstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

const metadataSuffix = ".json"

var (
	jobIDPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	sha256Pattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	debugNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

var (
	ErrNotFound       = errors.New("artifact not found")
	ErrExpired        = errors.New("private artifact expired")
	ErrInvalidPath    = errors.New("invalid artifact path")
	ErrMetadata       = errors.New("artifact metadata mismatch")
	ErrSizeLimit      = errors.New("artifact exceeds byte limit")
	ErrPublicOnly     = errors.New("artifact is not in the public namespace")
	ErrPrivateOnly    = errors.New("artifact is not in a private namespace")
	ErrContentChanged = errors.New("immutable artifact content changed")
	ErrSchema         = errors.New("artifact schema mismatch")
)

type Limits struct {
	SourceBytes int64
	ResultBytes int64
	DebugBytes  int64
	CacheBytes  int64
	PublicBytes int64
}

func DefaultLimits() Limits {
	return Limits{
		SourceBytes: 64 << 20,
		ResultBytes: 64 << 20,
		DebugBytes:  128 << 20,
		CacheBytes:  64 << 20,
		PublicBytes: 16 << 20,
	}
}

type FileStore struct {
	privateRoot  string
	publicRoot   string
	metadataRoot string
	limits       Limits
	now          func() time.Time
}

type storedMetadata struct {
	Reference     contracts.ArtifactReference `json:"reference"`
	SchemaVersion string                      `json:"schemaVersion,omitempty"`
}

var _ orchestration.ArtifactStore = (*FileStore)(nil)

func NewFileStore(root string, limits Limits) (*FileStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("artifact store root is required")
	}
	if limits.SourceBytes <= 0 || limits.ResultBytes <= 0 || limits.DebugBytes <= 0 || limits.CacheBytes <= 0 || limits.PublicBytes <= 0 {
		return nil, errors.New("artifact store limits must be positive")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact store root: %w", err)
	}
	store := &FileStore{
		privateRoot:  filepath.Join(absolute, "objects", "private"),
		publicRoot:   filepath.Join(absolute, "objects", "public"),
		metadataRoot: filepath.Join(absolute, "metadata", "private"),
		limits:       limits,
		now:          time.Now,
	}
	for _, directory := range []string{store.privateRoot, store.publicRoot, store.metadataRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create artifact store directory: %w", err)
		}
	}
	return store, nil
}

func (store *FileStore) Ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("artifact_unavailable: %w", err)
	}
	probe, err := os.CreateTemp(store.metadataRoot, ".readiness-")
	if err != nil {
		return fmt.Errorf("artifact_unavailable: %w", err)
	}
	name := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("artifact_unavailable: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("artifact_unavailable: %w", err)
	}
	return nil
}

func (store *FileStore) PutPrivate(ctx context.Context, write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	kind, _, _, err := parsePrivateID(write.ArtifactID)
	if err != nil {
		return contracts.ArtifactReference{}, err
	}
	if write.ExpiresAt == nil {
		return contracts.ArtifactReference{}, errors.New("private artifact expiry is required")
	}
	if !write.ExpiresAt.After(store.now()) {
		return contracts.ArtifactReference{}, ErrExpired
	}
	limit := store.privateLimit(kind)
	if err := validateWrite(write, limit); err != nil {
		return contracts.ArtifactReference{}, err
	}
	digest := sha256Hex(write.Body)
	if err := validatePrivateDigest(kind, write.ArtifactID, digest); err != nil {
		return contracts.ArtifactReference{}, err
	}
	reference := contracts.ArtifactReference{
		ArtifactID: write.ArtifactID,
		SHA256:     digest,
		Bytes:      int64(len(write.Body)),
		MediaType:  strings.TrimSpace(write.MediaType),
		Visibility: "private",
		ExpiresAt:  write.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if err := reference.Validate(); err != nil {
		return contracts.ArtifactReference{}, fmt.Errorf("private artifact reference: %w", err)
	}
	metadata, err := json.Marshal(storedMetadata{Reference: reference, SchemaVersion: write.SchemaVersion})
	if err != nil {
		return contracts.ArtifactReference{}, fmt.Errorf("encode private artifact metadata: %w", err)
	}
	if err := contextError(ctx); err != nil {
		return contracts.ArtifactReference{}, err
	}
	objectPath := store.privatePath(write.ArtifactID)
	created, err := putFileIfMissing(objectPath, write.Body, 0o600)
	if err != nil {
		return contracts.ArtifactReference{}, err
	}
	if _, err := putFileIfMissing(store.metadataPath(write.ArtifactID), metadata, 0o600); err != nil {
		if created {
			_ = os.Remove(objectPath)
		}
		return contracts.ArtifactReference{}, err
	}
	return reference, nil
}

func (store *FileStore) GetPrivate(ctx context.Context, reference contracts.ArtifactReference) ([]byte, error) {
	if err := reference.Validate(); err != nil {
		return nil, fmt.Errorf("private artifact reference: %w", err)
	}
	if reference.Visibility != "private" {
		return nil, ErrPrivateOnly
	}
	kind, _, _, err := parsePrivateID(reference.ArtifactID)
	if err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	metadata, err := store.readMetadata(reference.ArtifactID)
	if err != nil {
		return nil, err
	}
	if metadata.Reference != reference {
		return nil, ErrMetadata
	}
	expiresAt, err := time.Parse(time.RFC3339, metadata.Reference.ExpiresAt)
	if err != nil {
		return nil, ErrMetadata
	}
	if !expiresAt.After(store.now()) {
		return nil, ErrExpired
	}
	body, err := readBoundedFile(store.privatePath(reference.ArtifactID), store.privateLimit(kind))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != reference.Bytes || sha256Hex(body) != reference.SHA256 {
		return nil, ErrMetadata
	}
	return body, nil
}

func (store *FileStore) GetVerified(ctx context.Context, expected orchestration.ArtifactExpectation) ([]byte, error) {
	if err := expected.Reference.Validate(); err != nil {
		return nil, fmt.Errorf("verified artifact reference: %w", err)
	}
	if strings.TrimSpace(expected.MediaType) == "" || strings.TrimSpace(expected.SchemaVersion) == "" || expected.MaxBytes <= 0 {
		return nil, errors.New("verified artifact expectation is incomplete")
	}
	if expected.Reference.MediaType != expected.MediaType || expected.Reference.Bytes > expected.MaxBytes {
		return nil, ErrMetadata
	}
	if expected.Reference.Visibility == "private" {
		metadata, err := store.readMetadata(expected.Reference.ArtifactID)
		if err != nil {
			return nil, err
		}
		if metadata.SchemaVersion != expected.SchemaVersion {
			return nil, ErrSchema
		}
		return store.GetPrivate(ctx, expected.Reference)
	}
	if expected.Reference.Visibility != "public" {
		return nil, ErrMetadata
	}
	if _, err := parsePublicID(expected.Reference.ArtifactID); err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	body, err := readBoundedFile(store.publicPath(expected.Reference.ArtifactID), expected.MaxBytes)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != expected.Reference.Bytes || sha256Hex(body) != expected.Reference.SHA256 {
		return nil, ErrMetadata
	}
	return body, nil
}

func (store *FileStore) DeletePrivate(ctx context.Context, reference contracts.ArtifactReference) error {
	if reference.Visibility != "private" {
		return ErrPrivateOnly
	}
	if _, _, _, err := parsePrivateID(reference.ArtifactID); err != nil {
		return err
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	for _, path := range []string{store.privatePath(reference.ArtifactID), store.metadataPath(reference.ArtifactID)} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("delete private artifact: %w", err)
		}
	}
	return nil
}

func (store *FileStore) PutPublicIfMissing(ctx context.Context, write orchestration.ArtifactWrite) (contracts.ArtifactReference, error) {
	digest, err := parsePublicID(write.ArtifactID)
	if err != nil {
		return contracts.ArtifactReference{}, err
	}
	if write.ExpiresAt != nil {
		return contracts.ArtifactReference{}, errors.New("public immutable artifact cannot expire")
	}
	if err := validateWrite(write, store.limits.PublicBytes); err != nil {
		return contracts.ArtifactReference{}, err
	}
	if actual := sha256Hex(write.Body); actual != digest {
		return contracts.ArtifactReference{}, ErrMetadata
	}
	if err := contextError(ctx); err != nil {
		return contracts.ArtifactReference{}, err
	}
	if _, err := putFileIfMissing(store.publicPath(write.ArtifactID), write.Body, 0o644); err != nil {
		return contracts.ArtifactReference{}, err
	}
	reference := contracts.ArtifactReference{
		ArtifactID: write.ArtifactID,
		SHA256:     digest,
		Bytes:      int64(len(write.Body)),
		MediaType:  strings.TrimSpace(write.MediaType),
		Visibility: "public",
	}
	if err := reference.Validate(); err != nil {
		return contracts.ArtifactReference{}, fmt.Errorf("public artifact reference: %w", err)
	}
	return reference, nil
}

// PublicObjectPath maps only the immutable public diagram namespace. Private IDs are rejected even
// when the caller knows the exact storage key.
func (store *FileStore) PublicObjectPath(artifactID string) (string, error) {
	if _, err := parsePublicID(artifactID); err != nil {
		return "", ErrPublicOnly
	}
	return store.publicPath(artifactID), nil
}

func (store *FileStore) CleanupExpired(ctx context.Context, now time.Time, maxDeletes int) (int, error) {
	if maxDeletes <= 0 {
		return 0, errors.New("cleanup maxDeletes must be positive")
	}
	deleted := 0
	err := filepath.WalkDir(store.metadataRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := contextError(ctx); err != nil {
			return err
		}
		if deleted >= maxDeletes {
			return filepath.SkipAll
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), metadataSuffix) {
			return nil
		}
		body, err := readBoundedFile(path, 64<<10)
		if err != nil {
			return err
		}
		var metadata storedMetadata
		if err := json.Unmarshal(body, &metadata); err != nil {
			return fmt.Errorf("decode private artifact metadata during cleanup: %w", err)
		}
		if metadata.Reference.Visibility != "private" {
			return ErrMetadata
		}
		relative, err := filepath.Rel(store.metadataRoot, path)
		if err != nil || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return ErrMetadata
		}
		artifactID := strings.TrimSuffix(filepath.ToSlash(relative), metadataSuffix)
		if metadata.Reference.ArtifactID != artifactID {
			return ErrMetadata
		}
		expiresAt, err := time.Parse(time.RFC3339, metadata.Reference.ExpiresAt)
		if err != nil {
			return ErrMetadata
		}
		if expiresAt.After(now) {
			return nil
		}
		if err := store.DeletePrivate(ctx, metadata.Reference); err != nil {
			return err
		}
		deleted++
		return nil
	})
	if err != nil {
		return deleted, fmt.Errorf("cleanup private artifacts: %w", err)
	}
	return deleted, nil
}

func (store *FileStore) readMetadata(artifactID string) (storedMetadata, error) {
	body, err := readBoundedFile(store.metadataPath(artifactID), 64<<10)
	if err != nil {
		return storedMetadata{}, err
	}
	var metadata storedMetadata
	if err := json.Unmarshal(body, &metadata); err != nil {
		return storedMetadata{}, ErrMetadata
	}
	if err := metadata.Reference.Validate(); err != nil {
		return storedMetadata{}, ErrMetadata
	}
	return metadata, nil
}

func (store *FileStore) privateLimit(kind string) int64 {
	switch kind {
	case "source":
		return store.limits.SourceBytes
	case "result":
		return store.limits.ResultBytes
	case "cache":
		return store.limits.CacheBytes
	default:
		return store.limits.DebugBytes
	}
}

func (store *FileStore) privatePath(artifactID string) string {
	return filepath.Join(store.privateRoot, filepath.FromSlash(artifactID))
}

func (store *FileStore) publicPath(artifactID string) string {
	return filepath.Join(store.publicRoot, filepath.FromSlash(artifactID))
}

func (store *FileStore) metadataPath(artifactID string) string {
	return filepath.Join(store.metadataRoot, filepath.FromSlash(artifactID)+metadataSuffix)
}

func parsePrivateID(artifactID string) (kind string, jobID string, leaf string, err error) {
	if artifactID != strings.TrimSpace(artifactID) || strings.Contains(artifactID, `\`) || strings.HasPrefix(artifactID, "/") {
		return "", "", "", ErrInvalidPath
	}
	parts := strings.Split(artifactID, "/")
	if len(parts) == 5 && parts[0] == "render-cache" && parts[1] == "v1" {
		stage := orchestration.CacheStage(parts[2])
		leaf = parts[4]
		if !stage.Valid() || !sha256Pattern.MatchString(leaf) || parts[3] != leaf[:2] {
			return "", "", "", ErrInvalidPath
		}
		return "cache", string(stage), leaf, nil
	}
	if len(parts) != 5 || parts[0] != "render-jobs" || parts[1] != "v1" || !jobIDPattern.MatchString(parts[3]) {
		return "", "", "", ErrPrivateOnly
	}
	kind, jobID, leaf = parts[2], parts[3], parts[4]
	if kind != "source" && kind != "result" && kind != "debug" {
		return "", "", "", ErrPrivateOnly
	}
	switch kind {
	case "source":
		if !sha256Pattern.MatchString(leaf) {
			return "", "", "", ErrInvalidPath
		}
	case "result":
		if !strings.HasSuffix(leaf, ".json") || !sha256Pattern.MatchString(strings.TrimSuffix(leaf, ".json")) {
			return "", "", "", ErrInvalidPath
		}
	case "debug":
		if !debugNamePattern.MatchString(leaf) || leaf == "." || leaf == ".." {
			return "", "", "", ErrInvalidPath
		}
	}
	return kind, jobID, leaf, nil
}

func validatePrivateDigest(kind string, artifactID string, digest string) error {
	_, _, leaf, err := parsePrivateID(artifactID)
	if err != nil {
		return err
	}
	if kind == "source" && leaf != digest {
		return ErrMetadata
	}
	if kind == "result" && strings.TrimSuffix(leaf, ".json") != digest {
		return ErrMetadata
	}
	return nil
}

func parsePublicID(artifactID string) (string, error) {
	if artifactID != strings.TrimSpace(artifactID) || strings.Contains(artifactID, `\`) {
		return "", ErrInvalidPath
	}
	parts := strings.Split(artifactID, "/")
	if len(parts) != 5 || parts[0] != "diagrams" || parts[1] != "v1" || parts[2] != "svg-sha256" {
		return "", ErrPublicOnly
	}
	leaf := parts[4]
	if !strings.HasSuffix(leaf, ".svg") {
		return "", ErrInvalidPath
	}
	digest := strings.TrimSuffix(leaf, ".svg")
	if !sha256Pattern.MatchString(digest) || parts[3] != digest[:2] {
		return "", ErrInvalidPath
	}
	return digest, nil
}

func validateWrite(write orchestration.ArtifactWrite, limit int64) error {
	if strings.TrimSpace(write.MediaType) == "" {
		return errors.New("artifact media type is required")
	}
	if int64(len(write.Body)) > limit {
		return ErrSizeLimit
	}
	return nil
}

func putFileIfMissing(path string, body []byte, mode os.FileMode) (bool, error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return false, fmt.Errorf("create artifact directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".rin-artifact-*")
	if err != nil {
		return false, fmt.Errorf("create artifact temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return false, fmt.Errorf("set artifact file mode: %w", err)
	}
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return false, fmt.Errorf("write artifact: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return false, fmt.Errorf("sync artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return false, fmt.Errorf("close artifact: %w", err)
	}
	if err := os.Link(temporaryPath, path); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrExist) {
		return false, fmt.Errorf("commit artifact: %w", err)
	}
	existing, err := readBoundedFile(path, int64(len(body))+1)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(existing, body) {
		return false, ErrContentChanged
	}
	return false, nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open artifact: %w", err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read artifact: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, ErrSizeLimit
	}
	return body, nil
}

func sha256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func contextError(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
