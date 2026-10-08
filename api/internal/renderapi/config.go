package renderapi

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"
)

// The default render timeout is a LaTeXML budget: conversion cost is dominated by the work,
// not by the API. A book with thousands of formulas is a legitimate multi-minute job, and the
// previous 120s ceiling forced large real books onto the fallback engine. The value stays
// overridable through RIN_RENDERER_RENDER_TIMEOUT_SECONDS.
const (
	defaultRendererVersion      = "0.1.0-dev"
	defaultDiagramEngine        = "rin-diagram-v19-smart-cjk"
	defaultProjectArchiveSize   = int64(64 << 20)
	defaultProjectFileCount     = 700
	defaultProjectFileSize      = int64(16 << 20)
	defaultLateXMLHTMLSize      = int64(32 << 20)
	defaultFinalOutputSize      = int64(32 << 20)
	defaultDiagramSize          = int64(128 << 10)
	defaultMathSourceSize       = int64(32 << 10)
	defaultRenderTimeout        = 60 * time.Minute
	defaultMathTimeout          = 20 * time.Second
	defaultDiagramParallelism   = 2
	defaultMathParallelism      = 4
	defaultMathFallbackCount    = 300
	defaultKaTeXVersion         = "0.16.22"
	defaultMathJaxVersion       = "4.1.3"
	defaultMathJaxFontVersion   = "mathjax-newcm-4.1.3"
	defaultMathJaxWarmWorkers   = 2
	defaultMathJaxRequestSize   = int64(32 << 20)
	defaultMathJaxResponseSize  = int64(64 << 20)
	defaultMathJaxMaxTasks      = int64(500)
	defaultMathJaxMaxRSS        = int64(512 << 20)
	defaultMarkdownWorkers      = 2
	defaultMarkdownRequestSize  = int64(32 << 20)
	defaultMarkdownResponseSize = int64(64 << 20)
	defaultMarkdownMaxTasks     = int64(500)
	defaultMarkdownMaxRSS       = int64(768 << 20)
	defaultLateXMLLockFile      = "/opt/rin-renderer/engines/latexml/latexml.lock"
	defaultTypstTimeout         = 120 * time.Second
	defaultTypstOutputSize      = int64(32 << 20)
)

type Config struct {
	Addr                        string
	PublicBaseURL               string
	ServiceToken                string
	DefaultDocumentEngine       string
	DefaultDiagramPolicy        string
	DefaultMathPolicy           string
	ProjectArchiveMaxBytes      int64
	ProjectFileMaxCount         int
	ProjectFileMaxBytes         int64
	DiagramMaxBytes             int64
	MathSourceMaxBytes          int64
	MathTexSVGFallbackMaxCount  int
	RenderTimeout               time.Duration
	DiagramTimeout              time.Duration
	MathTimeout                 time.Duration
	DiagramParallelism          int
	MathParallelism             int
	LateXMLBin                  string
	LateXMLWorkerEndpoint       string
	LateXMLWorkerToken          string
	LateXMLLockFile             string
	LateXMLProfile              string
	LateXMLIncludeStyles        bool
	LateXMLMaxHTMLBytes         int64
	FinalOutputMaxBytes         int64
	MathPrimaryEngine           string
	MathOutputStrategy          string
	MathJaxNodeBin              string
	MathJaxScript               string
	MathJaxVersion              string
	MathJaxOutput               string
	MathJaxFontVersion          string
	MathJaxWarmWorkersEnabled   bool
	MathJaxWarmWorkerCount      int
	MathJaxWarmMaxRequestBytes  int64
	MathJaxWarmMaxResponseBytes int64
	MathJaxWarmMaxTasks         uint64
	MathJaxWarmMaxRSSBytes      uint64
	MathJaxWarmStartTimeout     time.Duration
	MathJaxWarmStopGrace        time.Duration
	MarkdownNodeBin             string
	MarkdownScript              string
	MarkdownWorkerCount         int
	MarkdownMaxRequestBytes     int64
	MarkdownMaxResponseBytes    int64
	MarkdownMaxTasks            uint64
	MarkdownMaxRSSBytes         uint64
	MarkdownStartTimeout        time.Duration
	MarkdownStopGrace           time.Duration
	MarkdownRepoAssets          bool
	KaTeXNodeBin                string
	TypstBin                    string
	TypstBinHash                string
	TypstFontPath               string
	TypstFontHash               string
	TypstPackageHash            string
	TypstVersion                string
	TypstTimeout                time.Duration
	TypstMaxOutputBytes         int64
	TypstMathRender             bool
	KaTeXScript                 string
	DiagramWorkerEndpoint       string
	DiagramWorkerToken          string
	RendererVersion             string
	LateXMLVersion              string
	LaTeXMLAdapterVersion       string
	PerlVersion                 string
	TeXLiveVersion              string
	DvisvgmVersion              string
	DiagramEngineVersion        string
	KaTeXVersion                string
	StorageEnvID                string
	StorageAccessToken          string
	StorageBucket               string
	StorageBaseURL              string
	StoragePublicBaseURL        string
	StorageProvider             string
	LocalAssetRoot              string
	LocalPublicBaseURL          string
	ReadHeaderTimeout           time.Duration
}

func ConfigFromEnv() Config {
	documentEngine := cleanDocumentEngine(envString("RIN_RENDERER_DEFAULT_DOCUMENT_ENGINE", "latexml"))
	if documentEngine == "" {
		documentEngine = "latexml"
	}
	lock, lockPath := loadLateXMLLock(strings.TrimSpace(os.Getenv("RIN_RENDERER_LATEXML_LOCK_FILE")))
	lateXMLVersion := envString("RIN_RENDERER_LATEXML_VERSION", "")
	if lateXMLVersion == "" {
		lateXMLVersion = firstConfigNonEmpty(lock.Commit, lock.Tag)
	}
	texLiveVersion := envString("RIN_RENDERER_TEXLIVE_VERSION", "")
	if texLiveVersion == "" {
		texLiveVersion = lock.TeXLive
	}
	perlVersion := envString("RIN_RENDERER_PERL_VERSION", "")
	if perlVersion == "" {
		perlVersion = lock.Perl
	}
	adapterVersion := envString("RIN_RENDERER_LATEXML_ADAPTER_VERSION", "")
	if adapterVersion == "" {
		adapterVersion = lock.AdapterVersion
	}
	renderTimeout := envDurationSeconds("RIN_RENDERER_RENDER_TIMEOUT_SECONDS", defaultRenderTimeout)
	diagramTimeout := envDurationSeconds("RIN_RENDERER_DIAGRAM_TIMEOUT_SECONDS", renderTimeout)
	return Config{
		Addr:                        envString("RIN_RENDERER_ADDR", ":8090"),
		PublicBaseURL:               envString("RIN_RENDERER_PUBLIC_BASE_URL", ""),
		ServiceToken:                strings.TrimSpace(os.Getenv("RIN_RENDERER_SERVICE_TOKEN")),
		DefaultDocumentEngine:       documentEngine,
		DefaultDiagramPolicy:        cleanDiagramPolicy(envString("RIN_RENDERER_DEFAULT_DIAGRAM_POLICY", "rin")),
		DefaultMathPolicy:           cleanMathPolicy(envString("RIN_RENDERER_DEFAULT_MATH_POLICY", "server")),
		ProjectArchiveMaxBytes:      envInt64("RIN_RENDERER_PROJECT_ARCHIVE_MAX_BYTES", defaultProjectArchiveSize),
		ProjectFileMaxCount:         envInt("RIN_RENDERER_PROJECT_FILE_MAX_COUNT", defaultProjectFileCount),
		ProjectFileMaxBytes:         envInt64("RIN_RENDERER_PROJECT_FILE_MAX_BYTES", defaultProjectFileSize),
		DiagramMaxBytes:             envInt64("RIN_RENDERER_DIAGRAM_MAX_BYTES", defaultDiagramSize),
		MathSourceMaxBytes:          envInt64("RIN_RENDERER_MATH_SOURCE_MAX_BYTES", defaultMathSourceSize),
		MathTexSVGFallbackMaxCount:  envInt("RIN_RENDERER_MATH_TEXSVG_FALLBACK_MAX_COUNT", defaultMathFallbackCount),
		RenderTimeout:               renderTimeout,
		DiagramTimeout:              diagramTimeout,
		MathTimeout:                 envDurationSeconds("RIN_RENDERER_MATH_TIMEOUT_SECONDS", defaultMathTimeout),
		DiagramParallelism:          envInt("RIN_RENDERER_DIAGRAM_PARALLELISM", defaultDiagramParallelism),
		MathParallelism:             envInt("RIN_RENDERER_MATH_PARALLELISM", defaultMathParallelism),
		LateXMLBin:                  envString("RIN_RENDERER_LATEXML_BIN", "latexmlc"),
		LateXMLWorkerEndpoint:       envString("RIN_RENDERER_LATEXML_ENDPOINT", ""),
		LateXMLWorkerToken:          strings.TrimSpace(os.Getenv("RIN_RENDERER_LATEXML_TOKEN")),
		LateXMLLockFile:             lockPath,
		LateXMLProfile:              envString("RIN_RENDERER_LATEXML_PROFILE", ""),
		LateXMLIncludeStyles:        envBool("RIN_RENDERER_LATEXML_INCLUDE_STYLES", true),
		LateXMLMaxHTMLBytes:         envInt64("RIN_RENDERER_LATEXML_MAX_HTML_BYTES", defaultLateXMLHTMLSize),
		FinalOutputMaxBytes:         envInt64("RIN_RENDERER_FINAL_OUTPUT_MAX_BYTES", defaultFinalOutputSize),
		MathPrimaryEngine:           cleanMathEngine(envString("RIN_RENDERER_MATH_PRIMARY_ENGINE", "mathjax-chtml")),
		MathOutputStrategy:          cleanMathOutputStrategy(envString("RIN_RENDERER_MATH_OUTPUT_STRATEGY", "chtml")),
		MathJaxNodeBin:              envString("RIN_RENDERER_MATHJAX_NODE_BIN", "node"),
		MathJaxScript:               envString("RIN_RENDERER_MATHJAX_SCRIPT", "../engines/mathjax/render-mathjax.mjs"),
		MathJaxVersion:              envString("RIN_RENDERER_MATHJAX_VERSION", defaultMathJaxVersion),
		MathJaxOutput:               envString("RIN_RENDERER_MATHJAX_OUTPUT", "chtml"),
		MathJaxFontVersion:          envString("RIN_RENDERER_MATHJAX_FONT_VERSION", defaultMathJaxFontVersion),
		MathJaxWarmWorkersEnabled:   envBool("RIN_RENDERER_MATHJAX_WARM_WORKERS", false),
		MathJaxWarmWorkerCount:      envPositiveInt("RIN_RENDERER_MATHJAX_WARM_WORKER_COUNT", defaultMathJaxWarmWorkers),
		MathJaxWarmMaxRequestBytes:  envPositiveInt64("RIN_RENDERER_MATHJAX_WARM_MAX_REQUEST_BYTES", defaultMathJaxRequestSize),
		MathJaxWarmMaxResponseBytes: envPositiveInt64("RIN_RENDERER_MATHJAX_WARM_MAX_RESPONSE_BYTES", defaultMathJaxResponseSize),
		MathJaxWarmMaxTasks:         uint64(envPositiveInt64("RIN_RENDERER_MATHJAX_WARM_MAX_TASKS", defaultMathJaxMaxTasks)),
		MathJaxWarmMaxRSSBytes:      uint64(envPositiveInt64("RIN_RENDERER_MATHJAX_WARM_MAX_RSS_BYTES", defaultMathJaxMaxRSS)),
		MathJaxWarmStartTimeout:     time.Duration(envPositiveInt64("RIN_RENDERER_MATHJAX_WARM_START_TIMEOUT_SECONDS", 10)) * time.Second,
		MathJaxWarmStopGrace:        time.Duration(envPositiveInt64("RIN_RENDERER_MATHJAX_WARM_STOP_GRACE_MILLISECONDS", 1000)) * time.Millisecond,
		MarkdownNodeBin:             envString("RIN_RENDERER_MARKDOWN_NODE_BIN", "node"),
		MarkdownScript:              envString("RIN_RENDERER_MARKDOWN_SCRIPT", "../engines/markdown/worker.mjs"),
		MarkdownWorkerCount:         envPositiveInt("RIN_RENDERER_MARKDOWN_WORKER_COUNT", defaultMarkdownWorkers),
		MarkdownMaxRequestBytes:     envPositiveInt64("RIN_RENDERER_MARKDOWN_MAX_REQUEST_BYTES", defaultMarkdownRequestSize),
		MarkdownMaxResponseBytes:    envPositiveInt64("RIN_RENDERER_MARKDOWN_MAX_RESPONSE_BYTES", defaultMarkdownResponseSize),
		MarkdownMaxTasks:            uint64(envPositiveInt64("RIN_RENDERER_MARKDOWN_MAX_TASKS", defaultMarkdownMaxTasks)),
		MarkdownMaxRSSBytes:         uint64(envPositiveInt64("RIN_RENDERER_MARKDOWN_MAX_RSS_BYTES", defaultMarkdownMaxRSS)),
		MarkdownStartTimeout:        time.Duration(envPositiveInt64("RIN_RENDERER_MARKDOWN_START_TIMEOUT_SECONDS", 15)) * time.Second,
		MarkdownStopGrace:           time.Duration(envPositiveInt64("RIN_RENDERER_MARKDOWN_STOP_GRACE_MILLISECONDS", 1000)) * time.Millisecond,
		MarkdownRepoAssets:          envBool("RIN_RENDERER_MARKDOWN_REPOSITORY_ASSETS_ENABLED", true),
		KaTeXNodeBin:                envString("RIN_RENDERER_KATEX_NODE_BIN", "node"),
		TypstBin:                    envString("RIN_RENDERER_TYPST_BIN", ""),
		TypstBinHash:                strings.ToLower(strings.TrimSpace(os.Getenv("RIN_RENDERER_TYPST_BIN_SHA256"))),
		TypstFontPath:               envString("RIN_RENDERER_TYPST_FONT_PATH", ""),
		TypstFontHash:               strings.ToLower(strings.TrimSpace(os.Getenv("RIN_RENDERER_TYPST_FONT_SHA256"))),
		TypstPackageHash:            strings.ToLower(strings.TrimSpace(os.Getenv("RIN_RENDERER_TYPST_PACKAGE_SHA256"))),
		TypstVersion:                envString("RIN_RENDERER_TYPST_VERSION", ""),
		TypstTimeout:                envDurationSeconds("RIN_RENDERER_TYPST_TIMEOUT_SECONDS", defaultTypstTimeout),
		TypstMaxOutputBytes:         envInt64("RIN_RENDERER_TYPST_MAX_OUTPUT_BYTES", defaultTypstOutputSize),
		TypstMathRender:             envBool("RIN_RENDERER_TYPST_MATH_RENDER", false),
		KaTeXScript:                 envString("RIN_RENDERER_KATEX_SCRIPT", "../engines/katex/render-katex.mjs"),
		DiagramWorkerEndpoint:       envString("RIN_RENDERER_TEXSVG_ENDPOINT", ""),
		DiagramWorkerToken:          strings.TrimSpace(os.Getenv("RIN_RENDERER_TEXSVG_TOKEN")),
		RendererVersion:             envString("RIN_RENDERER_VERSION", defaultRendererVersion),
		LateXMLVersion:              lateXMLVersion,
		LaTeXMLAdapterVersion:       adapterVersion,
		PerlVersion:                 perlVersion,
		TeXLiveVersion:              texLiveVersion,
		DvisvgmVersion:              envString("RIN_RENDERER_DVISVGM_VERSION", ""),
		DiagramEngineVersion:        envString("RIN_RENDERER_DIAGRAM_ENGINE_VERSION", defaultDiagramEngine),
		KaTeXVersion:                envString("RIN_RENDERER_KATEX_VERSION", defaultKaTeXVersion),
		StorageEnvID: envStringAny(
			"RIN_RENDERER_CLOUDBASE_ENV_ID",
			"RINSPACE_CLOUDBASE_ENV_ID",
			"CLOUDBASE_ENV_ID",
			"RENDERER_CLOUDBASE_ENV_ID",
		),
		StorageAccessToken: envStringAny(
			"RIN_RENDERER_CLOUDBASE_API_KEY",
			"RINSPACE_CLOUDBASE_API_KEY",
			"CLOUDBASE_API_KEY",
			"RENDERER_CLOUDBASE_API_KEY",
		),
		StorageBucket: envStringAnyDefault("rin-renderer",
			"RIN_RENDERER_STORAGE_BUCKET",
			"RINSPACE_STORAGE_BUCKET",
			"CLOUDBASE_STORAGE_BUCKET",
			"RENDERER_STORAGE_BUCKET",
		),
		StoragePublicBaseURL: envString("RIN_RENDERER_STORAGE_PUBLIC_BASE_URL", ""),
		StorageProvider:      envString("RIN_RENDERER_STORAGE_PROVIDER", "cloudbase"),
		LocalAssetRoot:       envString("RIN_RENDERER_LOCAL_ASSET_ROOT", ""),
		LocalPublicBaseURL:   envString("RIN_RENDERER_LOCAL_PUBLIC_BASE_URL", ""),
		StorageBaseURL: envStringAny(
			"RIN_RENDERER_STORAGE_BASE_URL",
			"RINSPACE_STORAGE_BASE_URL",
			"CLOUDBASE_STORAGE_BASE_URL",
			"RENDERER_STORAGE_BASE_URL",
		),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

type lateXMLLock struct {
	Repo           string `json:"repo"`
	Commit         string `json:"commit"`
	Tag            string `json:"tag"`
	TeXLive        string `json:"texlive"`
	Perl           string `json:"perl"`
	AdapterVersion string `json:"adapterVersion"`
	CorpusVersion  string `json:"corpusVersion"`
}

func loadLateXMLLock(configuredPath string) (lateXMLLock, string) {
	if configuredPath != "" {
		lock, ok := readLateXMLLock(configuredPath)
		if ok {
			return lock, configuredPath
		}
		return lateXMLLock{}, configuredPath
	}
	for _, path := range []string{
		defaultLateXMLLockFile,
		"../engines/latexml/latexml.lock",
		"../../../engines/latexml/latexml.lock",
		"rin-renderer/engines/latexml/latexml.lock",
	} {
		lock, ok := readLateXMLLock(path)
		if ok {
			return lock, path
		}
	}
	return lateXMLLock{}, defaultLateXMLLockFile
}

func readLateXMLLock(path string) (lateXMLLock, bool) {
	body, err := os.ReadFile(path)
	if err != nil {
		return lateXMLLock{}, false
	}
	var lock lateXMLLock
	if err := json.Unmarshal(body, &lock); err != nil {
		return lateXMLLock{}, false
	}
	return lock, true
}

func firstConfigNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func envString(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envPositiveInt(key string, fallback int) int {
	value := envInt(key, fallback)
	if value <= 0 {
		return fallback
	}
	return value
}

func envPositiveInt64(key string, fallback int64) int64 {
	value := envInt64(key, fallback)
	if value <= 0 {
		return fallback
	}
	return value
}

func envBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt64(key string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func envDurationSeconds(key string, fallback time.Duration) time.Duration {
	seconds := envInt64(key, int64(fallback/time.Second))
	return time.Duration(seconds) * time.Second
}

func envStringAny(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func envStringAnyDefault(fallback string, keys ...string) string {
	if value := envStringAny(keys...); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}
