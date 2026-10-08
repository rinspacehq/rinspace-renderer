package codeservice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

const (
	DefaultTheme                = "github-light"
	ShikiVersion                = "4.4.2"
	DefaultPluginVersion        = "rin-markdown-code/v1"
	DefaultSanitizerVersion     = "rin-markdown-final-sanitizer/v2"
	DefaultCompatibilityVersion = "rin-reader-code/v1"
	DefaultMaxBatchSize         = 512
	DefaultMaxSourceBytes       = int64(512 << 10)
	DefaultMaxCacheBytes        = int64(4 << 20)
	codeCacheMediaType          = "application/vnd.rin.code-cache+json"
)

type RenderRequest struct {
	ID       string `json:"id"`
	Source   string `json:"source"`
	Language string `json:"language,omitempty"`
}

type RenderDiagnostic struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type RenderResult struct {
	ID                string             `json:"id"`
	HTML              string             `json:"html"`
	CanonicalLanguage string             `json:"canonicalLanguage,omitempty"`
	Highlighted       bool               `json:"highlighted"`
	Diagnostics       []RenderDiagnostic `json:"diagnostics"`
}

type Renderer interface {
	RenderBatch(context.Context, string, []RenderRequest) ([]RenderResult, error)
}

type Config struct {
	Renderer             Renderer
	Cache                orchestration.Cache
	Store                orchestration.ArtifactStore
	EngineVersion        string
	Theme                string
	PluginVersion        string
	SanitizerVersion     string
	CompatibilityVersion string
	MaxBatchSize         int
	MaxSourceBytes       int64
	MaxCacheBytes        int64
	CacheTTL             time.Duration
	Now                  func() time.Time
}

type Service struct{ config Config }

type cachedRender struct {
	SchemaVersion     string             `json:"schemaVersion"`
	HTML              string             `json:"html"`
	CanonicalLanguage string             `json:"canonicalLanguage,omitempty"`
	Highlighted       bool               `json:"highlighted"`
	Diagnostics       []RenderDiagnostic `json:"diagnostics"`
}

func New(config Config) (*Service, error) {
	if config.Renderer == nil {
		return nil, errors.New("code service requires a Shiki renderer")
	}
	if config.EngineVersion != ShikiVersion {
		return nil, fmt.Errorf("code service requires pinned Shiki version %s", ShikiVersion)
	}
	if config.Theme == "" {
		config.Theme = DefaultTheme
	}
	if config.Theme != DefaultTheme {
		return nil, fmt.Errorf("unsupported code theme %q", config.Theme)
	}
	if config.PluginVersion == "" {
		config.PluginVersion = DefaultPluginVersion
	}
	if config.SanitizerVersion == "" {
		config.SanitizerVersion = DefaultSanitizerVersion
	}
	if config.CompatibilityVersion == "" {
		config.CompatibilityVersion = DefaultCompatibilityVersion
	}
	if config.MaxBatchSize < 0 || config.MaxSourceBytes < 0 || config.MaxCacheBytes < 0 || config.CacheTTL < 0 {
		return nil, errors.New("code service limits cannot be negative")
	}
	if config.MaxBatchSize == 0 {
		config.MaxBatchSize = DefaultMaxBatchSize
	}
	if config.MaxSourceBytes == 0 {
		config.MaxSourceBytes = DefaultMaxSourceBytes
	}
	if config.MaxCacheBytes == 0 {
		config.MaxCacheBytes = DefaultMaxCacheBytes
	}
	if config.CacheTTL == 0 {
		config.CacheTTL = 30 * 24 * time.Hour
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if (config.Cache == nil) != (config.Store == nil) {
		return nil, errors.New("code cache and artifact store must be configured together")
	}
	return &Service{config: config}, nil
}

func (service *Service) ResolveCode(ctx context.Context, batch orchestration.CodeBatch) (orchestration.CodeBatchResult, error) {
	if service == nil {
		return orchestration.CodeBatchResult{}, errors.New("code service is not configured")
	}
	if err := batch.Validate(); err != nil {
		return orchestration.CodeBatchResult{}, err
	}
	if batch.Theme != service.config.Theme {
		return orchestration.CodeBatchResult{}, fmt.Errorf("unsupported code theme %q", batch.Theme)
	}
	if len(batch.Items) > service.config.MaxBatchSize {
		return orchestration.CodeBatchResult{}, fmt.Errorf("code batch exceeds %d items", service.config.MaxBatchSize)
	}
	result := orchestration.CodeBatchResult{
		ContractVersion: orchestration.CodeResultContractVersion,
		Units:           make([]orchestration.CodeResolvedUnit, len(batch.Items)),
		CSS:             []string{},
		Versions: map[string]string{
			"engine": "shiki", "engine-version": service.config.EngineVersion,
			"plugin": service.config.PluginVersion, "theme": service.config.Theme,
			"sanitizer": service.config.SanitizerVersion, "compatibility": service.config.CompatibilityVersion,
			"batch-contract":   orchestration.CodeBatchContractVersion,
			"result-contract":  orchestration.CodeResultContractVersion,
			"wrapper-contract": orchestration.CodeWrapperContract,
			"cache-schema":     orchestration.CodeCacheSchemaVersion,
		},
	}
	type target struct {
		key     orchestration.CacheKey
		request RenderRequest
		indexes []int
	}
	targets := make([]target, 0, len(batch.Items))
	byDigest := map[string]int{}
	for index, unit := range batch.Items {
		key, err := service.cacheKey(unit)
		if err != nil {
			return orchestration.CodeBatchResult{}, err
		}
		result.Units[index] = baseResolved(unit, key)
		if int64(len([]byte(unit.Source))) > service.config.MaxSourceBytes {
			applyCached(&result.Units[index], plainFallback(unit.Source, unit.Language, "code.source.too_large", "Code source exceeds byte limit."), "miss", unit.SourceLocation)
			continue
		}
		if cached, hit := service.cached(ctx, key); hit {
			applyCached(&result.Units[index], cached, "hit", unit.SourceLocation)
			continue
		}
		if existing, ok := byDigest[key.Digest]; ok {
			targets[existing].indexes = append(targets[existing].indexes, index)
			continue
		}
		byDigest[key.Digest] = len(targets)
		targets = append(targets, target{
			key: key, request: RenderRequest{ID: unit.ID, Source: unit.Source, Language: unit.Language}, indexes: []int{index},
		})
	}

	for start := 0; start < len(targets); start += service.config.MaxBatchSize {
		end := start + service.config.MaxBatchSize
		if end > len(targets) {
			end = len(targets)
		}
		requests := make([]RenderRequest, end-start)
		for index := start; index < end; index++ {
			requests[index-start] = targets[index].request
		}
		rendered, renderErr := service.config.Renderer.RenderBatch(ctx, batch.Theme, requests)
		if renderErr == nil && len(rendered) != len(requests) {
			renderErr = errors.New("Shiki renderer returned wrong batch size")
		}
		for targetIndex := start; targetIndex < end; targetIndex++ {
			current := targets[targetIndex]
			var cached cachedRender
			if renderErr != nil {
				cached = plainFallback(current.request.Source, current.request.Language, "code.worker.failed", renderErr.Error())
			} else {
				item := rendered[targetIndex-start]
				if item.ID != current.request.ID || strings.TrimSpace(item.HTML) == "" {
					return orchestration.CodeBatchResult{}, errors.New("Shiki renderer returned mismatched or empty output")
				}
				cached = cachedRender{
					SchemaVersion: orchestration.CodeCacheSchemaVersion, HTML: item.HTML,
					CanonicalLanguage: item.CanonicalLanguage, Highlighted: item.Highlighted,
					Diagnostics: append([]RenderDiagnostic(nil), item.Diagnostics...),
				}
			}
			if !validCached(cached, service.config.MaxCacheBytes) {
				return orchestration.CodeBatchResult{}, errors.New("Shiki renderer returned invalid or oversized output")
			}
			if renderErr == nil {
				service.storeCached(ctx, current.key, cached)
			}
			for offset, index := range current.indexes {
				status := "hit"
				if offset == 0 {
					status = "miss"
				}
				applyCached(&result.Units[index], cached, status, batch.Items[index].SourceLocation)
			}
		}
	}
	if err := result.Validate(batch); err != nil {
		return orchestration.CodeBatchResult{}, fmt.Errorf("validate code service result: %w", err)
	}
	return result, nil
}

func (service *Service) cacheKey(unit contracts.WorkUnit) (orchestration.CacheKey, error) {
	return orchestration.BuildCacheKey(orchestration.CacheKeyInput{
		Stage: orchestration.CacheStageCode,
		NormalizedInputs: map[string]string{
			"source": firstNonEmpty(unit.Source, "<rin-empty>"), "source-present": presentValue(unit.Source),
			"language": firstNonEmpty(unit.Language, "<rin-plain>"), "language-present": presentValue(unit.Language),
		},
		Versions: map[string]string{
			"code-engine": "shiki@" + service.config.EngineVersion, "plugin": service.config.PluginVersion,
			"theme": service.config.Theme, "sanitizer": service.config.SanitizerVersion,
			"code-contract":   orchestration.CodeBatchContractVersion + "+" + orchestration.CodeResultContractVersion + "+" + orchestration.CodeWrapperContract + "+" + orchestration.CodeCacheSchemaVersion,
			"output-strategy": "html", "compatibility": service.config.CompatibilityVersion,
		},
	})
}

func (service *Service) cached(ctx context.Context, key orchestration.CacheKey) (cachedRender, bool) {
	if service.config.Cache == nil {
		return cachedRender{}, false
	}
	lookup := service.config.Cache.Lookup(ctx, key, orchestration.CacheExpectation{
		SchemaVersion: orchestration.CodeCacheSchemaVersion, MediaType: codeCacheMediaType,
		MaxBytes: service.config.MaxCacheBytes,
	}, service.config.Now())
	if !lookup.Hit || len(lookup.Body) == 0 {
		return cachedRender{}, false
	}
	var cached cachedRender
	if json.Unmarshal(lookup.Body, &cached) != nil || !validCached(cached, service.config.MaxCacheBytes) {
		return cachedRender{}, false
	}
	return cached, true
}

func (service *Service) storeCached(ctx context.Context, key orchestration.CacheKey, cached cachedRender) {
	if service.config.Cache == nil {
		return
	}
	body, err := json.Marshal(cached)
	if err != nil || int64(len(body)) > service.config.MaxCacheBytes {
		return
	}
	artifactID, err := orchestration.CacheArtifactID(orchestration.CacheStageCode, body)
	if err != nil {
		return
	}
	reference, err := service.config.Store.PutPrivate(ctx, orchestration.ArtifactWrite{
		ArtifactID: artifactID, Body: body, MediaType: codeCacheMediaType,
		SchemaVersion: orchestration.CodeCacheSchemaVersion,
	})
	if err != nil {
		return
	}
	now := service.config.Now()
	service.config.Cache.Store(ctx, orchestration.CacheRecord{
		Key: key, Artifact: reference, SchemaVersion: orchestration.CodeCacheSchemaVersion,
		ArtifactSchemaVersion: orchestration.CodeCacheSchemaVersion,
		CreatedAt:             now, LastHitAt: now, ExpiresAt: now.Add(service.config.CacheTTL),
	}, orchestration.CacheExpectation{
		SchemaVersion: orchestration.CodeCacheSchemaVersion, MediaType: codeCacheMediaType,
		MaxBytes: service.config.MaxCacheBytes,
	}, now)
}

func baseResolved(unit contracts.WorkUnit, key orchestration.CacheKey) orchestration.CodeResolvedUnit {
	return orchestration.CodeResolvedUnit{
		ID: unit.ID, Engine: "shiki", Theme: DefaultTheme,
		Source: orchestration.CodeSourceMetadata{
			Source: unit.Source, Language: unit.Language, Meta: unit.Meta, SourceLocation: unit.SourceLocation,
		},
		Cache:       orchestration.CodeCacheResult{Status: "miss", KeyVersion: key.Version, Digest: key.Digest},
		Diagnostics: []contracts.Diagnostic{},
	}
}

func applyCached(target *orchestration.CodeResolvedUnit, cached cachedRender, status string, location *contracts.SourceLocation) {
	target.State = "succeeded"
	target.HTML = cached.HTML
	target.Engine = "shiki"
	target.EngineVersion = ShikiVersion
	target.Theme = DefaultTheme
	target.CanonicalLanguage = cached.CanonicalLanguage
	target.Highlighted = cached.Highlighted
	target.Cache.Status = status
	for _, diagnostic := range cached.Diagnostics {
		target.Diagnostics = append(target.Diagnostics, contracts.Diagnostic{
			Code:     firstNonEmpty(diagnostic.Code, "code.worker.message"),
			Severity: cleanSeverity(diagnostic.Severity), Message: firstNonEmpty(diagnostic.Message, "Code worker message."),
			Stage: "code", SourceLocation: location,
		})
	}
}

func plainFallback(source, language, code, message string) cachedRender {
	label := firstNonEmpty(language, "text")
	return cachedRender{
		SchemaVersion: orchestration.CodeCacheSchemaVersion,
		HTML:          `<pre class="rin-code-pre rin-code-plain" data-rin-code-language="` + stdhtml.EscapeString(label) + `"><code>` + stdhtml.EscapeString(source) + `</code></pre>`,
		Diagnostics:   []RenderDiagnostic{{Code: code, Severity: "warning", Message: message}},
	}
}

func validCached(cached cachedRender, maxBytes int64) bool {
	if cached.SchemaVersion != orchestration.CodeCacheSchemaVersion || strings.TrimSpace(cached.HTML) == "" || int64(len([]byte(cached.HTML))) > maxBytes {
		return false
	}
	if cached.Highlighted && strings.TrimSpace(cached.CanonicalLanguage) == "" {
		return false
	}
	for _, diagnostic := range cached.Diagnostics {
		if strings.TrimSpace(diagnostic.Code) == "" || strings.TrimSpace(diagnostic.Message) == "" {
			return false
		}
	}
	return true
}

func cleanSeverity(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "error":
		return "error"
	case "info":
		return "info"
	default:
		return "warning"
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func presentValue(value string) string {
	if value == "" {
		return "false"
	}
	return "true"
}
