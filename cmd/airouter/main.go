package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"airouter/internal/config"
	"airouter/internal/crypto"
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

	// logFile, when non-nil, is the full-trace sink passed to the server; stderr
	// keeps a truncated copy. nil means stderr-only (truncated) tracing.
	var logFile io.Writer
	if cfg.LogFile != "" {
		f, err := os.OpenFile(cfg.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("open log file: %v", err)
		}
		defer f.Close()
		log.SetOutput(io.MultiWriter(os.Stderr, f))
		logFile = f
	}

	secret, isDev := cfg.EffectiveSecret()
	if isDev {
		log.Println("WARNING: AIROUTER_SECRET not set; using insecure dev key. Stored API keys are not protected.")
	}
	switch {
	case cfg.DebugLevel >= 2:
		log.Println("debug level 2 (trace) enabled; full request and response bodies will be logged (includes prompt content)")
	case cfg.DebugLevel == 1:
		log.Println("debug mode enabled; failed upstream exchanges will be logged (may include prompt content)")
	}
	if cfg.HARFile != "" {
		log.Println("HAR capture enabled; full request/response bodies and verbatim credentials will be recorded (GET /debug/har; flushed on shutdown)")
	}
	cipher, err := crypto.New(secret)
	if err != nil {
		log.Fatalf("init cipher: %v", err)
	}

	st, err := store.Open(cfg.DBPath, cipher)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	app := server.New(st, cfg.DebugLevel, logFile, cfg.HARFile, version)
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

	go func() {
		log.Printf("airouter %s listening on %s (dashboard at /dashboard)", version, cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	if rec := app.HAR(); rec != nil && cfg.HARFile != "" {
		if err := rec.WriteFile(cfg.HARFile); err != nil {
			log.Printf("write HAR file: %v", err)
		} else {
			log.Printf("wrote HAR capture to %s", cfg.HARFile)
		}
	}
}
