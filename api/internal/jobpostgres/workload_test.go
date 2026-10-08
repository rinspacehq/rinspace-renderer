package jobpostgres

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAdmissionAndRefinedWorkloadProfilesAreBoundedAndVersioned(t *testing.T) {
	admission, err := AdmissionWorkload(WorkloadAdmission{
		ContentKind: "latex", DocumentEngine: "auto", ResourceClass: "document-latexml",
		PriorityClass: "publish", ProjectBytes: 300 << 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if admission.ProfileVersion != WorkloadProfileVersion || admission.AdmissionProfileKey != admission.ProfileKey || !strings.Contains(admission.ProfileKey, "/latex/auto/unknown/medium/diagrams-unknown") {
		t.Fatalf("admission workload = %#v", admission)
	}
	var admissionFeatures map[string]any
	if err := json.Unmarshal(admission.Features, &admissionFeatures); err != nil || admissionFeatures["stage"] != "admission" {
		t.Fatalf("admission features = %#v, %v", admissionFeatures, err)
	}

	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	refined, err := RefinedWorkload("latex", "latexml", "document-latexml", "publish", 300<<10, WorkloadRefinement{
		DocumentClass: "book", FileCount: 12, PageCount: 120, MathCount: 40,
		DiagramCount: 1, CodeBlockCount: 2,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if refined.FeatureStage != "analysis" || refined.RefinedAt == nil || !strings.Contains(refined.ProfileKey, "/latex/latexml/book/xlarge/diagrams-present") {
		t.Fatalf("refined workload = %#v", refined)
	}
	if _, err := RefinedWorkload("latex", "latexml", "document-latexml", "publish", 1, WorkloadRefinement{DocumentClass: "unknown"}, now); err == nil {
		t.Fatal("RefinedWorkload accepted an unknown analyzed document class")
	}
}

func TestDurationWindowAndStatistics(t *testing.T) {
	var samples []int64
	for value := int64(1); value <= 205; value++ {
		samples = appendDurationSample(samples, value)
	}
	if len(samples) != workloadSampleLimit || samples[0] != 6 || samples[len(samples)-1] != 205 {
		t.Fatalf("rolling samples = len %d, first %d, last %d", len(samples), samples[0], samples[len(samples)-1])
	}
	p50, p90, mean := durationStatistics([]int64{100, 500, 200, 400, 300})
	if p50 != 300 || p90 != 500 || mean != 300 {
		t.Fatalf("duration statistics = %d/%d/%d", p50, p90, mean)
	}
}

func TestFailureClassesAreFixedAndDoNotContainSource(t *testing.T) {
	tests := map[string]string{
		"document_render_timeout": "timeout", "worker_lease_expired": "timeout",
		"invalid_project_source": "invalid", "source_artifact_unavailable": "unavailable",
		"result_storage_failed": "unavailable", "unexpected-private-message": "internal",
	}
	for code, want := range tests {
		if got := failureClass(code); got != want {
			t.Fatalf("failureClass(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestAdmissionAndRefinedWorkloadAcceptTypstContentKind(t *testing.T) {
	admission, err := AdmissionWorkload(WorkloadAdmission{
		ContentKind: "typst", DocumentEngine: "typst", ResourceClass: "typst-pdf",
		PriorityClass: "preview", ProjectBytes: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if admission.FeatureStage != "admission" || admission.AdmissionProfileKey != admission.ProfileKey ||
		!strings.Contains(admission.ProfileKey, "/typst/typst/unknown/") {
		t.Fatalf("typst admission workload = %#v", admission)
	}

	now := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	refined, err := RefinedWorkload("typst", "typst", "typst-pdf", "preview", 3, WorkloadRefinement{
		DocumentClass: "article", FileCount: 1, PageCount: 1,
	}, now)
	if err != nil || refined.FeatureStage != "analysis" || refined.RefinedAt == nil ||
		!strings.Contains(refined.ProfileKey, "/typst/typst/article/") {
		t.Fatalf("typst refined workload = %#v, %v", refined, err)
	}

	if _, err := AdmissionWorkload(WorkloadAdmission{
		ContentKind: "sphinx", DocumentEngine: "typst", ResourceClass: "typst-pdf",
		PriorityClass: "preview", ProjectBytes: 3,
	}); err == nil {
		t.Fatal("AdmissionWorkload accepted an unsupported content kind")
	}
}
