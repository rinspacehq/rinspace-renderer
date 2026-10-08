package renderapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/finaloutput"
)

type projectEngineAdapter interface {
	Engine() string
	Render(context.Context, projectEngineRequest) projectEngineOutcome
}

type projectEngineRequest struct {
	Project        projectRenderContext
	Fallback       bool
	PrimaryEngine  string
	FallbackEngine string
}

type projectEngineOutcome struct {
	Response     ProjectRenderResponse
	Status       int
	Diagnostics  []Diagnostic
	FailedEngine string
	Err          error
}

type projectEngineRegistry struct {
	mu       sync.RWMutex
	adapters map[string]projectEngineAdapter
}

func newProjectEngineRegistry() *projectEngineRegistry {
	return &projectEngineRegistry{adapters: map[string]projectEngineAdapter{}}
}

func (registry *projectEngineRegistry) register(adapter projectEngineAdapter) error {
	if adapter == nil {
		return errors.New("project engine adapter is nil")
	}
	engine := strings.TrimSpace(adapter.Engine())
	if engine == "" {
		return errors.New("project engine adapter has an empty engine")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.adapters[engine]; exists {
		return fmt.Errorf("project engine adapter %q is already registered", engine)
	}
	registry.adapters[engine] = adapter
	return nil
}

func (registry *projectEngineRegistry) lookup(engine string) (projectEngineAdapter, bool) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	adapter, ok := registry.adapters[strings.TrimSpace(engine)]
	return adapter, ok
}

func (registry *projectEngineRegistry) engines() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	engines := make([]string, 0, len(registry.adapters))
	for engine := range registry.adapters {
		engines = append(engines, engine)
	}
	sort.Strings(engines)
	return engines
}

type lateXMLProjectEngineAdapter struct{ server *Server }

func (adapter lateXMLProjectEngineAdapter) Engine() string { return "latexml" }

func (adapter lateXMLProjectEngineAdapter) Render(ctx context.Context, request projectEngineRequest) projectEngineOutcome {
	response, status, diagnostics, failedEngine, err := adapter.server.renderLateXMLProject(ctx, request.Project)
	return projectEngineOutcome{Response: response, Status: status, Diagnostics: diagnostics, FailedEngine: failedEngine, Err: err}
}

// autoProjectEngineAdapter resolves the wire-level "auto" engine. It is an
// explicit selection of latexml. The outer quality gate can recover reading
// using a validated author PDF, never a less capable HTML engine. The removed latexml ->
// legacy-rinjs fallback returned HTTP 200 pages that had quietly dropped
// formulas, references and document structure.
type autoProjectEngineAdapter struct {
	registry *projectEngineRegistry
}

func (adapter autoProjectEngineAdapter) Engine() string { return "auto" }

func (adapter autoProjectEngineAdapter) Render(ctx context.Context, request projectEngineRequest) projectEngineOutcome {
	primary, ok := adapter.registry.lookup("latexml")
	if !ok {
		return missingProjectEngineOutcome("latexml", request.Project.Diagnostics)
	}
	outcome := primary.Render(ctx, request)
	if outcome.Err == nil {
		outcome.Response.PrimaryEngine = "latexml"
	}
	return outcome
}

func (s *Server) registerProjectEngines() error {
	registry := newProjectEngineRegistry()
	if err := registry.register(lateXMLProjectEngineAdapter{server: s}); err != nil {
		return err
	}
	if err := registry.register(autoProjectEngineAdapter{registry: registry}); err != nil {
		return err
	}
	s.projectEngines = registry
	return nil
}

func (s *Server) renderProjectUsingEngine(ctx context.Context, engine string, project projectRenderContext) projectEngineOutcome {
	adapter, ok := s.projectEngines.lookup(engine)
	if !ok {
		return missingProjectEngineOutcome(engine, project.Diagnostics)
	}
	renderCtx := ctx
	cancel := func() {}
	// A valid exact-commit PDF gives auto a bounded reading recovery path.
	if engine == "auto" && availableAuthorPDF(project) != nil {
		renderCtx, cancel = context.WithTimeout(ctx, 90*time.Second)
	}
	defer cancel()
	outcome := adapter.Render(renderCtx, projectEngineRequest{Project: project})
	return s.validateAndRecoverProjectOutcome(ctx, engine, project, outcome)
}

func (s *Server) validateAndRecoverProjectOutcome(ctx context.Context, engine string, project projectRenderContext, outcome projectEngineOutcome) (result projectEngineOutcome) {
	defer func() {
		if engine == "auto" && result.Err != nil && result.Status >= 422 && ctx.Err() == nil &&
			(result.FailedEngine == "latexml" || result.FailedEngine == "rin-texsvg" || result.FailedEngine == "math") {
			if recovered, ok := s.authorPDFOutcome(project, result.Diagnostics); ok {
				result = recovered
			}
		}
	}()
	if outcome.Err != nil || outcome.Status < http.StatusOK || outcome.Status >= http.StatusMultipleChoices {
		return outcome
	}
	assetPaths := make([]string, 0, len(outcome.Response.AssetFiles))
	for _, asset := range outcome.Response.AssetFiles {
		assetPaths = append(assetPaths, asset.Path)
	}
	if err := finaloutput.Validate(finaloutput.Input{
		Adapter: outcome.Response.Engine, Fragment: outcome.Response.HTML,
		MaxFragmentBytes: finalOutputLimit(s.cfg), RequiredArtifacts: outcome.Response.GeneratedArtifacts,
		ClientOwnedAssets: assetPaths, LocalAssetBaseURL: localAssetBaseURL(s.cfg),
	}); err != nil {
		diagnostic := Diagnostic{
			Severity: "error", Code: finaloutput.Code(err), Message: err.Error(), Engine: outcome.Response.Engine,
		}
		outcome.Diagnostics = append(outcome.Diagnostics, diagnostic)
		outcome.Response.Diagnostics = append(outcome.Response.Diagnostics, diagnostic)
		outcome.Status = http.StatusBadGateway
		if diagnostic.Code == "final_output.render_error" {
			outcome.Status = http.StatusUnprocessableEntity
		}
		outcome.FailedEngine = outcome.Response.Engine
		outcome.Err = fmt.Errorf("final output validation: %w", err)
	} else {
		outcome.Response.GeneratedArtifacts = uniqueArtifactReferences(outcome.Response.GeneratedArtifacts)
	}
	return outcome
}

func localAssetBaseURL(config Config) string {
	if config.StorageProvider == "local" {
		return config.LocalPublicBaseURL
	}
	return ""
}

func finalOutputLimit(config Config) int64 {
	if config.FinalOutputMaxBytes > 0 {
		return config.FinalOutputMaxBytes
	}
	return finaloutput.DefaultMaxFragmentBytes
}

func missingProjectEngineOutcome(engine string, diagnostics []Diagnostic) projectEngineOutcome {
	err := fmt.Errorf("project engine adapter %q is unavailable", engine)
	diagnostics = append([]Diagnostic(nil), diagnostics...)
	diagnostics = append(diagnostics, Diagnostic{
		Severity: "error", Code: "renderer.engine.unavailable", Message: err.Error(), Engine: engine,
	})
	return projectEngineOutcome{Status: http.StatusServiceUnavailable, Diagnostics: diagnostics, FailedEngine: engine, Err: err}
}
