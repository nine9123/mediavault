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
	"mediavault/internal/describe"
	"mediavault/internal/faces"
	"mediavault/internal/ollama"
	"mediavault/internal/scan"
	"mediavault/internal/server"
	"mediavault/internal/store"
	"mediavault/internal/transcode"
)

func main() {
	cfg := config.Load()
	log.Printf("media root: %s (read-only)", cfg.MediaRoot)
	log.Printf("data dir:   %s (disposable cache: db + thumbnails)", cfg.DataDir)
	if cfg.DerivedDir != "" {
		log.Printf("derived dir: %s (serving browser-friendly transcodes when present)", cfg.DerivedDir)
	} else {
		log.Printf("derived dir: (unset) — WMV/AVI/HEVC/ALAC won't play until DERIVED_DIR is set and `transcode` has run")
	}
	if cfg.OllamaURL != "" {
		if cfg.OllamaEmbedModel != "" {
			log.Printf("captions: on — %s + semantic embeddings %s via %s", cfg.OllamaModel, cfg.OllamaEmbedModel, cfg.OllamaURL)
		} else {
			log.Printf("captions: on — %s via %s (keyword; set OLLAMA_EMBED_MODEL for semantic search)", cfg.OllamaModel, cfg.OllamaURL)
		}
		go func() {
			hctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			models, err := ollama.Models(hctx, cfg)
			if err != nil {
				log.Printf("ollama: NOT reachable at %s: %v — captions/search will be disabled until this resolves", cfg.OllamaURL, err)
				return
			}
			log.Printf("ollama: reachable (%d models installed)", len(models))
			if !ollama.HasModel(models, cfg.OllamaModel) {
				log.Printf("ollama: WARNING vision model %q is not installed — run `ollama pull %s` on the server", cfg.OllamaModel, cfg.OllamaModel)
			}
			if cfg.OllamaEmbedModel != "" && !ollama.HasModel(models, cfg.OllamaEmbedModel) {
				log.Printf("ollama: WARNING embedding model %q is not installed — run `ollama pull %s`", cfg.OllamaEmbedModel, cfg.OllamaEmbedModel)
			}
		}()
	}

	if cfg.FacesEnabled() {
		log.Printf("people: on — face recognition via %s (durable names: %s)", cfg.FaceURL, cfg.PeoplePath())
	}

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
	// progress never blocks reads (WAL + single-writer). The done channel lets
	// shutdown wait for an in-flight scan to persist before we close the DB.
	scanDone := make(chan struct{})
	go func() {
		scanner.RunPeriodic(ctx)
		close(scanDone)
	}()

	if cfg.OllamaURL != "" {
		go describe.New(cfg, st).Run(ctx)
	}

	// One recognizer, shared: the worker fills the faces table, the HTTP layer
	// reads it, and both cluster with the same similarity threshold.
	var rec *faces.Recognizer
	if cfg.FacesEnabled() {
		rec = faces.New(cfg, st)
		go rec.Run(ctx)
	}

	tx := transcode.New(ctx, cfg)

	api := server.New(cfg, st, scanner, tx, rec)
	log.Print(api.Auth().Describe())

	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: api.Routes(),
	}

	// One budget for the whole shutdown, HTTP drain and scanner together.
	// idlegate stops this container with `docker stop -t 10`: anything still
	// running at 10 s is SIGKILLed, exits 137, and TrueNAS marks the app Crashed.
	stopping := make(chan time.Time, 1)
	httpDone := make(chan struct{})
	go func() {
		defer close(httpDone)
		<-ctx.Done()
		log.Println("received stop signal, shutting down")
		deadline := time.Now().Add(shutdownBudget)
		stopping <- deadline
		shutCtx, cancel := context.WithDeadline(context.Background(), deadline)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Printf("listening on %s", cfg.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
	// The listener is closed; let open requests finish, and give an in-flight
	// scan a moment to persist its current batch before the deferred st.Close()
	// runs. Writes are transactional, so blowing past this budget (SIGKILL) is
	// still safe: unfinished items are simply redone by the next scan.
	budget, cancelBudget := context.WithDeadline(context.Background(), <-stopping)
	defer cancelBudget()
	select {
	case <-httpDone:
	case <-budget.Done():
		log.Println("requests still open; closing them")
	}
	select {
	case <-scanDone:
	case <-budget.Done():
		log.Println("scanner still busy; exiting anyway (writes are transactional)")
	}
	log.Println("shutdown complete")
}

// shutdownBudget is how long a stop may take, from the signal to exit 0. Well
// inside idlegate's (and Docker's) default 10 s grace, so a slow disk can't
// push it into a SIGKILL.
const shutdownBudget = 4 * time.Second
