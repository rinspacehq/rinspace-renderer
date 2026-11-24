package jobapi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobpostgres"
	"github.com/rinspacehq/rinspace-renderer/api/internal/jobresult"
	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfinspect"
	"github.com/rinspacehq/rinspace-renderer/api/internal/publicationpdf"
)

func TestPublicationPDFIdentityAndAssets(t *testing.T) {
	handler, input := publicationPDFFixture(t, printFixture{})
	for _, test := range []struct {
		name    string
		token   string
		reading string
		source  string
		code    int
	}{
		{"permitted", "token-a", input.ReadingSHA256, input.SourceSHA256, http.StatusOK},
		{"permission", "token-b", input.ReadingSHA256, input.SourceSHA256, http.StatusNotFound},
		{"reading mismatch", "token-a", strings.Repeat("1", 64), input.SourceSHA256, http.StatusConflict},
		{"source mismatch", "token-a", input.ReadingSHA256, strings.Repeat("2", 64), http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := input
			candidate.ReadingSHA256, candidate.SourceSHA256 = test.reading, test.source
			response := requestPublicationPDF(t, handler, test.token, candidate)
			if response.Code != test.code {
				t.Fatalf("publication PDF = %d %s", response.Code, response.Body.String())
			}
			if test.code == http.StatusOK {
				assertPDFReceipt(t, response, candidate, 2)
			}
		})
	}
	handler.printSlot <- struct{}{}
	defer func() { <-handler.printSlot }()
	if response := requestPublicationPDF(t, handler, "token-a", input); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy publication printer accepted a second job: %d", response.Code)
	}
}

func TestPublicationTagPDFBindsPublicFieldsAndProjectScope(t *testing.T) {
	handler, previous := publicationPDFFixture(t, printFixture{})
	snapshot := contracts.PublicationTagPDFSnapshot{Title: "中文标签", Excerpt: "A <script> example is plain text."}
	edition, err := snapshot.Edition("tag-wiki:42")
	if err != nil {
		t.Fatal(err)
	}
	input := contracts.PublicationPDFInput{PublicationPDFEdition: edition, ProfileSHA256: previous.ProfileSHA256}
	for _, test := range []struct {
		name, scope, title string
		code               int
	}{
		{"published fields", edition.ProjectID, snapshot.Title, http.StatusOK},
		{"changed fields", edition.ProjectID, "新名称", http.StatusUnprocessableEntity},
		{"wrong scope", "tag-wiki:43", snapshot.Title, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			fields := snapshot
			fields.Title = test.title
			response := requestTagPublicationPDF(t, handler, test.scope, input, fields)
			if response.Code != test.code {
				t.Fatalf("tag PDF = %d %s", response.Code, response.Body)
			}
			if test.code == http.StatusOK {
				assertPDFReceipt(t, response, input, 2)
			}
		})
	}
}

func TestArchivedMarkdownPublicationPDFUsesPersistedResult(t *testing.T) {
	handler, input := publicationPDFFixture(t, printFixture{})
	reading, archive := expirePublicationFixture(t, handler)
	wrongReading := input
	wrongReading.ReadingSHA256 = strings.Repeat("1", 64)
	for _, test := range []struct {
		name, token, scope string
		input              contracts.PublicationPDFInput
		archive            []byte
		code               int
	}{
		{"expired original job", "token-a", input.ProjectID, input, archive, http.StatusOK},
		{"migration principal", "token-b", input.ProjectID, input, archive, http.StatusForbidden},
		{"different project", "token-a", "book:42", input, archive, http.StatusUnprocessableEntity},
		{"different source", "token-a", input.ProjectID, input, []byte("different archive"), http.StatusConflict},
		{"different reading", "token-a", input.ProjectID, wrongReading, archive, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := requestArchivedMarkdownPDF(t, handler, test.token, test.scope, test.input, reading, test.archive)
			if response.Code != test.code {
				t.Fatalf("archived PDF = %d %s", response.Code, response.Body.String())
			}
			if test.code == http.StatusOK {
				assertPDFReceipt(t, response, test.input, 2)
			}
		})
	}
}

func expirePublicationFixture(t *testing.T, handler *Handler) (json.RawMessage, []byte) {
	t.Helper()
	store := handler.store.(*fakeStore)
	output, err := jobresult.Decode(store.bodies["fixture/result.json"])
	if err != nil {
		t.Fatal(err)
	}
	reading, err := json.Marshal(output.Result)
	if err != nil {
		t.Fatal(err)
	}
	archive := store.bodies["fixture/source.zip"]
	clear(store.bodies)
	clear(handler.repository.(*fakeRepository).artifacts)
	if len(archive) == 0 {
		t.Fatal("source fixture is missing")
	}
	return reading, archive
}

func requestArchivedMarkdownPDF(t *testing.T, handler *Handler, token, scope string, input contracts.PublicationPDFInput, reading json.RawMessage, archive []byte) *httptest.ResponseRecorder {
	t.Helper()
	_, key, err := input.Identity()
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("input", string(metadata)); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("result", string(reading)); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile("source", "archive.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(archive); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := authorizedRequest(http.MethodPost, "/api/render/publication-pdf/markdown-backfill", &body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.Header.Set("X-Rin-Renderer-Token", token)
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("X-Rin-Renderer-Owner-Scope", scope)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func requestTagPublicationPDF(t *testing.T, handler *Handler, scope string, input contracts.PublicationPDFInput, snapshot contracts.PublicationTagPDFSnapshot) *httptest.ResponseRecorder {
	t.Helper()
	_, key, err := input.Identity()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"snapshot": snapshot, "input": input})
	if err != nil {
		t.Fatal(err)
	}
	request := authorizedRequest(http.MethodPost, "/api/render/publication-pdf/tag-snapshot", bytes.NewReader(raw))
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("X-Rin-Renderer-Owner-Scope", scope)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestPublicationPDFChromium(t *testing.T) {
	chromium := os.Getenv("RIN_RENDERER_MARKDOWN_PDF_CHROMIUM")
	if chromium == "" {
		t.Skip("set RIN_RENDERER_MARKDOWN_PDF_CHROMIUM to the locked headless browser")
	}
	script, err := filepath.Abs("../../../engines/markdown-pdf/print.mjs")
	if err != nil {
		t.Fatal(err)
	}
	handler, input := publicationPDFFixture(t, publicationpdf.BrowserPrinter{
		Node: os.Getenv("RIN_RENDERER_MARKDOWN_NODE_BIN"), Chromium: chromium, Script: script,
	})
	response := requestPublicationPDF(t, handler, "token-a", input)
	if response.Code != http.StatusOK {
		t.Fatalf("real publication PDF = %d %s", response.Code, response.Body.String())
	}
	assertPDFReceipt(t, response, input, 2)
	snapshot := contracts.PublicationTagPDFSnapshot{Title: "样本标签", Excerpt: "用于验证中文标签的公开用法。"}
	tagEdition, err := snapshot.Edition("tag-wiki:42")
	if err != nil {
		t.Fatal(err)
	}
	tag := contracts.PublicationPDFInput{PublicationPDFEdition: tagEdition, ProfileSHA256: input.ProfileSHA256}
	tagResponse := requestTagPublicationPDF(t, handler, tagEdition.ProjectID, tag, snapshot)
	if tagResponse.Code != http.StatusOK {
		t.Fatalf("real tag PDF = %d %s", tagResponse.Code, tagResponse.Body)
	}
	assertPDFReceipt(t, tagResponse, tag, 1)
	reading, archive := expirePublicationFixture(t, handler)
	archived := requestArchivedMarkdownPDF(t, handler, "token-a", input.ProjectID, input, reading, archive)
	if archived.Code != http.StatusOK {
		t.Fatalf("real archived publication PDF = %d %s", archived.Code, archived.Body)
	}
	assertPDFReceipt(t, archived, input, 2)
	if sample := os.Getenv("RIN_PUBLICATION_PDF_TEST_OUTPUT"); sample != "" {
		if err := os.WriteFile(sample, response.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sample+".receipt", []byte(response.Header().Get(publicationPDFReceiptHeader)), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sample+".tag.pdf", tagResponse.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func publicationPDFFixture(t *testing.T, printer PublicationPDFPrinter) (*Handler, contracts.PublicationPDFInput) {
	t.Helper()
	const assetPath = "assets/images/cover.png"
	const diagramID = "diagrams/fixture.svg"
	diagram := []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10" width="10" height="10"><path d="M0 0 L9 9"/></svg>`)
	var imageBytes bytes.Buffer
	if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 10, 10))); err != nil {
		t.Fatal(err)
	}
	var source bytes.Buffer
	writer := zip.NewWriter(&source)
	for _, file := range []struct {
		name string
		body []byte
	}{{assetPath, imageBytes.Bytes()}, {"main.md", []byte("# Sample book")}} {
		member, err := writer.Create(file.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write(file.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	repository := newFakeRepository()
	input := contracts.PublicationPDFInput{
		PublicationPDFEdition: contracts.PublicationPDFEdition{
			ProjectID: "book:77", SourceRevision: "git:" + strings.Repeat("a", 40),
			SourceSHA256: publicationFixtureSHA(source.Bytes()), ReadingSHA256: strings.Repeat("b", 64),
		},
		ProfileSHA256: strings.Repeat("d", 64),
	}
	bundle := contracts.DocumentBundle{
		SchemaVersion: contracts.DocumentBundleSchemaVersionV1, ProjectHash: strings.Repeat("c", 64),
		BundleHash: input.ReadingSHA256, State: "final", ContentKind: "markdown", DocumentEngine: "markdown",
		Title: "发布样书",
		Pages: []contracts.DocumentPage{
			{ID: "first", SourcePath: "first.md", FragmentFormat: "html", Fragment: `<h1>序章</h1><img data-rin-asset-path="` + assetPath + `" src="https://example.invalid/cover.png" alt="Verified picture"><img data-rin-diagram-object-id="` + diagramID + `" src="https://example.invalid/diagram.svg" alt="Verified diagram"><p><a href="last.md#end">跳到终章</a></p>`},
			{ID: "last", SourcePath: "last.md", FragmentFormat: "html", Fragment: `<h1 id="end">终章</h1><p>最后一章的内容。</p>`},
		},
		Assets: []contracts.AssetReference{{
			ID: "cover", Kind: "project-file", ProjectPath: assetPath, MediaType: "image/png",
			SHA256: publicationFixtureSHA(imageBytes.Bytes()), Bytes: int64(imageBytes.Len()),
		}},
		Provenance: contracts.Provenance{
			Adapter: "markdown", AdapterVersion: "1", EngineVersion: "1",
			ProjectGraphSchemaVersion: contracts.ProjectGraphSchemaVersion,
		},
	}
	stored := jobresult.StoredOutput{
		SchemaVersion: jobresult.StoredOutputSchemaVersion, Compatibility: json.RawMessage(`{}`),
		Result: contracts.RenderResult{
			SchemaVersion: contracts.RenderResultSchemaVersion, JobID: testJobID, RequestID: "test-request",
			ProjectHash: bundle.ProjectHash, ResultHash: input.ReadingSHA256, ContentKind: "markdown", Engine: "rin-markdown",
			Inline: &bundle, Assets: []contracts.ArtifactReference{{
				ArtifactID: diagramID, SHA256: publicationFixtureSHA(diagram), Bytes: int64(len(diagram)),
				MediaType: "image/svg+xml", Visibility: "public",
			}}, Diagnostics: []contracts.Diagnostic{},
			Versions: map[string]string{"renderer": "test"}, Cache: contracts.RenderCacheSummary{ReusedStages: []string{}},
		},
	}
	if err := stored.Validate(); err != nil {
		t.Fatal(err)
	}
	resultBytes, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(map[string]string{
		"outputKind": "document", "controlProjectId": input.ProjectID, "sourceCommit": strings.TrimPrefix(input.SourceRevision, "git:"),
		"documentMode": "book",
	})
	if err != nil {
		t.Fatal(err)
	}
	repository.job.OwnerScope = input.ProjectID
	repository.job.State, repository.job.ContentKind = "succeeded", "markdown"
	repository.job.SourceArtifactID, repository.job.ResultArtifactID, repository.job.RequestMetadata = "source", "result", meta
	sourceArtifact := jobpostgres.Artifact{
		StorageKey: "fixture/source.zip", Visibility: "private", SHA256: input.SourceSHA256,
		MediaType: "application/zip", ByteSize: int64(source.Len()), ExpiresAt: &repository.job.ExpiresAt,
	}
	repository.artifacts["source"] = sourceArtifact
	repository.artifacts["result"] = jobpostgres.Artifact{
		StorageKey: "fixture/result.json", Visibility: "private", SHA256: publicationFixtureSHA(resultBytes),
		MediaType: "application/json", ByteSize: int64(len(resultBytes)), SchemaVersion: jobresult.StoredOutputSchemaVersion,
	}
	store := &fakeStore{bodies: map[string][]byte{"fixture/source.zip": source.Bytes(), "fixture/result.json": resultBytes}}
	handler := newTestHandler(t, &fakeAdmission{}, repository, store)
	config := handler.config
	config.PublicationPDF = &PublicationPDFConfig{
		ProfileSHA256: input.ProfileSHA256, Printer: printer, PublicationPrincipal: "principal-a",
		PublicAssets: fixturePublicImage{id: diagramID, body: diagram},
	}
	config.Now = func() time.Time { return repository.job.CreatedAt }
	handler, err = New(handler.auth, handler.admission, repository, store, config)
	if err != nil {
		t.Fatal(err)
	}
	return handler, input
}

type fixturePublicImage struct {
	id   string
	body []byte
}

func (image fixturePublicImage) ReadPublicObject(_ context.Context, id string, size int64) ([]byte, error) {
	if id != image.id || size != int64(len(image.body)) {
		return nil, errors.New("unexpected diagram object")
	}
	return image.body, nil
}

func requestPublicationPDF(t *testing.T, handler *Handler, token string, input contracts.PublicationPDFInput) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := input.Identity()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/render/jobs/"+testJobID+"/publication-pdf", bytes.NewReader(raw))
	request.Header.Set("X-Rin-Renderer-Token", token)
	request.Header.Set("X-Rin-Renderer-Owner-Scope", input.ProjectID)
	request.Header.Set("Idempotency-Key", key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertPDFReceipt(t *testing.T, response *httptest.ResponseRecorder, input contracts.PublicationPDFInput, pages int) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(response.Header().Get(publicationPDFReceiptHeader))
	if err != nil {
		t.Fatal(err)
	}
	var receipt contracts.PublicationPDFResult
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	if err := receipt.ValidateFor(input); err != nil || receipt.Pages != pages ||
		receipt.SHA256 != publicationFixtureSHA(response.Body.Bytes()) || receipt.Bytes != int64(response.Body.Len()) {
		t.Fatalf("receipt mismatch: %#v %v", receipt, err)
	}
}

type printFixture struct{}

func (printFixture) Print(ctx context.Context, bundle contracts.DocumentBundle, book bool, resolve publicationpdf.ResolveImage) ([]byte, pdfinspect.Result, error) {
	if _, err := publicationpdf.MarkdownHTML(ctx, bundle, book, resolve); err != nil {
		return nil, pdfinspect.Result{}, err
	}
	body := []byte("fixture PDF bytes")
	return body, pdfinspect.Result{PageCount: 2, SourceBytes: int64(len(body)), SourceSHA256: publicationFixtureSHA(body)}, nil
}

func publicationFixtureSHA(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
