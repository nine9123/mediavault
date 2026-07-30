// Package describe captions media with a vision model served by Ollama and (if
// an embedding model is configured) embeds those captions for semantic search.
// It runs as a slow background worker — one image at a time, LLM inference is
// the bottleneck — backfilling over time. Entirely optional: only runs when
// OLLAMA_URL is set. Captions go to items.description (and full-text search);
// embeddings go to item_embeddings (semantic search).
package describe

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/media"
	"mediavault/internal/ollama"
	"mediavault/internal/store"
)

const frameMaxPx = 512

type Describer struct {
	cfg       config.Config
	store     *store.Store
	failed    map[string]struct{} // frames we couldn't extract; skip for this run
	failedEmb map[string]struct{} // captions the embed model rejected; skip for this run
}

func New(cfg config.Config, st *store.Store) *Describer {
	return &Describer{
		cfg:       cfg,
		store:     st,
		failed:    map[string]struct{}{},
		failedEmb: map[string]struct{}{},
	}
}

func (d *Describer) embedOn() bool { return d.cfg.OllamaEmbedModel != "" }

// Run backfills captions (and embeddings), then re-checks periodically — which
// doubles as backoff when the Ollama server is unreachable.
func (d *Describer) Run(ctx context.Context) {
	if d.embedOn() {
		log.Printf("describe: on — caption %q + embed %q via %s", d.cfg.OllamaModel, d.cfg.OllamaEmbedModel, d.cfg.OllamaURL)
	} else {
		log.Printf("describe: on — caption %q via %s (set OLLAMA_EMBED_MODEL for semantic search)", d.cfg.OllamaModel, d.cfg.OllamaURL)
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	d.pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.pass(ctx)
		}
	}
}

func (d *Describer) pass(ctx context.Context) {
	d.captionPass(ctx)
	if d.embedOn() {
		d.embedPass(ctx)
	}
}

// captionPass captions undescribed visual items with the vision model.
func (d *Describer) captionPass(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		items, err := d.store.UndescribedItems(ctx, 20)
		if err != nil {
			log.Printf("describe: list: %v", err)
			return
		}
		progressed := false
		for _, it := range items {
			if ctx.Err() != nil {
				return
			}
			if _, skip := d.failed[it.Relpath]; skip {
				continue
			}
			abs := filepath.Join(d.cfg.MediaRoot, filepath.FromSlash(it.Relpath))
			jpeg, err := media.FrameJPEG(ctx, d.cfg, abs, it.Type, it.Offset, frameMaxPx)
			if err != nil {
				d.failed[it.Relpath] = struct{}{} // no frame; skip this run
				continue
			}
			desc, err := ollama.Caption(ctx, d.cfg, jpeg)
			if err != nil {
				log.Printf("describe: caption: %v", err) // server down/busy -> back off
				return
			}
			if desc == "" {
				d.failed[it.Relpath] = struct{}{}
				continue
			}
			if err := d.store.SetDescription(ctx, it.Relpath, desc); err != nil {
				log.Printf("describe: save %s: %v", it.Relpath, err)
				continue
			}
			progressed = true
		}
		if !progressed {
			return
		}
	}
}

// embedPass computes caption embeddings for items that don't have one yet.
func (d *Describer) embedPass(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		items, err := d.store.CaptionedButUnembedded(ctx, 50)
		if err != nil {
			log.Printf("describe: embed list: %v", err)
			return
		}
		if len(items) == 0 {
			return
		}
		progressed := false
		for _, it := range items {
			if ctx.Err() != nil {
				return
			}
			if _, skip := d.failedEmb[it.Relpath]; skip {
				continue
			}
			vec, err := ollama.Embed(ctx, d.cfg, it.Description)
			if err != nil {
				// Separate "this caption is bad" from "the server is unhappy". A 4xx
				// is the former, and returning on it stalls the whole backfill:
				// CaptionedButUnembedded keeps handing the item back, newest first,
				// so every ticker pass dies on the same row and nothing behind it in
				// the queue is ever embedded. Skip the item instead.
				if ollama.PermanentError(err) {
					log.Printf("describe: embed %s: %v (skipping)", it.Relpath, err)
					d.failedEmb[it.Relpath] = struct{}{}
					continue
				}
				log.Printf("describe: embed: %v", err) // server down/busy -> back off
				return
			}
			if err := d.store.SetEmbedding(ctx, it.Relpath, vec); err != nil {
				log.Printf("describe: embed save %s: %v", it.Relpath, err)
				continue
			}
			progressed = true
		}
		// A batch of nothing but skips would otherwise re-query the same rows
		// forever, since none of them gain an embedding.
		if !progressed {
			return
		}
	}
}
