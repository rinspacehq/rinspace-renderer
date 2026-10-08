package renderapi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/nodeworker"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectidentity"
)

func TestMarkdownProjectExecutorPublishesFinalBundle(t *testing.T) {
	cfg := ConfigFromEnv()
	cfg.MarkdownScript = filepath.Clean("../../../engines/markdown/worker.mjs")
	cfg.MarkdownWorkerCount = 1
	executor, err := NewProjectExecutorWithMarkdown(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()

	source := "# Server article\n\nA safe paragraph with **GFM-compatible** markup."
	result := executor.Execute(context.Background(), ProjectExecutionRequest{
		ContentKind: "markdown", JobID: "11111111-1111-4111-8111-111111111111", RequestID: "render-11111111111111111111111111111111",
		ArchiveName: "article.zip", Archive: markdownExecutorArchive(t, "article.md", source),
		Engine: "unified", Title: "Server article", ExplicitMainFile: "article.md", ProjectStatus: "published-preview",
	})
	if result.Err != nil || result.Status != 200 || result.Canonical == nil {
		t.Fatalf("Markdown execution failed: status=%d message=%q err=%v diagnostics=%#v", result.Status, result.Message, result.Err, result.Diagnostics)
	}
	if result.Canonical.JobID != "11111111-1111-4111-8111-111111111111" || result.Canonical.RequestID != "render-11111111111111111111111111111111" {
		t.Fatalf("Markdown execution lost cross-system identity: %#v", result.Canonical)
	}
	bundle := result.Canonical.Inline
	if result.Canonical.ContentKind != "markdown" || result.Canonical.Engine != "rin-markdown" || bundle == nil ||
		bundle.State != "final" || len(bundle.Pages) != 1 || bundle.Pages[0].FragmentFormat != "html" ||
		!strings.Contains(bundle.Pages[0].Fragment, "<strong>GFM-compatible</strong>") || strings.Contains(bundle.Pages[0].Fragment, "rin-work") {
		t.Fatalf("unexpected final Markdown result: %#v", result.Canonical)
	}
	digest := sha256.Sum256([]byte(source))
	if !containsExact(bundle.Pages[0].DependencyHashes, hex.EncodeToString(digest[:])) {
		t.Fatalf("final Bundle omitted exact source identity: %#v", bundle.Pages[0].DependencyHashes)
	}
	encoded, err := json.Marshal(result.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["diagnostics"].([]any); !ok {
		t.Fatalf("canonical Markdown diagnostics must be a JSON array: %s", encoded)
	}
}

func TestMarkdownProjectExecutorPublishesV2MathAndCodeBlocks(t *testing.T) {
	cfg := ConfigFromEnv()
	cfg.MarkdownScript = filepath.Clean("../../../engines/markdown/worker.mjs")
	cfg.MarkdownWorkerCount = 1
	executor, err := NewProjectExecutorWithMarkdown(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()

	source := "# Work blocks\n\n$$\nx^2\n$$\n\n```go\nfmt.Println(\"x\")\n```"
	result := executor.Execute(context.Background(), ProjectExecutionRequest{
		ContentKind: "markdown", RequestID: "33333333-3333-4333-8333-333333333333",
		ArchiveName: "article.zip", Archive: markdownExecutorArchive(t, "article.md", source),
		Engine: "unified", Title: "Work blocks", ExplicitMainFile: "article.md", ProjectStatus: "published-preview",
	})
	if result.Err != nil || result.Status != 200 || result.Canonical == nil || result.Canonical.Inline == nil {
		t.Fatalf("Markdown v2 work-block execution failed: status=%d message=%q err=%v diagnostics=%#v", result.Status, result.Message, result.Err, result.Diagnostics)
	}
	bundle := result.Canonical.Inline
	if bundle.SchemaVersion != contracts.DocumentBundleSchemaVersionV2 ||
		!containsBlockKind(bundle.Pages[0].Blocks, contracts.DocumentBlockMath) ||
		!containsBlockKind(bundle.Pages[0].Blocks, contracts.DocumentBlockCode) ||
		strings.Contains(bundle.Pages[0].Fragment, "<rin-work") ||
		strings.Count(bundle.Pages[0].Fragment, "data-rin-block-id=") != len(bundle.Pages[0].Blocks) {
		t.Fatalf("unexpected Markdown v2 work-block result: %#v", bundle.Pages[0])
	}
}

func TestMarkdownProjectExecutorPublishesOrderedBookBundle(t *testing.T) {
	cfg := ConfigFromEnv()
	cfg.MarkdownScript = filepath.Clean("../../../engines/markdown/worker.mjs")
	cfg.MarkdownWorkerCount = 1
	executor, err := NewProjectExecutorWithMarkdown(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()

	archive := markdownExecutorFiles(t, map[string]string{
		"03-啊啊啊.md":         "# 啊啊啊\n\n[Next](01-可爱捏/01-非常可爱.md)",
		"01-可爱捏/01-非常可爱.md": "# 非常可爱\n\nThe end.",
	})
	result := executor.Execute(context.Background(), ProjectExecutionRequest{
		ContentKind: "markdown", RequestID: "22222222-2222-4222-8222-222222222222",
		ArchiveName: "book.zip", Archive: archive, Engine: "unified", Title: "Server Book",
		DocumentMode: "book", ProjectStatus: "published-preview",
		MarkdownBookPages: []projectidentity.MarkdownBookPage{
			{Path: "03-啊啊啊.md", ID: "md-003-啊啊啊", Title: "啊啊啊"},
			{Path: "01-可爱捏/01-非常可爱.md", ID: "md-001-可爱捏-sec-001-非常可爱", Title: "非常可爱"},
		},
	})
	if result.Err != nil || result.Status != 200 || result.Canonical == nil || result.Canonical.Inline == nil {
		t.Fatalf("Markdown Book execution failed: status=%d message=%q err=%v diagnostics=%#v", result.Status, result.Message, result.Err, result.Diagnostics)
	}
	bundle := result.Canonical.Inline
	if bundle.Title != "Server Book" || len(bundle.Pages) != 2 || bundle.Pages[0].ID != "md-003-啊啊啊" ||
		bundle.Pages[1].ID != "md-001-可爱捏-sec-001-非常可爱" || bundle.Pages[0].SourcePath != "03-啊啊啊.md" ||
		bundle.Pages[1].SourcePath != "01-可爱捏/01-非常可爱.md" ||
		len(bundle.WorkUnits) != 0 || bundle.Provenance.Adapter == "" || result.Canonical.Cache.Hit {
		t.Fatalf("unexpected final Markdown Book result: %#v", result.Canonical)
	}
	for _, page := range bundle.Pages {
		if len(page.DependencyHashes) == 0 || page.FragmentFormat != "html" {
			t.Fatalf("Book page omitted final source/dependency identity: %#v", page)
		}
	}
	if result.Response.MainFile != "03-啊啊啊.md" || result.Response.HTML != bundle.Pages[0].Fragment {
		t.Fatalf("legacy compatibility projection was not derived from canonical first page: %#v", result.Response)
	}
}

func TestMarkdownExecutionFailurePreservesBoundedWorkerCode(t *testing.T) {
	result := markdownExecutionFailure("markdown.finalize.failed", "Markdown finalization failed",
		fmt.Errorf("worker: %w", &nodeworker.RemoteError{Code: "markdown.finalize.url_disallowed", Message: "bounded"}))
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "markdown.finalize.url_disallowed" {
		t.Fatalf("worker diagnostic code was lost: %#v", result.Diagnostics)
	}
	if result.Diagnostics[0].Message != "bounded" {
		t.Fatalf("bounded worker diagnostic message was lost: %#v", result.Diagnostics)
	}
	result = markdownExecutionFailure("markdown.finalize.failed", "Markdown finalization failed",
		&nodeworker.RemoteError{Code: "nodeworker.contract.invalid", Message: "bounded"})
	if result.Diagnostics[0].Code != "markdown.finalize.failed" {
		t.Fatalf("non-Markdown worker code escaped: %#v", result.Diagnostics)
	}
	long := strings.Repeat("x", 300) + "\nsource"
	if got := boundedWorkerDiagnosticMessage(long, "fallback"); len([]rune(got)) != 256 || strings.Contains(got, "\n") {
		t.Fatalf("worker diagnostic message was not bounded: %q", got)
	}
}

func TestMarkdownExecutionFailureExposesContentFreeBookCacheContract(t *testing.T) {
	result := markdownExecutionFailure("markdown.finalize.failed", "Markdown finalization failed",
		fmt.Errorf("Markdown Book global metadata hash changed during finalization"))
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "markdown.book_cache.failed" ||
		result.Diagnostics[0].Message != "Markdown Book global metadata hash changed during finalization" {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
}

func TestMarkdownExecutionFailureExposesFinalBundleContract(t *testing.T) {
	result := markdownExecutionFailure("markdown.finalize.failed", "Markdown finalization failed",
		fmt.Errorf("validate final Markdown bundle: page 1: invalid dependency hash"))
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "markdown.bundle_contract.failed" ||
		result.Diagnostics[0].Message != "validate final Markdown bundle: page 1: invalid dependency hash" {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	result = markdownExecutionFailure("markdown.finalize.failed", "Markdown finalization failed",
		fmt.Errorf("finalize Markdown: decode worker response: invalid character"))
	if result.Diagnostics[0].Code != "markdown.worker_contract.failed" ||
		result.Diagnostics[0].Message != "finalize Markdown: decode worker response: invalid character" {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
	result = markdownExecutionFailure("markdown.finalize.failed", "Markdown finalization failed",
		fmt.Errorf("Markdown finalizer returned a non-publishable result"))
	if result.Diagnostics[0].Code != "markdown.worker_contract.failed" {
		t.Fatalf("diagnostics = %#v", result.Diagnostics)
	}
}

func TestMarkdownWorkerEnvironmentMatchesSupervisorLimits(t *testing.T) {
	environment := markdownWorkerEnvironment(Config{
		MarkdownMaxRequestBytes:  32 << 20,
		MarkdownMaxResponseBytes: 64 << 20,
	})
	if environment["RIN_NODE_WORKER_MAX_REQUEST_BYTES"] != "33554432" ||
		environment["RIN_NODE_WORKER_MAX_RESPONSE_BYTES"] != "67108864" {
		t.Fatalf("environment = %#v", environment)
	}
}

func markdownExecutorArchive(t *testing.T, name string, source string) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	file, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte(source))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func markdownExecutorFiles(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write([]byte(files[name]))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func containsExact(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsBlockKind(blocks []contracts.DocumentBlock, target contracts.DocumentBlockKind) bool {
	for _, block := range blocks {
		if block.Kind == target {
			return true
		}
	}
	return false
}
