package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"airouter/internal/config"
	"airouter/internal/crypto"
	"airouter/internal/observability"
	"airouter/internal/server"
	"airouter/internal/store"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfg := config.Load()

	if cfg.Version {
		fmt.Printf("airouter %s\n", version)
		return
	}

	logger := observability.NewLogger(cfg.DebugLevel, os.Stderr)
	slog.SetDefault(logger)
	mainLog := logger.With("component", "main")

	secret, isDev := cfg.EffectiveSecret()
	if isDev {
		mainLog.Warn("insecure_dev_secret",
			"event", "insecure_dev_secret",
			"msg_detail", "AIROUTER_SECRET not set; using insecure dev key. Stored API keys are not protected.",
		)
	}
	switch {
	case cfg.DebugLevel >= 2:
		mainLog.Info("debug_logging_enabled",
			"event", "debug_logging_enabled",
			"debug_level", cfg.DebugLevel,
			"detail", "TRACE enabled; detailed request and probe metadata will be logged (no bodies)",
		)
	case cfg.DebugLevel == 1:
		mainLog.Info("debug_logging_enabled",
			"event", "debug_logging_enabled",
			"debug_level", cfg.DebugLevel,
			"detail", "DEBUG enabled; access lines and upstream failure metadata will be logged (no bodies)",
		)
	}
	if cfg.DisableDashboard {
		mainLog.Info("dashboard_disabled",
			"event", "dashboard_disabled",
			"detail", "web dashboard and /static assets are not mounted; proxy routes and GET /debug/har remain available",
		)
	}
	switch {
	case cfg.HARFile != "":
		mainLog.Info("har_capture_enabled",
			"event", "har_capture_enabled",
			"path", cfg.HARFile,
			"detail", "file mode: always-on capture; download at GET /debug/har; flushed to path on shutdown",
		)
	case cfg.DisableDashboard:
		mainLog.Info("har_runtime_unavailable",
			"event", "har_runtime_unavailable",
			"detail", "runtime dashboard-controlled HAR capture is unavailable; GET /debug/har remains mounted",
		)
	default:
		mainLog.Info("har_runtime_available",
			"event", "har_runtime_available",
			"detail", "runtime HAR capture controlled from dashboard settings; download at GET /debug/har after Stop",
		)
	}
	cipher, err := crypto.New(secret)
	if err != nil {
		mainLog.Error("init_cipher_failed", "event", "init_cipher_failed", "error", err)
		os.Exit(1)
	}

	st, err := store.Open(cfg.DBPath, cipher)
	if err != nil {
		mainLog.Error("open_store_failed", "event", "open_store_failed", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	app := server.New(st, logger, cfg.HARFile, version, cfg.DisableDashboard)
	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: app.Handler(),
		// ReadHeaderTimeout bounds header reads (slowloris hardening) without
		// capping the body or the response stream. ReadTimeout is deliberately
		// omitted: it is a total deadline over the whole request including
		// uploads and would cut off long SSE streams and slow large prompts.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		mainLog.Info("server_listening",
			"event", "server_listening",
			"version", version,
			"addr", cfg.ListenAddr,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case <-stop:
	case err := <-errCh:
		if err != nil {
			mainLog.Error("server_failed", "event", "server_failed", "error", err)
			os.Exit(1)
		}
		return
	}

	mainLog.Info("shutdown_started", "event", "shutdown_started")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		mainLog.Error("shutdown_failed", "event", "shutdown_failed", "error", err)
	}
	if rec := app.HAR(); rec != nil && cfg.HARFile != "" {
		if err := rec.WriteFile(cfg.HARFile); err != nil {
			mainLog.Error("har_write_failed", "event", "har_write_failed", "path", cfg.HARFile, "error", err)
		} else {
			mainLog.Info("har_written", "event", "har_written", "path", cfg.HARFile)
		}
	}
}
