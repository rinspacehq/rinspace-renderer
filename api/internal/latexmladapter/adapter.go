package latexmladapter

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/processcontrol"
	"github.com/rinspacehq/rinspace-renderer/api/internal/projectcore"
)

const DefaultMaxHTMLBytes = int64(32 << 20)

//go:embed bindings/ctexart.cls.ltxml
var ctexArticleBinding string

var (
	ErrUnavailable  = errors.New("latexml adapter unavailable")
	ErrRenderFailed = errors.New("latexml render failed")
)

type Config struct {
	Binary   string
	Endpoint string
	Token    string
	Profile  string
	// IncludeStyles passes --includestyles so LaTeXML reads the project's own
	// *.sty files. Without it LaTeXML only accepts its bundled bindings and
	// silently drops every declaration a work makes in its own style file.
	IncludeStyles bool
	Timeout       time.Duration
	MaxHTMLBytes  int64
}

type Adapter struct {
	Config     Config
	HTTPClient *http.Client
}

type Request struct {
	Files    []projectcore.File
	Manifest projectcore.Manifest
	Title    string
}

type Result struct {
	HTML        string
	Diagnostics []Diagnostic
}

type Diagnostic struct {
	Severity string            `json:"severity"`
	Code     string            `json:"code"`
	Message  string            `json:"message"`
	Source   map[string]string `json:"source,omitempty"`
}

type workerRenderRequest struct {
	Files        []projectcore.File   `json:"files"`
	Manifest     projectcore.Manifest `json:"manifest"`
	Title        string               `json:"title,omitempty"`
	MainFile     string               `json:"mainFile"`
	Profile      string               `json:"profile,omitempty"`
	MaxHTMLBytes int64                `json:"maxHtmlBytes,omitempty"`
}

type workerRenderResponse struct {
	HTML        string       `json:"html"`
	Error       string       `json:"error,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

func (a Adapter) Render(ctx context.Context, req Request) (Result, error) {
	cfg := a.normalizedConfig()
	mainFile, ok := projectcore.CleanProjectPath(req.Manifest.MainFile)
	if !ok || mainFile == "" {
		return failure("latexml.mainfile.invalid", "LaTeXML render requires a valid mainFile", nil)
	}
	mainFile = resolveLateXMLMainFile(req.Files, mainFile)
	if idx := findMainFileIndex(req.Files, mainFile); idx >= 0 {
		mainFile = filepath.ToSlash(req.Files[idx].Path)
	}
	req.Manifest.MainFile = mainFile
	normalizedFiles, err := normalizeXeCJKSources(req.Files, mainFile)
	if err != nil {
		return failure("latexml.workspace.write_failed", "failed to prepare LaTeXML workspace files", err)
	}
	req.Files = normalizedFiles
	if cfg.Endpoint != "" {
		return a.renderWorker(ctx, cfg, req, mainFile)
	}
	return a.renderCLI(ctx, cfg, req, mainFile)
}

func (a Adapter) renderCLI(ctx context.Context, cfg Config, req Request, mainFile string) (Result, error) {
	binary, err := resolveBinary(cfg.Binary)
	if err != nil {
		return Result{Diagnostics: []Diagnostic{{
			Severity: "error",
			Code:     "latexml.unavailable",
			Message:  err.Error(),
		}}}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	workDir, err := os.MkdirTemp("", "rin-renderer-latexml-*")
	if err != nil {
		return failure("latexml.workspace.create_failed", "failed to create LaTeXML workspace", err)
	}
	keepWorkDir := false
	defer func() {
		if !keepWorkDir {
			_ = os.RemoveAll(workDir)
		}
	}()

	if err := writeProjectFiles(workDir, req.Files); err != nil {
		return failure("latexml.workspace.write_failed", "failed to write LaTeXML workspace", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, filepath.FromSlash(mainFile))); err != nil {
		return failure("latexml.mainfile.missing", fmt.Sprintf("mainFile is not available in workspace: %s", mainFile), err)
	}

	outputDir := filepath.Join(workDir, ".rin-renderer")
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return failure("latexml.workspace.output_failed", "failed to create LaTeXML output directory", err)
	}
	bindingsDir := filepath.Join(outputDir, "bindings")
	if err := os.MkdirAll(bindingsDir, 0o755); err != nil {
		return failure("latexml.workspace.output_failed", "failed to create LaTeXML bindings directory", err)
	}
	if err := os.WriteFile(filepath.Join(bindingsDir, "ctexart.cls.ltxml"), []byte(ctexArticleBinding), 0o644); err != nil {
		return failure("latexml.workspace.output_failed", "failed to write LaTeXML class binding", err)
	}
	outputPath := filepath.Join(outputDir, "out.html")
	logPath := filepath.Join(outputDir, "latexml.log")

	runCtx := ctx
	cancel := func() {}
	if cfg.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, cfg.Timeout)
	}
	defer cancel()

	args := latexmlArgs(cfg, outputPath, logPath, mainFile)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.Command(binary, args...)
	cmd.Dir = workDir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = processcontrol.Run(runCtx, cmd, processcontrol.DefaultGrace)
	logs := readRunLogs(logPath, stdout.String(), stderr.String())
	diagnostics := diagnosticsFromLogs(logs)
	if err != nil {
		diagnostics = appendMissingDiagnostics(diagnostics, failureDiagnosticsFromLogs(logs)...)
		code := "latexml.failed"
		message := fmt.Sprintf("LaTeXML command failed: %s", err.Error())
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			code = "latexml.timeout"
			message = fmt.Sprintf("LaTeXML command exceeded timeout: %s", cfg.Timeout)
		}
		if truthyEnv("RIN_RENDERER_KEEP_FAILED_LATEXML_WORKDIR") {
			keepWorkDir = true
			diagnostics = appendDiagnostic(diagnostics, "warning", "latexml.workspace.preserved", "LaTeXML workspace preserved after failure", map[string]string{
				"path": workDir,
			})
		}
		diagnostics = appendDiagnostic(diagnostics, "error", code, message, nil)
		return Result{Diagnostics: diagnostics}, fmt.Errorf("%w: %s", ErrRenderFailed, message)
	}

	rawHTML, err := readLimitedFile(outputPath, cfg.MaxHTMLBytes)
	if err != nil {
		diagnostics = appendDiagnostic(diagnostics, "error", "latexml.output.read_failed", "failed to read LaTeXML HTML output", nil)
		return Result{Diagnostics: diagnostics}, fmt.Errorf("%w: %w", ErrRenderFailed, err)
	}
	html := extractHTMLBodyFragment(string(rawHTML))
	if strings.TrimSpace(html) == "" {
		diagnostics = appendDiagnostic(diagnostics, "error", "latexml.output.empty", "LaTeXML returned empty HTML", nil)
		return Result{Diagnostics: diagnostics}, fmt.Errorf("%w: empty HTML", ErrRenderFailed)
	}
	return Result{HTML: html, Diagnostics: diagnostics}, nil
}

func (a Adapter) renderWorker(ctx context.Context, cfg Config, req Request, mainFile string) (Result, error) {
	payload := workerRenderRequest{
		Files:        req.Files,
		Manifest:     req.Manifest,
		Title:        req.Title,
		MainFile:     mainFile,
		Profile:      cfg.Profile,
		MaxHTMLBytes: cfg.MaxHTMLBytes,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return failure("latexml.worker.request_failed", "failed to encode LaTeXML worker request", err)
	}

	runCtx := ctx
	cancel := func() {}
	if cfg.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, cfg.Timeout)
	}
	defer cancel()

	httpReq, err := http.NewRequestWithContext(runCtx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return failure("latexml.worker.request_failed", "failed to create LaTeXML worker request", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if cfg.Token != "" {
		httpReq.Header.Set("X-Rin-Renderer-Token", cfg.Token)
		httpReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	}

	resp, err := a.httpClient().Do(httpReq)
	if err != nil {
		code := "latexml.unavailable"
		message := "LaTeXML worker is unavailable"
		wrapped := ErrUnavailable
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			code = "latexml.timeout"
			message = fmt.Sprintf("LaTeXML worker exceeded timeout: %s", cfg.Timeout)
			wrapped = ErrRenderFailed
		}
		diagnostics := []Diagnostic{{
			Severity: "error",
			Code:     code,
			Message:  fmt.Sprintf("%s: %s", message, err.Error()),
			Source: map[string]string{
				"endpoint": cfg.Endpoint,
			},
		}}
		return Result{Diagnostics: diagnostics}, fmt.Errorf("%w: %s", wrapped, diagnostics[0].Message)
	}
	defer resp.Body.Close()

	responseLimit := cfg.MaxHTMLBytes + int64(1<<20)
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, responseLimit+1))
	if err != nil {
		return failure("latexml.worker.response_failed", "failed to read LaTeXML worker response", err)
	}
	if int64(len(respBody)) > responseLimit {
		diagnostics := []Diagnostic{{
			Severity: "error",
			Code:     "latexml.output.read_failed",
			Message:  fmt.Sprintf("LaTeXML worker response exceeds %d bytes", responseLimit),
			Source: map[string]string{
				"endpoint": cfg.Endpoint,
			},
		}}
		return Result{Diagnostics: diagnostics}, fmt.Errorf("%w: %s", ErrRenderFailed, diagnostics[0].Message)
	}

	var rendered workerRenderResponse
	if err := json.Unmarshal(respBody, &rendered); err != nil {
		diagnostics := []Diagnostic{{
			Severity: "error",
			Code:     "latexml.worker.invalid_response",
			Message:  fmt.Sprintf("LaTeXML worker returned invalid JSON: %s", err.Error()),
			Source: map[string]string{
				"endpoint": cfg.Endpoint,
			},
		}}
		return Result{Diagnostics: diagnostics}, fmt.Errorf("%w: %s", ErrRenderFailed, diagnostics[0].Message)
	}
	diagnostics := normalizeWorkerDiagnostics(rendered.Diagnostics, cfg.Endpoint)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code, wrapped := workerHTTPFailure(resp.StatusCode)
		message := firstNonEmpty(rendered.Error, strings.TrimSpace(string(respBody)), resp.Status)
		diagnostics = appendDiagnostic(diagnostics, "error", code, message, map[string]string{
			"endpoint": cfg.Endpoint,
			"status":   resp.Status,
		})
		return Result{Diagnostics: diagnostics}, fmt.Errorf("%w: LaTeXML worker failed: %s", wrapped, message)
	}

	if int64(len(rendered.HTML)) > cfg.MaxHTMLBytes {
		diagnostics = appendDiagnostic(diagnostics, "error", "latexml.output.read_failed", fmt.Sprintf("LaTeXML worker HTML output exceeds %d bytes", cfg.MaxHTMLBytes), map[string]string{
			"endpoint": cfg.Endpoint,
		})
		return Result{Diagnostics: diagnostics}, fmt.Errorf("%w: HTML output too large", ErrRenderFailed)
	}
	html := extractHTMLBodyFragment(rendered.HTML)
	if strings.TrimSpace(html) == "" {
		diagnostics = appendDiagnostic(diagnostics, "error", "latexml.output.empty", "LaTeXML worker returned empty HTML", map[string]string{
			"endpoint": cfg.Endpoint,
		})
		return Result{Diagnostics: diagnostics}, fmt.Errorf("%w: empty HTML", ErrRenderFailed)
	}
	return Result{HTML: html, Diagnostics: diagnostics}, nil
}

func (a Adapter) normalizedConfig() Config {
	cfg := a.Config
	cfg.Binary = strings.TrimSpace(cfg.Binary)
	if cfg.Binary == "" {
		cfg.Binary = "latexmlc"
	}
	cfg.Endpoint = strings.TrimSpace(cfg.Endpoint)
	cfg.Token = strings.TrimSpace(cfg.Token)
	cfg.Profile = strings.TrimSpace(cfg.Profile)
	if cfg.MaxHTMLBytes <= 0 {
		cfg.MaxHTMLBytes = DefaultMaxHTMLBytes
	}
	return cfg
}

func (a Adapter) httpClient() *http.Client {
	if a.HTTPClient != nil {
		return a.HTTPClient
	}
	return http.DefaultClient
}

func normalizeWorkerDiagnostics(items []Diagnostic, endpoint string) []Diagnostic {
	if len(items) == 0 {
		return nil
	}
	diagnostics := make([]Diagnostic, 0, len(items))
	for _, item := range items {
		item.Severity = firstNonEmpty(item.Severity, "info")
		item.Code = firstNonEmpty(item.Code, "latexml.message")
		item.Message = strings.TrimSpace(item.Message)
		if item.Message == "" {
			continue
		}
		if len(item.Message) > 1200 {
			item.Message = item.Message[:1200] + "..."
		}
		if item.Source == nil {
			item.Source = map[string]string{}
		}
		if endpoint != "" && item.Source["endpoint"] == "" {
			item.Source["endpoint"] = endpoint
		}
		diagnostics = append(diagnostics, item)
	}
	return diagnostics
}

func workerHTTPFailure(statusCode int) (string, error) {
	switch statusCode {
	case http.StatusRequestTimeout, http.StatusGatewayTimeout:
		return "latexml.timeout", ErrRenderFailed
	case http.StatusServiceUnavailable:
		return "latexml.unavailable", ErrUnavailable
	default:
		return "latexml.failed", ErrRenderFailed
	}
}

func resolveBinary(binary string) (string, error) {
	binary = strings.TrimSpace(binary)
	if binary == "" {
		binary = "latexmlc"
	}
	if strings.Contains(binary, "/") {
		info, err := os.Stat(binary)
		if err != nil {
			return "", fmt.Errorf("RIN_RENDERER_LATEXML_BIN is not available: %s", binary)
		}
		if info.IsDir() {
			return "", fmt.Errorf("RIN_RENDERER_LATEXML_BIN points to a directory: %s", binary)
		}
		return binary, nil
	}
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("RIN_RENDERER_LATEXML_BIN is not available on PATH: %s", binary)
	}
	return path, nil
}

func latexmlArgs(cfg Config, outputPath string, logPath string, mainFile string) []string {
	args := []string{
		"--expire=-1",
		// LaTeX permits kernel commands such as AddToHook before documentclass.
		// Load the engine's own kernel binding before it starts reading the source.
		"--preload=LaTeX.pool",
		"--path=" + filepath.Join(filepath.Dir(outputPath), "bindings"),
		"--format=html5",
		"--destination=" + outputPath,
		"--log=" + logPath,
	}
	if cfg.IncludeStyles {
		args = append(args, "--includestyles")
	}
	if cfg.Timeout > 0 {
		args = append(args, fmt.Sprintf("--timeout=%d", timeoutSeconds(cfg.Timeout)))
	}
	if cfg.Profile != "" {
		args = append(args, "--profile="+cfg.Profile)
	}
	return append(args, filepath.ToSlash(mainFile))
}

func timeoutSeconds(timeout time.Duration) int64 {
	if timeout <= 0 {
		return 0
	}
	return int64((timeout + time.Second - 1) / time.Second)
}

func writeProjectFiles(root string, files []projectcore.File) error {
	for _, file := range files {
		cleaned, ok := projectcore.CleanProjectPath(file.Path)
		if !ok {
			return fmt.Errorf("unsafe project path: %s", file.Path)
		}
		data := []byte(file.Body)
		if strings.EqualFold(strings.TrimSpace(file.Encoding), "base64") {
			decoded, err := base64.StdEncoding.DecodeString(file.Body)
			if err != nil {
				return fmt.Errorf("decode %s: %w", cleaned, err)
			}
			data = decoded
		}
		target := filepath.Join(root, filepath.FromSlash(cleaned))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

var (
	xecjkCommandPattern         = regexp.MustCompile(`(?i)\\(?:setCJK|CJKset|CJKfamily|CJKfontspec|CJKglue|begin\{CJK|end\{CJK|addCJK|addcjk|newcjk|CJKencoding)`)
	lateXMLCompatPackagePattern = regexp.MustCompile(`(?im)(?i)^([ \t]*)(\\(?:usepackage|requirepackage)\*?)(\s*\[[^\]]*\])?\s*\{([^}]*)\}(.*)$`)
	elegantClassPattern         = regexp.MustCompile(`(?im)^([ \t]*)(\\documentclass)(\s*\[[^\]]*\])?\s*\{([^}]+)\}(.*)$`)
	elegantTheoremPattern       = regexp.MustCompile(`(?im)^([ \t]*)(\\elegantnewtheorem)\s*\{([^}]*)\}\s*\{([^}]*)\}\s*\{[^}]*\}(?:\s*\{[^}]*\})?(.*)$`)
	elegantBookUsagePattern     = regexp.MustCompile(`(?im)\\(?:elegantnewtheorem|cover|extrainfo|theoremstyle|newtheorem|protect(?:Proposition|Claim|Conjecture|Corollary|Definition|Example|Problem|Remark|Solution|Theorem))`)
	documentClassPattern        = regexp.MustCompile(`(?im)^[ \t]*\\documentclass\b`)
)

const xecjkCompatibilitySentinel = "% RIN_RENDERER_XECJK_COMPAT"
const xecjkCompatibilityShim = xecjkCompatibilitySentinel + `
\scrollmode
\makeatletter
\ifx\rinpc@ifnextchar\undefined
\def\rinpc@ifnextchar#1#2#3{%
\def\rinpc@target{#1}%
\def\rinpc@then{#2}%
\def\rinpc@else{#3}%
\futurelet\rinpc@tmp\rinpc@ifnextchar@
}
\def\rinpc@ifnextchar@{%
\ifx\rinpc@tmp\rinpc@target \let\rinpc@next\rinpc@then \else \let\rinpc@next\rinpc@else \fi
\rinpc@next
}
\fi
\ifx\setCJKmainfont\undefined
\def\rinpc@setCJKmainfont@with[#1]#2{}
\def\rinpc@setCJKmainfont@without#1{}
\def\setCJKmainfont{\rinpc@ifnextchar[{ \rinpc@setCJKmainfont@with}{\rinpc@setCJKmainfont@without}}
\fi
\ifx\setCJKsansfont\undefined
\def\rinpc@setCJKsansfont@with[#1]#2{}
\def\rinpc@setCJKsansfont@without#1{}
\def\setCJKsansfont{\rinpc@ifnextchar[{ \rinpc@setCJKsansfont@with}{\rinpc@setCJKsansfont@without}}
\fi
\ifx\setCJKmonofont\undefined
\def\rinpc@setCJKmonofont@with[#1]#2{}
\def\rinpc@setCJKmonofont@without#1{}
\def\setCJKmonofont{\rinpc@ifnextchar[{ \rinpc@setCJKmonofont@with}{\rinpc@setCJKmonofont@without}}
\fi
\ifx\setCJKromanfont\undefined
\def\rinpc@setCJKromanfont@with[#1]#2{}
\def\rinpc@setCJKromanfont@without#1{}
\def\setCJKromanfont{\rinpc@ifnextchar[{ \rinpc@setCJKromanfont@with}{\rinpc@setCJKromanfont@without}}
\fi
\ifx\setmainfont\undefined
\def\rinpc@setmainfont@with[#1]#2{}
\def\rinpc@setmainfont@without#1{}
\def\setmainfont{\rinpc@ifnextchar[{ \rinpc@setmainfont@with}{\rinpc@setmainfont@without}}
\fi
\ifx\setCJKfamilyfont\undefined
\def\rinpc@setCJKfamilyfont@with[#1]#2{}
\def\rinpc@setCJKfamilyfont@without#1{}
\def\setCJKfamilyfont{\rinpc@ifnextchar[{ \rinpc@setCJKfamilyfont@with}{\rinpc@setCJKfamilyfont@without}}
\fi
\ifx\CJKfontspec\undefined
\def\rinpc@CJKfontspec@with[#1]#2{}
\def\rinpc@CJKfontspec@without#1{}
\def\CJKfontspec{\rinpc@ifnextchar[{ \rinpc@CJKfontspec@with}{\rinpc@CJKfontspec@without}}
\fi
\ifx\CJKfamily\undefined
\def\CJKfamily#1{}
\fi
\ifx\CJKsetecglue\undefined
\def\CJKsetecglue#1{}
\fi
\ifx\CJKsetcharclass\undefined
\def\CJKsetcharclass#1#2#3{}
\fi
\ifx\addCJKfontfeatures\undefined
\def\addCJKfontfeatures#1{}
\fi
\ifx\xeCJKsetup\undefined
\def\xeCJKsetup#1{}
\fi
\ifx\CJKglue\undefined
\def\CJKglue{}
\fi
\ifx\CJKunderdot\undefined
\def\CJKunderdot#1{}
\fi
\ifx\CJKencoding\undefined
\def\CJKencoding{}
\fi
\ifx\songti\undefined
\def\songti{}
\fi
\ifx\fangsong\undefined
\def\fangsong{}
\fi
\ifx\heiti\undefined
\def\heiti{}
\fi
\ifx\setlength\undefined
\def\setlength#1#2{}
\fi
\ifx\coloneqq\undefined
\def\coloneqq{:=}
\fi
\ifx\undersign\undefined
\def\undersign{\par\begin{flushright}First Author \& Second Author\end{flushright}\par}
\fi
\makeatother
\ifx\CJK\undefined
\newenvironment{CJK}[2]{}
{}
\fi
\ifx\csname CJK*\endcsname\undefined
\expandafter\def\csname CJK*\endcsname#1#2{}
\expandafter\def\csname endCJK*\endcsname{}
\fi
\ifx\tblr\undefined
\newenvironment{tblr}[1]{\begin{tabular}{llllllllllll}}{\end{tabular}}
\fi
\ifx\longtblr\undefined
\newenvironment{longtblr}[1]{\begin{tabular}{llllllllllll}}{\end{tabular}}
\fi
\ifx\algorithm\undefined
\newenvironment{algorithm}[1][]{\par\noindent}{\par}
\fi
\ifx\KwData\undefined
\def\KwData#1{\par\noindent\textbf{Data: }#1\par}
\fi
\ifx\KwResult\undefined
\def\KwResult#1{\par\noindent\textbf{Result: }#1\par}
\fi
\ifx\KwIn\undefined
\def\KwIn#1{\par\noindent\textbf{Input: }#1\par}
\fi
\ifx\KwOut\undefined
\def\KwOut#1{\par\noindent\textbf{Output: }#1\par}
\fi
\ifx\SetKw\undefined
\def\SetKw#1#2{}
\fi
\ifx\SetKwInput\undefined
\def\SetKwInput#1#2{}
\fi
\ifx\DontPrintSemicolon\undefined
\def\DontPrintSemicolon{}
\fi
`
const elegantBookCompatibilitySentinel = "% RIN_RENDERER_ELEGANTBOOK_COMPAT"
const elegantBookCompatibilityShim = elegantBookCompatibilitySentinel + `
\makeatletter
\ifx\rinpc@ifnextchar\undefined
\def\rinpc@ifnextchar#1#2#3{%
\def\rinpc@target{#1}%
\def\rinpc@then{#2}%
\def\rinpc@else{#3}%
\futurelet\rinpc@tmp\rinpc@ifnextchar@
}
\def\rinpc@ifnextchar@{%
\ifx\rinpc@tmp\rinpc@target \let\rinpc@next\rinpc@then \else \let\rinpc@next\rinpc@else \fi
\rinpc@next
}
\fi
\def\rinpc@optionaltitle#1{%
\def\rinpc@tmp{#1}%
\ifx\rinpc@tmp\empty\else\space(#1)\fi
}
\ifcsname c@rinpc@theorem\endcsname\else
\ifcsname c@chapter\endcsname
\newcounter{rinpc@theorem}[chapter]
\def\therinpc@theorem{\thechapter.\arabic{rinpc@theorem}}
\else
\newcounter{rinpc@theorem}
\def\therinpc@theorem{\arabic{rinpc@theorem}}
\fi
\fi
\def\rinpc@define@theorem#1#2{%
\ifcsname #1\endcsname\else
\newenvironment{#1}[1][]{\par\refstepcounter{rinpc@theorem}\noindent\textbf{#2~\therinpc@theorem\rinpc@optionaltitle{##1}}\quad}{\par}%
\fi
\ifcsname #1*\endcsname\else
\newenvironment{#1*}[1][]{\par\noindent\textbf{#2\rinpc@optionaltitle{##1}}\quad}{\par}%
\fi
}
\def\rinpc@define@simpleenv#1#2{%
\ifcsname #1\endcsname\else
\newenvironment{#1}[1][]{\par\noindent\textbf{#2\rinpc@optionaltitle{##1}}\quad}{\par}%
\fi
}
\ifx\theoremstyle\undefined
\def\theoremstyle#1{}
\fi
\ifx\setlength\undefined
\def\setlength#1#2{}
\fi
\ifx\newtheoremstyle\undefined
\def\newtheoremstyle#1#2#3#4#5#6#7#8#9{}
\fi
\ifx\newtheorem\undefined
\def\newtheorem{\rinpc@ifnextchar*{\rinpc@newtheorem@star}{\rinpc@newtheorem@normal}}
\def\rinpc@newtheorem@star*#1#2{\rinpc@define@simpleenv{#1}{#2}}
\def\rinpc@newtheorem@normal#1{\rinpc@ifnextchar[{\rinpc@newtheorem@shared{#1}}{\rinpc@newtheorem@plain{#1}}}
\def\rinpc@newtheorem@shared#1[#2]#3{\rinpc@define@theorem{#1}{#3}}
\def\rinpc@newtheorem@plain#1#2{\rinpc@define@theorem{#1}{#2}\rinpc@ifnextchar[{\rinpc@newtheorem@trail}{\relax}}
\def\rinpc@newtheorem@trail[#1]{}
\fi
\ifx\elegantnewtheorem\undefined
\def\elegantnewtheorem#1#2#3#4{\newtheorem{#1}{#2}}
\fi
\ifx\cover\undefined
\def\cover#1{}
\fi
\ifx\extrainfo\undefined
\def\extrainfo#1{}
\fi
\ifx\propositionname\undefined
\def\propositionname{Proposition}
\fi
\ifx\claimname\undefined
\def\claimname{Claim}
\fi
\ifx\conjecturename\undefined
\def\conjecturename{Conjecture}
\fi
\ifx\corollaryname\undefined
\def\corollaryname{Corollary}
\fi
\ifx\postulatename\undefined
\def\postulatename{Postulate}
\fi
\ifx\axiomname\undefined
\def\axiomname{Axiom}
\fi
\ifx\lemmaname\undefined
\def\lemmaname{Lemma}
\fi
\ifx\definitionname\undefined
\def\definitionname{Definition}
\fi
\ifx\notename\undefined
\def\notename{Note}
\fi
\ifx\examplename\undefined
\def\examplename{Example}
\fi
\ifx\exercisename\undefined
\def\exercisename{Exercise}
\fi
\ifx\problemname\undefined
\def\problemname{Problem}
\fi
\ifx\remarkname\undefined
\def\remarkname{Remark}
\fi
\ifx\solutionname\undefined
\def\solutionname{Solution}
\fi
\ifx\proofname\undefined
\def\proofname{Proof}
\fi
\ifx\assumptionname\undefined
\def\assumptionname{Assumption}
\fi
\ifx\conclusionname\undefined
\def\conclusionname{Conclusion}
\fi
\ifx\propertyname\undefined
\def\propertyname{Property}
\fi
\ifx\introductionname\undefined
\def\introductionname{Introduction}
\fi
\ifx\theoremname\undefined
\def\theoremname{Theorem}
\fi
\ifx\protectProposition\undefined
\def\protectProposition{\propositionname}
\fi
\ifx\protectClaim\undefined
\def\protectClaim{\claimname}
\fi
\ifx\protectConjecture\undefined
\def\protectConjecture{\conjecturename}
\fi
\ifx\protectCorollary\undefined
\def\protectCorollary{\corollaryname}
\fi
\ifx\protectDefinition\undefined
\def\protectDefinition{\definitionname}
\fi
\ifx\protectExample\undefined
\def\protectExample{\examplename}
\fi
\ifx\protectProblem\undefined
\def\protectProblem{\problemname}
\fi
\ifx\protectRemark\undefined
\def\protectRemark{\remarkname}
\fi
\ifx\protectSolution\undefined
\def\protectSolution{\solutionname}
\fi
\ifx\protectTheorem\undefined
\def\protectTheorem{\theoremname}
\fi
\ifx\undefined\booklanguage
\def\booklanguage{}
\fi
\ifx\xlongequal\undefined
\def\xlongequal{\@ifnextchar[{\rinpc@xlongequal@opt}{\rinpc@xlongequal@plain}}
\def\rinpc@xlongequal@opt[#1]#2{\mathrel{\mathop{=}\limits^{#2}_{#1}}}
\def\rinpc@xlongequal@plain#1{\mathrel{\mathop{=}\limits^{#1}}}
\fi
\ifx\xlongrightarrow\undefined
\def\xlongrightarrow{\@ifnextchar[{\rinpc@xlongrightarrow@opt}{\rinpc@xlongrightarrow@plain}}
\def\rinpc@xlongrightarrow@opt[#1]#2{\mathrel{\mathop{\longrightarrow}\limits^{#2}_{#1}}}
\def\rinpc@xlongrightarrow@plain#1{\mathrel{\mathop{\longrightarrow}\limits^{#1}}}
\fi
\makeatother
`

const elegantBookEnvironmentCompatibilitySentinel = "% RIN_RENDERER_ELEGANTBOOK_ENV_COMPAT"
const elegantBookEnvironmentCompatibilityShim = elegantBookEnvironmentCompatibilitySentinel + `
\makeatletter
\rinpc@define@theorem{theorem}{\theoremname}
\rinpc@define@theorem{definition}{\definitionname}
\rinpc@define@theorem{postulate}{\postulatename}
\rinpc@define@theorem{axiom}{\axiomname}
\rinpc@define@theorem{corollary}{\corollaryname}
\rinpc@define@theorem{lemma}{\lemmaname}
\rinpc@define@theorem{proposition}{\propositionname}
\rinpc@define@simpleenv{example}{\examplename}
\rinpc@define@simpleenv{exercise}{\exercisename}
\rinpc@define@simpleenv{problem}{\problemname}
\rinpc@define@simpleenv{note}{\notename}
\rinpc@define@simpleenv{proof}{\proofname}
\rinpc@define@simpleenv{solution}{\solutionname}
\rinpc@define@simpleenv{remark}{\remarkname}
\rinpc@define@simpleenv{assumption}{\assumptionname}
\rinpc@define@simpleenv{conclusion}{\conclusionname}
\rinpc@define@simpleenv{property}{\propertyname}
\rinpc@define@simpleenv{introduction}{\introductionname}
\ifcsname custom\endcsname\else
\newenvironment{custom}[1]{\par\noindent\textbf{#1}\quad}{\par}
\fi
\makeatother
`

func normalizeXeCJKSources(files []projectcore.File, mainFile string) ([]projectcore.File, error) {
	mainFile = filepath.ToSlash(mainFile)
	normalized := make([]projectcore.File, len(files))
	mainIndex := findMainFileIndex(files, mainFile)
	needsCompat := false
	needsElegantCompat := false
	needsElegantBookEnvironmentCompat := false

	for i, file := range files {
		normalized[i] = file
		if !shouldNormalizeFileForXeCJK(file.Path, file.Kind) {
			continue
		}

		raw, err := decodeFileBody(file.Body, file.Encoding)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", file.Path, err)
		}
		sourceText := string(raw)
		needsElegantBookEnvironmentCompat = needsElegantBookEnvironmentCompat || usesElegantBookDocumentClass(sourceText)
		nextBody, usedCompat, changed := rewriteXeCJKForLateXML(sourceText)
		nextBody, usedElegantCompat, changedElegant := rewriteElegantBookForLateXML(nextBody)
		if changed || changedElegant {
			normalized[i].Body = encodeFileBody(nextBody, file.Encoding)
			normalized[i].Encoding = file.Encoding
			normalized[i].Bytes = int64(len(nextBody))
		}
		needsCompat = needsCompat || usedCompat
		needsElegantCompat = needsElegantCompat || usedElegantCompat
	}
	if mainIndex < 0 {
		return normalized, nil
	}

	mainFileCopy := normalized[mainIndex]
	mainBody, err := decodeFileBody(mainFileCopy.Body, mainFileCopy.Encoding)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", mainFileCopy.Path, err)
	}
	mainText := string(mainBody)
	compatPrefix := ""
	if needsCompat && !strings.Contains(mainText, xecjkCompatibilitySentinel) {
		compatPrefix = xecjkCompatibilityShim + "\n"
	}
	if needsElegantCompat && !strings.Contains(mainText, elegantBookCompatibilitySentinel) {
		compatPrefix = compatPrefix + elegantBookCompatibilityShim + "\n"
	}
	if needsElegantBookEnvironmentCompat && !strings.Contains(mainText, elegantBookEnvironmentCompatibilitySentinel) {
		compatPrefix = compatPrefix + elegantBookEnvironmentCompatibilityShim + "\n"
	}
	if compatPrefix == "" {
		return normalized, nil
	}
	mainText = injectLateXMLCompatPrefix(mainText, compatPrefix)
	normalized[mainIndex].Body = encodeFileBody(mainText, mainFileCopy.Encoding)
	normalized[mainIndex].Encoding = mainFileCopy.Encoding
	normalized[mainIndex].Bytes = int64(len(mainText))
	return normalized, nil
}

func findMainFileIndex(files []projectcore.File, mainFile string) int {
	mainFile = filepath.ToSlash(strings.TrimSpace(mainFile))
	if mainFile == "" {
		return -1
	}
	for i, file := range files {
		if filepath.ToSlash(file.Path) == mainFile {
			return i
		}
	}

	mainFileBase := filepath.Base(mainFile)
	if mainFileBase == "" || mainFileBase != strings.TrimSpace(mainFile) {
		return -1
	}
	for i, file := range files {
		if filepath.Base(filepath.ToSlash(file.Path)) == mainFileBase {
			return i
		}
	}
	return -1
}

func injectLateXMLCompatPrefix(source string, prefix string) string {
	prefix = strings.TrimSuffix(prefix, "\n")
	lines := strings.Split(strings.TrimPrefix(source, "\ufeff"), "\n")
	for i, line := range lines {
		if !documentClassPattern.MatchString(line) {
			continue
		}
		withPrefix := make([]string, 0, len(lines)+2)
		withPrefix = append(withPrefix, lines[:i+1]...)
		withPrefix = append(withPrefix, prefix)
		withPrefix = append(withPrefix, lines[i+1:]...)
		return strings.Join(withPrefix, "\n")
	}
	if prefix == "" {
		return strings.TrimPrefix(source, "\ufeff")
	}
	return prefix + "\n" + strings.TrimPrefix(source, "\ufeff")
}

func resolveLateXMLMainFile(files []projectcore.File, requested string) string {
	requested = filepath.ToSlash(requested)
	if strings.TrimSpace(requested) == "" {
		return requested
	}
	if !canRewriteMainFileSelection(requested) && filepath.Ext(requested) != ".tex" {
		return requested
	}
	if hasDocumentClassForPath(files, requested) {
		return requested
	}

	documentClassFiles := listDocumentClassFiles(files)
	if len(documentClassFiles) != 1 {
		return requested
	}
	return documentClassFiles[0]
}

func canRewriteMainFileSelection(requested string) bool {
	base := strings.ToLower(filepath.Base(filepath.ToSlash(requested)))
	switch strings.TrimSpace(base) {
	case "main.tex", "paper.tex", "article.tex", "ms.tex":
		return true
	default:
		return false
	}
}

func hasDocumentClassForPath(files []projectcore.File, target string) bool {
	target = filepath.ToSlash(target)
	for _, file := range files {
		if filepath.ToSlash(file.Path) != target {
			continue
		}
		raw, err := decodeFileBody(file.Body, file.Encoding)
		if err != nil {
			return false
		}
		return documentClassPattern.MatchString(string(raw))
	}
	return false
}

func listDocumentClassFiles(files []projectcore.File) []string {
	documentClassFiles := make([]string, 0)
	for _, file := range files {
		if !shouldNormalizeFileForXeCJK(file.Path, file.Kind) {
			continue
		}
		raw, err := decodeFileBody(file.Body, file.Encoding)
		if err != nil {
			continue
		}
		if !documentClassPattern.MatchString(string(raw)) {
			continue
		}
		documentClassFiles = append(documentClassFiles, filepath.ToSlash(file.Path))
	}
	return documentClassFiles
}

func rewriteElegantBookForLateXML(source string) (string, bool, bool) {
	compatUsed := false
	changed := false
	lines := strings.Split(source, "\n")
	for i, line := range lines {
		if elegantBookUsagePattern.MatchString(line) {
			compatUsed = true
		}
		matches := elegantClassPattern.FindStringSubmatch(line)
		if len(matches) == 0 {
			if rewrittenLine, theoremChanged := rewriteElegantTheoremLine(line); theoremChanged {
				lines[i] = rewrittenLine
				compatUsed = true
				changed = true
			}
			continue
		}
		name := strings.TrimSpace(matches[4])
		if !strings.EqualFold(name, "elegantbook") {
			continue
		}
		lines[i] = fmt.Sprintf("%s%s%s{book}%s", matches[1], matches[2], matches[3], matches[5])
		compatUsed = true
		changed = true
	}
	return strings.Join(lines, "\n"), compatUsed, changed
}

func usesElegantBookDocumentClass(source string) bool {
	matches := elegantClassPattern.FindAllStringSubmatch(source, -1)
	for _, match := range matches {
		if len(match) > 4 && strings.EqualFold(strings.TrimSpace(match[4]), "elegantbook") {
			return true
		}
	}
	return false
}

func rewriteElegantTheoremLine(line string) (string, bool) {
	matches := elegantTheoremPattern.FindStringSubmatch(line)
	if len(matches) == 0 {
		return line, false
	}
	return fmt.Sprintf("%s\\newtheorem{%s}{%s}%s", matches[1], strings.TrimSpace(matches[3]), strings.TrimSpace(matches[4]), matches[5]), true
}

func decodeFileBody(body string, encoding string) ([]byte, error) {
	if strings.EqualFold(strings.TrimSpace(encoding), "base64") {
		return base64.StdEncoding.DecodeString(body)
	}
	return []byte(body), nil
}

func encodeFileBody(body string, encoding string) string {
	if strings.EqualFold(strings.TrimSpace(encoding), "base64") {
		return base64.StdEncoding.EncodeToString([]byte(body))
	}
	return body
}

func truthyEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func shouldNormalizeFileForXeCJK(path string, kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "tex":
		return true
	}
	switch filepath.Ext(strings.ToLower(path)) {
	case ".tex", ".sty", ".cls", ".clo", ".ltx", ".bbx", ".cbx":
		return true
	default:
		return false
	}
}

func rewriteXeCJKForLateXML(source string) (string, bool, bool) {
	compatUsed := false
	changed := false
	lines := strings.Split(source, "\n")
	for i, line := range lines {
		nextLine, lineChanged := rewriteXeCJKPackageLine(line)
		if lineChanged {
			changed = true
		}
		line = nextLine
		line = strings.ReplaceAll(line, `\begin{CJK*}`, `\begin{CJK}`)
		line = strings.ReplaceAll(line, `\end{CJK*}`, `\end{CJK}`)
		lines[i] = line
	}
	compatUsed = compatUsed || changed || xecjkCommandPattern.MatchString(source)
	return strings.Join(lines, "\n"), compatUsed, changed
}

func rewriteXeCJKPackageLine(line string) (string, bool) {
	matches := lateXMLCompatPackagePattern.FindStringSubmatch(line)
	if len(matches) == 0 {
		return line, false
	}

	indent := matches[1]
	command := matches[2]
	options := matches[3]
	packages := matches[4]
	rest := matches[5]
	parts := strings.Split(packages, ",")
	next := make([]string, 0, len(parts))
	foundCompat := false
	for _, rawName := range parts {
		name := strings.TrimSpace(rawName)
		if name == "" {
			continue
		}
		if shouldStripLateXMLCompatPackage(name) {
			foundCompat = true
			continue
		}
		next = append(next, name)
	}
	if !foundCompat {
		return line, false
	}
	if len(next) == 0 {
		return indent + "% xeCJK package removed by renderer compatibility shim for latexml", true
	}
	return fmt.Sprintf("%s%s%s{%s}%s", indent, command, options, strings.Join(next, ","), rest), true
}

func shouldStripLateXMLCompatPackage(name string) bool {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return false
	}
	if strings.HasPrefix(name, "xecjk") {
		return true
	}
	switch name {
	case "fontspec", "xunicode", "xparse", "expl3", "luainputenc", "luatex", "luatexja", "luatexja-fontspec", "xetex", "xetex-inputenc", "ctex", "tabularray", "algorithm2e", "mdframed", "extarrows", "cmupint":
		return true
	default:
		return false
	}
}

func readLimitedFile(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, fmt.Errorf("LaTeXML output exceeds %d bytes", maxBytes)
	}
	return body, nil
}

func readRunLogs(logPath string, stdout string, stderr string) string {
	parts := make([]string, 0, 3)
	if strings.TrimSpace(stdout) != "" {
		parts = append(parts, stdout)
	}
	if strings.TrimSpace(stderr) != "" {
		parts = append(parts, stderr)
	}
	if body, err := os.ReadFile(logPath); err == nil && strings.TrimSpace(string(body)) != "" {
		parts = append(parts, string(body))
	}
	return strings.Join(parts, "\n")
}

func diagnosticsFromLogs(logs string) []Diagnostic {
	diagnostics := make([]Diagnostic, 0)
	lines := nonEmptyLogLines(logs)
	// Preserve errors before warnings and progress messages. A long class-load
	// trace must not consume the budget before the first undefined command.
	for _, wanted := range []string{"error", "warning", "info"} {
		for _, line := range lines {
			severity, code := classifyLogLine(line)
			if severity != wanted {
				continue
			}
			diagnostics = appendDiagnostic(diagnostics, severity, code, line, nil)
			if len(diagnostics) >= 50 {
				return diagnostics
			}
		}
	}
	return diagnostics
}

func failureDiagnosticsFromLogs(logs string) []Diagnostic {
	lines := nonEmptyLogLines(logs)
	diagnostics := make([]Diagnostic, 0)
	for _, line := range lines {
		severity, code := classifyLogLine(line)
		if severity != "error" && severity != "warning" {
			continue
		}
		diagnostics = appendDiagnostic(diagnostics, severity, code, line, map[string]string{
			"log": "latexml.log",
		})
		if len(diagnostics) >= 20 {
			break
		}
	}

	tail := lastStrings(lines, 20)
	if len(tail) > 0 {
		diagnostics = appendDiagnostic(diagnostics, "error", "latexml.log.tail", "LaTeXML log tail:\n"+strings.Join(tail, "\n"), map[string]string{
			"log":     "latexml.log",
			"section": "tail",
		})
	}
	return diagnostics
}

func appendMissingDiagnostics(items []Diagnostic, extras ...Diagnostic) []Diagnostic {
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		seen[diagnosticKey(item)] = true
	}
	for _, extra := range extras {
		key := diagnosticKey(extra)
		if seen[key] {
			continue
		}
		seen[key] = true
		items = append(items, extra)
	}
	return items
}

func diagnosticKey(item Diagnostic) string {
	return item.Severity + "\x00" + item.Code + "\x00" + item.Message
}

func nonEmptyLogLines(logs string) []string {
	lines := make([]string, 0)
	scanner := bufio.NewScanner(strings.NewReader(logs))
	scanner.Buffer(make([]byte, 1024), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func lastStrings(items []string, count int) []string {
	if count <= 0 || len(items) == 0 {
		return nil
	}
	if len(items) <= count {
		return append([]string(nil), items...)
	}
	return append([]string(nil), items[len(items)-count:]...)
}

func classifyLogLine(line string) (string, string) {
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "fatal:"):
		return "error", "latexml.fatal"
	case strings.Contains(lower, "error:"):
		return "error", "latexml.error"
	case strings.Contains(lower, "warning:") || strings.Contains(lower, "warn:"):
		return "warning", "latexml.warning"
	case strings.Contains(lower, "info:") || strings.Contains(lower, "note:"):
		return "info", "latexml.info"
	default:
		return "info", "latexml.message"
	}
}

func appendDiagnostic(items []Diagnostic, severity string, code string, message string, source map[string]string) []Diagnostic {
	message = strings.TrimSpace(message)
	if message == "" {
		return items
	}
	if len(message) > 1200 {
		message = message[:1200] + "..."
	}
	return append(items, Diagnostic{
		Severity: firstNonEmpty(severity, "info"),
		Code:     firstNonEmpty(code, "latexml.message"),
		Message:  message,
		Source:   source,
	})
}

func extractHTMLBodyFragment(value string) string {
	html := strings.TrimSpace(value)
	lower := strings.ToLower(html)
	bodyStart := strings.Index(lower, "<body")
	if bodyStart < 0 {
		return html
	}
	openEnd := strings.Index(html[bodyStart:], ">")
	if openEnd < 0 {
		return html
	}
	contentStart := bodyStart + openEnd + 1
	content := html[contentStart:]
	contentLower := strings.ToLower(content)
	bodyEnd := strings.LastIndex(contentLower, "</body>")
	if bodyEnd < 0 {
		return strings.TrimSpace(content)
	}
	return strings.TrimSpace(content[:bodyEnd])
}

func failure(code string, message string, err error) (Result, error) {
	if err != nil {
		message = fmt.Sprintf("%s: %s", message, err.Error())
	}
	diagnostics := []Diagnostic{{
		Severity: "error",
		Code:     code,
		Message:  message,
	}}
	return Result{Diagnostics: diagnostics}, fmt.Errorf("%w: %s", ErrRenderFailed, message)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
