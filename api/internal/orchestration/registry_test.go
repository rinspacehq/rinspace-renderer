package orchestration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration/orchestrationtest"
)

func TestAdapterRegistryRoutesByContentKindAndEngine(t *testing.T) {
	registry := orchestration.NewAdapterRegistry()
	markdown := &orchestrationtest.FakeAdapter{CapabilitiesValue: orchestration.DocumentCapabilities{
		Engine: "rin-markdown", ContentKinds: []contracts.ContentKind{contracts.ContentKindMarkdown}, Features: []string{"gfm", "math"},
	}}
	latexml := &orchestrationtest.FakeAdapter{CapabilitiesValue: orchestration.DocumentCapabilities{
		Engine: "latexml", ContentKinds: []contracts.ContentKind{contracts.ContentKindLaTeX}, Features: []string{"latex"},
	}}
	if err := registry.Register(markdown); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(latexml); err != nil {
		t.Fatal(err)
	}
	if got, ok := registry.Lookup(contracts.ContentKindMarkdown, "rin-markdown"); !ok || got != markdown {
		t.Fatal("markdown adapter lookup failed")
	}
	if _, ok := registry.Lookup(contracts.ContentKindLaTeX, "rin-markdown"); ok {
		t.Fatal("registry crossed content-kind boundary")
	}
	capabilities := registry.Capabilities()
	if len(capabilities) != 2 || capabilities[0].Engine != "latexml" || capabilities[1].Engine != "rin-markdown" {
		t.Fatalf("capabilities are not deterministic: %#v", capabilities)
	}
	if err := registry.Register(markdown); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("expected duplicate registration error, got %v", err)
	}
}

func TestOrchestrationFakesNeedNoHeavyWorkers(t *testing.T) {
	services := &orchestrationtest.FakeWorkServices{
		MathFunc: func(_ context.Context, batch orchestration.MathBatch) (orchestration.MathBatchResult, error) {
			return orchestration.MathBatchResult{Units: []orchestration.MathResolvedUnit{{ID: batch.Items[0].Unit.ID, HTML: "<mjx-container></mjx-container>"}}}, nil
		},
	}
	var math orchestration.MathService = services
	display := false
	resolved, err := math.ResolveMath(context.Background(), orchestration.MathBatch{Items: []orchestration.MathBatchItem{{
		Unit: contracts.WorkUnit{Kind: contracts.WorkUnitMath, ID: "rw_11111111111111111111111111111111", Source: "x", Display: &display, MacroContextHash: strings.Repeat("1", 64)},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Units) != 1 || resolved.Units[0].ID == "" {
		t.Fatalf("unexpected fake result: %#v", resolved)
	}

	cache := orchestrationtest.NewMemoryCache()
	var typedCache orchestration.Cache = cache
	versions := map[string]string{}
	for _, name := range orchestration.RequiredCacheVersions(orchestration.CacheStageMath) {
		versions[name] = "v1"
	}
	key, err := orchestration.BuildCacheKey(orchestration.CacheKeyInput{
		Stage: orchestration.CacheStageMath, NormalizedInputs: map[string]string{"source": "x"}, Versions: versions,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	record := orchestration.CacheRecord{Key: key, ExpiresAt: now.Add(time.Hour)}
	if result := typedCache.Store(context.Background(), record, orchestration.CacheExpectation{}, now); !result.Stored {
		t.Fatalf("fake cache store: %#v", result)
	}
	if result := typedCache.Lookup(context.Background(), key, orchestration.CacheExpectation{}, now); !result.Hit {
		t.Fatalf("fake cache lookup: %#v", result)
	}
}
