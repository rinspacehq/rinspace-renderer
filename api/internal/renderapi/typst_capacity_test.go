package renderapi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/typstadapter"
)

func TestTypstOutputLimitDiagnosticSurvivesCompilerWarnings(t *testing.T) {
	err := fmt.Errorf("%w: 70000000 bytes; limit is 67108864 bytes", typstadapter.ErrOutputTooLarge)
	result := typstCompileFailure(context.Background(), err, "warning: html export is under active development and incomplete")
	if typstDiagnosticCode(result) != "typst.output.too_large" || !strings.Contains(result.Diagnostics[0].Message, "70000000 bytes; limit is 67108864 bytes") {
		t.Fatalf("compiler warning concealed output rejection: %#v", result.Diagnostics)
	}
	if cfg := ConfigFromEnv(); cfg.TypstMaxOutputBytes != typstadapter.MaxNativeHTMLBytes {
		t.Fatalf("default compiler and adapter limits differ: %d", cfg.TypstMaxOutputBytes)
	}
}

func TestTypstCompilerCapacityAccepts64MiBButNotUnboundedOutput(t *testing.T) {
	cfg := typstTestConfig(t, `<html><body><p>capacity</p></body></html>`)
	executor := newTypstProjectExecutor(cfg, nil)
	compiler := executor.compiler()
	compiler.MaxOutput = typstadapter.MaxNativeHTMLBytes
	if err := compiler.Validate(); err != nil {
		t.Fatalf("64 MiB compiler limit rejected: %v", err)
	}
	compiler.MaxOutput++
	if err := compiler.Validate(); err == nil {
		t.Fatal("unbounded compiler output was admitted")
	}
}
