package renderapi

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigFromEnvUsesLateXMLLockWhenVersionEnvMissing(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "latexml.lock")
	if err := os.WriteFile(lockPath, []byte(`{
  "repo": "https://github.com/brucemiller/LaTeXML",
  "commit": "commit-from-lock",
  "tag": null,
  "texlive": "TeX Live 2022/Debian",
  "perl": "5.40",
  "adapterVersion": "0.1.0",
  "corpusVersion": "0.1.0"
}`), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	t.Setenv("RIN_RENDERER_LATEXML_LOCK_FILE", lockPath)
	t.Setenv("RIN_RENDERER_LATEXML_VERSION", "")
	t.Setenv("RIN_RENDERER_TEXLIVE_VERSION", "")
	t.Setenv("RIN_RENDERER_PERL_VERSION", "")
	t.Setenv("RIN_RENDERER_LATEXML_ADAPTER_VERSION", "")

	cfg := ConfigFromEnv()

	if cfg.LateXMLLockFile != lockPath {
		t.Fatalf("expected lock path %q, got %q", lockPath, cfg.LateXMLLockFile)
	}
	if cfg.LateXMLVersion != "commit-from-lock" ||
		cfg.TeXLiveVersion != "TeX Live 2022/Debian" ||
		cfg.PerlVersion != "5.40" ||
		cfg.LaTeXMLAdapterVersion != "0.1.0" {
		t.Fatalf("expected versions from lock, got %#v", cfg)
	}
}

func TestConfigFromEnvVersionEnvOverridesLateXMLLock(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "latexml.lock")
	if err := os.WriteFile(lockPath, []byte(`{
  "repo": "https://github.com/brucemiller/LaTeXML",
  "commit": "commit-from-lock",
  "tag": null,
  "texlive": "TeX Live 2022/Debian",
  "perl": "5.40",
  "adapterVersion": "0.1.0",
  "corpusVersion": "0.1.0"
}`), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	t.Setenv("RIN_RENDERER_LATEXML_LOCK_FILE", lockPath)
	t.Setenv("RIN_RENDERER_LATEXML_VERSION", "commit-from-env")
	t.Setenv("RIN_RENDERER_TEXLIVE_VERSION", "TeXLive-env")
	t.Setenv("RIN_RENDERER_PERL_VERSION", "Perl-env")
	t.Setenv("RIN_RENDERER_LATEXML_ADAPTER_VERSION", "adapter-env")

	cfg := ConfigFromEnv()

	if cfg.LateXMLVersion != "commit-from-env" ||
		cfg.TeXLiveVersion != "TeXLive-env" ||
		cfg.PerlVersion != "Perl-env" ||
		cfg.LaTeXMLAdapterVersion != "adapter-env" {
		t.Fatalf("expected env versions to override lock, got %#v", cfg)
	}
}

func TestConfigFromEnvReadsLateXMLWorkerEndpoint(t *testing.T) {
	t.Setenv("RIN_RENDERER_LATEXML_ENDPOINT", " http://latexml-worker:8092/render ")
	t.Setenv("RIN_RENDERER_LATEXML_TOKEN", " worker-token ")

	cfg := ConfigFromEnv()

	if cfg.LateXMLWorkerEndpoint != "http://latexml-worker:8092/render" || cfg.LateXMLWorkerToken != "worker-token" {
		t.Fatalf("expected latexml worker config from env, got %#v", cfg)
	}
}

func TestConfigFromEnvReadsMathSettings(t *testing.T) {
	t.Setenv("RIN_RENDERER_DEFAULT_MATH_POLICY", " source-debug ")
	t.Setenv("RIN_RENDERER_MATH_SOURCE_MAX_BYTES", "4096")
	t.Setenv("RIN_RENDERER_MATH_TEXSVG_FALLBACK_MAX_COUNT", "12")
	t.Setenv("RIN_RENDERER_MATH_TIMEOUT_SECONDS", "7")
	t.Setenv("RIN_RENDERER_MATH_PARALLELISM", "3")
	t.Setenv("RIN_RENDERER_MATH_PRIMARY_ENGINE", " mathjax ")
	t.Setenv("RIN_RENDERER_MATH_OUTPUT_STRATEGY", " hybrid-svg ")
	t.Setenv("RIN_RENDERER_MATHJAX_NODE_BIN", "/usr/local/bin/node")
	t.Setenv("RIN_RENDERER_MATHJAX_SCRIPT", "/opt/rin-renderer/engines/mathjax/render-mathjax.mjs")
	t.Setenv("RIN_RENDERER_MATHJAX_VERSION", "4.1.3")
	t.Setenv("RIN_RENDERER_MATHJAX_OUTPUT", "chtml")
	t.Setenv("RIN_RENDERER_MATHJAX_FONT_VERSION", "mathjax-newcm-test")
	t.Setenv("RIN_RENDERER_KATEX_NODE_BIN", "/usr/bin/node")
	t.Setenv("RIN_RENDERER_KATEX_SCRIPT", "/opt/rin-renderer/engines/katex/render-katex.mjs")
	t.Setenv("RIN_RENDERER_KATEX_VERSION", "0.16.22")

	cfg := ConfigFromEnv()

	if cfg.DefaultMathPolicy != "source-debug" ||
		cfg.MathSourceMaxBytes != 4096 ||
		cfg.MathTexSVGFallbackMaxCount != 12 ||
		cfg.MathTimeout.Seconds() != 7 ||
		cfg.MathParallelism != 3 ||
		cfg.MathPrimaryEngine != "mathjax-chtml" ||
		cfg.MathOutputStrategy != "complex-svg" ||
		cfg.MathJaxNodeBin != "/usr/local/bin/node" ||
		cfg.MathJaxScript != "/opt/rin-renderer/engines/mathjax/render-mathjax.mjs" ||
		cfg.MathJaxVersion != "4.1.3" ||
		cfg.MathJaxOutput != "chtml" ||
		cfg.MathJaxFontVersion != "mathjax-newcm-test" ||
		cfg.KaTeXNodeBin != "/usr/bin/node" ||
		cfg.KaTeXScript != "/opt/rin-renderer/engines/katex/render-katex.mjs" ||
		cfg.KaTeXVersion != "0.16.22" {
		t.Fatalf("expected math config from env, got %#v", cfg)
	}

	t.Setenv("RIN_RENDERER_DEFAULT_MATH_POLICY", "unknown")
	cfg = ConfigFromEnv()
	if cfg.DefaultMathPolicy != "server" {
		t.Fatalf("expected invalid math policy to fall back to server, got %q", cfg.DefaultMathPolicy)
	}
	t.Setenv("RIN_RENDERER_MATH_PRIMARY_ENGINE", "unknown")
	cfg = ConfigFromEnv()
	if cfg.MathPrimaryEngine != "mathjax-chtml" {
		t.Fatalf("expected invalid math primary engine to fall back to mathjax-chtml, got %q", cfg.MathPrimaryEngine)
	}
	t.Setenv("RIN_RENDERER_MATH_OUTPUT_STRATEGY", "unknown")
	cfg = ConfigFromEnv()
	if cfg.MathOutputStrategy != "chtml" {
		t.Fatalf("expected invalid math output strategy to fall back to chtml, got %q", cfg.MathOutputStrategy)
	}
}

func TestConfigFromEnvDiagramTimeoutInheritsRenderTimeoutUnlessOverridden(t *testing.T) {
	t.Setenv("RIN_RENDERER_RENDER_TIMEOUT_SECONDS", "300")
	t.Setenv("RIN_RENDERER_DIAGRAM_TIMEOUT_SECONDS", "")

	cfg := ConfigFromEnv()
	if cfg.RenderTimeout != 300*time.Second || cfg.DiagramTimeout != cfg.RenderTimeout {
		t.Fatalf("diagram timeout should inherit render timeout, got render=%s diagram=%s", cfg.RenderTimeout, cfg.DiagramTimeout)
	}

	t.Setenv("RIN_RENDERER_DIAGRAM_TIMEOUT_SECONDS", "45")
	cfg = ConfigFromEnv()
	if cfg.DiagramTimeout != 45*time.Second {
		t.Fatalf("explicit diagram timeout should override inherited timeout, got %s", cfg.DiagramTimeout)
	}
}

func TestConfigFromEnvReadsWarmMathJaxWorkerPolicy(t *testing.T) {
	t.Setenv("RIN_RENDERER_MATHJAX_WARM_WORKERS", "true")
	t.Setenv("RIN_RENDERER_MATHJAX_WARM_WORKER_COUNT", "3")
	t.Setenv("RIN_RENDERER_MATHJAX_WARM_MAX_REQUEST_BYTES", "4096")
	t.Setenv("RIN_RENDERER_MATHJAX_WARM_MAX_RESPONSE_BYTES", "8192")
	t.Setenv("RIN_RENDERER_MATHJAX_WARM_MAX_TASKS", "17")
	t.Setenv("RIN_RENDERER_MATHJAX_WARM_MAX_RSS_BYTES", "65536")
	t.Setenv("RIN_RENDERER_MATHJAX_WARM_START_TIMEOUT_SECONDS", "4")
	t.Setenv("RIN_RENDERER_MATHJAX_WARM_STOP_GRACE_MILLISECONDS", "250")
	cfg := ConfigFromEnv()
	if !cfg.MathJaxWarmWorkersEnabled || cfg.MathJaxWarmWorkerCount != 3 ||
		cfg.MathJaxWarmMaxRequestBytes != 4096 || cfg.MathJaxWarmMaxResponseBytes != 8192 ||
		cfg.MathJaxWarmMaxTasks != 17 || cfg.MathJaxWarmMaxRSSBytes != 65536 ||
		cfg.MathJaxWarmStartTimeout != 4*time.Second || cfg.MathJaxWarmStopGrace != 250*time.Millisecond {
		t.Fatalf("unexpected warm worker config: %#v", cfg)
	}

	t.Setenv("RIN_RENDERER_MATHJAX_WARM_WORKERS", "invalid")
	t.Setenv("RIN_RENDERER_MATHJAX_WARM_WORKER_COUNT", "0")
	cfg = ConfigFromEnv()
	if cfg.MathJaxWarmWorkersEnabled || cfg.MathJaxWarmWorkerCount != defaultMathJaxWarmWorkers {
		t.Fatalf("invalid warm worker settings should use safe defaults: %#v", cfg)
	}
}

func TestConfigFromEnvReadsMarkdownWorkerPolicy(t *testing.T) {
	t.Setenv("RIN_RENDERER_MARKDOWN_NODE_BIN", "/usr/local/bin/node")
	t.Setenv("RIN_RENDERER_MARKDOWN_SCRIPT", "/opt/rin-renderer/engines/markdown/worker.mjs")
	t.Setenv("RIN_RENDERER_MARKDOWN_WORKER_COUNT", "3")
	t.Setenv("RIN_RENDERER_MARKDOWN_MAX_REQUEST_BYTES", "4096")
	t.Setenv("RIN_RENDERER_MARKDOWN_MAX_RESPONSE_BYTES", "8192")
	t.Setenv("RIN_RENDERER_MARKDOWN_MAX_TASKS", "17")
	t.Setenv("RIN_RENDERER_MARKDOWN_MAX_RSS_BYTES", "65536")
	t.Setenv("RIN_RENDERER_MARKDOWN_START_TIMEOUT_SECONDS", "4")
	t.Setenv("RIN_RENDERER_MARKDOWN_STOP_GRACE_MILLISECONDS", "250")
	cfg := ConfigFromEnv()
	if cfg.MarkdownNodeBin != "/usr/local/bin/node" || cfg.MarkdownScript != "/opt/rin-renderer/engines/markdown/worker.mjs" ||
		cfg.MarkdownWorkerCount != 3 || cfg.MarkdownMaxRequestBytes != 4096 || cfg.MarkdownMaxResponseBytes != 8192 ||
		cfg.MarkdownMaxTasks != 17 || cfg.MarkdownMaxRSSBytes != 65536 || cfg.MarkdownStartTimeout != 4*time.Second ||
		cfg.MarkdownStopGrace != 250*time.Millisecond {
		t.Fatalf("unexpected Markdown worker config: %#v", cfg)
	}

	t.Setenv("RIN_RENDERER_MARKDOWN_WORKER_COUNT", "0")
	t.Setenv("RIN_RENDERER_MARKDOWN_MAX_TASKS", "0")
	cfg = ConfigFromEnv()
	if cfg.MarkdownWorkerCount != defaultMarkdownWorkers || cfg.MarkdownMaxTasks != uint64(defaultMarkdownMaxTasks) {
		t.Fatalf("invalid Markdown worker settings should use safe defaults: %#v", cfg)
	}
}

func TestConfigFromEnvReadsMarkdownRepositoryAssetPromotionSwitch(t *testing.T) {
	t.Setenv("RIN_RENDERER_MARKDOWN_REPOSITORY_ASSETS_ENABLED", "false")
	if cfg := ConfigFromEnv(); cfg.MarkdownRepoAssets {
		t.Fatal("explicitly disabled Markdown repository asset promotion remained enabled")
	}
	t.Setenv("RIN_RENDERER_MARKDOWN_REPOSITORY_ASSETS_ENABLED", "true")
	if cfg := ConfigFromEnv(); !cfg.MarkdownRepoAssets {
		t.Fatal("explicitly enabled Markdown repository asset promotion remained disabled")
	}
}

func TestConfigFromEnvUsesDefaultStorageBucket(t *testing.T) {
	t.Setenv("RIN_RENDERER_STORAGE_BUCKET", "")
	t.Setenv("RINSPACE_STORAGE_BUCKET", "")
	t.Setenv("CLOUDBASE_STORAGE_BUCKET", "")
	t.Setenv("RENDERER_STORAGE_BUCKET", "")

	cfg := ConfigFromEnv()

	if cfg.StorageBucket != "rin-renderer" {
		t.Fatalf("expected default storage bucket rin-renderer, got %q", cfg.StorageBucket)
	}
}

func TestConfigFromEnvReadsLegacyStorageEnv(t *testing.T) {
	t.Setenv("RIN_RENDERER_CLOUDBASE_ENV_ID", "")
	t.Setenv("RINSPACE_CLOUDBASE_ENV_ID", " rin-env ")
	t.Setenv("RIN_RENDERER_CLOUDBASE_API_KEY", "")
	t.Setenv("RINSPACE_CLOUDBASE_API_KEY", " rin-key ")
	t.Setenv("RIN_RENDERER_STORAGE_BUCKET", "")
	t.Setenv("RINSPACE_STORAGE_BUCKET", " rin-bucket ")
	t.Setenv("RIN_RENDERER_STORAGE_BASE_URL", "")
	t.Setenv("RINSPACE_STORAGE_BASE_URL", " https://storage.example.test ")

	cfg := ConfigFromEnv()

	if cfg.StorageEnvID != "rin-env" ||
		cfg.StorageAccessToken != "rin-key" ||
		cfg.StorageBucket != "rin-bucket" ||
		cfg.StorageBaseURL != "https://storage.example.test" {
		t.Fatalf("expected legacy storage env fallback, got %#v", cfg)
	}
}

func TestConfigFromEnvReadsFinalOutputAndPublicStorageBoundary(t *testing.T) {
	t.Setenv("RIN_RENDERER_FINAL_OUTPUT_MAX_BYTES", "1048576")
	t.Setenv("RIN_RENDERER_STORAGE_PUBLIC_BASE_URL", " https://cdn.example.test ")

	cfg := ConfigFromEnv()

	if cfg.FinalOutputMaxBytes != 1048576 || cfg.StoragePublicBaseURL != "https://cdn.example.test" {
		t.Fatalf("unexpected final output/public storage config: %#v", cfg)
	}
}
