package publicationpdf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfinspect"
	"github.com/rinspacehq/rinspace-renderer/api/internal/processcontrol"
)

const (
	MaxPrintInputBytes = 64 << 20
	maxPDFBytes        = 32 << 20
)

type BrowserPrinter struct {
	Node     string
	Script   string
	Chromium string
}

// Print renders a final Markdown bundle using the configured local browser
// toolchain. The caller supplies only verified assets and owns job admission.
func (printer BrowserPrinter) Print(ctx context.Context, bundle contracts.DocumentBundle, book bool, resolveImage ResolveImage) ([]byte, pdfinspect.Result, error) {
	empty := pdfinspect.Result{}
	if !filepath.IsAbs(printer.Node) || !filepath.IsAbs(printer.Script) || !filepath.IsAbs(printer.Chromium) {
		return nil, empty, errors.New("Markdown PDF toolchain requires absolute executable and script paths")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	html, err := MarkdownHTML(ctx, bundle, book, resolveImage)
	if err != nil {
		return nil, empty, err
	}
	if len(html) > MaxPrintInputBytes {
		return nil, empty, errors.New("Markdown PDF input exceeds 64 MiB")
	}
	dir, err := os.MkdirTemp("", "rin-renderer-print-")
	if err != nil {
		return nil, empty, err
	}
	defer os.RemoveAll(dir)
	input := filepath.Join(dir, "document.html")
	if err := os.WriteFile(input, []byte(html), 0600); err != nil {
		return nil, empty, err
	}
	output, err := os.Create(filepath.Join(dir, "document.pdf"))
	if err != nil {
		return nil, empty, err
	}
	defer output.Close()
	cmd := exec.Command(printer.Node, printer.Script, printer.Chromium, input)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + dir, "TMPDIR=" + dir, "PATH=/usr/bin:/bin", "LANG=C.UTF-8", "TZ=UTC"}
	cmd.Stdout = output
	if err := processcontrol.Run(ctx, cmd, 3*time.Second); err != nil {
		return nil, empty, fmt.Errorf("print Markdown PDF: %w", err)
	}
	body, err := io.ReadAll(io.NewSectionReader(output, 0, maxPDFBytes+1))
	if err != nil {
		return nil, empty, err
	}
	if len(body) > maxPDFBytes {
		return nil, empty, errors.New("Markdown PDF exceeds 32 MiB")
	}
	result, err := pdfinspect.Inspect(body)
	if err != nil {
		return nil, empty, fmt.Errorf("inspect printed Markdown PDF: %w", err)
	}
	return body, result, nil
}
