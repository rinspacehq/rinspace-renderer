package projectcore

type Limits struct {
	ArchiveMaxBytes int64
	FileMaxCount    int
	FileMaxBytes    int64
}

type File struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Encoding string `json:"encoding,omitempty"`
	MIME     string `json:"mime,omitempty"`
	Body     string `json:"body,omitempty"`
	Bytes    int64  `json:"bytes"`
}

type Diagnostic struct {
	Severity string            `json:"severity"`
	Code     string            `json:"code"`
	Message  string            `json:"message"`
	Source   map[string]string `json:"source,omitempty"`
}

type Manifest struct {
	Version             string                 `json:"version"`
	Title               string                 `json:"title"`
	MainFile            string                 `json:"mainFile"`
	Files               []File                 `json:"files"`
	FileCount           int                    `json:"fileCount"`
	Source              string                 `json:"source,omitempty"`
	AnalysisSource      string                 `json:"analysisSource,omitempty"`
	ResolvedSource      string                 `json:"resolvedSource,omitempty"`
	Includes            []Reference            `json:"includes,omitempty"`
	DiagramPlaceholders []DiagramPlaceholder   `json:"diagramPlaceholders"`
	AssetInventory      AssetInventory         `json:"assetInventory"`
	Bibliography        BibliographyInventory  `json:"bibliography"`
	GeneratedLists      GeneratedListInventory `json:"generatedLists"`
	Diagnostics         []Diagnostic           `json:"diagnostics,omitempty"`
}

type BuildOptions struct {
	Title            string
	ExplicitMainFile string
	MetadataMainFile string
	ActiveFile       string
}

type Resolution struct {
	Source         string
	AnalysisSource string
	ResolvedSource string
	Includes       []Reference
	AssetInventory AssetInventory
	Bibliography   BibliographyInventory
	GeneratedLists GeneratedListInventory
	Diagnostics    []Diagnostic
}

type DiagramPlaceholder struct {
	ID           string        `json:"id"`
	Type         string        `json:"type"`
	Options      string        `json:"options,omitempty"`
	Body         string        `json:"body"`
	Source       string        `json:"source"`
	SourceFile   string        `json:"sourceFile"`
	SourceLine   int           `json:"sourceLine"`
	SourceColumn int           `json:"sourceColumn"`
	Placeholder  string        `json:"placeholder"`
	Layout       DiagramLayout `json:"layout"`
}

type DiagramLayout struct {
	Alignment string `json:"alignment,omitempty"`
}

type Reference struct {
	Kind       string `json:"kind"`
	Command    string `json:"command"`
	RawRef     string `json:"rawRef"`
	Path       string `json:"path,omitempty"`
	SourceFile string `json:"sourceFile"`
	Line       int    `json:"line"`
	Resolved   bool   `json:"resolved"`
}

type Asset struct {
	Path       string      `json:"path"`
	MIME       string      `json:"mime,omitempty"`
	Encoding   string      `json:"encoding,omitempty"`
	Referenced bool        `json:"referenced"`
	References []Reference `json:"references,omitempty"`
}

type AssetInventory struct {
	References         []Reference `json:"references"`
	Assets             []Asset     `json:"assets"`
	Missing            []Reference `json:"missing"`
	Unused             []Asset     `json:"unused"`
	GraphicsPaths      []string    `json:"graphicsPaths"`
	GraphicsExtensions []string    `json:"graphicsExtensions"`
}

type BibliographyInventory struct {
	References    []Reference `json:"references"`
	Missing       []Reference `json:"missing"`
	BibPaths      []string    `json:"bibPaths"`
	Styles        []Reference `json:"styles"`
	MissingStyles []Reference `json:"missingStyles"`
}

type GeneratedListInventory struct {
	References []GeneratedListReference `json:"references"`
	Missing    []GeneratedListReference `json:"missing"`
}

type GeneratedListReference struct {
	Kind               string   `json:"kind"`
	ListType           string   `json:"listType"`
	Command            string   `json:"command"`
	Label              string   `json:"label"`
	Path               string   `json:"path,omitempty"`
	Resolved           bool     `json:"resolved"`
	ExpectedExtensions []string `json:"expectedExtensions"`
	FallbackExtensions []string `json:"fallbackExtensions,omitempty"`
	SourceFile         string   `json:"sourceFile"`
	Line               int      `json:"line"`
}
