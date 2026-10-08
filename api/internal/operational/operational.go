package operational

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Probe func(context.Context) error

type Component struct {
	Ready  bool   `json:"ready"`
	Reason string `json:"reason,omitempty"`
}

type Snapshot struct {
	Status     string               `json:"status"`
	Ready      bool                 `json:"ready"`
	Degraded   []string             `json:"degraded"`
	Components map[string]Component `json:"components"`
	CheckedAt  time.Time            `json:"checkedAt"`
}

type histogram struct {
	Count   uint64
	Sum     float64
	Buckets []uint64
}

type Registry struct {
	mu         sync.RWMutex
	probes     map[string]Probe
	collectors map[string]Probe
	counters   map[string]uint64
	gauges     map[string]float64
	histograms map[string]histogram
	logger     *slog.Logger
	now        func() time.Time
}

var durationBuckets = []float64{0.1, 1, 5, 30, 120, 600, 1800}

func New(writer io.Writer) *Registry {
	if writer == nil {
		writer = io.Discard
	}
	return &Registry{
		probes: map[string]Probe{}, collectors: map[string]Probe{}, counters: map[string]uint64{}, gauges: map[string]float64{},
		histograms: map[string]histogram{},
		logger:     slog.New(slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo})),
		now:        time.Now,
	}
}

func (registry *Registry) AddCollector(name string, collector Probe) error {
	if !validToken(name) || collector == nil {
		return errors.New("operational collector is invalid")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.collectors[name] = collector
	return nil
}

var defaultRegistry = New(io.Discard)

func Default() *Registry { return defaultRegistry }

func SetDefault(registry *Registry) {
	if registry != nil {
		defaultRegistry = registry
	}
}

func (registry *Registry) AddProbe(name string, probe Probe) error {
	if !validToken(name) || probe == nil {
		return errors.New("operational probe is invalid")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.probes[name] = probe
	return nil
}

func (registry *Registry) Ready(ctx context.Context) error {
	snapshot := registry.Snapshot(ctx)
	if snapshot.Ready {
		return nil
	}
	return fmt.Errorf("renderer is not ready: %s", strings.Join(snapshot.Degraded, ","))
}

func (registry *Registry) Snapshot(ctx context.Context) Snapshot {
	registry.mu.RLock()
	probes := make(map[string]Probe, len(registry.probes))
	for name, probe := range registry.probes {
		probes[name] = probe
	}
	now := registry.now
	registry.mu.RUnlock()
	names := make([]string, 0, len(probes))
	for name := range probes {
		names = append(names, name)
	}
	sort.Strings(names)
	snapshot := Snapshot{Status: "ready", Ready: true, Components: map[string]Component{}, CheckedAt: now().UTC()}
	for _, name := range names {
		err := probes[name](ctx)
		component := Component{Ready: err == nil}
		if err != nil {
			component.Reason = normalizedReason(err)
			snapshot.Ready = false
			snapshot.Status = "degraded"
			snapshot.Degraded = append(snapshot.Degraded, name)
		}
		snapshot.Components[name] = component
	}
	if snapshot.Degraded == nil {
		snapshot.Degraded = []string{}
	}
	return snapshot
}

func (registry *Registry) Event(event string, attributes ...slog.Attr) {
	registry.log(slog.LevelInfo, event, attributes...)
}

// Warn emits an operational event at warning level. Use it for conditions that
// must reach an operator without waiting for a failed health check: a render
// that degrades or fails is not a routine info line.
func (registry *Registry) Warn(event string, attributes ...slog.Attr) {
	registry.log(slog.LevelWarn, event, attributes...)
}

func (registry *Registry) log(level slog.Level, event string, attributes ...slog.Attr) {
	if !validToken(event) {
		return
	}
	values := make([]any, 0, len(attributes)+1)
	values = append(values, slog.String("event", event))
	for _, attribute := range attributes {
		if safeAttribute(attribute) {
			values = append(values, attribute)
		}
	}
	registry.logger.Log(context.Background(), level, "rin_renderer", values...)
}

func (registry *Registry) Count(metric string, labels ...string) {
	key, ok := metricKey(metric, labels)
	if !ok {
		return
	}
	registry.mu.Lock()
	registry.counters[key]++
	registry.mu.Unlock()
}

func (registry *Registry) Gauge(metric string, value float64, labels ...string) {
	key, ok := metricKey(metric, labels)
	if !ok || value < 0 {
		return
	}
	registry.mu.Lock()
	registry.gauges[key] = value
	registry.mu.Unlock()
}

func (registry *Registry) AddGauge(metric string, delta float64, labels ...string) {
	key, ok := metricKey(metric, labels)
	if !ok {
		return
	}
	registry.mu.Lock()
	registry.gauges[key] += delta
	if registry.gauges[key] < 0 {
		registry.gauges[key] = 0
	}
	registry.mu.Unlock()
}

func (registry *Registry) Observe(metric string, duration time.Duration, labels ...string) {
	key, ok := metricKey(metric, labels)
	if !ok {
		return
	}
	seconds := duration.Seconds()
	if seconds < 0 {
		seconds = 0
	}
	registry.mu.Lock()
	value := registry.histograms[key]
	if value.Buckets == nil {
		value.Buckets = make([]uint64, len(durationBuckets))
	}
	value.Count++
	value.Sum += seconds
	for index, upper := range durationBuckets {
		if seconds <= upper {
			value.Buckets[index]++
		}
	}
	registry.histograms[key] = value
	registry.mu.Unlock()
}

func (registry *Registry) Metrics(response http.ResponseWriter, request *http.Request) {
	registry.mu.RLock()
	collectors := make([]Probe, 0, len(registry.collectors))
	for _, collector := range registry.collectors {
		collectors = append(collectors, collector)
	}
	registry.mu.RUnlock()
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Second)
	defer cancel()
	for _, collector := range collectors {
		_ = collector(ctx)
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	response.Header().Set("Content-Type", "text/plain; version=0.0.4")
	counterKeys := sortedKeys(registry.counters)
	for _, key := range counterKeys {
		fmt.Fprintf(response, "%s %d\n", key, registry.counters[key])
	}
	gaugeKeys := sortedKeys(registry.gauges)
	for _, key := range gaugeKeys {
		fmt.Fprintf(response, "%s %g\n", key, registry.gauges[key])
	}
	histogramKeys := sortedKeys(registry.histograms)
	for _, key := range histogramKeys {
		value := registry.histograms[key]
		for index, upper := range durationBuckets {
			fmt.Fprintf(response, "%s_bucket{le=\"%g\"} %d\n", key, upper, value.Buckets[index])
		}
		fmt.Fprintf(response, "%s_bucket{le=\"+Inf\"} %d\n", key, value.Count)
		fmt.Fprintf(response, "%s_sum %g\n%s_count %d\n", key, value.Sum, key, value.Count)
	}
}

func metricKey(metric string, labels []string) (string, bool) {
	if !validMetric(metric) || len(labels) > 4 {
		return "", false
	}
	key := "rin_renderer_" + metric
	for _, label := range labels {
		if !validToken(label) {
			return "", false
		}
		key += "_" + strings.ReplaceAll(label, "-", "_")
	}
	return key, true
}

func validMetric(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && character != '_' {
			return false
		}
	}
	return true
}

func validToken(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func safeAttribute(attribute slog.Attr) bool {
	allowed := false
	switch attribute.Key {
	case "request_id", "job_id", "attempt_id", "content_kind", "engine", "resource_class",
		"priority_class", "state", "failure_code", "stage", "renderer_version", "estimator_version",
		"queue_wait_ms", "duration_ms", "input_bytes", "output_bytes", "sample_count", "cache_hit", "retryable",
		"diagnostic_count", "codes":
		allowed = true
	default:
		return false
	}
	if !allowed {
		return false
	}
	switch attribute.Value.Kind() {
	case slog.KindString:
		return safeLogValue(attribute.Value.String())
	case slog.KindBool, slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindDuration, slog.KindTime:
		return true
	default:
		return false
	}
}

func safeLogValue(value string) bool {
	if len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '_' && character != '-' &&
			character != '.' && character != ':' && character != ',' {
			return false
		}
	}
	return true
}

func normalizedReason(err error) string {
	if err == nil {
		return ""
	}
	value := strings.ToLower(err.Error())
	for _, candidate := range []string{
		"shutting_down", "database_unavailable", "artifact_unavailable", "worker_unavailable",
		"scheduler_unavailable", "completion_delivery_degraded", "completion_dispatcher_unavailable",
	} {
		if strings.Contains(strings.ReplaceAll(value, " ", "_"), candidate) {
			return candidate
		}
	}
	return "unavailable"
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
