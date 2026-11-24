package publicationpdf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
	"github.com/rinspacehq/rinspace-renderer/api/internal/pdfinspect"
	"github.com/rinspacehq/rinspace-renderer/api/internal/processcontrol"
)

const (
	MaxPrintInputBytes  = 64 << 20
	maxPDFBytes         = 32 << 20
	publicationPrintUID = 65532
)

type BrowserPrinter struct {
	Node     string
	Script   string
	Chromium string
	Isolate  bool
}

func (printer BrowserPrinter) Validate() error {
	if !filepath.IsAbs(printer.Node) || !filepath.IsAbs(printer.Script) || !filepath.IsAbs(printer.Chromium) {
		return errors.New("Markdown PDF toolchain requires absolute executable and script paths")
	}
	for _, path := range []string{printer.Node, printer.Chromium, printer.Script} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || (path != printer.Script && info.Mode().Perm()&0111 == 0) {
			return errors.New("Markdown PDF toolchain file is unavailable")
		}
	}
	return nil
}

// CheckIsolation probes the production sandbox before enabling the print route.
func (printer BrowserPrinter) CheckIsolation(ctx context.Context) error {
	if !printer.Isolate {
		return errors.New("Markdown PDF publication requires a network namespace")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	probe := publicationPrintCommand("/usr/bin/true")
	probe.Env = []string{"LANG=C.UTF-8"}
	if err := processcontrol.Run(ctx, probe, time.Second); err != nil {
		return fmt.Errorf("start unprivileged Markdown PDF network namespace: %w", err)
	}
	return nil
}

func publicationPrintCommand(program string, args ...string) *exec.Cmd {
	flags := []string{"--no-new-privs"}
	if os.Geteuid() == 0 {
		id := strconv.Itoa(publicationPrintUID)
		flags = append(flags, "--reuid="+id, "--regid="+id, "--clear-groups",
			"--inh-caps=-all", "--ambient-caps=-all", "--bounding-set=-all")
	}
	flags = append(flags, "--", "/usr/bin/unshare", "--user", "--map-current-user", "--net", "--", program)
	return exec.Command("/usr/bin/setpriv", append(flags, args...)...)
}

// Print renders a final Markdown bundle using the configured local browser
// toolchain. The caller supplies only verified assets and owns job admission.
func (printer BrowserPrinter) Print(ctx context.Context, bundle contracts.DocumentBundle, book bool, resolveImage ResolveImage) ([]byte, pdfinspect.Result, error) {
	empty := pdfinspect.Result{}
	if err := printer.Validate(); err != nil {
		return nil, empty, err
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
	args := []string{printer.Script, printer.Chromium, input}
	var cmd *exec.Cmd
	if printer.Isolate {
		if os.Geteuid() == 0 {
			for _, path := range []string{input, dir} {
				if err := os.Chown(path, publicationPrintUID, publicationPrintUID); err != nil {
					return nil, empty, fmt.Errorf("prepare unprivileged Markdown PDF input: %w", err)
				}
			}
		}
		cmd = publicationPrintCommand(printer.Node, args...)
	} else {
		cmd = exec.Command(printer.Node, args...)
	}
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
