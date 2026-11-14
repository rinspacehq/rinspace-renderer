package renderapi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectidentity"
	"github.com/rinspacehq/rinspace-renderer/api/internal/typstadapter"
)

func TestFragmentsHTTPSubmitsTrustedPrivateProjectToQueue(t *testing.T) {
	cfg := typstTestConfig(t, "unused fake compiler")
	cfg.TypstSandboxBin = "/reviewed/sandbox"
	cfg.TypstSandboxHash = strings.Repeat("a", 64)
	forwarded := 0
	var archives [][]byte
	jobs := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded++
		if r.URL.Path != "/api/render/jobs" || r.Header.Get("Idempotency-Key") != "same-fragment-operation" || r.Header.Get("X-Rin-Renderer-Owner-Scope") != "synthetic-learning-session" {
			t.Fatal("queue identity or idempotency changed")
		}
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			t.Fatal(err)
		}
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("contentKind") != "typst" || r.FormValue("documentMode") != "article" || r.FormValue("renderProfileId") != TypstFragmentsProfileID(cfg) || r.FormValue("projectId") != "" {
			t.Fatal("fragment was converted into a public publication")
		}
		options, err := typstadapter.ParseFragmentOptions(r.FormValue("options"))
		if err != nil || len(options.BlockIDs) != 2 {
			t.Fatalf("missing fragment identity: %+v %v", options, err)
		}
		file, _, err := r.FormFile("source")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		archive, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		archives = append(archives, archive)
		reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{}
		for _, entry := range reader.File {
			source, err := entry.Open()
			if err != nil {
				t.Fatal(err)
			}
			files[entry.Name], err = io.ReadAll(source)
			source.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := typstadapter.ValidateFragmentProject(files, options.BlockIDs); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job":{"id":"synthetic-job"}}`))
	})
	server := NewServerWithJobs(cfg, jobs)
	defer server.(io.Closer).Close()
	input := map[string]any{"schema": "typst-fragments/v1", "renderProfileId": TypstFragmentsProfileID(cfg), "blocks": []typstadapter.FragmentBlock{{ID: "first", Source: "See @eq-later."}, {ID: "second", Source: "$x=1$ <eq-later>"}}}
	raw, _ := json.Marshal(input)
	for i := 0; i < 2; i++ {
		request := httptest.NewRequest(http.MethodPost, "/api/render/fragments", bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Rin-Renderer-Token", cfg.ServiceToken)
		request.Header.Set("Idempotency-Key", "same-fragment-operation")
		request.Header.Set("X-Rin-Renderer-Owner-Scope", "synthetic-learning-session")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 202 {
			t.Fatalf("fragment submission: %d %s", response.Code, response.Body.String())
		}
		project, err := projectidentity.Import("fragments.zip", archives[i], contracts.ContentKindTypst, "main.typ", projectLimitsFromConfig(cfg))
		if err != nil {
			t.Fatal(err)
		}
		archiveHash := sha256.Sum256(archives[i])
		if response.Header().Get("X-Rin-Fragment-Project-SHA256") != project.Graph.ProjectHash ||
			response.Header().Get("X-Rin-Fragment-Source-SHA256") != hex.EncodeToString(archiveHash[:]) || project.Graph.ProjectHash == hex.EncodeToString(archiveHash[:]) {
			t.Fatal("submission conflated canonical project identity with ZIP artifact integrity")
		}
	}
	if forwarded != 2 || !bytes.Equal(archives[0], archives[1]) {
		t.Fatal("same fragment request changed queued source bytes")
	}
	for _, body := range []string{`{"schema":"typst-fragments/v1","blocks":[]}`, `{"schema":"typst-fragments/v1","actor":"forged"}`} {
		request := httptest.NewRequest(http.MethodPost, "/api/render/fragments", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Rin-Renderer-Token", cfg.ServiceToken)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 400 || forwarded != 2 {
			t.Fatal("invalid fragments reached queue")
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/api/render/fragments", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 401 || forwarded != 2 {
		t.Fatal("unauthenticated fragments reached queue")
	}
}

func TestFragmentsCannotRunInSynchronousOrUnisolatedRuntime(t *testing.T) {
	for _, withQueue := range []bool{false, true} {
		cfg := typstTestConfig(t, "unused fake compiler")
		var jobs http.Handler
		if withQueue {
			jobs = http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unisolated fragments reached queue") })
		}
		server := NewServerWithJobs(cfg, jobs)
		request := httptest.NewRequest(http.MethodPost, "/api/render/fragments", strings.NewReader(`{}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Rin-Renderer-Token", cfg.ServiceToken)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		server.(io.Closer).Close()
		if response.Code != 503 {
			t.Fatalf("fragments admitted without queue/isolation: %d", response.Code)
		}
	}
}

func nativeFragmentTestConfig(t *testing.T) Config {
	t.Helper()
	cfg := testConfig()
	cfg.TypstBin = os.Getenv("RIN_TYPST_TEST_BINARY")
	cfg.TypstFontPath = os.Getenv("RIN_TYPST_TEST_FONT")
	cfg.TypstSandboxBin = os.Getenv("RIN_TYPST_TEST_SANDBOX")
	if cfg.TypstBin == "" || cfg.TypstFontPath == "" || cfg.TypstSandboxBin == "" {
		t.Skip("configure real pinned Typst/font and reviewed sandbox")
	}
	cfg.TypstBinHash = "29273eaa04f6d00edd0c2bec578f565fc9c65be856bfbffc894567c68ed0b237"
	cfg.TypstFontHash = "79c18ebe7b811951e8311bad7103ebeae8c337ed9988ea69e8a78a66cfe029b9"
	body, err := os.ReadFile(cfg.TypstSandboxBin)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	cfg.TypstSandboxHash = hex.EncodeToString(digest[:])
	cfg.TypstPackageHash = typstadapter.EmptyPackageHash
	cfg.TypstVersion = "0.15.1"
	cfg.TypstMathRender = true
	return cfg
}

func TestPinnedFragmentExecutorUsesIsolationMathJaxAndFinalOutput(t *testing.T) {
	cfg := nativeFragmentTestConfig(t)
	files, ids, err := typstadapter.BuildFragmentProject([]typstadapter.FragmentBlock{{ID: "first", Source: "= First\nSee @eq-later.\n$x=1$ <eq-first>"}, {ID: "second", Source: "= Second\n$y=x+1$ <eq-later>\nSee @eq-first."}})
	if err != nil {
		t.Fatal(err)
	}
	executor := NewProjectExecutor(cfg)
	defer executor.Close()
	request := ProjectExecutionRequest{ContentKind: "typst", JobID: "fragment-job", RequestID: "fragment-request", ArchiveName: "fragments.zip", Archive: zipTypstProject(t, files), ExplicitMainFile: "main.typ", DocumentMode: "article", FragmentIDs: ids}
	result := executor.Execute(context.Background(), request)
	if result.Err != nil || result.Canonical == nil || result.Canonical.Validate() != nil {
		t.Fatalf("fragment final output failed: %v %+v", result.Err, result.Diagnostics)
	}
	for _, marker := range []string{`id="fragment-first"`, `id="fragment-second"`, `id="eq-later"`, `href="#eq-later"`, `<mjx-container`, `<math`} {
		if !strings.Contains(result.Response.HTML, marker) {
			t.Fatalf("fragment lost %q", marker)
		}
	}
	if result.Response.Versions.TypstProfile != TypstFragmentsProfileID(cfg) || len(result.Canonical.Assets) != 0 {
		t.Fatal("private fragment profile or asset boundary changed")
	}
	if target := os.Getenv("RIN_TYPST_FRAGMENT_RESULT_REPORT"); target != "" {
		if !filepath.IsAbs(target) {
			t.Fatal("result report must be outside the source tree")
		}
		body, err := json.MarshalIndent(result.Canonical, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, append(body, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	files["main.typ"] = []byte("forged wrapper")
	request.Archive = zipTypstProject(t, files)
	if invalid := executor.Execute(context.Background(), request); invalid.Err == nil || typstDiagnosticCode(invalid) != "typst.fragments.invalid" {
		t.Fatal("untrusted wrapper was compiled")
	}
	legacy := cfg
	legacy.TypstSandboxBin = ""
	legacy.TypstSandboxHash = ""
	if TypstHTMLProfileID(cfg) == TypstHTMLProfileID(legacy) || TypstFragmentsProfileID(legacy) != "" {
		t.Fatal("sandbox change reused an ordinary profile")
	}
}
