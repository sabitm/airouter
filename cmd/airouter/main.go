package main

import (
	"context"
	"errors"
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

func main() {
	cfg := config.Load()

	secret, isDev := cfg.EffectiveSecret()
	if isDev {
		log.Println("WARNING: AIROUTER_SECRET not set; using insecure dev key. Stored API keys are not protected.")
	}
	if cfg.Debug {
		log.Println("debug mode enabled; failed upstream exchanges will be logged (may include prompt content)")
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

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: server.New(st, cfg.Debug).Handler(),
	}

	go func() {
		log.Printf("airouter listening on %s (dashboard at /dashboard)", cfg.ListenAddr)
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
}
