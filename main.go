// Command server is the mediavault entrypoint. It wires config -> store ->
// scanner -> HTTP, runs the background scanner alongside serving, and shuts
// down gracefully on SIGINT/SIGTERM.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/scan"
	"mediavault/internal/server"
	"mediavault/internal/store"
)

func main() {
	cfg := config.Load()
	log.Printf("media root: %s (read-only)", cfg.MediaRoot)
	log.Printf("data dir:   %s (disposable cache: db + thumbnails)", cfg.DataDir)

	if err := os.MkdirAll(cfg.ThumbDir(), 0o755); err != nil {
		log.Fatalf("prepare data dir: %v", err)
	}

	st, err := store.Open(cfg.DBPath())
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	scanner := scan.New(cfg, st)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Background scanner: initial scan on boot, then periodic rescans. A scan in
	// progress never blocks reads (WAL + single-writer).
	go scanner.RunPeriodic(ctx)

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: server.New(cfg, st, scanner).Routes(),
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Printf("listening on %s", cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
	log.Println("shutdown complete")
}
