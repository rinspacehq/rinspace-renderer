package diagramservice

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/diagramengine"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderstorage"
)

const (
	DefaultPluginVersion        = "rin-diagram-input/v1"
	DefaultSanitizerVersion     = "rin-svg-sanitizer/v1"
	DefaultCompatibilityVersion = "rin-reader-diagram/v1"
	DefaultMaxBatchSize         = 512
	DefaultMaxSourceBytes       = int64(128 << 10)
	DefaultMaxOptionsBytes      = int64(8 << 10)
	DefaultMaxSVGBytes          = int64(4 << 20)
	DefaultMemoryCacheEntries   = 4096
	DefaultParallelism          = 2
	DefaultTimeout              = 25 * time.Second
)

type Renderer interface {
	Render(context.Context, diagramengine.Request) (diagramengine.Response, error)
}

type PublicStore interface {
	PutPublicObjectIfMissing(context.Context, string, string, []byte) (renderstorage.PutResult, error)
	ReadPublicObject(context.Context, string, int64) ([]byte, error)
	PublicObjectURL(string) string
}

type Cache interface {
	Get(string) (CachedRender, bool)
	Put(string, CachedRender)
	Delete(string)
}

type CachedDiagnostic struct {
	Code     string
	Severity string
	Message  string
}

type CachedRender struct {
	SchemaVersion string
	DiagramID     string
	DiagramType   string
	SVGHash       string
	ObjectID      string
	SVGBytes      int64
	Uploaded      bool
	EngineCached  bool
	RenderURL     string
	Diagnostics   []CachedDiagnostic
}

type Config struct {
	Renderer             Renderer
	Store                PublicStore
	Cache                Cache
	EngineVersion        string
	FontVersion          string
	PluginVersion        string
	SanitizerVersion     string
	CompatibilityVersion string
	MaxBatchSize         int
	MaxSourceBytes       int64
	MaxOptionsBytes      int64
	MaxSVGBytes          int64
	Parallelism          int
	Timeout              time.Duration
	Pool                 *ResourcePool
}

type Service struct {
	config   Config
	pool     *ResourcePool
	flightMu sync.Mutex
	flights  map[string]*diagramFlight
}

// ResourcePool is process-safe and can be shared by multiple API adapters so direct requests and
// durable project executors consume the same TeX SVG capacity.
type ResourcePool struct{ slots chan struct{} }

func NewResourcePool(parallelism int) (*ResourcePool, error) {
	if parallelism <= 0 {
		return nil, errors.New("diagram resource pool parallelism must be positive")
	}
	return &ResourcePool{slots: make(chan struct{}, parallelism)}, nil
}

func (pool *ResourcePool) Capacity() int {
	if pool == nil {
		return 0
	}
	return cap(pool.slots)
}

func (pool *ResourcePool) Active() int {
	if pool == nil {
		return 0
	}
	return len(pool.slots)
}

type diagramFlight struct {
	done   chan struct{}
	result CachedRender
	err    error
}

func New(config Config) (*Service, error) {
	if config.Renderer == nil || config.Store == nil {
		return nil, errors.New("diagram service requires renderer and public store")
	}
	if strings.TrimSpace(config.EngineVersion) == "" || strings.TrimSpace(config.FontVersion) == "" {
		return nil, errors.New("diagram service requires engine and font versions")
	}
	if strings.TrimSpace(config.PluginVersion) == "" {
		config.PluginVersion = DefaultPluginVersion
	}
	if strings.TrimSpace(config.SanitizerVersion) == "" {
		config.SanitizerVersion = DefaultSanitizerVersion
	}
	if strings.TrimSpace(config.CompatibilityVersion) == "" {
		config.CompatibilityVersion = DefaultCompatibilityVersion
	}
	if config.MaxBatchSize < 0 || config.MaxSourceBytes < 0 || config.MaxOptionsBytes < 0 ||
		config.MaxSVGBytes < 0 || config.Parallelism < 0 || config.Timeout < 0 {
		return nil, errors.New("diagram service limits cannot be negative")
	}
	if config.MaxBatchSize == 0 {
		config.MaxBatchSize = DefaultMaxBatchSize
	}
	if config.MaxSourceBytes == 0 {
		config.MaxSourceBytes = DefaultMaxSourceBytes
	}
	if config.MaxOptionsBytes == 0 {
		config.MaxOptionsBytes = DefaultMaxOptionsBytes
	}
	if config.MaxSVGBytes == 0 {
		config.MaxSVGBytes = DefaultMaxSVGBytes
	}
	if config.Parallelism == 0 {
		config.Parallelism = DefaultParallelism
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	pool := config.Pool
	if pool == nil {
		var err error
		pool, err = NewResourcePool(config.Parallelism)
		if err != nil {
			return nil, err
		}
	} else if pool.Capacity() != config.Parallelism {
		return nil, errors.New("diagram resource pool capacity does not match configured parallelism")
	}
	return &Service{
		config: config, pool: pool,
		flights: make(map[string]*diagramFlight),
	}, nil
}

func (service *Service) ResolveDiagrams(ctx context.Context, batch orchestration.DiagramBatch) (orchestration.DiagramBatchResult, error) {
	if service == nil {
		return orchestration.DiagramBatchResult{}, errors.New("diagram service is not configured")
	}
	if err := batch.Validate(); err != nil {
		return orchestration.DiagramBatchResult{}, err
	}
	if len(batch.Items) > service.config.MaxBatchSize {
		return orchestration.DiagramBatchResult{}, fmt.Errorf("diagram batch exceeds %d items", service.config.MaxBatchSize)
	}
	result := orchestration.DiagramBatchResult{
		ContractVersion: orchestration.DiagramResultContractVersion,
		Units:           make([]orchestration.DiagramResolvedUnit, len(batch.Items)),
		Versions: map[string]string{
			"engine": "rin-texsvg", "engine-version": service.config.EngineVersion,
			"font": service.config.FontVersion, "plugin": service.config.PluginVersion,
			"sanitizer": service.config.SanitizerVersion, "compatibility": service.config.CompatibilityVersion,
			"batch-contract":    orchestration.DiagramBatchContractVersion,
			"result-contract":   orchestration.DiagramResultContractVersion,
			"artifact-contract": orchestration.DiagramArtifactContract,
			"cache-schema":      orchestration.DiagramCacheSchemaVersion,
		},
	}
	type target struct {
		key     orchestration.CacheKey
		item    orchestration.DiagramBatchItem
		indexes []int
	}
	targets := make([]target, 0, len(batch.Items))
	byDigest := map[string]int{}
	for index, input := range batch.Items {
		kind, ok := NormalizeType(input.Unit.DiagramType)
		if !ok || kind != input.Unit.DiagramType {
			return orchestration.DiagramBatchResult{}, fmt.Errorf("diagram unit %q requires a canonical supported type", input.Unit.ID)
		}
		item := input
		item.Unit.Source = normalizeText(input.Unit.Source)
		item.Unit.Options = normalizeText(input.Unit.Options)
		item.Body = normalizeText(input.Body)
		key, err := service.cacheKey(item)
		if err != nil {
			return orchestration.DiagramBatchResult{}, err
		}
		result.Units[index] = baseResolved(input, key, "miss")
		if int64(len(item.Unit.Source)) > service.config.MaxSourceBytes || int64(len(item.Body)) > service.config.MaxSourceBytes {
			failResolved(&result.Units[index], "diagram.source.too_large", "diagram source exceeds max bytes")
			continue
		}
		if int64(len(item.Unit.Options)) > service.config.MaxOptionsBytes {
			failResolved(&result.Units[index], "diagram.options.too_large", "diagram options exceed max bytes")
			continue
		}
		if existing, ok := byDigest[key.Digest]; ok {
			targets[existing].indexes = append(targets[existing].indexes, index)
			result.Units[index].Cache.Status = "hit"
			continue
		}
		byDigest[key.Digest] = len(targets)
		targets = append(targets, target{key: key, item: item, indexes: []int{index}})
	}

	type resolution struct {
		cached CachedRender
		hit    bool
		err    error
	}
	resolutions := make([]resolution, len(targets))
	var group sync.WaitGroup
	for index := range targets {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			current := targets[index]
			cached, hit, err := service.resolveTarget(ctx, current.key, current.item)
			resolutions[index] = resolution{cached: cached, hit: hit, err: err}
		}(index)
	}
	group.Wait()
	if err := ctx.Err(); err != nil {
		return orchestration.DiagramBatchResult{}, err
	}
	for targetIndex, current := range targets {
		resolved := resolutions[targetIndex]
		if resolved.err != nil {
			for _, index := range current.indexes {
				if !resolved.hit {
					result.Units[index].Cache.Status = "miss"
				}
				failResolved(&result.Units[index], "diagram.render.failed", resolved.err.Error())
			}
			continue
		}
		for offset, index := range current.indexes {
			status := "hit"
			if !resolved.hit && offset == 0 {
				status = "miss"
			}
			applyRender(&result.Units[index], resolved.cached, service.config.EngineVersion, service.config.Store.PublicObjectURL(resolved.cached.ObjectID), status)
		}
	}
	if err := result.Validate(batch); err != nil {
		return orchestration.DiagramBatchResult{}, err
	}
	return result, nil
}

func (service *Service) resolveTarget(ctx context.Context, key orchestration.CacheKey, item orchestration.DiagramBatchItem) (CachedRender, bool, error) {
	for {
		if cached, hit := service.cached(ctx, key); hit {
			return cached, true, nil
		}
		flight, leader := service.beginFlight(key.Digest)
		if leader {
			return service.resolveFlightLeader(ctx, key, item, flight)
		}
		select {
		case <-ctx.Done():
			return CachedRender{}, false, ctx.Err()
		case <-flight.done:
			if flight.err != nil && ctx.Err() == nil &&
				(errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded)) {
				continue
			}
			return flight.result, flight.err == nil, flight.err
		}
	}
}

func (service *Service) resolveFlightLeader(ctx context.Context, key orchestration.CacheKey, item orchestration.DiagramBatchItem, flight *diagramFlight) (CachedRender, bool, error) {
	var resolved CachedRender
	var resolveErr error
	defer func() { service.finishFlight(key.Digest, flight, resolved, resolveErr) }()
	// A previous leader can store between the first lookup and registering this flight.
	if cached, hit := service.cached(ctx, key); hit {
		resolved = cached
		return cached, true, nil
	}
	renderCtx := ctx
	cancel := func() {}
	if service.config.Timeout > 0 {
		renderCtx, cancel = context.WithTimeout(ctx, service.config.Timeout)
	}
	defer cancel()
	select {
	case service.pool.slots <- struct{}{}:
		defer func() { <-service.pool.slots }()
	case <-renderCtx.Done():
		resolveErr = renderCtx.Err()
		return CachedRender{}, false, resolveErr
	}
	resolved, resolveErr = service.render(renderCtx, item)
	if resolveErr == nil && service.config.Cache != nil {
		service.config.Cache.Put(key.Digest, resolved)
	}
	return resolved, false, resolveErr
}

func (service *Service) beginFlight(key string) (*diagramFlight, bool) {
	service.flightMu.Lock()
	defer service.flightMu.Unlock()
	if existing := service.flights[key]; existing != nil {
		return existing, false
	}
	flight := &diagramFlight{done: make(chan struct{})}
	service.flights[key] = flight
	return flight, true
}

func (service *Service) finishFlight(key string, flight *diagramFlight, result CachedRender, err error) {
	service.flightMu.Lock()
	flight.result = result
	flight.err = err
	delete(service.flights, key)
	close(flight.done)
	service.flightMu.Unlock()
}

func (service *Service) cached(ctx context.Context, key orchestration.CacheKey) (CachedRender, bool) {
	if service.config.Cache == nil {
		return CachedRender{}, false
	}
	cached, ok := service.config.Cache.Get(key.Digest)
	if !ok || !validCached(cached) || cached.SVGBytes > service.config.MaxSVGBytes || !service.verifyStored(ctx, cached) {
		if ok {
			service.config.Cache.Delete(key.Digest)
		}
		return CachedRender{}, false
	}
	return cached, true
}

func (service *Service) render(ctx context.Context, item orchestration.DiagramBatchItem) (CachedRender, error) {
	request := diagramengine.Request{Type: item.Unit.DiagramType, Options: item.Unit.Options}
	if item.SourceMode == orchestration.DiagramSourceBody {
		request.Body = item.Body
	} else {
		request.Source = item.Unit.Source
		request.Body = item.Body
	}
	rendered, err := service.config.Renderer.Render(ctx, request)
	if err != nil {
		return CachedRender{}, fmt.Errorf("diagram worker render: %w", err)
	}
	if int64(len(rendered.SVG)) > service.config.MaxSVGBytes {
		return CachedRender{}, fmt.Errorf("diagram worker SVG exceeds %d bytes", service.config.MaxSVGBytes)
	}
	svg, err := renderstorage.NormalizeSVGObject([]byte(rendered.SVG))
	if err != nil {
		return CachedRender{}, fmt.Errorf("sanitize diagram SVG: %w", err)
	}
	if int64(len(svg.Bytes)) > service.config.MaxSVGBytes {
		return CachedRender{}, fmt.Errorf("normalized diagram SVG exceeds %d bytes", service.config.MaxSVGBytes)
	}
	stored, err := service.config.Store.PutPublicObjectIfMissing(ctx, svg.ObjectID, renderstorage.SVGContentType, svg.Bytes)
	if err != nil {
		return CachedRender{}, fmt.Errorf("store public diagram SVG: %w", err)
	}
	if stored.ObjectID != svg.ObjectID {
		return CachedRender{}, errors.New("public diagram store returned an unexpected object ID")
	}
	diagramID := strings.TrimSpace(rendered.ID)
	if diagramID == "" {
		diagramID = "svg-sha256-" + svg.Hash[:12]
	}
	diagramType, ok := NormalizeType(firstNonEmpty(rendered.Type, item.Unit.DiagramType))
	if !ok {
		diagramType = item.Unit.DiagramType
	}
	diagnostics := make([]CachedDiagnostic, 0, len(rendered.Diagnostics))
	for _, diagnostic := range rendered.Diagnostics {
		diagnostics = append(diagnostics, CachedDiagnostic{
			Code:     firstNonEmpty(diagnostic.Code, "diagram.worker.message"),
			Severity: cleanSeverity(diagnostic.Severity), Message: firstNonEmpty(diagnostic.Message, "diagram worker message"),
		})
	}
	cached := CachedRender{
		SchemaVersion: orchestration.DiagramCacheSchemaVersion,
		DiagramID:     diagramID, DiagramType: diagramType, SVGHash: svg.Hash, ObjectID: stored.ObjectID,
		SVGBytes: int64(len(svg.Bytes)), Uploaded: stored.Uploaded, EngineCached: rendered.Cached,
		RenderURL: rendered.URL, Diagnostics: diagnostics,
	}
	if !service.verifyStoredAfterWrite(ctx, cached) {
		return CachedRender{}, errors.New("public diagram store failed post-write integrity verification")
	}
	return cached, nil
}

func (service *Service) verifyStoredAfterWrite(ctx context.Context, cached CachedRender) bool {
	for attempt, delay := range []time.Duration{0, 50 * time.Millisecond, 150 * time.Millisecond} {
		if attempt > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return false
			case <-timer.C:
			}
		}
		if service.verifyStored(ctx, cached) {
			return true
		}
	}
	return false
}

func (service *Service) verifyStored(ctx context.Context, cached CachedRender) bool {
	body, err := service.config.Store.ReadPublicObject(ctx, cached.ObjectID, service.config.MaxSVGBytes)
	if err != nil || int64(len(body)) != cached.SVGBytes {
		return false
	}
	return renderstorage.SVGHash(body) == cached.SVGHash
}

func (service *Service) cacheKey(item orchestration.DiagramBatchItem) (orchestration.CacheKey, error) {
	return orchestration.BuildCacheKey(orchestration.CacheKeyInput{
		Stage: orchestration.CacheStageDiagram,
		NormalizedInputs: map[string]string{
			"diagram-type": item.Unit.DiagramType, "source": item.Unit.Source,
			"source-mode": string(item.SourceMode), "options": firstNonEmpty(item.Unit.Options, "<rin-empty>"),
			"options-present": presentValue(item.Unit.Options),
		},
		ProjectContext: map[string]string{
			"engine-body": firstNonEmpty(item.Body, "<rin-empty>"), "engine-body-present": presentValue(item.Body),
		},
		Versions: map[string]string{
			"diagram-engine": "rin-texsvg@" + service.config.EngineVersion,
			"plugin":         service.config.PluginVersion, "font": service.config.FontVersion,
			"sanitizer": service.config.SanitizerVersion, "diagram-contract": orchestration.DiagramBatchContractVersion + "+" + orchestration.DiagramResultContractVersion + "+" + orchestration.DiagramArtifactContract + "+" + orchestration.DiagramCacheSchemaVersion,
			"output-strategy": "svg", "compatibility": service.config.CompatibilityVersion,
		},
	})
}

func baseResolved(item orchestration.DiagramBatchItem, key orchestration.CacheKey, status string) orchestration.DiagramResolvedUnit {
	return orchestration.DiagramResolvedUnit{
		ID: item.Unit.ID,
		Source: orchestration.DiagramSourceMetadata{
			DiagramType: item.Unit.DiagramType, Source: item.Unit.Source, Options: item.Unit.Options,
			Layout: item.Unit.Layout, SourceMode: item.SourceMode, SourceLocation: item.Unit.SourceLocation,
		},
		Cache:       orchestration.DiagramCacheResult{Status: status, KeyVersion: key.Version, Digest: key.Digest},
		Diagnostics: []contracts.Diagnostic{},
	}
}

func applyRender(unit *orchestration.DiagramResolvedUnit, cached CachedRender, engineVersion, url, cacheStatus string) {
	unit.State = "succeeded"
	unit.DiagramID = cached.DiagramID
	unit.Engine = "rin-texsvg"
	unit.EngineVersion = engineVersion
	unit.URL = url
	unit.SVGHash = cached.SVGHash
	unit.ObjectID = cached.ObjectID
	unit.SVGBytes = cached.SVGBytes
	unit.Uploaded = cached.Uploaded && cacheStatus == "miss"
	unit.EngineCached = cached.EngineCached
	unit.RenderURL = cached.RenderURL
	unit.Cache.Status = cacheStatus
	unit.Artifact = &contracts.ArtifactReference{
		ArtifactID: cached.ObjectID, SHA256: cached.SVGHash, Bytes: cached.SVGBytes,
		MediaType: renderstorage.SVGContentType, Visibility: "public",
	}
	for _, diagnostic := range cached.Diagnostics {
		unit.Diagnostics = append(unit.Diagnostics, contracts.Diagnostic{
			Code: diagnostic.Code, Severity: diagnostic.Severity, Message: diagnostic.Message,
			Stage: "diagram", SourceLocation: unit.Source.SourceLocation,
		})
	}
}

func failResolved(unit *orchestration.DiagramResolvedUnit, code, message string) {
	unit.State = "failed"
	unit.Diagnostics = append(unit.Diagnostics, contracts.Diagnostic{
		Code: code, Severity: "error", Message: message, Stage: "diagram",
		SourceLocation: unit.Source.SourceLocation,
	})
}

func validCached(cached CachedRender) bool {
	if cached.SchemaVersion != orchestration.DiagramCacheSchemaVersion || strings.TrimSpace(cached.DiagramID) == "" || strings.TrimSpace(cached.DiagramType) == "" ||
		len(cached.SVGHash) != 64 || strings.Trim(cached.SVGHash, "0123456789abcdef") != "" || cached.SVGBytes <= 0 {
		return false
	}
	expected := fmt.Sprintf("diagrams/v1/svg-sha256/%s/%s.svg", cached.SVGHash[:2], cached.SVGHash)
	return cached.ObjectID == expected
}

func normalizeText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.TrimSpace(value)
}

func cleanSeverity(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "warning":
		return "warning"
	case "error":
		return "error"
	default:
		return "info"
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

type MemoryCache struct {
	mu      sync.Mutex
	maximum int
	entries map[string]CachedRender
	order   []string
}

func NewMemoryCache(maximum int) *MemoryCache {
	if maximum <= 0 {
		maximum = DefaultMemoryCacheEntries
	}
	return &MemoryCache{maximum: maximum, entries: map[string]CachedRender{}}
}

func (cache *MemoryCache) Get(key string) (CachedRender, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	value, ok := cache.entries[key]
	return value, ok
}

func (cache *MemoryCache) Put(key string, value CachedRender) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if _, exists := cache.entries[key]; !exists {
		cache.order = append(cache.order, key)
	}
	cache.entries[key] = value
	for len(cache.entries) > cache.maximum && len(cache.order) > 0 {
		oldest := cache.order[0]
		cache.order = cache.order[1:]
		delete(cache.entries, oldest)
	}
}

func (cache *MemoryCache) Delete(key string) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	delete(cache.entries, key)
}

var _ orchestration.DiagramService = (*Service)(nil)
