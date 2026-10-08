package orchestration

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

type AdapterRegistry struct {
	mu         sync.RWMutex
	adapters   map[adapterKey]DocumentAdapter
	registered []DocumentAdapter
}

type adapterKey struct {
	contentKind contracts.ContentKind
	engine      string
}

func NewAdapterRegistry() *AdapterRegistry {
	return &AdapterRegistry{adapters: map[adapterKey]DocumentAdapter{}}
}

func (registry *AdapterRegistry) Register(adapter DocumentAdapter) error {
	if adapter == nil {
		return errors.New("document adapter is nil")
	}
	capabilities := adapter.Capabilities()
	engine := strings.TrimSpace(capabilities.Engine)
	if engine == "" || len(capabilities.ContentKinds) == 0 {
		return errors.New("document adapter requires an engine and content kinds")
	}
	seenKinds := map[contracts.ContentKind]struct{}{}
	for _, contentKind := range capabilities.ContentKinds {
		if !contentKind.Valid() {
			return fmt.Errorf("adapter %q has invalid content kind %q", engine, contentKind)
		}
		if _, exists := seenKinds[contentKind]; exists {
			return fmt.Errorf("adapter %q repeats content kind %q", engine, contentKind)
		}
		seenKinds[contentKind] = struct{}{}
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	for contentKind := range seenKinds {
		key := adapterKey{contentKind: contentKind, engine: engine}
		if _, exists := registry.adapters[key]; exists {
			return fmt.Errorf("adapter already registered for %s/%s", contentKind, engine)
		}
	}
	for contentKind := range seenKinds {
		registry.adapters[adapterKey{contentKind: contentKind, engine: engine}] = adapter
	}
	registry.registered = append(registry.registered, adapter)
	return nil
}

func (registry *AdapterRegistry) Lookup(contentKind contracts.ContentKind, engine string) (DocumentAdapter, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	adapter, ok := registry.adapters[adapterKey{contentKind: contentKind, engine: strings.TrimSpace(engine)}]
	return adapter, ok
}

func (registry *AdapterRegistry) Capabilities() []DocumentCapabilities {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	capabilities := make([]DocumentCapabilities, 0, len(registry.registered))
	for _, adapter := range registry.registered {
		capability := adapter.Capabilities()
		capability.ContentKinds = append([]contracts.ContentKind(nil), capability.ContentKinds...)
		capability.Features = append([]string(nil), capability.Features...)
		sort.Slice(capability.ContentKinds, func(i, j int) bool { return capability.ContentKinds[i] < capability.ContentKinds[j] })
		sort.Strings(capability.Features)
		capabilities = append(capabilities, capability)
	}
	sort.Slice(capabilities, func(i, j int) bool { return capabilities[i].Engine < capabilities[j].Engine })
	return capabilities
}
