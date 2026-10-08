package jobpostgres

import (
	"testing"
	"time"
)

func TestWaitEstimatorSimulatesSlotsAndObservedDurations(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	policy := testEstimationPolicy()
	policy.Scheduling.Resources.DocumentLight = 1
	runningStarted := now
	jobs := []estimateJob{
		estimateFixture("running", "running", "publish", now.Add(-time.Minute), 30, 10_000, 30_000, &runningStarted),
		estimateFixture("ahead", "queued", "publish", now.Add(-time.Second), 25, 20_000, 40_000, nil),
		estimateFixture("target", "queued", "publish", now, 80, 5_000, 10_000, nil),
	}
	p50, ok := simulateEstimatedStart("target", jobs, map[string]int64{}, policy, now, 50)
	if !ok || !p50.start.Equal(now.Add(30*time.Second)) || p50.jobsAhead != 1 || p50.samples != 25 {
		t.Fatalf("p50 simulation = %#v, %v", p50, ok)
	}
	p90, ok := simulateEstimatedStart("target", jobs, map[string]int64{}, policy, now, 90)
	if !ok || !p90.start.Equal(now.Add(70*time.Second)) || p90.jobsAhead != 1 || p90.samples != 25 {
		t.Fatalf("p90 simulation = %#v, %v", p90, ok)
	}
}

func TestWaitEstimatorReturnsUnknownOnlyForRelevantCost(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	policy := testEstimationPolicy()
	policy.Scheduling.Resources.DocumentLight = 1
	target := estimateFixture("target", "queued", "publish", now, 20, 1_000, 2_000, nil)
	unknownBehind := estimateFixture("behind", "queued", "publish", now.Add(time.Second), 0, 0, 0, nil)
	unknownBehind.P50MS, unknownBehind.P90MS = nil, nil
	if result, ok := simulateEstimatedStart("target", []estimateJob{target, unknownBehind}, map[string]int64{}, policy, now, 50); !ok || !result.start.Equal(now) {
		t.Fatalf("job behind target changed estimate = %#v, %v", result, ok)
	}
	target.SampleCount = 19
	if _, ok := simulateEstimatedStart("target", []estimateJob{target}, map[string]int64{}, policy, now, 50); ok {
		t.Fatal("estimate with fewer than 20 target samples should be unknown")
	}
}

func TestWaitEstimatorToleratesUnknownRunningCostOnlyWhenASlotIsFree(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	policy := testEstimationPolicy()
	started := now
	unknown := estimateFixture("unknown-running", "running", "publish", now.Add(-time.Minute), 0, 0, 0, &started)
	unknown.P50MS, unknown.P90MS = nil, nil
	target := estimateFixture("target", "queued", "publish", now, 20, 1_000, 2_000, nil)
	if result, ok := simulateEstimatedStart("target", []estimateJob{unknown, target}, map[string]int64{}, policy, now, 50); !ok || !result.start.Equal(now) {
		t.Fatalf("free slot estimate = %#v, %v", result, ok)
	}
	policy.Scheduling.Resources.DocumentLight = 1
	if _, ok := simulateEstimatedStart("target", []estimateJob{unknown, target}, map[string]int64{}, policy, now, 50); ok {
		t.Fatal("fully occupied unknown slot should produce an unknown estimate")
	}
}

func TestWaitEstimatorUsesWeightedFairAging(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	policy := testEstimationPolicy()
	policy.Scheduling.Resources.DocumentLight = 1
	agedMigration := estimateFixture("migration", "queued", "migration", now.Add(-10*time.Minute), 50, 10_000, 10_000, nil)
	target := estimateFixture("target", "queued", "publish", now, 50, 10_000, 10_000, nil)
	result, ok := simulateEstimatedStart("target", []estimateJob{target, agedMigration}, map[string]int64{}, policy, now, 50)
	if !ok || !result.start.Equal(now.Add(10*time.Second)) || result.jobsAhead != 1 {
		t.Fatalf("aged fair simulation = %#v, %v", result, ok)
	}
}

func TestWaitEstimatorUsesPreviewPriorityBetweenPublishAndRebuild(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	policy := testEstimationPolicy()
	policy.Scheduling.Resources.DocumentLight = 1
	jobs := []estimateJob{
		estimateFixture("migration", "queued", "migration", now, 50, 10_000, 10_000, nil),
		estimateFixture("rebuild", "queued", "rebuild", now, 50, 10_000, 10_000, nil),
		estimateFixture("preview", "queued", "preview", now, 50, 10_000, 10_000, nil),
		estimateFixture("publish", "queued", "publish", now, 50, 10_000, 10_000, nil),
	}
	result, ok := simulateEstimatedStart("migration", jobs,
		map[string]int64{"publish": 0, "preview": 0, "rebuild": 0, "migration": 0}, policy, now, 50)
	if !ok || result.jobsAhead != 3 || !result.start.Equal(now.Add(30*time.Second)) {
		t.Fatalf("preview priority simulation = %#v, %v", result, ok)
	}
}

func TestSchedulingPolicyRequiresBoundedPDFCapacityAndPriorityOrder(t *testing.T) {
	policy := testEstimationPolicy().Scheduling
	if estimateResourceCapacity(policy.Resources, "latex-pdf") != 1 || policy.Validate() != nil {
		t.Fatalf("valid PDF scheduling policy = %#v", policy)
	}
	policy.Weights.Preview = policy.Weights.Publish
	if policy.Validate() == nil {
		t.Fatal("scheduling policy accepted publish <= preview")
	}
	policy = testEstimationPolicy().Scheduling
	policy.Resources.LatexPDF = 0
	if policy.Validate() == nil {
		t.Fatal("scheduling policy accepted zero PDF capacity")
	}
}

func TestEstimateConfidenceUsesBoundedTiers(t *testing.T) {
	for samples, want := range map[int64]string{20: "low", 49: "low", 50: "medium", 99: "medium", 100: "high"} {
		if got := estimateConfidence(samples); got != want {
			t.Fatalf("estimateConfidence(%d) = %q, want %q", samples, got, want)
		}
	}
}

func testEstimationPolicy() EstimationPolicy {
	return EstimationPolicy{MinimumSamples: 20, Scheduling: SchedulingPolicy{
		Resources: ResourceCapacities{DocumentLight: 2, DocumentLaTeXML: 1, MathNode: 2, TeXSVG: 2, BatchMigration: 1, LatexPDF: 1,
			DocumentTypst: 1, TypstPDF: 1, Heavy: 1},
		Weights:       PriorityWeights{Publish: 8, Preview: 5, Rebuild: 3, Migration: 1},
		AgingInterval: 5 * time.Minute, MaxAgingSteps: 12, PrincipalRunning: 2,
	}}
}

func estimateFixture(id, state, priority string, queued time.Time, samples, p50, p90 int64, started *time.Time) estimateJob {
	return estimateJob{
		ID: id, PrincipalID: "principal-" + id, OwnerScope: "owner", PriorityClass: priority,
		State: state, QueuedAt: queued, AvailableAt: queued, StartedAt: started,
		SampleCount: samples, P50MS: &p50, P90MS: &p90,
	}
}
