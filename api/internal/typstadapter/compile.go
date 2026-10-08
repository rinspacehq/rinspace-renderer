package typstadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/contracts"
)

const (
	maxCompilerInputBytes  = 64 << 20
	maxCompilerOutputBytes = 32 << 20
	maxCompilerLogBytes    = 64 << 10
)

// Compiler is an isolated local executor. The deployment worker must still
// provide a network-disabled container and resource limits before admitting
// jobs; a CLI flag alone cannot guarantee that packages are fetched offline.
type Compiler struct {
	BinaryPath string
	BinaryHash string
	FontPath   string
	FontHash   string
	Timeout    time.Duration
	MaxOutput  int64
}

type CompileResult struct {
	Output []byte
	Log    string
}

func (compiler Compiler) Validate() error {
	if !filepath.IsAbs(compiler.BinaryPath) || !filepath.IsAbs(compiler.FontPath) ||
		compiler.Timeout <= 0 || compiler.Timeout > 5*time.Minute ||
		compiler.MaxOutput <= 0 || compiler.MaxOutput > maxCompilerOutputBytes {
		return errors.New("Typst compiler profile is incomplete")
	}
	for _, expected := range []struct{ path, hash string }{{compiler.BinaryPath, compiler.BinaryHash}, {compiler.FontPath, compiler.FontHash}} {
		if len(expected.hash) != 64 || strings.ToLower(expected.hash) != expected.hash {
			return errors.New("Typst compiler artifact hash is invalid")
		}
		info, err := os.Stat(expected.path)
		if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 256<<20 {
			return errors.New("Typst compiler artifact is missing or too large")
		}
		stream, err := os.Open(expected.path)
		if err != nil {
			return err
		}
		hasher := sha256.New()
		_, copyErr := io.Copy(hasher, stream)
		_ = stream.Close()
		if copyErr != nil {
			return copyErr
		}
		if hex.EncodeToString(hasher.Sum(nil)) != expected.hash {
			return errors.New("Typst compiler artifact does not match its pinned hash")
		}
	}
	return nil
}

func (compiler Compiler) Compile(ctx context.Context, files map[string][]byte, entrypoint, format string) (CompileResult, error) {
	if err := compiler.Validate(); err != nil {
		return CompileResult{}, err
	}
	if format != "html" && format != "pdf" {
		return CompileResult{}, errors.New("Typst output format is unsupported")
	}
	cleaned, ok := contracts.CleanProjectPath(entrypoint)
	if !ok || cleaned != entrypoint || !strings.HasSuffix(strings.ToLower(entrypoint), ".typ") || len(files) == 0 || len(files) > 700 {
		return CompileResult{}, errors.New("Typst project entrypoint is invalid")
	}
	var total int64
	for path, body := range files {
		cleaned, ok := contracts.CleanProjectPath(path)
		if !ok || cleaned != path || len(body) > 16<<20 {
			return CompileResult{}, errors.New("Typst project file is invalid")
		}
		total += int64(len(body))
		if total > maxCompilerInputBytes {
			return CompileResult{}, errors.New("Typst project is too large")
		}
	}
	if _, ok := files[entrypoint]; !ok {
		return CompileResult{}, errors.New("Typst entrypoint is missing")
	}
	base, err := os.MkdirTemp("", "rin-typst-job-")
	if err != nil {
		return CompileResult{}, err
	}
	defer os.RemoveAll(base)
	projectRoot := filepath.Join(base, "project")
	fontRoot := filepath.Join(base, "fonts")
	packageRoot := filepath.Join(base, "packages")
	for _, directory := range []string{projectRoot, fontRoot, packageRoot} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return CompileResult{}, err
		}
	}
	for name, body := range files {
		target := filepath.Join(projectRoot, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return CompileResult{}, err
		}
		if err := os.WriteFile(target, body, 0o600); err != nil {
			return CompileResult{}, err
		}
	}
	font, err := os.ReadFile(compiler.FontPath)
	if err != nil {
		return CompileResult{}, err
	}
	fontTarget := filepath.Join(fontRoot, filepath.Base(compiler.FontPath))
	if err := os.WriteFile(fontTarget, font, 0o600); err != nil {
		return CompileResult{}, err
	}
	outputPath := filepath.Join(base, "output."+format)
	args := []string{"compile", "--format", format, "--root", projectRoot, "--ignore-system-fonts",
		"--font-path", fontRoot, "--package-path", packageRoot, "--package-cache-path", packageRoot,
		"--jobs", "1", "--creation-timestamp", "0", "--diagnostic-format", "short"}
	if format == "html" {
		args = append(args, "--features", "html")
	}
	args = append(args, filepath.Join(projectRoot, filepath.FromSlash(entrypoint)), outputPath)
	bounded, cancel := context.WithTimeout(ctx, compiler.Timeout)
	defer cancel()
	command := exec.CommandContext(bounded, compiler.BinaryPath, args...)
	command.Dir = projectRoot
	command.Env = []string{"HOME=" + base, "TMPDIR=" + base, "LANG=C.UTF-8", "SOURCE_DATE_EPOCH=0", "PATH=/usr/bin:/bin"}
	var log cappedLog
	command.Stdout = io.Discard
	command.Stderr = &log
	if err := command.Run(); err != nil {
		if bounded.Err() != nil {
			return CompileResult{Log: log.String()}, fmt.Errorf("Typst compilation timed out or was canceled: %w", bounded.Err())
		}
		return CompileResult{Log: log.String()}, fmt.Errorf("Typst compilation failed: %w", err)
	}
	info, err := os.Stat(outputPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > compiler.MaxOutput {
		return CompileResult{Log: log.String()}, errors.New("Typst output is missing or exceeds its limit")
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		return CompileResult{Log: log.String()}, err
	}
	if format == "pdf" && !bytes.HasPrefix(output, []byte("%PDF-")) {
		return CompileResult{Log: log.String()}, errors.New("Typst PDF output has an invalid header")
	}
	return CompileResult{Output: output, Log: log.String()}, nil
}

type cappedLog struct{ bytes.Buffer }

func (log *cappedLog) Write(body []byte) (int, error) {
	length := len(body)
	remaining := maxCompilerLogBytes - log.Len()
	if remaining > 0 {
		if len(body) > remaining {
			body = body[:remaining]
		}
		_, _ = log.Buffer.Write(body)
	}
	return length, nil
}
