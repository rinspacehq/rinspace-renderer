package markdownadapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
)

const (
	Engine                     = "rin-markdown"
	AdapterVersion             = "0.6.0"
	DefaultCodeTheme           = "github-light"
	compileDraftOperation      = "markdown.compile-draft"
	compileBookOperation       = "markdown.compile-book-draft"
	planBookCacheOperation     = "markdown.plan-book-cache"
	finalizeOperation          = "markdown.finalize"
	bookPageCacheSchemaVersion = "rin-markdown-book-page-cache/v2"
	bookPageCacheMediaType     = "application/vnd.rin.markdown-book-page-cache+json"
	defaultBookPageCacheBytes  = int64(16 << 20)
	defaultBookPageCacheTTL    = 30 * 24 * time.Hour
)

type Worker interface {
	Do(context.Context, string, any, any) error
}

type Adapter struct {
	Worker                 Worker
	MaxFileBytes           int64
	Cache                  orchestration.Cache
	Store                  orchestration.ArtifactStore
	MathFontVersion        string
	BookCacheCompatibility string
	MaxBookPageCacheBytes  int64
	BookPageCacheTTL       time.Duration
	Now                    func() time.Time
}

type workerBundleResponse struct {
	Publishable      bool                          `json:"publishable"`
	Bundle           contracts.DocumentBundle      `json:"bundle"`
	Artifacts        []contracts.ArtifactReference `json:"artifacts,omitempty"`
	SanitizerVersion string                        `json:"sanitizerVersion,omitempty"`
	PageCache        *bookPageCacheResult          `json:"pageCache,omitempty"`
}

type compilePayload struct {
	Source           string                     `json:"source"`
	SourcePath       string                     `json:"sourcePath"`
	ProjectHash      string                     `json:"projectHash"`
	Title            string                     `json:"title,omitempty"`
	Language         string                     `json:"language,omitempty"`
	DependencyHashes []string                   `json:"dependencyHashes"`
	MacroContextHash string                     `json:"macroContextHash"`
	Assets           []contracts.AssetReference `json:"assets"`
}

type compileBookPayload struct {
	ProjectHash      string                     `json:"projectHash"`
	Title            string                     `json:"title,omitempty"`
	Language         string                     `json:"language,omitempty"`
	MacroContextHash string                     `json:"macroContextHash"`
	Assets           []contracts.AssetReference `json:"assets"`
	Pages            []compileBookPagePayload   `json:"pages"`
}

type compileBookPagePayload struct {
	Source           string   `json:"source"`
	SourcePath       string   `json:"sourcePath"`
	PageID           string   `json:"pageId"`
	Title            string   `json:"title,omitempty"`
	DependencyHashes []string `json:"dependencyHashes"`
}

type finalizePayload struct {
	Bundle       contracts.DocumentBundle       `json:"bundle"`
	ResolvedWork resolvedWorkPayload            `json:"resolvedWork"`
	PageCache    map[string]bookPageCacheRecord `json:"pageCache,omitempty"`
}

type bookCachePlan struct {
	SchemaVersion      string              `json:"schemaVersion"`
	GlobalMetadataHash string              `json:"globalMetadataHash"`
	Pages              []bookCachePlanPage `json:"pages"`
}

type bookCachePlanPage struct {
	ID               string `json:"id"`
	SourcePath       string `json:"sourcePath"`
	PageSemanticHash string `json:"pageSemanticHash"`
}

type bookPageCacheRecord struct {
	SchemaVersion    string                 `json:"schemaVersion"`
	PageSemanticHash string                 `json:"pageSemanticHash"`
	PageHash         string                 `json:"pageHash"`
	Page             contracts.DocumentPage `json:"page"`
}

type bookPageCacheResult struct {
	SchemaVersion         string                `json:"schemaVersion"`
	GlobalMetadataHash    string                `json:"globalMetadataHash"`
	ProjectDependencyHash string                `json:"projectDependencyHash"`
	ReusedPageIDs         []string              `json:"reusedPageIds"`
	Records               []bookPageCacheRecord `json:"records"`
}

type Finalization struct {
	Bundle                contracts.DocumentBundle
	GlobalMetadataHash    string
	ProjectDependencyHash string
	ProjectResultKey      *orchestration.CacheKey
	ReusedPageIDs         []string
	CacheDecision         string
}

type resolvedWorkPayload struct {
	Units map[string]resolvedUnitPayload `json:"units"`
}

type resolvedUnitPayload struct {
	ID          string                       `json:"id"`
	Kind        contracts.WorkUnitKind       `json:"kind"`
	HTML        string                       `json:"html"`
	CSS         []string                     `json:"css"`
	Artifact    *contracts.ArtifactReference `json:"artifact,omitempty"`
	Diagnostics []contracts.Diagnostic       `json:"diagnostics"`
}

func (adapter Adapter) Capabilities() orchestration.DocumentCapabilities {
	return orchestration.DocumentCapabilities{
		Engine: Engine, ContentKinds: []contracts.ContentKind{contracts.ContentKindMarkdown},
		Features: []string{"gfm", "math", "directives", "footnotes", "shiki", "sanitized-html"},
	}
}

func (adapter Adapter) Analyze(ctx context.Context, snapshot orchestration.ProjectSnapshot, options orchestration.RenderOptions) (orchestration.Analysis, error) {
	if adapter.Worker == nil || adapter.MaxFileBytes <= 0 {
		return orchestration.Analysis{}, errors.New("Markdown adapter is not configured")
	}
	if err := snapshot.Graph.Validate(); err != nil {
		return orchestration.Analysis{}, fmt.Errorf("Markdown project graph: %w", err)
	}
	if snapshot.Graph.ContentKind != contracts.ContentKindMarkdown {
		return orchestration.Analysis{}, errors.New("Markdown adapter requires Markdown content")
	}
	mode := stringOption(snapshot.Graph.Options, "documentMode")
	if mode == "" {
		mode = "article"
	}
	if mode != "article" && mode != "book" {
		return orchestration.Analysis{}, fmt.Errorf("unsupported Markdown document mode %q", mode)
	}
	if mode == "article" && (len(snapshot.Graph.Entrypoints) != 1 || snapshot.Graph.Entrypoints[0].Role != contracts.EntrypointRoleDocument) {
		return orchestration.Analysis{}, errors.New("Markdown article adapter requires exactly one document entrypoint")
	}
	if mode == "book" && len(snapshot.Graph.Entrypoints) < 2 {
		return orchestration.Analysis{}, errors.New("Markdown Book adapter requires at least two page entrypoints")
	}
	entrypoints := make([]string, 0, len(snapshot.Graph.Entrypoints))
	for _, entrypoint := range snapshot.Graph.Entrypoints {
		if mode == "book" && entrypoint.Role != contracts.EntrypointRoleBookPage {
			return orchestration.Analysis{}, errors.New("Markdown Book entrypoints must use the book-page role")
		}
		if _, err := snapshot.ReadFileVerified(ctx, entrypoint.Path, adapter.MaxFileBytes); err != nil {
			return orchestration.Analysis{}, err
		}
		entrypoints = append(entrypoints, entrypoint.Path)
	}
	dependencies := make([]string, 0, len(snapshot.Graph.Files))
	for _, file := range snapshot.Graph.Files {
		dependencies = append(dependencies, file.SHA256)
	}
	return orchestration.Analysis{
		Entrypoints: entrypoints, DependencyHashes: dependencies,
		WorkloadFeatures: map[string]int64{"files": int64(len(snapshot.Graph.Files)), "pages": int64(len(entrypoints))},
		Diagnostics:      []contracts.Diagnostic{},
	}, nil
}

func (adapter Adapter) Compile(ctx context.Context, snapshot orchestration.ProjectSnapshot, analysis orchestration.Analysis) (contracts.DocumentBundle, error) {
	assets := make([]contracts.AssetReference, 0)
	assetNumber := 0
	for _, file := range snapshot.Graph.Files {
		if file.Role != contracts.ProjectFileRoleAsset {
			continue
		}
		assetNumber++
		assets = append(assets, contracts.AssetReference{
			ID: fmt.Sprintf("asset-%d", assetNumber), Kind: "project-file", SHA256: file.SHA256,
			Bytes: file.Bytes, MediaType: file.MediaType, ProjectPath: file.Path,
		})
	}
	macroHash := orchestration.ComputeMathMacroContextHash(nil)
	var response workerBundleResponse
	mode := stringOption(snapshot.Graph.Options, "documentMode")
	if mode == "" {
		mode = "article"
	}
	if mode != "article" && mode != "book" {
		return contracts.DocumentBundle{}, fmt.Errorf("unsupported Markdown document mode %q", mode)
	}
	operation := compileDraftOperation
	var payload any
	if mode == "book" {
		pages := make([]compileBookPagePayload, 0, len(analysis.Entrypoints))
		for _, sourcePath := range analysis.Entrypoints {
			source, err := snapshot.ReadFileVerified(ctx, sourcePath, adapter.MaxFileBytes)
			if err != nil {
				return contracts.DocumentBundle{}, err
			}
			pages = append(pages, compileBookPagePayload{
				Source: string(source), SourcePath: sourcePath, PageID: pageOption(snapshot.Graph.Options, "pageIds", sourcePath, stablePageID(sourcePath)),
				Title: pageOption(snapshot.Graph.Options, "pageTitles", sourcePath, ""), DependencyHashes: []string{},
			})
		}
		operation = compileBookOperation
		payload = compileBookPayload{ProjectHash: snapshot.Graph.ProjectHash, Title: stringOption(snapshot.Graph.Options, "title"),
			Language: stringOption(snapshot.Graph.Options, "language"), MacroContextHash: macroHash, Assets: assets, Pages: pages}
	} else {
		if len(analysis.Entrypoints) != 1 {
			return contracts.DocumentBundle{}, errors.New("Markdown analysis requires one article entrypoint")
		}
		sourcePath := analysis.Entrypoints[0]
		source, err := snapshot.ReadFileVerified(ctx, sourcePath, adapter.MaxFileBytes)
		if err != nil {
			return contracts.DocumentBundle{}, err
		}
		payload = compilePayload{Source: string(source), SourcePath: sourcePath, ProjectHash: snapshot.Graph.ProjectHash,
			Title: stringOption(snapshot.Graph.Options, "title"), Language: stringOption(snapshot.Graph.Options, "language"),
			DependencyHashes: append([]string(nil), analysis.DependencyHashes...), MacroContextHash: macroHash, Assets: assets}
	}
	if err := adapter.Worker.Do(ctx, operation, payload, &response); err != nil {
		return contracts.DocumentBundle{}, fmt.Errorf("compile Markdown draft: %w", err)
	}
	if response.Publishable || response.Bundle.State != contracts.DocumentBundleStateDraft {
		return contracts.DocumentBundle{}, errors.New("Markdown compiler returned a publishable or non-draft result")
	}
	if err := response.Bundle.Validate(); err != nil {
		return contracts.DocumentBundle{}, fmt.Errorf("validate Markdown draft: %w", err)
	}
	return response.Bundle, nil
}

func pageOption(options map[string]any, key string, path string, fallback string) string {
	values, ok := options[key].(map[string]any)
	if !ok {
		return fallback
	}
	value, _ := values[path].(string)
	if value = strings.TrimSpace(value); value == "" {
		return fallback
	}
	return value
}

func stablePageID(path string) string {
	digest := sha256.Sum256([]byte(path))
	return fmt.Sprintf("page-%x", digest[:12])
}

func (adapter Adapter) Finalize(ctx context.Context, draft contracts.DocumentBundle, resolved orchestration.ResolvedWork) (contracts.DocumentBundle, error) {
	final, err := adapter.FinalizeDetailed(ctx, draft, resolved)
	return final.Bundle, err
}

func (adapter Adapter) FinalizeDetailed(ctx context.Context, draft contracts.DocumentBundle, resolved orchestration.ResolvedWork) (Finalization, error) {
	return adapter.FinalizeDetailedWithBookReuse(ctx, draft, resolved, true)
}

// FinalizeDetailedWithBookReuse lets rollout/shadow callers force a clean full Book finalization
// while still writing verified page records for a later incremental equivalence run.
func (adapter Adapter) FinalizeDetailedWithBookReuse(ctx context.Context, draft contracts.DocumentBundle, resolved orchestration.ResolvedWork, allowBookReuse bool) (Finalization, error) {
	units := make(map[string]resolvedUnitPayload, len(resolved.Units))
	for id, unit := range resolved.Units {
		units[id] = resolvedUnitPayload{
			ID: unit.ID, Kind: unit.Kind, HTML: unit.HTML, CSS: append([]string(nil), unit.CSS...),
			Artifact: unit.Artifact, Diagnostics: append([]contracts.Diagnostic(nil), unit.Diagnostics...),
		}
	}
	resolvedPayload := resolvedWorkPayload{Units: units}
	cachedPages := map[string]bookPageCacheRecord{}
	var plan bookCachePlan
	keys := map[string]orchestration.CacheKey{}
	cacheEnabled := len(draft.Pages) > 1 && adapter.bookCacheConfigured()
	cacheReady := false
	cacheDecision := "not-book"
	if len(draft.Pages) > 1 {
		cacheDecision = "full-render-cache-unconfigured"
	}
	if cacheEnabled {
		cacheDecision = "full-render-plan-unavailable"
		if !allowBookReuse {
			cacheDecision = "full-render-reuse-disabled"
		}
		planErr := adapter.Worker.Do(ctx, planBookCacheOperation, finalizePayload{
			Bundle: draft, ResolvedWork: resolvedPayload,
		}, &plan)
		if planErr == nil && validateBookCachePlan(plan, draft) == nil {
			cacheReady = true
			if allowBookReuse {
				cacheDecision = "full-render-cache-miss"
			} else {
				cacheDecision = "full-render-reuse-disabled"
			}
			versions := adapter.bookCacheVersions(draft)
			now := adapter.now()
			for _, page := range plan.Pages {
				key, err := orchestration.BuildCacheKey(orchestration.CacheKeyInput{
					Stage:            orchestration.CacheStagePageFinalizer,
					NormalizedInputs: map[string]string{"page-semantic-hash": page.PageSemanticHash},
					ProjectContext:   map[string]string{"content-kind": "markdown"},
					Versions:         versions,
				})
				if err != nil {
					cacheReady = false
					cacheDecision = "full-render-key-unavailable"
					cachedPages = map[string]bookPageCacheRecord{}
					keys = map[string]orchestration.CacheKey{}
					break
				}
				keys[page.ID] = key
				if !allowBookReuse {
					continue
				}
				lookup := adapter.Cache.Lookup(ctx, key, adapter.bookCacheExpectation(), now)
				if !lookup.Hit {
					continue
				}
				var record bookPageCacheRecord
				if json.Unmarshal(lookup.Body, &record) != nil || !validBookPageCacheRecord(record, page) {
					continue
				}
				cachedPages[page.ID] = record
			}
		}
	}
	var response workerBundleResponse
	if err := adapter.Worker.Do(ctx, finalizeOperation, finalizePayload{
		Bundle: draft, ResolvedWork: resolvedPayload, PageCache: cachedPages,
	}, &response); err != nil {
		return Finalization{}, fmt.Errorf("finalize Markdown: %w", err)
	}
	if !response.Publishable || response.Bundle.State != contracts.DocumentBundleStateFinal {
		return Finalization{}, errors.New("Markdown finalizer returned a non-publishable result")
	}
	if err := response.Bundle.Validate(); err != nil {
		return Finalization{}, fmt.Errorf("validate final Markdown bundle: %w", err)
	}
	final := Finalization{Bundle: response.Bundle, CacheDecision: cacheDecision}
	if len(draft.Pages) > 1 {
		if response.PageCache == nil || response.PageCache.SchemaVersion != bookPageCacheSchemaVersion {
			return Finalization{}, errors.New("Markdown Book finalizer omitted page cache identities")
		}
		final.GlobalMetadataHash = response.PageCache.GlobalMetadataHash
		final.ProjectDependencyHash = response.PageCache.ProjectDependencyHash
		if !sha256Hex(final.GlobalMetadataHash) || !sha256Hex(final.ProjectDependencyHash) {
			return Finalization{}, errors.New("Markdown Book finalizer returned invalid dependency hashes")
		}
		if cacheReady {
			versions := adapter.bookCacheVersions(draft)
			versions["render-result-contract"] = contracts.RenderResultSchemaVersion
			key, err := orchestration.BuildCacheKey(orchestration.CacheKeyInput{
				Stage:            orchestration.CacheStageProjectResult,
				NormalizedInputs: map[string]string{"project-dependency-hash": final.ProjectDependencyHash},
				ProjectContext:   map[string]string{"content-kind": "markdown"},
				Versions:         versions,
			})
			if err == nil {
				final.ProjectResultKey = &key
			}
		}
		final.ReusedPageIDs = append([]string(nil), response.PageCache.ReusedPageIDs...)
		if len(final.ReusedPageIDs) > 0 {
			final.CacheDecision = "incremental-page-reuse"
		}
		if cacheReady {
			if response.PageCache.GlobalMetadataHash != plan.GlobalMetadataHash {
				return Finalization{}, errors.New("Markdown Book global metadata hash changed during finalization")
			}
			adapter.storeBookPageCache(ctx, plan, keys, *response.PageCache)
		}
	}
	return final, nil
}

func (adapter Adapter) bookCacheConfigured() bool {
	return adapter.Cache != nil && adapter.Store != nil && strings.TrimSpace(adapter.MathFontVersion) != "" &&
		strings.TrimSpace(adapter.BookCacheCompatibility) != ""
}

func (adapter Adapter) bookCacheVersions(draft contracts.DocumentBundle) map[string]string {
	return map[string]string{
		"finalizer":                draft.Provenance.EngineVersion,
		"plugin":                   draft.Provenance.AdapterVersion,
		"font":                     adapter.MathFontVersion,
		"theme":                    DefaultCodeTheme,
		"sanitizer":                "rin-markdown-final-sanitizer/v2",
		"document-bundle-contract": draft.SchemaVersion,
		"output-strategy":          "mathjax-chtml",
		"compatibility":            adapter.BookCacheCompatibility,
	}
}

func (adapter Adapter) bookCacheExpectation() orchestration.CacheExpectation {
	maximum := adapter.MaxBookPageCacheBytes
	if maximum <= 0 {
		maximum = defaultBookPageCacheBytes
	}
	return orchestration.CacheExpectation{SchemaVersion: bookPageCacheSchemaVersion, MediaType: bookPageCacheMediaType, MaxBytes: maximum}
}

func (adapter Adapter) now() time.Time {
	if adapter.Now != nil {
		return adapter.Now().UTC()
	}
	return time.Now().UTC()
}

func validateBookCachePlan(plan bookCachePlan, draft contracts.DocumentBundle) error {
	if plan.SchemaVersion != bookPageCacheSchemaVersion || !sha256Hex(plan.GlobalMetadataHash) || len(plan.Pages) != len(draft.Pages) {
		return errors.New("Markdown Book cache plan is invalid")
	}
	for index, page := range plan.Pages {
		if page.ID != draft.Pages[index].ID || page.SourcePath != draft.Pages[index].SourcePath || !sha256Hex(page.PageSemanticHash) {
			return errors.New("Markdown Book cache plan page identity is invalid")
		}
	}
	return nil
}

func sha256Hex(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validBookPageCacheRecord(record bookPageCacheRecord, planned bookCachePlanPage) bool {
	return record.SchemaVersion == bookPageCacheSchemaVersion && record.PageSemanticHash == planned.PageSemanticHash &&
		sha256Hex(record.PageHash) &&
		record.Page.ID == planned.ID && record.Page.SourcePath == planned.SourcePath && record.Page.FragmentFormat == contracts.FragmentFormatHTML
}

func (adapter Adapter) storeBookPageCache(ctx context.Context, plan bookCachePlan, keys map[string]orchestration.CacheKey, result bookPageCacheResult) {
	reused := map[string]struct{}{}
	for _, id := range result.ReusedPageIDs {
		reused[id] = struct{}{}
	}
	planByID := map[string]bookCachePlanPage{}
	for _, page := range plan.Pages {
		planByID[page.ID] = page
	}
	now := adapter.now()
	ttl := adapter.BookPageCacheTTL
	if ttl <= 0 {
		ttl = defaultBookPageCacheTTL
	}
	expectation := adapter.bookCacheExpectation()
	for _, record := range result.Records {
		planned, ok := planByID[record.Page.ID]
		if !ok || !validBookPageCacheRecord(record, planned) {
			continue
		}
		if _, ok := reused[record.Page.ID]; ok {
			continue
		}
		body, err := json.Marshal(record)
		if err != nil || int64(len(body)) > expectation.MaxBytes {
			continue
		}
		artifactID, err := orchestration.CacheArtifactID(orchestration.CacheStagePageFinalizer, body)
		if err != nil {
			continue
		}
		expires := now.Add(ttl).UTC().Truncate(time.Second)
		reference, err := adapter.Store.PutPrivate(ctx, orchestration.ArtifactWrite{
			ArtifactID: artifactID, Body: body, MediaType: bookPageCacheMediaType,
			SchemaVersion: bookPageCacheSchemaVersion, ExpiresAt: &expires,
		})
		if err != nil {
			continue
		}
		adapter.Cache.Store(ctx, orchestration.CacheRecord{
			Key: keys[record.Page.ID], Artifact: reference, SchemaVersion: bookPageCacheSchemaVersion,
			ArtifactSchemaVersion: bookPageCacheSchemaVersion, CreatedAt: now, LastHitAt: now, ExpiresAt: expires,
		}, expectation, now)
	}
}

func stringOption(options map[string]any, key string) string {
	value, _ := options[key].(string)
	return strings.TrimSpace(value)
}

var _ orchestration.DocumentAdapter = Adapter{}
