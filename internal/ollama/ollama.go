// Package ollama is a tiny client for the endpoints mediavault uses on a
// self-hosted Ollama server: image captioning (/api/generate with a vision
// model) and text embeddings (/api/embed). Callers set timeouts via the
// context; the client has only a generous hard cap.
//
// Requires Ollama 0.2 or newer for /api/embed. The older /api/embeddings is
// deliberately not used: it hands the prompt straight to the runner with no
// tokenize-and-truncate step, so a single over-long input returns HTTP 500
// ("the input length exceeds the context length") instead of an embedding.
package ollama

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"mediavault/internal/config"
)

var client = &http.Client{Timeout: 5 * time.Minute}

const (
	// captionNumPredict caps caption generation. The prompt asks for one or two
	// sentences, so text beyond this is a vision-model repetition loop rather
	// than a description. /api/generate defaults to generating until it fills
	// the context, which yields captions too long to embed or to display.
	captionNumPredict = 200

	// maxCaptionChars bounds what we hand back even if the model ignores
	// num_predict. Roughly the byte budget of captionNumPredict tokens, with
	// room to spare.
	maxCaptionChars = 1200
)

// StatusError is a non-200 reply from the Ollama server.
type StatusError struct {
	Code int
	Body string
}

func (e *StatusError) Error() string { return fmt.Sprintf("status %d: %s", e.Code, e.Body) }

// PermanentError reports whether err is the server rejecting this particular
// request (4xx) rather than a transient problem with the server, the model, or
// the network. Callers use it to choose between skipping one input and backing
// off entirely: retrying a 4xx forever makes no progress, and if the input sits
// at the head of a work queue it starves everything behind it.
func PermanentError(err error) bool {
	var se *StatusError
	if !errors.As(err, &se) {
		return false
	}
	return se.Code >= 400 && se.Code < 500 && se.Code != http.StatusTooManyRequests
}

// Caption asks the vision model to describe a JPEG frame. The result is length
// capped at both ends of the wire — see captionNumPredict and maxCaptionChars —
// because a runaway caption is not just unusable for embedding: it also lands in
// items.description, the full-text index, and the UI.
func Caption(ctx context.Context, cfg config.Config, jpeg []byte) (string, error) {
	var out struct {
		Response string `json:"response"`
	}
	err := post(ctx, cfg.OllamaURL, "/api/generate", map[string]any{
		"model":  cfg.OllamaModel,
		"prompt": cfg.OllamaPrompt,
		"images": []string{base64.StdEncoding.EncodeToString(jpeg)},
		"stream": false,
		"options": map[string]any{
			"num_predict": captionNumPredict,
		},
	}, &out)
	return clampCaption(strings.TrimSpace(out.Response), maxCaptionChars), err
}

// clampCaption bounds a caption, cutting back to a word boundary. A caption this
// long means the model looped instead of describing anything, so we keep the
// readable head rather than storing the loop.
func clampCaption(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexAny(cut, " \t\n"); i > max/2 {
		cut = cut[:i]
	}
	// The byte slice above can land mid-rune when there was no late word break.
	cut = strings.ToValidUTF8(strings.TrimSpace(cut), "")
	return cut + "…"
}

// Embed returns a unit-normalized embedding of text (so cosine == dot product).
//
// Inputs longer than the embedding model's context are truncated server-side
// rather than rejected. num_ctx is deliberately not sent: for embedding models
// Ollama clamps it to the context the model was trained with (2048 for
// nomic-embed-text, 512 for mxbai-embed-large, 256 for all-minilm), so raising
// it cannot make a long input fit.
func Embed(ctx context.Context, cfg config.Config, text string) ([]float32, error) {
	var out struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if err := post(ctx, cfg.OllamaURL, "/api/embed", map[string]any{
		"model": cfg.OllamaEmbedModel,
		"input": text,
		// Already the default, but state it: an over-long caption should yield an
		// embedding of its leading tokens, not an error that stalls the backfill.
		"truncate": true,
	}, &out); err != nil {
		return nil, err
	}
	if len(out.Embeddings) == 0 || len(out.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("empty embedding")
	}
	emb := out.Embeddings[0]
	v := make([]float32, len(emb))
	var s float64
	for i, x := range emb {
		v[i] = float32(x)
		s += x * x
	}
	if s = math.Sqrt(s); s > 0 {
		for i := range v {
			v[i] = float32(float64(v[i]) / s)
		}
	}
	return v, nil
}

func post(ctx context.Context, base, path string, payload, out any) error {
	if base == "" {
		return fmt.Errorf("OLLAMA_URL not set")
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &StatusError{Code: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Models lists installed model names (GET /api/tags) — used for a startup
// health check so a missing/mistyped model is obvious.
func Models(ctx context.Context, cfg config.Config) ([]string, error) {
	if cfg.OllamaURL == "" {
		return nil, fmt.Errorf("OLLAMA_URL not set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(cfg.OllamaURL, "/")+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &StatusError{Code: resp.StatusCode, Body: string(bytes.TrimSpace(b))}
	}
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Models))
	for _, m := range out.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

// HasModel reports whether want matches an installed model, tolerating the
// implicit ":latest" tag (want "llava" matches "llava:latest").
func HasModel(models []string, want string) bool {
	for _, m := range models {
		if m == want || m == want+":latest" || strings.HasPrefix(m, want+":") {
			return true
		}
	}
	return false
}
