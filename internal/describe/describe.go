// Package describe captions media with a vision model served by Ollama, so the
// captions can be searched in natural language. It runs as a slow background
// worker (one image at a time — LLM inference is the bottleneck), backfilling
// undescribed items over time. Entirely optional: only runs when OLLAMA_URL is
// set. Captions are written to items.description and folded into full-text search.
package describe

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/media"
	"mediavault/internal/store"
)

const frameMaxPx = 512

type Describer struct {
	cfg    config.Config
	store  *store.Store
	hc     *http.Client
	failed map[string]struct{} // frames we couldn't extract; skip for this run
}

func New(cfg config.Config, st *store.Store) *Describer {
	return &Describer{
		cfg:    cfg,
		store:  st,
		hc:     &http.Client{Timeout: 180 * time.Second}, // vision inference can be slow
		failed: map[string]struct{}{},
	}
}

// Run backfills captions, then re-checks periodically (which also serves as
// backoff if the Ollama server is unreachable).
func (d *Describer) Run(ctx context.Context) {
	log.Printf("describe: on — %s via %s", d.cfg.OllamaModel, d.cfg.OllamaURL)
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
			jpeg, err := media.FrameJPEG(ctx, d.cfg, abs, it.Type, frameMaxPx)
			if err != nil {
				d.failed[it.Relpath] = struct{}{} // can't get a frame; don't retry this run
				continue
			}
			desc, err := d.caption(ctx, jpeg)
			if err != nil {
				// Almost always the Ollama server being down/busy — back off and
				// let the next tick retry rather than hammering it.
				log.Printf("describe: ollama: %v", err)
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
			return // nothing left we can caption right now
		}
	}
}

func (d *Describer) caption(ctx context.Context, jpeg []byte) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":  d.cfg.OllamaModel,
		"prompt": d.cfg.OllamaPrompt,
		"images": []string{base64.StdEncoding.EncodeToString(jpeg)},
		"stream": false,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(d.cfg.OllamaURL, "/")+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	var out struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Response), nil
}
