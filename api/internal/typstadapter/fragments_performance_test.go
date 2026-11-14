package typstadapter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Explicitly opt in: this exercises all growing prefixes instead of imposing
// a synthetic performance workload on every ordinary adapter test run.
func TestPinnedFragmentsTeacherSizedPrefixes(t *testing.T) {
	if os.Getenv("RIN_TYPST_FRAGMENT_PERFORMANCE") != "1" {
		t.Skip("teacher-sized prefix performance probe is not enabled")
	}
	compiler := pinnedCorpusCompiler(t)
	if os.Getenv("RIN_TYPST_TEST_SANDBOX") != "" {
		compiler = configureTestSandbox(t, compiler)
	}
	blocks := make([]FragmentBlock, 32)
	for i := range blocks {
		blocks[i] = FragmentBlock{ID: fmt.Sprintf("step-%d", i+1), Source: strings.Repeat("A definition and its assumptions stay together. ", 90) + fmt.Sprintf("\n$ x_%d^2 + y_%d^2 = 1 $ <eq-%d>\n", i+1, i+1, i+1)}
	}
	type sample struct {
		Blocks        int     `json:"blocks"`
		SourceBytes   int     `json:"sourceBytes"`
		FinalBytes    int     `json:"finalBytes"`
		DurationMS    float64 `json:"durationMs"`
		CompilerCPUMS float64 `json:"launcherProcessCpuMs"`
	}
	samples := make([]sample, 0, len(blocks))
	total := time.Duration(0)
	totalCPU := time.Duration(0)
	cpuStat := os.Getenv("RIN_TYPST_PERFORMANCE_CPU_STAT")
	beforeScopeCPU := readPerformanceScopeCPU(t, cpuStat)
	for count := 1; count <= len(blocks); count++ {
		started := time.Now()
		result, err := CompileFragments(t.Context(), compiler, blocks[:count])
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("prefix %d failed: %v: %s", count, err, result.Log)
		}
		total += elapsed
		totalCPU += result.CompilerCPU
		sourceBytes := 0
		for _, block := range blocks[:count] {
			sourceBytes += len(block.Source)
		}
		if strings.Count(result.Document.Pages[0].HTML, "<math") != count {
			t.Fatalf("prefix %d lost formulas", count)
		}
		samples = append(samples, sample{count, sourceBytes, len(result.Document.Pages[0].HTML), float64(elapsed.Microseconds()) / 1000, float64(result.CompilerCPU.Microseconds()) / 1000})
		if count == 1 || count == 8 || count == 16 || count == 32 {
			t.Logf("blocks=%d source=%d final=%d elapsed=%s", count, sourceBytes, len(result.Document.Pages[0].HTML), elapsed)
		}
	}
	t.Logf("all 32 growing prefixes total=%s; not a model/MathJax/voice latency measurement", total)
	prefixScopeCPU := readPerformanceScopeCPU(t, cpuStat) - beforeScopeCPU
	repeatedStarted := time.Now()
	repeated, err := CompileFragments(t.Context(), compiler, blocks)
	if err != nil {
		t.Fatal(err)
	}
	repeatedMS := float64(time.Since(repeatedStarted).Microseconds()) / 1000
	repeatedScopeCPU := readPerformanceScopeCPU(t, cpuStat) - beforeScopeCPU - prefixScopeCPU
	if target := os.Getenv("RIN_TYPST_FRAGMENT_PERFORMANCE_REPORT"); target != "" {
		if !filepath.IsAbs(target) {
			t.Fatal("performance report path must be absolute")
		}
		report := struct {
			Template              string   `json:"template"`
			Scope                 string   `json:"scope"`
			TotalMS               float64  `json:"totalMs"`
			CompilerCPUMS         float64  `json:"launcherProcessCpuMs"`
			RepeatedFullMS        float64  `json:"repeatedFullMs"`
			RepeatedCompilerCPUMS float64  `json:"repeatedLauncherProcessCpuMs"`
			PrefixScopeCPUMS      float64  `json:"prefixScopeCpuMs"`
			RepeatedScopeCPUMS    float64  `json:"repeatedScopeCpuMs"`
			Sandboxed             bool     `json:"sandboxed"`
			Samples               []sample `json:"samples"`
		}{FragmentTemplateVersion, "32 original synthetic prefixes plus repeated full input; new compiler process each time, uncontrolled host page cache. Launcher process CPU excludes sandbox children. Dedicated scope CPU deltas include Go adapter and all compiler descendants during this test, not the preceding build. Zero scope delta means unmeasured. Not model/MathJax/voice, p95 or teacher quality.", float64(total.Microseconds()) / 1000, float64(totalCPU.Microseconds()) / 1000, repeatedMS, float64(repeated.CompilerCPU.Microseconds()) / 1000, float64(prefixScopeCPU) / 1000, float64(repeatedScopeCPU) / 1000, compiler.SandboxPath != "", samples}
		body, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, append(body, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readPerformanceScopeCPU(t *testing.T, path string) int64 {
	t.Helper()
	if path == "" {
		return 0
	}
	if !filepath.IsAbs(path) || !strings.Contains(path, "reader-tutor-fragments-perf-") {
		t.Fatal("CPU accounting requires this task's dedicated scope, never the host-wide cgroup")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "usage_usec" {
			value, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
	}
	t.Fatal("scope CPU accounting is unavailable")
	return 0
}
