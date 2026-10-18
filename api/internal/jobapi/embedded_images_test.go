package jobapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobresult"
)

func TestCanonicalJobHTTPDeliversImagesWithoutCompatibilityOrCrossOwnerAccess(t *testing.T) {
	fixture, err := os.ReadFile("../../../packages/document-contract/fixtures/document-bundle/typst-final-images-v3.json")
	if err != nil {
		t.Fatal(err)
	}
	var bundle contracts.DocumentBundle
	if err := json.Unmarshal(fixture, &bundle); err != nil {
		t.Fatal(err)
	}
	canonical := contracts.RenderResult{SchemaVersion: contracts.RenderResultSchemaVersion, JobID: testJobID, RequestID: "request-a", ProjectHash: bundle.ProjectHash, ResultHash: bundle.BundleHash, ContentKind: contracts.ContentKindTypst, Engine: "typst", Inline: &bundle, Assets: []contracts.ArtifactReference{}, Diagnostics: []contracts.Diagnostic{}, Versions: map[string]string{"rinRenderer": "test", "typst": "0.15.1"}, Cache: contracts.RenderCacheSummary{ReusedStages: []string{}}}
	if err := canonical.Validate(); err != nil {
		t.Fatal(err)
	}
	stored, err := json.Marshal(jobresult.StoredOutput{SchemaVersion: jobresult.StoredOutputSchemaVersion, Result: canonical, Compatibility: json.RawMessage(`{"privateCompatibility":"must-not-be-needed"}`)})
	if err != nil {
		t.Fatal(err)
	}
	repository := newFakeRepository()
	repository.job.State = "succeeded"
	repository.job.ContentKind = "typst"
	repository.job.ResultArtifactID = "result-id"
	expires := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	repository.artifact = jobpostgres.Artifact{ID: "result-id", StorageKey: "result-id", SHA256: strings.Repeat("a", 64), ByteSize: int64(len(stored)), Visibility: "private", MediaType: "application/json", SchemaVersion: jobresult.StoredOutputSchemaVersion, ExpiresAt: &expires}
	handler := newTestHandler(t, &fakeAdmission{}, repository, &fakeStore{body: stored})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID+"/result", nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "privateCompatibility") {
		t.Fatalf("canonical endpoint: %d %s", response.Code, response.Body.String())
	}
	var delivered contracts.RenderResult
	if err := json.Unmarshal(response.Body.Bytes(), &delivered); err != nil {
		t.Fatal(err)
	}
	if err := delivered.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(delivered.Inline.Assets) != 1 || delivered.Inline.Assets[0].BodyBase64 != bundle.Assets[0].BodyBase64 {
		t.Fatal("resource bytes did not survive the canonical HTTP endpoint")
	}
	for _, owner := range []string{"other-book", ""} {
		req := authorizedRequest(http.MethodGet, "/api/render/jobs/"+testJobID+"/result", nil)
		req.Header.Set("X-Rin-Renderer-Owner-Scope", owner)
		denied := httptest.NewRecorder()
		handler.ServeHTTP(denied, req)
		if denied.Code == http.StatusOK || strings.Contains(denied.Body.String(), bundle.Assets[0].BodyBase64) {
			t.Fatal("cross-owner resource bytes leaked")
		}
	}
}
