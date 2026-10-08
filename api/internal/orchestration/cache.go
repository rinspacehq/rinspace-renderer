package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"sort"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

const CacheKeyVersion = "rin-cache-key/v1"

type CacheStage string

const (
	CacheStageAnalysis      CacheStage = "analysis"
	CacheStageDocumentDraft CacheStage = "document-draft"
	CacheStageMath          CacheStage = "math"
	CacheStageDiagram       CacheStage = "diagram"
	CacheStageCode          CacheStage = "code"
	CacheStagePageFinalizer CacheStage = "page-finalizer"
	CacheStageProjectResult CacheStage = "project-result"
)

var cacheStageVersions = map[CacheStage][]string{
	CacheStageAnalysis: {
		"analyzer", "plugin", "project-graph-contract", "compatibility",
	},
	CacheStageDocumentDraft: {
		"document-engine", "plugin", "project-graph-contract", "document-bundle-contract",
		"output-strategy", "compatibility",
	},
	CacheStageMath: {
		"math-engine", "plugin", "font", "sanitizer", "math-contract", "output-strategy", "compatibility",
	},
	CacheStageDiagram: {
		"diagram-engine", "plugin", "font", "sanitizer", "diagram-contract", "output-strategy", "compatibility",
	},
	CacheStageCode: {
		"code-engine", "plugin", "theme", "sanitizer", "code-contract", "output-strategy", "compatibility",
	},
	CacheStagePageFinalizer: {
		"finalizer", "plugin", "font", "theme", "sanitizer", "document-bundle-contract",
		"output-strategy", "compatibility",
	},
	CacheStageProjectResult: {
		"finalizer", "plugin", "font", "theme", "sanitizer", "document-bundle-contract",
		"render-result-contract", "output-strategy", "compatibility",
	},
}

type CacheKeyInput struct {
	Stage            CacheStage
	NormalizedInputs map[string]string
	ProjectContext   map[string]string
	Versions         map[string]string
}

type CacheKey struct {
	Stage   CacheStage
	Version string
	Digest  string
}

func CacheStages() []CacheStage {
	return []CacheStage{
		CacheStageAnalysis, CacheStageDocumentDraft, CacheStageMath, CacheStageDiagram,
		CacheStageCode, CacheStagePageFinalizer, CacheStageProjectResult,
	}
}

func (stage CacheStage) Valid() bool {
	_, ok := cacheStageVersions[stage]
	return ok
}

func RequiredCacheVersions(stage CacheStage) []string {
	values := cacheStageVersions[stage]
	return append([]string(nil), values...)
}

func BuildCacheKey(input CacheKeyInput) (CacheKey, error) {
	if _, ok := cacheStageVersions[input.Stage]; !ok {
		return CacheKey{}, fmt.Errorf("unsupported cache stage %q", input.Stage)
	}
	if len(input.NormalizedInputs) == 0 {
		return CacheKey{}, errors.New("cache key requires normalized inputs")
	}
	for _, required := range cacheStageVersions[input.Stage] {
		if strings.TrimSpace(input.Versions[required]) == "" {
			return CacheKey{}, fmt.Errorf("cache stage %q requires version %q", input.Stage, required)
		}
	}
	for label, fields := range map[string]map[string]string{
		"input": input.NormalizedInputs, "context": input.ProjectContext, "version": input.Versions,
	} {
		if err := validateCacheFields(label, fields); err != nil {
			return CacheKey{}, err
		}
	}
	digest := sha256.New()
	writeCacheField(digest, "key-version", "", CacheKeyVersion)
	writeCacheField(digest, "stage", "", string(input.Stage))
	writeCacheFields(digest, "input", input.NormalizedInputs)
	writeCacheFields(digest, "context", input.ProjectContext)
	writeCacheFields(digest, "version", input.Versions)
	return CacheKey{Stage: input.Stage, Version: CacheKeyVersion, Digest: hex.EncodeToString(digest.Sum(nil))}, nil
}

func (key CacheKey) Validate() error {
	if _, ok := cacheStageVersions[key.Stage]; !ok {
		return fmt.Errorf("unsupported cache stage %q", key.Stage)
	}
	if key.Version != CacheKeyVersion {
		return fmt.Errorf("unsupported cache key version %q", key.Version)
	}
	if len(key.Digest) != 64 || strings.Trim(key.Digest, "0123456789abcdef") != "" {
		return errors.New("cache key digest must be lowercase SHA-256")
	}
	return nil
}

func (key CacheKey) String() string {
	if key.Validate() != nil {
		return ""
	}
	return "rin-cache/v1/" + string(key.Stage) + "/" + key.Digest
}

func CacheArtifactID(stage CacheStage, body []byte) (string, error) {
	if !stage.Valid() {
		return "", fmt.Errorf("unsupported cache artifact stage %q", stage)
	}
	digest := sha256.Sum256(body)
	encoded := hex.EncodeToString(digest[:])
	return "render-cache/v1/" + string(stage) + "/" + encoded[:2] + "/" + encoded, nil
}

type CacheRecord struct {
	Key                   CacheKey
	Artifact              contracts.ArtifactReference
	SchemaVersion         string
	ArtifactSchemaVersion string
	CreatedAt             time.Time
	LastHitAt             time.Time
	ExpiresAt             time.Time
}

type CacheExpectation struct {
	SchemaVersion string
	MediaType     string
	MaxBytes      int64
}

type CacheLookupResult struct {
	Record      CacheRecord
	Body        []byte
	Hit         bool
	Reason      string
	Maintenance string
}

type CacheStoreResult struct {
	Stored bool
	Reason string
}

type Cache interface {
	Lookup(context.Context, CacheKey, CacheExpectation, time.Time) CacheLookupResult
	Store(context.Context, CacheRecord, CacheExpectation, time.Time) CacheStoreResult
}

func validateCacheFields(label string, fields map[string]string) error {
	for name, value := range fields {
		if !validCacheFieldName(name) || strings.TrimSpace(value) == "" {
			return fmt.Errorf("cache %s field %q has an invalid name or empty value", label, name)
		}
	}
	return nil
}

func validCacheFieldName(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value[1:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') &&
			character != '-' && character != '.' {
			return false
		}
	}
	return true
}

func writeCacheFields(digest hash.Hash, label string, fields map[string]string) {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		writeCacheField(digest, label, name, fields[name])
	}
}

func writeCacheField(digest hash.Hash, label, name, value string) {
	for _, part := range []string{label, name, value} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(part))
	}
}
