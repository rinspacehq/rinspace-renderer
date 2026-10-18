// typstcheck validates a local project with the same sealed compiler and HTML
// adapter as the service. It never connects to an account or publishes output.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/mathrender"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
	"github.com/rinspacehq/rinspace-renderer/api/internal/typstadapter"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	root := flag.String("root", "", "local project root")
	entry := flag.String("entry", "main.typ", "project entrypoint")
	mode := flag.String("mode", "article", "article or book")
	binary := flag.String("binary", os.Getenv("RIN_TYPST_TEST_BINARY"), "pinned compiler")
	font := flag.String("font", os.Getenv("RIN_TYPST_TEST_FONT"), "pinned font")
	packages := flag.String("package-root", "", "sealed offline package directory")
	packageHash := flag.String("package-hash", "", "exact package snapshot hash")
	pdf := flag.Bool("pdf", false, "also verify PDF compilation")
	mathjax := flag.Bool("mathjax", false, "present MathML using the shared pinned MathJax engine")
	publication := flag.Bool("publication", false, "exercise the actual publication executor and canonical result validation without publishing")
	report := flag.String("report", "", "write an exclusive local JSON evidence file")
	flag.Parse()
	if *root == "" || !filepath.IsAbs(*root) || (*mode != "article" && *mode != "book") {
		return fmt.Errorf("absolute -root and valid -mode are required")
	}
	files := map[string][]byte{}
	var size int64
	if err := filepath.WalkDir(*root, func(location string, item os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if item.IsDir() {
			return nil
		}
		info, err := item.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > 16<<20 || len(files) >= 700 {
			return fmt.Errorf("unsupported project file: %s", item.Name())
		}
		size += info.Size()
		if size > 64<<20 {
			return fmt.Errorf("project too large")
		}
		name, err := filepath.Rel(*root, location)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(name)], err = os.ReadFile(location)
		return err
	}); err != nil {
		return err
	}
	if *publication {
		result, err := checkPublication(files, *entry, *mode, *binary, *font, *packages, *packageHash)
		if err != nil {
			return err
		}
		return writeReport(result, *report)
	}
	compiler := typstadapter.Compiler{BinaryPath: *binary, FontPath: *font,
		PackagePath: *packages, PackageHash: *packageHash,
		BinaryHash: "29273eaa04f6d00edd0c2bec578f565fc9c65be856bfbffc894567c68ed0b237",
		FontHash:   "79c18ebe7b811951e8311bad7103ebeae8c337ed9988ea69e8a78a66cfe029b9",
		Timeout:    5 * time.Minute, MaxOutput: 32 << 20}
	start := time.Now()
	compiled, err := compiler.Compile(context.Background(), files, *entry, "html")
	if err != nil {
		return fmt.Errorf("compile: %w\n%s", err, compiled.Log)
	}
	compileTime := time.Since(start)
	adapted, err := typstadapter.AdaptHTML(compiled.Output, *mode, files)
	if err != nil {
		return fmt.Errorf("adapt: %w", err)
	}
	if *mathjax {
		adapted, err = typstadapter.RenderMathML(context.Background(), adapted,
			mathrender.NewMathJaxRenderer("../engines/mathjax/render-mathjax.mjs", 60*time.Second, 32<<10))
		if err != nil {
			return err
		}
	}
	mathCount := 0
	pages := make([]map[string]any, 0, len(adapted.Pages))
	for _, page := range adapted.Pages {
		mathCount += strings.Count(page.HTML, "<math")
		pages = append(pages, map[string]any{"id": page.ID, "title": page.Title, "level": page.Level, "bytes": len(page.HTML)})
	}
	var pdfBytes int
	if *pdf {
		result, err := compiler.Compile(context.Background(), files, *entry, "pdf")
		if err != nil {
			return fmt.Errorf("PDF compile: %w\n%s", err, result.Log)
		}
		pdfBytes = len(result.Output)
	}
	result := map[string]any{"passed": true, "mode": *mode,
		"adapterVersion": typstadapter.AdapterVersion, "files": len(files), "sourceBytes": size,
		"compileMilliseconds": compileTime.Milliseconds(), "htmlBytes": len(compiled.Output),
		"assets": len(adapted.Assets), "math": mathCount, "pages": pages, "pdfBytes": pdfBytes,
		"mathjax": *mathjax, "totalMilliseconds": time.Since(start).Milliseconds(), "compilerLog": compiled.Log,
		"nativeHTMLComplete": typstadapter.UnsupportedHTML(compiled.Log) == ""}
	return writeReport(result, *report)
}

func writeReport(result map[string]any, report string) error {
	if report != "" {
		file, err := os.OpenFile(report, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := json.NewEncoder(file).Encode(result); err != nil {
			return err
		}
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func checkPublication(files map[string][]byte, entry, mode, binary, font, packages, packageHash string) (map[string]any, error) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		file, err := writer.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := file.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	cfg := renderapi.ConfigFromEnv()
	cfg.TypstBin, cfg.TypstFontPath = binary, font
	cfg.TypstBinHash = "29273eaa04f6d00edd0c2bec578f565fc9c65be856bfbffc894567c68ed0b237"
	cfg.TypstFontHash = "79c18ebe7b811951e8311bad7103ebeae8c337ed9988ea69e8a78a66cfe029b9"
	if packages == "" && packageHash == "" {
		packageHash = typstadapter.EmptyPackageHash
	}
	cfg.TypstPackagePath, cfg.TypstPackageHash = packages, packageHash
	cfg.TypstVersion = "0.15.1"
	cfg.TypstMathRender = true
	cfg.MathParallelism, cfg.MathJaxWarmWorkerCount = 1, 1
	cfg.MathJaxWarmWorkersEnabled = false
	executor := renderapi.NewProjectExecutor(cfg)
	defer executor.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	start := time.Now()
	// A valid synthetic identity exercises the canonical publication contract;
	// it is never submitted to a queue or associated with a remote project.
	result := executor.Execute(ctx, renderapi.ProjectExecutionRequest{ContentKind: "typst", JobID: "job-local-publication-check", RequestID: "request-local-publication-check", ArchiveName: "project.zip", Archive: archive.Bytes(), Title: "Local publication check", ExplicitMainFile: entry, DocumentMode: mode, ProjectID: mode + ":1", SourceCommit: strings.Repeat("a", 40), ControlProjectHash: strings.Repeat("b", 64)})
	if result.Err != nil {
		return nil, fmt.Errorf("publication executor: %w (%s)", result.Err, result.Message)
	}
	if result.Canonical == nil {
		return nil, fmt.Errorf("publication produced no canonical result")
	}
	if err := result.Canonical.Validate(); err != nil {
		return nil, err
	}
	mathCount, pageCount := 0, 0
	if result.Canonical.Inline != nil {
		for _, page := range result.Canonical.Inline.Pages {
			pageCount++
			mathCount += strings.Count(page.Fragment, "<mjx-container")
		}
	}
	return map[string]any{"passed": true, "publicationExecutor": true, "mode": mode, "fallback": result.Response.Fallback, "pages": pageCount, "mathjaxContainers": mathCount, "diagnostics": result.Diagnostics, "profile": renderapi.TypstHTMLProfileID(cfg), "compileTimeoutSeconds": cfg.TypstTimeout.Seconds(), "totalMilliseconds": time.Since(start).Milliseconds()}, nil
}
