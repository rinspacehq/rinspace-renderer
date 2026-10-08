package renderapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

type fakeProjectEngineAdapter struct {
	engine string
	render func(context.Context, projectEngineRequest) projectEngineOutcome
}

func TestProjectEngineRegistryPreservesDirectLateXMLResponses(t *testing.T) {
	source := `\documentclass{article}
\begin{document}
Rin Renderer parity.
\end{document}`
	files := []projectcore.File{{Path: "main.tex", Kind: "tex", Body: source, Bytes: int64(len(source))}}
	manifest := projectcore.BuildManifest(files, projectcore.BuildOptions{Title: "Parity", ExplicitMainFile: "main.tex"})
	project := projectRenderContext{
		RequestID: "parity-request", Title: "Parity", MainFile: manifest.MainFile, ProjectStatus: "published-preview",
		MathPolicy: "server", Renderer: "rinjs", Files: files, Manifest: manifest, Diagnostics: []Diagnostic{},
	}
	cfg := testConfig()
	cfg.LateXMLBin = fakeRenderAPILateXMLC(t)
	server := &Server{cfg: cfg}
	if err := server.registerProjectEngines(); err != nil {
		t.Fatal(err)
	}

	directLateXML, directStatus, directDiagnostics, directFailedEngine, directErr := server.renderLateXMLProject(context.Background(), project)
	registryLateXML := server.renderProjectUsingEngine(context.Background(), "latexml", project)
	if directStatus != registryLateXML.Status || directFailedEngine != registryLateXML.FailedEngine || !errors.Is(registryLateXML.Err, directErr) {
		t.Fatalf("latexml status/error parity changed: direct=(%d,%q,%v) registry=(%d,%q,%v)", directStatus, directFailedEngine, directErr, registryLateXML.Status, registryLateXML.FailedEngine, registryLateXML.Err)
	}
	if !reflect.DeepEqual(directDiagnostics, registryLateXML.Diagnostics) {
		t.Fatalf("latexml diagnostics parity changed:\ndirect=%#v\nregistry=%#v", directDiagnostics, registryLateXML.Diagnostics)
	}
	assertProjectResponseJSONEqual(t, directLateXML, registryLateXML.Response)
}

func assertProjectResponseJSONEqual(t *testing.T, left ProjectRenderResponse, right ProjectRenderResponse) {
	t.Helper()
	leftJSON, err := json.Marshal(left)
	if err != nil {
		t.Fatal(err)
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		t.Fatal(err)
	}
	if string(leftJSON) != string(rightJSON) {
		t.Fatalf("project response bytes changed:\ndirect=%s\nregistry=%s", leftJSON, rightJSON)
	}
}

func (adapter fakeProjectEngineAdapter) Engine() string { return adapter.engine }

func (adapter fakeProjectEngineAdapter) Render(ctx context.Context, request projectEngineRequest) projectEngineOutcome {
	return adapter.render(ctx, request)
}

func TestProjectEngineRegistryIsDeterministicAndRejectsDuplicates(t *testing.T) {
	registry := newProjectEngineRegistry()
	noop := func(context.Context, projectEngineRequest) projectEngineOutcome { return projectEngineOutcome{} }
	if err := registry.register(fakeProjectEngineAdapter{engine: "latexml", render: noop}); err != nil {
		t.Fatal(err)
	}
	if err := registry.register(fakeProjectEngineAdapter{engine: "auto", render: noop}); err != nil {
		t.Fatal(err)
	}
	if got, want := registry.engines(), []string{"auto", "latexml"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("engines=%v want=%v", got, want)
	}
	if err := registry.register(fakeProjectEngineAdapter{engine: "latexml", render: noop}); err == nil {
		t.Fatal("expected duplicate engine registration to fail")
	}
}

func TestProjectEngineSuccessRequiresSharedFinalOutputValidation(t *testing.T) {
	server := &Server{cfg: testConfig(), projectEngines: newProjectEngineRegistry()}
	if err := server.projectEngines.register(fakeProjectEngineAdapter{engine: "unsafe", render: func(context.Context, projectEngineRequest) projectEngineOutcome {
		return projectEngineOutcome{
			Status:   http.StatusOK,
			Response: ProjectRenderResponse{Engine: "unsafe", HTML: `<p>before</p><script>alert(1)</script>`},
		}
	}}); err != nil {
		t.Fatal(err)
	}
	outcome := server.renderProjectUsingEngine(t.Context(), "unsafe", projectRenderContext{})
	if outcome.Err == nil || outcome.Status != http.StatusBadGateway || outcome.FailedEngine != "unsafe" {
		t.Fatalf("unsafe adapter output was reported successful: %#v", outcome)
	}
	if len(outcome.Diagnostics) != 1 || outcome.Diagnostics[0].Code != "final_output.executable_element" {
		t.Fatalf("missing structured final-output diagnostic: %#v", outcome.Diagnostics)
	}
}

func TestIncompleteEngineOutputIsNeverSuccessful(t *testing.T) {
	server := &Server{cfg: testConfig(), projectEngines: newProjectEngineRegistry()}
	if err := server.projectEngines.register(fakeProjectEngineAdapter{engine: "latexml", render: func(context.Context, projectEngineRequest) projectEngineOutcome {
		return projectEngineOutcome{
			Status:   http.StatusOK,
			Response: ProjectRenderResponse{Engine: "latexml", HTML: `<p>before</p><span class="ltx_ERROR undefined">\Needspace</span>`},
		}
	}}); err != nil {
		t.Fatal(err)
	}
	outcome := server.renderProjectUsingEngine(t.Context(), "latexml", projectRenderContext{})
	if outcome.Err == nil || outcome.Status != http.StatusUnprocessableEntity || outcome.FailedEngine != "latexml" {
		t.Fatalf("incomplete output was reported successful: %#v", outcome)
	}
	if len(outcome.Diagnostics) != 1 || outcome.Diagnostics[0].Code != "final_output.render_error" {
		t.Fatalf("missing rendering-error diagnostic: %#v", outcome.Diagnostics)
	}
}

func TestAutoProjectEngineAliasesLateXMLAndNeverFallsBack(t *testing.T) {
	registry := newProjectEngineRegistry()
	calls := []string{}
	primaryErr := errors.New("latexml unavailable")
	primary := fakeProjectEngineAdapter{engine: "latexml", render: func(_ context.Context, request projectEngineRequest) projectEngineOutcome {
		calls = append(calls, "latexml")
		return projectEngineOutcome{Status: http.StatusServiceUnavailable, FailedEngine: "latexml", Err: primaryErr, Diagnostics: []Diagnostic{{Code: "latexml.unavailable", Engine: "latexml"}}}
	}}
	fallback := fakeProjectEngineAdapter{engine: "decoy-engine", render: func(_ context.Context, request projectEngineRequest) projectEngineOutcome {
		calls = append(calls, "decoy-engine")
		return projectEngineOutcome{Status: http.StatusOK, Response: ProjectRenderResponse{Engine: "decoy-engine"}}
	}}
	if err := registry.register(primary); err != nil {
		t.Fatal(err)
	}
	if err := registry.register(fallback); err != nil {
		t.Fatal(err)
	}
	auto := autoProjectEngineAdapter{registry: registry}
	if err := registry.register(auto); err != nil {
		t.Fatal(err)
	}
	outcome := auto.Render(context.Background(), projectEngineRequest{Project: projectRenderContext{Diagnostics: []Diagnostic{}}})
	if !errors.Is(outcome.Err, primaryErr) || outcome.Status != http.StatusServiceUnavailable || outcome.FailedEngine != "latexml" {
		t.Fatalf("auto did not propagate the latexml failure: %#v", outcome)
	}
	if !reflect.DeepEqual(calls, []string{"latexml"}) {
		t.Fatalf("auto must not fall back to another engine, calls=%v", calls)
	}
	if outcome.Response.Engine == "decoy-engine" {
		t.Fatalf("auto delegated to another engine: %#v", outcome.Response)
	}
}
