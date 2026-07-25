// Package ollama is a tiny client for the endpoints mediavault uses on a
// self-hosted Ollama server: image captioning (/api/generate with a vision
// model) and text embeddings (/api/embeddings). Callers set timeouts via the
// context; the client has only a generous hard cap.
package ollama

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"mediavault/internal/config"
)

var client = &http.Client{Timeout: 5 * time.Minute}

// Caption asks the vision model to describe a JPEG frame.
func Caption(ctx context.Context, cfg config.Config, jpeg []byte) (string, error) {
	var out struct {
		Response string `json:"response"`
	}
	err := post(ctx, cfg.OllamaURL, "/api/generate", map[string]any{
		"model":  cfg.OllamaModel,
		"prompt": cfg.OllamaPrompt,
		"images": []string{base64.StdEncoding.EncodeToString(jpeg)},
		"stream": false,
	}, &out)
	return strings.TrimSpace(out.Response), err
}

// Embed returns a unit-normalized embedding of text (so cosine == dot product).
func Embed(ctx context.Context, cfg config.Config, text string) ([]float32, error) {
	var out struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := post(ctx, cfg.OllamaURL, "/api/embeddings", map[string]any{
		"model":  cfg.OllamaEmbedModel,
		"prompt": text,
	}, &out); err != nil {
		return nil, err
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding")
	}
	v := make([]float32, len(out.Embedding))
	var s float64
	for i, x := range out.Embedding {
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
		return fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
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
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
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
