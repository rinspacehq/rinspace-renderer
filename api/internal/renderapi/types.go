package renderapi

import "github.com/rinspacehq/rinspace-renderer/api/internal/contracts"

type Diagnostic struct {
	Severity string            `json:"severity"`
	Code     string            `json:"code"`
	Message  string            `json:"message"`
	Engine   string            `json:"engine,omitempty"`
	Source   map[string]string `json:"source,omitempty"`
}

type Versions struct {
	RinRenderer    string `json:"rinRenderer"`
	FinalOutput    string `json:"finalOutput"`
	LateXML        string `json:"latexml,omitempty"`
	Typst          string `json:"typst,omitempty"`
	TypstProfile   string `json:"typstProfile,omitempty"`
	LaTeXMLAdapter string `json:"latexmlAdapter,omitempty"`
	Perl           string `json:"perl,omitempty"`
	TeXLive        string `json:"texlive,omitempty"`
	Dvisvgm        string `json:"dvisvgm,omitempty"`
	DiagramEngine  string `json:"diagramEngine,omitempty"`
	MathJax        string `json:"mathjax,omitempty"`
	MathJaxOutput  string `json:"mathjaxOutput,omitempty"`
	MathJaxFont    string `json:"mathjaxFont,omitempty"`
	KaTeX          string `json:"katex,omitempty"`
}

type Limits struct {
	ProjectArchiveMaxBytes     int64 `json:"projectArchiveMaxBytes"`
	ProjectFileMaxCount        int   `json:"projectFileMaxCount"`
	ProjectFileMaxBytes        int64 `json:"projectFileMaxBytes"`
	DiagramMaxBytes            int64 `json:"diagramMaxBytes"`
	FinalOutputMaxBytes        int64 `json:"finalOutputMaxBytes"`
	RenderTimeoutSeconds       int64 `json:"renderTimeoutSeconds"`
	DiagramTimeoutSeconds      int64 `json:"diagramTimeoutSeconds"`
	DiagramParallelism         int   `json:"diagramParallelism"`
	MathSourceMaxBytes         int64 `json:"mathSourceMaxBytes"`
	MathTexSVGFallbackMaxCount int   `json:"mathTexSvgFallbackMaxCount"`
	MathTimeoutSeconds         int64 `json:"mathTimeoutSeconds"`
	MathParallelism            int   `json:"mathParallelism"`
}

type CapabilitiesResponse struct {
	Version      string                 `json:"version"`
	Engines      map[string][]string    `json:"engines"`
	DiagramTypes []string               `json:"diagramTypes"`
	Limits       Limits                 `json:"limits"`
	Versions     Versions               `json:"versions"`
	Storage      StorageInfo            `json:"storage"`
	Async        AsyncCapabilities      `json:"async"`
	NodeWorkers  NodeWorkerCapabilities `json:"nodeWorkers"`
}

type NodeWorkerCapabilities struct {
	Enabled          bool   `json:"enabled"`
	Transport        string `json:"transport"`
	ContractVersion  string `json:"contractVersion"`
	MathJaxMode      string `json:"mathJaxMode"`
	Count            int    `json:"count"`
	MaxRequestBytes  int64  `json:"maxRequestBytes"`
	MaxResponseBytes int64  `json:"maxResponseBytes"`
	MaxTasks         uint64 `json:"maxTasks"`
	MaxRSSBytes      uint64 `json:"maxRssBytes"`
}

type AsyncCapabilities struct {
	Enabled         bool     `json:"enabled"`
	QueueScope      string   `json:"queueScope"`
	EventReplay     bool     `json:"eventReplay"`
	PollingFallback bool     `json:"pollingFallback"`
	Cancellation    bool     `json:"cancellation"`
	Readiness       string   `json:"readiness"`
	Degraded        []string `json:"degraded"`
}

type StorageInfo struct {
	Provider string `json:"provider"`
	Bucket   string `json:"bucket"`
	BaseURL  string `json:"baseUrl,omitempty"`
}

type ProjectRenderResponse struct {
	RequestID          string                        `json:"requestId"`
	Title              string                        `json:"title"`
	HTML               string                        `json:"html"`
	Engine             string                        `json:"engine"`
	Fallback           bool                          `json:"fallback"`
	PrimaryEngine      string                        `json:"primaryEngine,omitempty"`
	FallbackEngine     string                        `json:"fallbackEngine,omitempty"`
	MainFile           string                        `json:"mainFile"`
	Diagrams           []DiagramRef                  `json:"diagrams"`
	GeneratedArtifacts []contracts.ArtifactReference `json:"generatedArtifacts,omitempty"`
	Assets             []AssetRef                    `json:"assets"`
	AssetFiles         []AssetFile                   `json:"assetFiles,omitempty"`
	AssetManifest      any                           `json:"assetManifest,omitempty"`
	Reader             any                           `json:"reader"`
	Math               MathSummary                   `json:"math"`
	Versions           Versions                      `json:"versions"`
	Diagnostics        []Diagnostic                  `json:"diagnostics"`
	KnowledgeIndex     *contracts.KnowledgeIndex     `json:"knowledgeIndex,omitempty"`

	TexSource      string `json:"texSource,omitempty"`
	Source         string `json:"source,omitempty"`
	AnalysisSource string `json:"analysisSource,omitempty"`
	ResolvedSource string `json:"resolvedSource,omitempty"`
	AssetInventory any    `json:"assetInventory,omitempty"`
	Project        any    `json:"project,omitempty"`
	DocumentMode   string `json:"documentMode,omitempty"`
}

type MathSummary struct {
	Count            int `json:"count"`
	MathJaxCount     int `json:"mathJaxCount"`
	KaTeXCount       int `json:"katexCount"`
	SVGFallbackCount int `json:"svgFallbackCount"`
	FailedCount      int `json:"failedCount"`
}

type DiagramRef struct {
	Type         string `json:"type"`
	ObjectID     string `json:"objectId"`
	CloudBaseURL string `json:"cloudbaseUrl"`
}

type AssetRef struct {
	Path string `json:"path"`
	MIME string `json:"mime,omitempty"`
}

type AssetFile struct {
	Path       string `json:"path"`
	Filename   string `json:"filename,omitempty"`
	MIME       string `json:"mime,omitempty"`
	Encoding   string `json:"encoding,omitempty"`
	Body       string `json:"body,omitempty"`
	Bytes      int64  `json:"bytes,omitempty"`
	Referenced *bool  `json:"referenced,omitempty"`
}

type DiagramRequest struct {
	Type    string `json:"type,omitempty"`
	Options string `json:"options,omitempty"`
	Body    string `json:"body,omitempty"`
	Source  string `json:"source,omitempty"`
}

type DiagramRenderResponse struct {
	RequestID    string       `json:"requestId"`
	ID           string       `json:"id"`
	Type         string       `json:"type"`
	SVGHash      string       `json:"svgHash"`
	SVGBytes     int64        `json:"svgBytes,omitempty"`
	ObjectID     string       `json:"objectId"`
	CloudBaseURL string       `json:"cloudbaseUrl"`
	URL          string       `json:"url"`
	Uploaded     bool         `json:"uploaded"`
	Engine       string       `json:"engine"`
	Versions     Versions     `json:"versions"`
	Diagnostics  []Diagnostic `json:"diagnostics"`
	Cached       bool         `json:"cached"`
	RenderURL    string       `json:"renderUrl,omitempty"`
}

type ErrorResponse struct {
	RequestID   string       `json:"requestId,omitempty"`
	Error       string       `json:"error"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}
