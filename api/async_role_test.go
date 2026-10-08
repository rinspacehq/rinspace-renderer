package main

import (
	"context"
	"strings"
	"testing"

	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
)

func TestConfiguredRuntimeRole(t *testing.T) {
	t.Setenv("RIN_RENDERER_RUNTIME_ROLE", "")
	if role, err := configuredRuntimeRole(); err != nil || role != "all" {
		t.Fatalf("default role = %q, %v", role, err)
	}
	for _, expected := range []string{"all", "api", "worker", "typst-html-worker", "pdf-worker", "typst-pdf-worker"} {
		t.Setenv("RIN_RENDERER_RUNTIME_ROLE", expected)
		if role, err := configuredRuntimeRole(); err != nil || role != expected {
			t.Fatalf("configured role = %q, %v, want %q", role, err, expected)
		}
	}
	t.Setenv("RIN_RENDERER_RUNTIME_ROLE", "scheduler")
	if _, err := configuredRuntimeRole(); err == nil {
		t.Fatal("invalid runtime role was accepted")
	}
}

func TestConfiguredCompletionMode(t *testing.T) {
	t.Setenv("RIN_RENDERER_COMPLETION_MODE", "")
	if mode, err := configuredCompletionMode(); err != nil || mode != "control-plane" {
		t.Fatalf("default completion mode = %q, %v", mode, err)
	}
	for _, mode := range []string{"control-plane", "local"} {
		t.Setenv("RIN_RENDERER_COMPLETION_MODE", mode)
		if got, err := configuredCompletionMode(); err != nil || got != mode {
			t.Fatalf("completion mode = %q, %v, want %q", got, err, mode)
		}
	}
	t.Setenv("RIN_RENDERER_COMPLETION_MODE", "disabled")
	if _, err := configuredCompletionMode(); err == nil {
		t.Fatal("invalid completion mode was accepted")
	}
}

func TestLocalAssetListenerMustBeLoopbackAndMatchAPIOrigin(t *testing.T) {
	cfg := renderapi.Config{StorageProvider: "local", Addr: "127.0.0.1:8090", LocalAssetRoot: t.TempDir(), LocalPublicBaseURL: "http://127.0.0.1:8090"}
	t.Setenv("RIN_RENDERER_RUNTIME_ROLE", "api")
	if err := validateLocalStorageRuntime(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Addr = "0.0.0.0:8090"
	if err := validateLocalStorageRuntime(cfg); err == nil {
		t.Fatal("local asset API accepted a non-loopback listener")
	}
	cfg.Addr = "127.0.0.1:8092"
	if err := validateLocalStorageRuntime(cfg); err == nil {
		t.Fatal("local API accepted an asset URL for a different port")
	}
	t.Setenv("RIN_RENDERER_RUNTIME_ROLE", "worker")
	if err := validateLocalStorageRuntime(cfg); err != nil {
		t.Fatalf("local worker must be allowed to emit the API's asset origin: %v", err)
	}
}

func TestExplicitRuntimeRolesRequireDatabase(t *testing.T) {
	t.Setenv("RIN_RENDERER_DATABASE_URL", "")
	for _, role := range []string{"api", "worker", "typst-html-worker", "pdf-worker", "typst-pdf-worker"} {
		t.Setenv("RIN_RENDERER_RUNTIME_ROLE", role)
		_, err := buildAsyncRuntime(context.Background(), renderapi.ConfigFromEnv())
		if err == nil || !strings.Contains(err.Error(), "RIN_RENDERER_DATABASE_URL is required") {
			t.Fatalf("role %q missing database error = %v", role, err)
		}
	}
}

func TestPositiveEnvironmentInt(t *testing.T) {
	t.Setenv("RIN_RENDERER_WORKER_CONCURRENCY", "")
	if value, err := positiveEnvironmentInt("RIN_RENDERER_WORKER_CONCURRENCY", 1); err != nil || value != 1 {
		t.Fatalf("default worker concurrency = %d, %v", value, err)
	}
	t.Setenv("RIN_RENDERER_WORKER_CONCURRENCY", "2")
	if value, err := positiveEnvironmentInt("RIN_RENDERER_WORKER_CONCURRENCY", 1); err != nil || value != 2 {
		t.Fatalf("configured worker concurrency = %d, %v", value, err)
	}
	for _, invalid := range []string{"0", "-1", "many"} {
		t.Setenv("RIN_RENDERER_WORKER_CONCURRENCY", invalid)
		if _, err := positiveEnvironmentInt("RIN_RENDERER_WORKER_CONCURRENCY", 1); err == nil {
			t.Fatalf("invalid worker concurrency %q was accepted", invalid)
		}
	}
}
