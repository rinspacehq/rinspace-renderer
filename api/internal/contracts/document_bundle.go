package contracts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const (
	DocumentBundleSchemaVersionV1 = "rin-document-bundle/v1"
	DocumentBundleSchemaVersionV2 = "rin-document-bundle/v2"
	// DocumentBundleSchemaVersion remains the producer version until every v2 block emitter is ready.
	DocumentBundleSchemaVersion = DocumentBundleSchemaVersionV1
	FragmentFormatPlaceholders  = "html-with-rin-placeholders"
	FragmentFormatHTML          = "html"
	DocumentBundleStateDraft    = "draft"
	DocumentBundleStateFinal    = "final"
)

var (
	identifierPattern      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._:-]{0,127}$`)
	workIDPattern          = regexp.MustCompile(`^rw_[a-f0-9]{32}$`)
	blockIDPattern         = regexp.MustCompile(`^rb_[a-f0-9]{32}$`)
	canonicalWorkV1Pattern = regexp.MustCompile(`<rin-work data-id="(rw_[a-f0-9]{32})"></rin-work>`)
	canonicalWorkV2Pattern = regexp.MustCompile(`<rin-work data-id="(rw_[a-f0-9]{32})"(?: data-rin-block-id="(rb_[a-f0-9]{32})" data-rin-block-kind="(heading|paragraph|list-item|theorem|math|code|figure|table|quote)")?></rin-work>`)
	anyWorkElementPattern  = regexp.MustCompile(`(?i)<\s*/?\s*rin-work\b`)
)

type DocumentBundle struct {
	SchemaVersion  string           `json:"schemaVersion"`
	ProjectHash    string           `json:"projectHash"`
	BundleHash     string           `json:"bundleHash,omitempty"`
	State          string           `json:"state"`
	ContentKind    ContentKind      `json:"contentKind"`
	DocumentEngine string           `json:"documentEngine"`
	Title          string           `json:"title"`
	Pages          []DocumentPage   `json:"pages"`
	WorkUnits      []WorkUnit       `json:"workUnits"`
	Assets         []AssetReference `json:"assets"`
	Diagnostics    []Diagnostic     `json:"diagnostics"`
	Provenance     Provenance       `json:"provenance"`
	KnowledgeIndex *KnowledgeIndex  `json:"knowledgeIndex,omitempty"`
}

type DocumentPage struct {
	ID               string          `json:"id"`
	SourcePath       string          `json:"sourcePath"`
	Title            string          `json:"title,omitempty"`
	Fragment         string          `json:"fragment"`
	FragmentFormat   string          `json:"fragmentFormat"`
	TOC              []TOCEntry      `json:"toc"`
	DependencyHashes []string        `json:"dependencyHashes"`
	Blocks           []DocumentBlock `json:"blocks,omitempty"`
}

type DocumentBlockKind string

const (
	DocumentBlockHeading   DocumentBlockKind = "heading"
	DocumentBlockParagraph DocumentBlockKind = "paragraph"
	DocumentBlockListItem  DocumentBlockKind = "list-item"
	DocumentBlockTheorem   DocumentBlockKind = "theorem"
	DocumentBlockMath      DocumentBlockKind = "math"
	DocumentBlockCode      DocumentBlockKind = "code"
	DocumentBlockFigure    DocumentBlockKind = "figure"
	DocumentBlockTable     DocumentBlockKind = "table"
	DocumentBlockQuote     DocumentBlockKind = "quote"
)

type DocumentBlock struct {
	ID             string            `json:"id"`
	Kind           DocumentBlockKind `json:"kind"`
	Text           string            `json:"text"`
	TextHash       string            `json:"textHash"`
	HeadingPath    []string          `json:"headingPath"`
	SourceLocation *SourceLocation   `json:"sourceLocation,omitempty"`
}

type TOCEntry struct {
	ID    string `json:"id"`
	Depth int    `json:"depth"`
	Text  string `json:"text"`
}

type WorkUnitKind string

const (
	WorkUnitMath    WorkUnitKind = "math"
	WorkUnitDiagram WorkUnitKind = "diagram"
	WorkUnitCode    WorkUnitKind = "code"
)

// WorkUnit is a tagged union. Validate enforces the fields permitted and required by each Kind;
// adapters do not need to translate their internal LaTeXML DOM or MDAST into one another.
type WorkUnit struct {
	Kind                 WorkUnitKind              `json:"kind"`
	ID                   string                    `json:"id"`
	Source               string                    `json:"source"`
	Display              *bool                     `json:"display,omitempty"`
	MacroContextHash     string                    `json:"macroContextHash,omitempty"`
	AccessibilityContext *MathAccessibilityContext `json:"accessibilityContext,omitempty"`
	DiagramType          string                    `json:"diagramType,omitempty"`
	Options              string                    `json:"options,omitempty"`
	Layout               *DiagramLayout            `json:"layout,omitempty"`
	Language             string                    `json:"language,omitempty"`
	Meta                 string                    `json:"meta,omitempty"`
	SourceLocation       *SourceLocation           `json:"sourceLocation,omitempty"`
}

type MathAccessibilityContext struct {
	Language    string `json:"language,omitempty"`
	SpeechStyle string `json:"speechStyle,omitempty"`
	Label       string `json:"label,omitempty"`
}

type DiagramLayout struct {
	Alignment string `json:"alignment,omitempty"`
}

type SourceLocation struct {
	Path  string          `json:"path"`
	Start SourcePosition  `json:"start"`
	End   *SourcePosition `json:"end,omitempty"`
}

type SourcePosition struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type AssetReference struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
	MediaType   string `json:"mediaType"`
	ProjectPath string `json:"projectPath,omitempty"`
	ArtifactKey string `json:"artifactKey,omitempty"`
}

type Diagnostic struct {
	Code           string          `json:"code"`
	Severity       string          `json:"severity"`
	Message        string          `json:"message"`
	Stage          string          `json:"stage"`
	SourceLocation *SourceLocation `json:"sourceLocation,omitempty"`
}

type Provenance struct {
	Adapter                   string `json:"adapter"`
	AdapterVersion            string `json:"adapterVersion"`
	EngineVersion             string `json:"engineVersion"`
	ProjectGraphSchemaVersion string `json:"projectGraphSchemaVersion"`
}

// NewWorkID creates an adapter-owned, opaque identifier with 128 random bits. Adapters must call
// ValidateSourceHasNoWorkElements before injecting placeholders so user source cannot claim one.
func NewWorkID() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate work id: %w", err)
	}
	return "rw_" + hex.EncodeToString(random), nil
}

func WorkPlaceholder(id string) (string, error) {
	if !workIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid work id %q", id)
	}
	return `<rin-work data-id="` + id + `"></rin-work>`, nil
}

func ValidateSourceHasNoWorkElements(source string) error {
	if anyWorkElementPattern.MatchString(source) {
		return errors.New("source contains reserved rin-work element")
	}
	return nil
}

func (bundle DocumentBundle) Validate() error {
	if !SupportedDocumentBundleSchemaVersion(bundle.SchemaVersion) {
		return fmt.Errorf("unsupported document bundle schema version %q", bundle.SchemaVersion)
	}
	if !isSHA256(bundle.ProjectHash) || bundle.BundleHash != "" && !isSHA256(bundle.BundleHash) {
		return errors.New("document bundle has invalid project or bundle hash")
	}
	if !bundle.ContentKind.Valid() || strings.TrimSpace(bundle.DocumentEngine) == "" {
		return errors.New("document bundle has invalid content kind or engine")
	}
	if bundle.State != DocumentBundleStateDraft && bundle.State != DocumentBundleStateFinal {
		return fmt.Errorf("unsupported document bundle state %q", bundle.State)
	}
	if len(bundle.Pages) == 0 {
		return errors.New("document bundle requires at least one page")
	}
	if bundle.KnowledgeIndex != nil {
		if err := bundle.KnowledgeIndex.Validate(bundle.ProjectHash); err != nil {
			return fmt.Errorf("knowledge index: %w", err)
		}
	}

	workIDs := make(map[string]struct{}, len(bundle.WorkUnits))
	for index := range bundle.WorkUnits {
		unit := &bundle.WorkUnits[index]
		if err := unit.validate(); err != nil {
			return fmt.Errorf("work unit %d: %w", index, err)
		}
		if _, exists := workIDs[unit.ID]; exists {
			return fmt.Errorf("duplicate work id %q", unit.ID)
		}
		workIDs[unit.ID] = struct{}{}
	}

	pageIDs := map[string]struct{}{}
	placeholderCounts := map[string]int{}
	for index := range bundle.Pages {
		page := &bundle.Pages[index]
		if err := page.validate(bundle.State, bundle.SchemaVersion); err != nil {
			return fmt.Errorf("page %d: %w", index, err)
		}
		if _, exists := pageIDs[page.ID]; exists {
			return fmt.Errorf("duplicate page id %q", page.ID)
		}
		pageIDs[page.ID] = struct{}{}
		workPattern := canonicalWorkV1Pattern
		if bundle.SchemaVersion == DocumentBundleSchemaVersionV2 {
			workPattern = canonicalWorkV2Pattern
		}
		matches := workPattern.FindAllStringSubmatch(page.Fragment, -1)
		remaining := workPattern.ReplaceAllString(page.Fragment, "")
		if anyWorkElementPattern.MatchString(remaining) {
			return fmt.Errorf("page %q contains malformed or source-created rin-work element", page.ID)
		}
		for _, match := range matches {
			id := match[1]
			if bundle.State == DocumentBundleStateFinal {
				return fmt.Errorf("final page %q contains rin-work element", page.ID)
			}
			if _, exists := workIDs[id]; !exists {
				return fmt.Errorf("page %q references unknown work id %q", page.ID, id)
			}
			if len(match) > 3 && match[2] != "" {
				blockID, blockKind := match[2], DocumentBlockKind(match[3])
				matchedBlock := false
				for _, block := range page.Blocks {
					if block.ID == blockID && block.Kind == blockKind {
						matchedBlock = true
						break
					}
				}
				if !matchedBlock {
					return fmt.Errorf("page %q work id %q references unknown or mismatched block %q", page.ID, id, blockID)
				}
			}
			placeholderCounts[id]++
		}
	}
	if bundle.State == DocumentBundleStateDraft {
		for id := range workIDs {
			if placeholderCounts[id] != 1 {
				return fmt.Errorf("work id %q must have exactly one placeholder, got %d", id, placeholderCounts[id])
			}
		}
	}

	assetIDs := map[string]struct{}{}
	for index := range bundle.Assets {
		asset := &bundle.Assets[index]
		if err := asset.validate(); err != nil {
			return fmt.Errorf("asset %d: %w", index, err)
		}
		if _, exists := assetIDs[asset.ID]; exists {
			return fmt.Errorf("duplicate asset id %q", asset.ID)
		}
		assetIDs[asset.ID] = struct{}{}
	}
	for index := range bundle.Diagnostics {
		if err := bundle.Diagnostics[index].validate(); err != nil {
			return fmt.Errorf("diagnostic %d: %w", index, err)
		}
	}
	return bundle.Provenance.validate()
}

func (page DocumentPage) validate(bundleState string, schemaVersion string) error {
	if !ValidPageID(page.ID) {
		return fmt.Errorf("invalid page id %q", page.ID)
	}
	if err := validateCanonicalPath(page.SourcePath); err != nil {
		return fmt.Errorf("source path: %w", err)
	}
	expectedFormat := FragmentFormatPlaceholders
	if bundleState == DocumentBundleStateFinal {
		expectedFormat = FragmentFormatHTML
	}
	if page.FragmentFormat != expectedFormat {
		return fmt.Errorf("unsupported fragment format %q", page.FragmentFormat)
	}
	tocIDs := map[string]struct{}{}
	for _, entry := range page.TOC {
		if !ValidTOCID(entry.ID) || entry.Depth < 1 || entry.Depth > 6 {
			return fmt.Errorf("invalid toc entry %q", entry.ID)
		}
		if _, exists := tocIDs[entry.ID]; exists {
			return fmt.Errorf("duplicate toc id %q", entry.ID)
		}
		tocIDs[entry.ID] = struct{}{}
	}
	dependencies := map[string]struct{}{}
	for _, dependency := range page.DependencyHashes {
		if !isSHA256(dependency) {
			return errors.New("invalid dependency hash")
		}
		if _, exists := dependencies[dependency]; exists {
			return errors.New("duplicate dependency hash")
		}
		dependencies[dependency] = struct{}{}
	}
	if schemaVersion == DocumentBundleSchemaVersionV1 {
		if page.Blocks != nil {
			return errors.New("v1 page cannot contain blocks")
		}
		return nil
	}
	if len(page.Blocks) == 0 {
		return errors.New("v2 page requires semantic blocks")
	}
	blockIDs := make(map[string]struct{}, len(page.Blocks))
	for index := range page.Blocks {
		block := page.Blocks[index]
		if err := block.validate(); err != nil {
			return fmt.Errorf("block %d: %w", index, err)
		}
		if _, exists := blockIDs[block.ID]; exists {
			return fmt.Errorf("duplicate block id %q", block.ID)
		}
		blockIDs[block.ID] = struct{}{}
	}
	if bundleState == DocumentBundleStateFinal {
		fragmentBlockIDs, err := finalFragmentBlockIDs(page.Fragment)
		if err != nil {
			return err
		}
		if len(fragmentBlockIDs) != len(page.Blocks) {
			return fmt.Errorf("final v2 page block manifest has %d entries but fragment has %d block ids", len(page.Blocks), len(fragmentBlockIDs))
		}
		fragmentBlockIDCounts := make(map[string]int, len(fragmentBlockIDs))
		for _, id := range fragmentBlockIDs {
			fragmentBlockIDCounts[id]++
		}
		for id := range blockIDs {
			if fragmentBlockIDCounts[id] != 1 {
				return fmt.Errorf("block id %q must occur exactly once in final fragment", id)
			}
		}
	}
	return nil
}

func finalFragmentBlockIDs(fragment string) ([]string, error) {
	tokenizer := html.NewTokenizer(strings.NewReader(fragment))
	ids := []string{}
	for {
		switch tokenType := tokenizer.Next(); tokenType {
		case html.ErrorToken:
			if errors.Is(tokenizer.Err(), io.EOF) {
				return ids, nil
			}
			return nil, fmt.Errorf("tokenize final v2 page fragment: %w", tokenizer.Err())
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			seenBlockID := false
			for _, attribute := range token.Attr {
				if !strings.EqualFold(attribute.Key, "data-rin-block-id") {
					continue
				}
				if seenBlockID || !blockIDPattern.MatchString(attribute.Val) {
					return nil, errors.New("final v2 page contains malformed block id attribute")
				}
				seenBlockID = true
				ids = append(ids, attribute.Val)
			}
		}
	}
}

func SupportedDocumentBundleSchemaVersion(value string) bool {
	return value == DocumentBundleSchemaVersionV1 || value == DocumentBundleSchemaVersionV2
}

func (kind DocumentBlockKind) Valid() bool {
	switch kind {
	case DocumentBlockHeading, DocumentBlockParagraph, DocumentBlockListItem, DocumentBlockTheorem,
		DocumentBlockMath, DocumentBlockCode, DocumentBlockFigure, DocumentBlockTable, DocumentBlockQuote:
		return true
	default:
		return false
	}
}

func (block DocumentBlock) validate() error {
	if !blockIDPattern.MatchString(block.ID) {
		return fmt.Errorf("invalid block id %q", block.ID)
	}
	if !block.Kind.Valid() {
		return fmt.Errorf("invalid block kind %q", block.Kind)
	}
	if block.Text != NormalizeDocumentBlockText(block.Text) {
		return errors.New("block text is not canonical")
	}
	if block.TextHash != DocumentBlockTextHash(block.Text) {
		return errors.New("block text hash does not match canonical text")
	}
	for _, headingID := range block.HeadingPath {
		if !ValidTOCID(headingID) {
			return fmt.Errorf("invalid block heading path id %q", headingID)
		}
	}
	if block.SourceLocation != nil {
		if err := block.SourceLocation.validate(); err != nil {
			return err
		}
	}
	return nil
}

func NormalizeDocumentBlockText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func DocumentBlockTextHash(value string) string {
	digest := sha256.Sum256([]byte(NormalizeDocumentBlockText(value)))
	return hex.EncodeToString(digest[:])
}

func NewDocumentBlockID(seed string) (string, error) {
	seed = strings.TrimSpace(seed)
	if seed == "" {
		return "", errors.New("document block id seed is empty")
	}
	digest := sha256.Sum256([]byte("rin-document-block/v1\x00" + seed))
	return "rb_" + hex.EncodeToString(digest[:16]), nil
}

// ValidPageID preserves the stable IDs used by existing Rinspace books while keeping page
// identity safe as opaque metadata. Unlike DOM/work/asset identifiers, page IDs may contain
// Unicode letters, numbers, and combining marks; separators remain deliberately narrow.
func ValidPageID(value string) bool {
	return validUnicodeIdentifier(value, false)
}

// ValidTOCID accepts safe DOM anchor identifiers emitted by document engines. Unlike opaque
// page IDs, an existing source anchor may begin with a number and must remain unchanged so links
// in the canonical fragment continue to resolve.
func ValidTOCID(value string) bool {
	return validUnicodeIdentifier(value, true)
}

func validUnicodeIdentifier(value string, allowNumberStart bool) bool {
	if !utf8.ValidString(value) {
		return false
	}
	count := utf8.RuneCountInString(value)
	if count < 1 || count > 128 {
		return false
	}
	first := true
	for _, character := range value {
		if first {
			if !unicode.IsLetter(character) && (!allowNumberStart || !unicode.IsNumber(character)) {
				return false
			}
			first = false
			continue
		}
		if !unicode.IsLetter(character) && !unicode.IsNumber(character) && !unicode.IsMark(character) &&
			!strings.ContainsRune("._:-", character) {
			return false
		}
	}
	return true
}

func (unit WorkUnit) validate() error {
	if !workIDPattern.MatchString(unit.ID) {
		return fmt.Errorf("invalid opaque id %q", unit.ID)
	}
	if unit.SourceLocation != nil {
		if err := unit.SourceLocation.validate(); err != nil {
			return err
		}
	}
	switch unit.Kind {
	case WorkUnitMath:
		if unit.Display == nil || !isSHA256(unit.MacroContextHash) || strings.TrimSpace(unit.Source) == "" {
			return errors.New("math unit requires source, display, and macro context hash")
		}
		if unit.DiagramType != "" || unit.Layout != nil || unit.Language != "" || unit.Meta != "" {
			return errors.New("math unit contains fields from another work-unit kind")
		}
	case WorkUnitDiagram:
		if strings.TrimSpace(unit.DiagramType) == "" || strings.TrimSpace(unit.Source) == "" {
			return errors.New("diagram unit requires type and source")
		}
		if unit.Display != nil || unit.MacroContextHash != "" || unit.AccessibilityContext != nil || unit.Language != "" || unit.Meta != "" {
			return errors.New("diagram unit contains fields from another work-unit kind")
		}
		if unit.Layout != nil && unit.Layout.Alignment != "" && unit.Layout.Alignment != "center" && unit.Layout.Alignment != "flushleft" && unit.Layout.Alignment != "flushright" {
			return fmt.Errorf("unsupported diagram alignment %q", unit.Layout.Alignment)
		}
	case WorkUnitCode:
		if unit.Display != nil || unit.MacroContextHash != "" || unit.AccessibilityContext != nil || unit.DiagramType != "" || unit.Layout != nil || unit.Options != "" {
			return errors.New("code unit contains fields from another work-unit kind")
		}
	default:
		return fmt.Errorf("unsupported kind %q", unit.Kind)
	}
	return nil
}

func (unit WorkUnit) Validate() error { return unit.validate() }

func (location SourceLocation) validate() error {
	if err := validateCanonicalPath(location.Path); err != nil {
		return fmt.Errorf("source location path: %w", err)
	}
	if !location.Start.valid() || location.End != nil && !location.End.valid() {
		return errors.New("source location positions must be positive")
	}
	if location.End != nil && (location.End.Line < location.Start.Line || location.End.Line == location.Start.Line && location.End.Column < location.Start.Column) {
		return errors.New("source location end precedes start")
	}
	return nil
}

func (position SourcePosition) valid() bool { return position.Line > 0 && position.Column > 0 }

func (asset AssetReference) validate() error {
	if !identifierPattern.MatchString(asset.ID) || !isSHA256(asset.SHA256) || asset.Bytes < 0 || strings.TrimSpace(asset.MediaType) == "" {
		return errors.New("invalid asset identity or metadata")
	}
	switch asset.Kind {
	case "project-file":
		if asset.ArtifactKey != "" || validateCanonicalPath(asset.ProjectPath) != nil {
			return errors.New("project-file asset requires only a canonical projectPath")
		}
	case "generated":
		if asset.ProjectPath != "" || validateCanonicalPath(asset.ArtifactKey) != nil {
			return errors.New("generated asset requires only a canonical artifactKey")
		}
	default:
		return fmt.Errorf("unsupported asset kind %q", asset.Kind)
	}
	return nil
}

func (diagnostic Diagnostic) validate() error {
	if strings.TrimSpace(diagnostic.Code) == "" || strings.TrimSpace(diagnostic.Stage) == "" {
		return errors.New("diagnostic requires code and stage")
	}
	if diagnostic.Severity != "info" && diagnostic.Severity != "warning" && diagnostic.Severity != "error" {
		return fmt.Errorf("unsupported diagnostic severity %q", diagnostic.Severity)
	}
	if diagnostic.SourceLocation != nil {
		return diagnostic.SourceLocation.validate()
	}
	return nil
}

func (diagnostic Diagnostic) Validate() error { return diagnostic.validate() }

func (provenance Provenance) validate() error {
	if strings.TrimSpace(provenance.Adapter) == "" || strings.TrimSpace(provenance.AdapterVersion) == "" || strings.TrimSpace(provenance.EngineVersion) == "" {
		return errors.New("provenance requires adapter and engine versions")
	}
	if provenance.ProjectGraphSchemaVersion != ProjectGraphSchemaVersion {
		return fmt.Errorf("unsupported provenance project graph version %q", provenance.ProjectGraphSchemaVersion)
	}
	return nil
}

func validateCanonicalPath(value string) error {
	cleaned, ok := CleanProjectPath(value)
	if !ok || cleaned != value {
		return fmt.Errorf("path must be canonical: %q", value)
	}
	return nil
}
