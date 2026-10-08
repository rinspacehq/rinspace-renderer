package renderapi

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProjectOrchestratorMatchesProtocolAdapterResponse(t *testing.T) {
	source := []byte(`\documentclass{article}
\begin{document}
Orchestration parity.
\end{document}`)
	cfg := testConfig()
	cfg.LateXMLBin = fakeRenderAPILateXMLC(t)
	cfg.DefaultDocumentEngine = "latexml"
	handler := NewServer(cfg)
	server := handler.(*Server)

	httpResponse := performProjectRenderRequest(t, handler, "main.tex", source, map[string]string{
		"engine": "latexml", "title": "Parity", "status": "published-preview", "mathPolicy": "server",
	})
	var identity struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(httpResponse.Body.Bytes(), &identity); err != nil {
		t.Fatal(err)
	}
	outcome, preparationErr := server.orchestrateProject(context.Background(), projectRenderInput{
		RequestID: identity.RequestID, ArchiveName: "main.tex", Archive: source, Engine: "latexml",
		Title: "Parity", ProjectStatus: "published-preview", MathPolicy: "server",
	})
	if preparationErr != nil || outcome.Err != nil {
		t.Fatalf("direct orchestration failed: preparation=%v outcome=%v", preparationErr, outcome.Err)
	}
	directJSON, err := json.Marshal(outcome.Response)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(httpResponse.Body.Bytes()), directJSON) {
		t.Fatalf("protocol adapter changed orchestration response:\nhttp=%s\ndirect=%s", httpResponse.Body.Bytes(), directJSON)
	}
}

func TestProjectOrchestratorRejectsUnsafeArchiveWithoutHTTP(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("../main.tex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(`\documentclass{article}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(testConfig()).(*Server)
	_, preparationErr := server.orchestrateProject(context.Background(), projectRenderInput{
		RequestID: "unsafe", ArchiveName: "unsafe.zip", Archive: archive.Bytes(), Engine: "latexml",
	})
	if preparationErr == nil || preparationErr.Status != 400 || !strings.Contains(preparationErr.Cause.Error(), "unsafe archive path") {
		t.Fatalf("unexpected preparation error: %#v", preparationErr)
	}
}

func TestProjectOrchestratorRejectsMissingNestedDependency(t *testing.T) {
	server := NewServer(testConfig()).(*Server)
	_, preparationErr := server.orchestrateProject(context.Background(), projectRenderInput{
		RequestID: "missing-include", ArchiveName: "main.tex",
		Archive: []byte("\\documentclass{article}\n\\begin{document}\n\\input{sections/intro}\n\\end{document}\n"),
		Engine:  "latexml",
	})
	if preparationErr == nil || preparationErr.Status != http.StatusBadRequest ||
		!strings.Contains(preparationErr.Cause.Error(), "project.include.missing") {
		t.Fatalf("missing project dependency was accepted: %#v", preparationErr)
	}
}

func TestProjectOrchestratorRendersNestedDependencyFromArchive(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for name, content := range map[string]string{
		"main.tex":           "\\documentclass{article}\n\\begin{document}\n\\input{sections/intro}\n\\end{document}\n",
		"sections/intro.tex": "NESTED-DEPENDENCY-SENTINEL\n",
	} {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig()
	cfg.LateXMLBin = fakeRenderAPILateXMLC(t)
	server := NewServer(cfg).(*Server)
	outcome, preparationErr := server.orchestrateProject(context.Background(), projectRenderInput{
		RequestID: "nested-include", ArchiveName: "project.zip", Archive: archive.Bytes(),
		ExplicitMainFile: "main.tex", Engine: "latexml",
	})
	if preparationErr != nil || outcome.Err != nil || !strings.Contains(outcome.Response.ResolvedSource, "NESTED-DEPENDENCY-SENTINEL") {
		t.Fatalf("nested dependency was not rendered: preparation=%#v outcome=%#v", preparationErr, outcome)
	}
}

func TestProjectHTTPHandlerRemainsAProtocolAdapter(t *testing.T) {
	_, sourceFile, _, _ := runtime.Caller(0)
	body, err := os.ReadFile(filepath.Join(filepath.Dir(sourceFile), "server.go"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(body)
	start := strings.Index(source, "func (s *Server) handleProjectRender")
	end := strings.Index(source[start:], "\nfunc (s *Server) handleDiagramRender")
	if start < 0 || end < 0 {
		t.Fatal("could not locate project HTTP handler")
	}
	handler := source[start : start+end]
	for _, forbidden := range []string{"projectcore.", "renderLateXMLProject", "legacyRinJSProjectResponse", "switch engine", "markdown"} {
		if strings.Contains(handler, forbidden) {
			t.Fatalf("project HTTP handler contains orchestration branch %q", forbidden)
		}
	}
	for _, required := range []string{"requireServiceToken", "ParseMultipartForm", "orchestrateProject", "writeJSON"} {
		if !strings.Contains(handler, required) {
			t.Fatalf("project HTTP handler is missing protocol responsibility %q", required)
		}
	}
}

func performProjectRenderRequest(t *testing.T, handler http.Handler, filename string, source []byte, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("source", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(source); err != nil {
		t.Fatal(err)
	}
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/render/projects", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-Rin-Renderer-Token", "test-token")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("project request status=%d body=%s", response.Code, response.Body.String())
	}
	return response
}
