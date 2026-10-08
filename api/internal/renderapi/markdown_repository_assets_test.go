package renderapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectidentity"
)

func markdownTestPNG(t *testing.T) []byte {
	t.Helper()
	var encoded bytes.Buffer
	canvas := image.NewRGBA(image.Rect(0, 0, 2, 2))
	canvas.Set(0, 0, color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
	if err := png.Encode(&encoded, canvas); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func TestPrivateMarkdownRepositoryArchivePromotesImageAndQuiverSVG(t *testing.T) {
	pngBody := markdownTestPNG(t)
	pngHash := sha256.Sum256(pngBody)
	pngPath := "assets/images/" + hex.EncodeToString(pngHash[:]) + ".png"
	const svgPath = "assets/quiver/diagram.svg"
	const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><path d="M0 0L10 10"/></svg>`
	archive := markdownExecutorFiles(t, map[string]string{
		"content.md":    "![image](" + pngPath + ")\n![quiver](" + svgPath + ")",
		"rinspace.yaml": "status: draft\nsourceVisibility: private\n",
		pngPath:         string(pngBody),
		svgPath:         svg,
	})
	project, err := projectidentity.Import("article.zip", archive, contracts.ContentKindMarkdown, "content.md", projectcore.Limits{
		ArchiveMaxBytes: 1 << 20, FileMaxCount: 10, FileMaxBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string][]byte, len(project.Files))
	for _, file := range project.Files {
		files[file.Path], err = projectidentity.FileBytes(file)
		if err != nil {
			t.Fatal(err)
		}
	}
	assets := make([]contracts.AssetReference, 0, 2)
	for _, file := range project.Graph.Files {
		if file.Role == contracts.ProjectFileRoleAsset {
			assets = append(assets, contracts.AssetReference{
				ID: file.Path, Kind: "project-file", ProjectPath: file.Path,
				SHA256: file.SHA256, Bytes: file.Bytes, MediaType: file.MediaType,
			})
		}
	}
	bundle := contracts.DocumentBundle{
		SchemaVersion: contracts.DocumentBundleSchemaVersionV2,
		ProjectHash:   project.Graph.ProjectHash, BundleHash: strings.Repeat("2", 64),
		State: contracts.DocumentBundleStateFinal, ContentKind: contracts.ContentKindMarkdown,
		DocumentEngine: "rin-markdown", Title: "Archived assets",
		Pages: []contracts.DocumentPage{{
			ID: "page", SourcePath: "content.md", FragmentFormat: contracts.FragmentFormatHTML,
			Fragment: `<img src="` + pngPath + `" data-rin-asset-path="` + pngPath + `"><img src="` + svgPath + `" data-rin-asset-path="` + svgPath + `">`,
			TOC:      []contracts.TOCEntry{}, DependencyHashes: []string{},
		}},
		Assets: assets, WorkUnits: []contracts.WorkUnit{}, Diagnostics: []contracts.Diagnostic{},
	}
	executor := &markdownProjectExecutor{server: &Server{}}
	artifacts, err := executor.promoteRepositoryAssets(context.Background(), ProjectExecutionRequest{
		ProjectID: "article:42", SourceCommit: strings.Repeat("a", 40),
	}, &bundle, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 0 || strings.Count(bundle.Pages[0].Fragment, "/repos/a/42/raw/commit/") != 2 {
		t.Fatalf("private Markdown assets were not bound to the repository: artifacts=%#v fragment=%q", artifacts, bundle.Pages[0].Fragment)
	}
}

func TestPromoteMarkdownRepositoryAssetsUploadsOnlyReferencedPublicFiles(t *testing.T) {
	stored := map[string][]byte{}
	storage := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		const prefix = "/v1/storages/object/"
		objectKey := strings.TrimPrefix(request.URL.Path, prefix)
		objectKey = strings.TrimPrefix(objectKey, "public/")
		switch request.Method {
		case http.MethodHead:
			if _, ok := stored[objectKey]; !ok {
				http.NotFound(writer, request)
				return
			}
			writer.WriteHeader(http.StatusOK)
		case http.MethodPost:
			body, _ := io.ReadAll(request.Body)
			stored[objectKey] = body
			writer.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(writer, `{"Id":"asset","Key":%q}`, objectKey)
		case http.MethodGet:
			body, ok := stored[objectKey]
			if !ok {
				http.NotFound(writer, request)
				return
			}
			_, _ = writer.Write(body)
		default:
			writer.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer storage.Close()

	pngBody := markdownTestPNG(t)
	digest := sha256.Sum256(pngBody)
	hash := hex.EncodeToString(digest[:])
	assetPath := "assets/images/" + hash + ".png"
	bundle := contracts.DocumentBundle{
		SchemaVersion: contracts.DocumentBundleSchemaVersionV2,
		ProjectHash:   strings.Repeat("1", 64), BundleHash: strings.Repeat("2", 64),
		State: contracts.DocumentBundleStateFinal, ContentKind: contracts.ContentKindMarkdown,
		DocumentEngine: "rin-markdown", Title: "Asset",
		Pages: []contracts.DocumentPage{{
			ID: "page", SourcePath: "content.md",
			Fragment:       `<img src="` + assetPath + `" data-rin-asset-path="` + assetPath + `">`,
			FragmentFormat: contracts.FragmentFormatHTML, TOC: []contracts.TOCEntry{}, DependencyHashes: []string{},
		}},
		WorkUnits:   []contracts.WorkUnit{},
		Assets:      []contracts.AssetReference{{ID: "asset-1", Kind: "project-file", SHA256: hash, Bytes: int64(len(pngBody)), MediaType: "image/png", ProjectPath: assetPath}},
		Diagnostics: []contracts.Diagnostic{},
	}
	cfg := Config{StorageAccessToken: "token", StorageBucket: "rin-renderer", StorageBaseURL: storage.URL, StoragePublicBaseURL: "https://cdn.example", MarkdownRepoAssets: true}
	executor := &markdownProjectExecutor{cfg: cfg, server: &Server{cfg: cfg, operations: operational.New(io.Discard)}}
	artifacts, err := executor.promoteRepositoryAssets(context.Background(), ProjectExecutionRequest{
		ProjectID: "article:42", SourceCommit: strings.Repeat("a", 40),
	}, &bundle, map[string][]byte{
		"rinspace.yaml": []byte("status: published\nsourceVisibility: open\n"),
		assetPath:       pngBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].SHA256 != hash || artifacts[0].Visibility != "public" {
		t.Fatalf("unexpected promoted artifact: %#v", artifacts)
	}
	if !strings.Contains(bundle.Pages[0].Fragment, "https://cdn.example/") || strings.Contains(bundle.Pages[0].Fragment, `src="assets/`) {
		t.Fatalf("final Markdown did not use the promoted object: %s", bundle.Pages[0].Fragment)
	}
	if bundle.BundleHash == strings.Repeat("2", 64) || len(stored) != 1 {
		t.Fatalf("promotion did not update identity or wrote unexpected objects: bundle=%q stored=%d", bundle.BundleHash, len(stored))
	}
}

func TestMarkdownRepositoryAssetPromotionSwitchAndStorageFailurePreserveCandidateBundle(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		enabled bool
		status  int
		metric  string
		code    string
		missing bool
	}{
		{name: "disabled", enabled: false, status: http.StatusOK, metric: "rin_renderer_markdown_repository_asset_requests_total_public_disabled 1", code: "markdown.assets.disabled"},
		{name: "missing asset", enabled: true, status: http.StatusOK, metric: "rin_renderer_markdown_repository_asset_requests_total_public_failed 1", code: "markdown.assets.missing", missing: true},
		{name: "storage failure", enabled: true, status: http.StatusInternalServerError, metric: "rin_renderer_markdown_repository_asset_requests_total_public_failed 1", code: "markdown.assets.storage_failed"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			storage := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(scenario.status)
			}))
			defer storage.Close()

			pngBody := markdownTestPNG(t)
			digest := sha256.Sum256(pngBody)
			hash := hex.EncodeToString(digest[:])
			assetPath := "assets/images/" + hash + ".png"
			fragment := `<img src="` + assetPath + `" data-rin-asset-path="` + assetPath + `">`
			bundle := contracts.DocumentBundle{
				SchemaVersion: contracts.DocumentBundleSchemaVersionV2,
				ProjectHash:   strings.Repeat("1", 64), BundleHash: strings.Repeat("2", 64),
				State: contracts.DocumentBundleStateFinal, ContentKind: contracts.ContentKindMarkdown,
				DocumentEngine: "rin-markdown", Title: "Failure",
				Pages:       []contracts.DocumentPage{{ID: "page", SourcePath: "article.md", Fragment: fragment, FragmentFormat: contracts.FragmentFormatHTML, TOC: []contracts.TOCEntry{}, DependencyHashes: []string{}}},
				WorkUnits:   []contracts.WorkUnit{},
				Assets:      []contracts.AssetReference{{ID: "asset-1", Kind: "project-file", SHA256: hash, Bytes: int64(len(pngBody)), MediaType: "image/png", ProjectPath: assetPath}},
				Diagnostics: []contracts.Diagnostic{},
			}
			operations := operational.New(io.Discard)
			cfg := Config{
				StorageAccessToken: "token", StorageBucket: "rin-renderer", StorageBaseURL: storage.URL,
				StoragePublicBaseURL: "https://cdn.example", MarkdownRepoAssets: scenario.enabled,
			}
			executor := &markdownProjectExecutor{cfg: cfg, server: &Server{cfg: cfg, operations: operations}}
			files := map[string][]byte{
				"rinspace.yaml": []byte("status: published\nsourceVisibility: open\n"),
			}
			if !scenario.missing {
				files[assetPath] = pngBody
			}
			_, err := executor.promoteRepositoryAssets(context.Background(), ProjectExecutionRequest{
				ProjectID: "article:42", SourceCommit: strings.Repeat("a", 40),
			}, &bundle, files)
			if err == nil {
				t.Fatal("expected promotion to fail")
			}
			var failure *markdownRepositoryAssetFailure
			if !errors.As(err, &failure) || failure.Code != scenario.code {
				t.Fatalf("promotion failure code = %#v, want %q", failure, scenario.code)
			}
			result := markdownExecutionFailure("markdown.assets.failed", "Markdown repository asset promotion failed", err)
			if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != scenario.code || strings.Contains(result.Diagnostics[0].Message, assetPath) {
				t.Fatalf("unsafe or unclassified author diagnostic: %#v", result.Diagnostics)
			}
			if bundle.Pages[0].Fragment != fragment || bundle.BundleHash != strings.Repeat("2", 64) {
				t.Fatalf("failed promotion mutated the candidate bundle: %#v", bundle)
			}
			metrics := httptest.NewRecorder()
			operations.Metrics(metrics, httptest.NewRequest(http.MethodGet, "/metrics", nil))
			if !strings.Contains(metrics.Body.String(), scenario.metric) {
				t.Fatalf("missing promotion failure metric %q in %s", scenario.metric, metrics.Body.String())
			}
		})
	}
}

func TestVerifiedMarkdownRepositoryAssets(t *testing.T) {
	pngBody := markdownTestPNG(t)
	if body, mediaType, extension, err := verifiedMarkdownAsset(pngBody, "image/png", "assets/images/example.png"); err != nil || string(body) != string(pngBody) || mediaType != "image/png" || extension != ".png" {
		t.Fatalf("valid PNG was not preserved: media=%q extension=%q err=%v", mediaType, extension, err)
	}
	if _, _, _, err := verifiedMarkdownAsset(pngBody, "image/jpeg", "assets/images/example.png"); err == nil {
		t.Fatal("expected mismatched image media type to fail")
	}

	svg := []byte("<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 10 10\"><path d=\"M0 0L10 10\"/></svg>\n")
	if body, mediaType, extension, err := verifiedMarkdownAsset(svg, "image/svg+xml", "assets/quiver/diagram.svg"); err != nil || !strings.Contains(string(body), `<path`) || mediaType != "image/svg+xml; charset=utf-8" || extension != ".svg" {
		t.Fatalf("valid Quiver SVG was not normalized: media=%q extension=%q body=%q err=%v", mediaType, extension, body, err)
	}
	if _, _, _, err := verifiedMarkdownAsset([]byte(`<svg><script>alert(1)</script></svg>`), "image/svg+xml", "assets/quiver/diagram.svg"); err == nil {
		t.Fatal("expected SVG without visible safe content to fail")
	}
}

func TestMarkdownRepositoryAssetPathsPreserveHistoricalRelativeImages(t *testing.T) {
	for _, projectPath := range []string{"assets/images/example.png", "assets/quiver/example.svg", "docs/images/example.png", "figures/chart.webp"} {
		if !validMarkdownPromotablePath(projectPath) {
			t.Fatalf("valid relative image path was rejected: %s", projectPath)
		}
	}
	for _, projectPath := range []string{"../escape.png", ".hidden/image.png", "content.md", "assets/quiver/source.tikzcd"} {
		if validMarkdownPromotablePath(projectPath) {
			t.Fatalf("invalid promotable image path was accepted: %s", projectPath)
		}
	}
}

func TestMarkdownRepositoryAssetRewritePreservesRepositoryIdentity(t *testing.T) {
	tag := `<img src="assets/images/old.png" alt="diagram" data-rin-asset-path="assets/images/new.png">`
	got := replaceMarkdownAssetSource(tag, "https://cdn.example/assets/new.png?a=1&b=2")
	if !strings.Contains(got, `src="https://cdn.example/assets/new.png?a=1&amp;b=2"`) ||
		!strings.Contains(got, `data-rin-asset-path="assets/images/new.png"`) {
		t.Fatalf("asset rewrite lost the safe URL or repository marker: %s", got)
	}

	privateURL, err := markdownPrivateRepositoryAssetURL(ProjectExecutionRequest{
		ProjectID: "article:42", SourceCommit: strings.Repeat("a", 40),
	}, "assets/images/new.png")
	if err != nil {
		t.Fatal(err)
	}
	want := "/repos/a/42/raw/commit/" + strings.Repeat("a", 40) + "/assets/images/new.png"
	if privateURL != want {
		t.Fatalf("unexpected private repository URL: got %q want %q", privateURL, want)
	}
	if _, err := markdownPrivateRepositoryAssetURL(ProjectExecutionRequest{ProjectID: "article:42", SourceCommit: "main"}, "assets/images/new.png"); err == nil {
		t.Fatal("expected a mutable revision to be rejected")
	}
}

func TestMarkdownBundleHashChangesWithPromotedSource(t *testing.T) {
	bundle := contracts.DocumentBundle{
		SchemaVersion: contracts.DocumentBundleSchemaVersionV2,
		ProjectHash:   strings.Repeat("1", 64), BundleHash: strings.Repeat("2", 64),
		State: contracts.DocumentBundleStateFinal, ContentKind: contracts.ContentKindMarkdown,
		DocumentEngine: "rin-markdown", Title: "Asset",
		Pages:     []contracts.DocumentPage{{ID: "page", SourcePath: "article.md", Fragment: `<img src="assets/images/a.png">`, FragmentFormat: contracts.FragmentFormatHTML, TOC: []contracts.TOCEntry{}, DependencyHashes: []string{}}},
		WorkUnits: []contracts.WorkUnit{}, Assets: []contracts.AssetReference{}, Diagnostics: []contracts.Diagnostic{},
	}
	before, err := markdownBundleHash(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Pages[0].Fragment = `<img src="https://cdn.example/a.png">`
	after, err := markdownBundleHash(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if before == after || len(before) != 64 || len(after) != 64 {
		t.Fatalf("bundle hash did not track the rewritten source: before=%q after=%q", before, after)
	}
}
