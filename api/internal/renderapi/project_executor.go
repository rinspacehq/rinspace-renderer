package renderapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/finaloutput"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
	"github.com/rinspacehq/rinspace-renderer/api/internal/orchestration"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectidentity"
	"github.com/rinspacehq/rinspace-renderer/api/internal/typstadapter"
)

type ProjectExecutionRequest struct {
	ContentKind              string
	JobID                    string
	RequestID                string
	ArchiveName              string
	Archive                  []byte
	Engine                   string
	Title                    string
	ExplicitMainFile         string
	MetadataMainFile         string
	ActiveFile               string
	ProjectStatus            string
	MathPolicy               string
	Renderer                 string
	DocumentMode             string
	DisableMarkdownBookReuse bool
	MarkdownBookPages        []projectidentity.MarkdownBookPage
	ProjectID                string
	SourceCommit             string
	ControlProjectHash       string
}

type ProjectExecutionResult struct {
	Response          ProjectRenderResponse
	Canonical         *contracts.RenderResult
	LocalAssetBaseURL string
	Status            int
	Message           string
	Diagnostics       []Diagnostic
	Err               error
}

type ProjectExecutor struct {
	server   *Server
	markdown *markdownProjectExecutor
	typst    *typstProjectExecutor
}

func NewProjectExecutor(config Config) *ProjectExecutor {
	server := newServer(config, nil, nil, operational.Default())
	return &ProjectExecutor{server: server, typst: newTypstProjectExecutor(config, server)}
}

func NewProjectExecutorWithMarkdown(config Config, cache orchestration.Cache, store orchestration.ArtifactStore) (*ProjectExecutor, error) {
	server := newServer(config, nil, nil, operational.Default())
	markdown, err := newMarkdownProjectExecutor(config, server, cache, store)
	if err != nil {
		_ = server.Close()
		return nil, err
	}
	return &ProjectExecutor{server: server, markdown: markdown, typst: newTypstProjectExecutor(config, server)}, nil
}

func (executor *ProjectExecutor) Execute(ctx context.Context, request ProjectExecutionRequest) ProjectExecutionResult {
	request.JobID = firstNonEmpty(request.JobID, request.RequestID)
	request.RequestID = firstNonEmpty(request.RequestID, request.JobID)
	if request.ContentKind == "markdown" {
		if executor.markdown == nil {
			return markdownExecutionFailure("markdown.executor.unavailable", "Markdown executor is unavailable", errors.New("not configured"))
		}
		return executor.markdown.Execute(ctx, request)
	}
	if request.ContentKind == string(contracts.ContentKindTypst) {
		if executor.typst == nil {
			return typstExecutionFailure("typst.executor.unavailable", "Typst executor is unavailable", errors.New("not configured"))
		}
		return executor.typst.Execute(ctx, request)
	}
	outcome, preparationErr := executor.server.orchestrateProject(ctx, projectRenderInput{
		RequestID: request.RequestID, ArchiveName: request.ArchiveName, Archive: request.Archive,
		Engine: request.Engine, Title: request.Title, ExplicitMainFile: request.ExplicitMainFile,
		MetadataMainFile: request.MetadataMainFile, ActiveFile: request.ActiveFile,
		ProjectStatus: request.ProjectStatus, MathPolicy: request.MathPolicy, Renderer: request.Renderer,
		ProjectID: request.ProjectID, SourceCommit: request.SourceCommit, ProjectHash: request.ControlProjectHash,
	})
	if preparationErr != nil {
		return ProjectExecutionResult{Status: preparationErr.Status, Message: preparationErr.Message, Err: preparationErr.Cause}
	}
	outcome.Response.DocumentMode = request.DocumentMode
	return ProjectExecutionResult{Response: outcome.Response, LocalAssetBaseURL: localAssetBaseURL(executor.server.cfg), Status: outcome.Status, Diagnostics: outcome.Diagnostics, Err: outcome.Err}
}

func (executor *ProjectExecutor) Close() error {
	if executor == nil {
		return nil
	}
	var markdownErr error
	if executor.markdown != nil {
		markdownErr = executor.markdown.Close()
	}
	serverErr := executor.server.Close()
	if markdownErr != nil {
		return markdownErr
	}
	return serverErr
}

func CanonicalResult(jobID string, projectHash string, contentKind string, response ProjectRenderResponse, localAssetOrigin ...string) (contracts.RenderResult, error) {
	kind := contracts.ContentKind(contentKind)
	if !kind.Valid() || strings.TrimSpace(jobID) == "" || strings.TrimSpace(response.RequestID) == "" {
		return contracts.RenderResult{}, errors.New("canonical project result identity is invalid")
	}
	if response.Versions.FinalOutput != "" && response.Versions.FinalOutput != finaloutput.ContractVersion {
		return contracts.RenderResult{}, fmt.Errorf("unsupported final output contract %q", response.Versions.FinalOutput)
	}
	diagnostics := canonicalDiagnostics(response.Diagnostics)
	assetPaths := make([]string, 0, len(response.AssetFiles))
	for _, asset := range response.AssetFiles {
		assetPaths = append(assetPaths, asset.Path)
	}
	localAssetBaseURL := ""
	if len(localAssetOrigin) > 0 {
		localAssetBaseURL = localAssetOrigin[0]
	}
	if err := finaloutput.Validate(finaloutput.Input{
		Adapter: response.Engine, Fragment: response.HTML, RequiredArtifacts: response.GeneratedArtifacts,
		ClientOwnedAssets: assetPaths, LocalAssetBaseURL: localAssetBaseURL,
	}); err != nil {
		return contracts.RenderResult{}, fmt.Errorf("canonical final output: %w", err)
	}
	pages, err := canonicalProjectPages(response)
	if err != nil {
		return contracts.RenderResult{}, fmt.Errorf("canonical reader pages: %w", err)
	}
	for _, page := range pages {
		if err := finaloutput.ValidateRenderingErrors(page.Fragment); err != nil {
			return contracts.RenderResult{}, fmt.Errorf("canonical reader page %q: %w", page.ID, err)
		}
	}
	bundleSchemaVersion := contracts.DocumentBundleSchemaVersionV1
	if response.Engine == "latexml" {
		bundleSchemaVersion = contracts.DocumentBundleSchemaVersionV2
	}
	bundle := contracts.DocumentBundle{
		SchemaVersion: bundleSchemaVersion, ProjectHash: projectHash,
		State: contracts.DocumentBundleStateFinal, ContentKind: kind, DocumentEngine: response.Engine,
		Title:     response.Title,
		Pages:     pages,
		WorkUnits: []contracts.WorkUnit{}, Assets: canonicalProjectAssets(response.AssetFiles),
		Diagnostics: diagnostics,
		Provenance: contracts.Provenance{
			Adapter: response.Engine, AdapterVersion: response.Versions.RinRenderer,
			EngineVersion: canonicalEngineVersion(response), ProjectGraphSchemaVersion: contracts.ProjectGraphSchemaVersion,
		},
		KnowledgeIndex: response.KnowledgeIndex,
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		return contracts.RenderResult{}, err
	}
	digest := sha256.Sum256(encoded)
	bundle.BundleHash = hex.EncodeToString(digest[:])
	versions := canonicalVersions(response.Versions)
	versions["finalOutput"] = finaloutput.ContractVersion
	result := contracts.RenderResult{
		SchemaVersion: contracts.RenderResultSchemaVersion, JobID: jobID, RequestID: response.RequestID,
		ProjectHash: projectHash, ResultHash: bundle.BundleHash, ContentKind: kind, Engine: response.Engine,
		Inline: &bundle, Assets: uniqueArtifactReferences(response.GeneratedArtifacts), Diagnostics: diagnostics,
		Versions: versions, Cache: contracts.RenderCacheSummary{ReusedStages: []string{}},
	}
	if err := result.Validate(); err != nil {
		return contracts.RenderResult{}, err
	}
	return result, nil
}

// TypstHTMLResult turns one verified compiler output into a v1 semantic reader
// bundle. It has no TeX aux files and claims no v2 annotation blocks.
func TypstHTMLResult(jobID, requestID, projectHash, entrypoint, title, mode, rendererVersion, compilerVersion string, nativeHTML []byte, projectFiles map[string][]byte) (ProjectExecutionResult, error) {
	if rendererVersion == "" || compilerVersion == "" || entrypoint == "" || requestID == "" {
		return ProjectExecutionResult{}, errors.New("Typst render provenance is incomplete")
	}
	adapted, err := typstadapter.AdaptHTML(nativeHTML, mode, projectFiles)
	if err != nil {
		return ProjectExecutionResult{}, err
	}
	graphFiles := make([]contracts.ProjectGraphFile, 0, len(projectFiles))
	result := typstAdaptedResult(typstAdaptedInput{
		JobID: jobID, RequestID: requestID, ProjectHash: projectHash,
		Entrypoint: entrypoint, Title: title, Mode: mode,
		Profile: typstadapter.Profile{
			CompilerVersion: compilerVersion, AdapterVersion: typstadapter.AdapterVersion,
			FinalOutputVersion: finaloutput.ContractVersion, MathPresentation: typstadapter.MathPresentationNative,
		},
		RendererVersion: rendererVersion, CompilerVersion: compilerVersion,
		Adapted: adapted, ProjectFiles: graphFiles,
	})
	if result.Err != nil {
		return ProjectExecutionResult{}, result.Err
	}
	return result, nil
}

type canonicalReaderPayload struct {
	TOC []struct {
		ID    string `json:"id"`
		Text  string `json:"text"`
		Level int    `json:"level"`
	} `json:"toc"`
	Pages []struct {
		ID    string `json:"id"`
		Text  string `json:"text"`
		Level int    `json:"level"`
		HTML  string `json:"html"`
	} `json:"pages"`
}

func canonicalProjectPages(response ProjectRenderResponse) ([]contracts.DocumentPage, error) {
	fallback := []contracts.DocumentPage{{
		ID: "page-main", SourcePath: response.MainFile, Title: response.Title,
		Fragment: response.HTML, FragmentFormat: contracts.FragmentFormatHTML,
		TOC: []contracts.TOCEntry{}, DependencyHashes: []string{},
	}}
	if response.DocumentMode != "book" {
		return addCanonicalPageSemanticBlocks(response.Engine, fallback)
	}
	encoded, err := json.Marshal(response.Reader)
	if err != nil {
		return nil, err
	}
	var reader canonicalReaderPayload
	if err := json.Unmarshal(encoded, &reader); err != nil {
		return nil, err
	}
	if len(reader.Pages) == 0 {
		return nil, errors.New("book reader payload has no pages")
	}
	toc := make([]contracts.TOCEntry, 0, len(reader.TOC))
	for _, entry := range reader.TOC {
		toc = append(toc, contracts.TOCEntry{ID: entry.ID, Depth: entry.Level, Text: entry.Text})
	}
	pages := make([]contracts.DocumentPage, 0, len(reader.Pages))
	pageIDs := make(map[string]struct{}, len(reader.Pages))
	for index, page := range reader.Pages {
		pageID := canonicalReaderPageID(page.ID, index)
		if _, duplicate := pageIDs[pageID]; duplicate {
			pageID = hashedReaderPageID(page.ID, index)
		}
		pageIDs[pageID] = struct{}{}
		pages = append(pages, contracts.DocumentPage{
			ID: pageID, SourcePath: response.MainFile, Title: page.Text,
			Fragment: page.HTML, FragmentFormat: contracts.FragmentFormatHTML,
			TOC: append([]contracts.TOCEntry(nil), toc...), DependencyHashes: []string{},
		})
	}
	return addCanonicalPageSemanticBlocks(response.Engine, pages)
}

func addCanonicalPageSemanticBlocks(engine string, pages []contracts.DocumentPage) ([]contracts.DocumentPage, error) {
	if engine != "latexml" {
		return pages, nil
	}
	for index := range pages {
		fragment, blocks, err := addLateXMLSemanticBlocks(pages[index].Fragment, pages[index].SourcePath)
		if err != nil {
			return nil, fmt.Errorf("LaTeXML page %q semantic blocks: %w", pages[index].ID, err)
		}
		pages[index].Fragment = fragment
		pages[index].Blocks = blocks
	}
	return pages, nil
}

func canonicalReaderPageID(value string, index int) string {
	value = strings.TrimSpace(value)
	if contracts.ValidPageID(value) {
		return value
	}
	if candidate := "page-" + value; contracts.ValidPageID(candidate) {
		return candidate
	}
	return hashedReaderPageID(value, index)
}

func hashedReaderPageID(value string, index int) string {
	digest := sha256.Sum256([]byte(strconv.Itoa(index) + "\x00" + value))
	return "page-" + hex.EncodeToString(digest[:16])
}

func canonicalProjectAssets(files []AssetFile) []contracts.AssetReference {
	assets := make([]contracts.AssetReference, 0, len(files))
	for index, file := range files {
		body := []byte(file.Body)
		if file.Encoding == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(file.Body)
			if err != nil {
				continue
			}
			body = decoded
		}
		digest := sha256.Sum256(body)
		mediaType := firstNonEmpty(file.MIME, "application/octet-stream")
		assets = append(assets, contracts.AssetReference{
			ID: "asset-" + strconv.Itoa(index+1), Kind: "project-file", SHA256: hex.EncodeToString(digest[:]),
			Bytes: int64(len(body)), MediaType: mediaType, ProjectPath: file.Path,
		})
	}
	return assets
}

func canonicalDiagnostics(items []Diagnostic) []contracts.Diagnostic {
	result := make([]contracts.Diagnostic, 0, len(items))
	for _, item := range items {
		severity := item.Severity
		if severity != "info" && severity != "warning" && severity != "error" {
			severity = "info"
		}
		result = append(result, contracts.Diagnostic{Code: firstNonEmpty(item.Code, "renderer.message"), Severity: severity, Message: item.Message, Stage: firstNonEmpty(item.Engine, "document")})
	}
	return result
}

func canonicalEngineVersion(response ProjectRenderResponse) string {
	switch response.Engine {
	case "latexml":
		return firstNonEmpty(response.Versions.LateXML, response.Versions.RinRenderer)
	case "typst":
		return firstNonEmpty(response.Versions.Typst, response.Versions.RinRenderer)
	default:
		return response.Versions.RinRenderer
	}
}

func canonicalVersions(versions Versions) map[string]string {
	encoded, _ := json.Marshal(versions)
	var values map[string]string
	_ = json.Unmarshal(encoded, &values)
	result := map[string]string{}
	for key, value := range values {
		if strings.TrimSpace(value) != "" {
			result[key] = value
		}
	}
	return result
}
