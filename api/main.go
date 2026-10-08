package main

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rinspacehq/rinspace-renderer/api/internal/operational"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderapi"
	"github.com/rinspacehq/rinspace-renderer/api/internal/renderstorage"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if len(os.Args) == 2 && os.Args[1] == "migrate" {
		if err := runMigrations(ctx); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) != 1 {
		log.Fatalf("usage: %s [migrate]", os.Args[0])
	}
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	operations := operational.New(os.Stdout)
	operational.SetDefault(operations)
	cfg := renderapi.ConfigFromEnv()
	if err := validateLocalStorageRuntime(cfg); err != nil {
		return err
	}
	async, err := buildAsyncRuntime(ctx, cfg)
	if err != nil {
		return err
	}
	if async != nil {
		defer async.Close()
	}
	var handler http.Handler = renderapi.NewServer(cfg)
	if async != nil {
		handler = renderapi.NewServerWithOperations(cfg, async.handler, async.projects, operations)
	}
	if closer, ok := handler.(io.Closer); ok {
		defer closer.Close()
	}
	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	operations.Event("server_started", slog.String("renderer_version", cfg.RendererVersion))
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.ListenAndServe() }()
	select {
	case err := <-serverDone:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		if err := <-serverDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func validateLocalStorageRuntime(cfg renderapi.Config) error {
	if cfg.StorageProvider == "local" {
		listenHost, listenPort, splitErr := net.SplitHostPort(cfg.Addr)
		publicURL, parseErr := url.Parse(cfg.LocalPublicBaseURL)
		role, roleErr := configuredRuntimeRole()
		if splitErr != nil || parseErr != nil || roleErr != nil || publicURL == nil ||
			(listenHost != "localhost" && (net.ParseIP(listenHost) == nil || !net.ParseIP(listenHost).IsLoopback())) {
			return errors.New("local public asset storage requires a loopback API listener and public URL")
		}
		if (role == "api" || role == "all") && publicURL.Port() != listenPort {
			return errors.New("local API public asset URL must use the API listener port")
		}
		if _, err := renderstorage.NewLocalFileStore(cfg.LocalAssetRoot, cfg.LocalPublicBaseURL); err != nil {
			return err
		}
	}
	return nil
}
