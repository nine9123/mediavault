package server

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/model"
	"mediavault/internal/store"
)

// chainServer indexes the given images and stores the chains the scanner would
// have derived for them, then returns the router over that database.
func chainServer(t *testing.T, rels []string, defs []store.ChainDef) (*store.Store, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	now := time.Now()
	var batch []store.ItemWrite
	for _, rel := range rels {
		batch = append(batch, store.ItemWrite{Item: model.Item{
			Relpath: rel, Size: 1024, MTime: now, Type: model.MediaImage,
			Width: 800, Height: 1200, IndexedAt: now, SeenAt: now,
		}})
	}
	if err := st.WriteBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceAllChains(ctx, defs); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{MediaRoot: dir, DataDir: dir, SidecarDir: dir}
	return st, New(cfg, st, nil, nil, nil).Routes()
}

func relpaths(t *testing.T, out map[string]any) []string {
	t.Helper()
	items, _ := out["items"].([]any)
	var got []string
	for _, it := range items {
		got = append(got, it.(map[string]any)["relpath"].(string))
	}
	return got
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The reader pages through members in the chain's order — here a chain.json
// order that is not filename order — and a member that has gone missing drops
// out without leaving a hole in the page numbers.
func TestChainEndpointReturnsMembersInReadingOrder(t *testing.T) {
	order := []string{"ch3/cover.png", "ch3/01.png", "ch3/02.png", "ch3/03.png"}
	st, h := chainServer(t, append([]string{"loose.png"}, order...), []store.ChainDef{
		{Key: "ch3", Title: "Chapter 3", Kind: "manifest", Source: "ch3", Relpaths: order},
	})

	code, out := do(t, h, "GET", "/api/chain?key=ch3", "")
	if code != http.StatusOK {
		t.Fatalf("by key: status %d", code)
	}
	if got := relpaths(t, out); !sameStrings(got, order) {
		t.Errorf("by key: members %v, want %v", got, order)
	}
	ch := out["chain"].(map[string]any)
	if ch["title"] != "Chapter 3" || ch["count"] != float64(4) {
		t.Errorf("by key: chain %v", ch)
	}
	if _, ok := ch["position"]; ok {
		t.Errorf("by key: position %v, want none when no item was named", ch["position"])
	}

	code, out = do(t, h, "GET", "/api/chain?path="+url.QueryEscape("ch3/02.png"), "")
	if code != http.StatusOK {
		t.Fatalf("by path: status %d", code)
	}
	if pos := out["chain"].(map[string]any)["position"]; pos != float64(2) {
		t.Errorf("by path: position %v, want 2", pos)
	}

	if _, err := st.MarkMissing(context.Background(), []string{"ch3/01.png"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	code, out = do(t, h, "GET", "/api/chain?path="+url.QueryEscape("ch3/02.png"), "")
	if code != http.StatusOK {
		t.Fatalf("after missing: status %d", code)
	}
	want := []string{"ch3/cover.png", "ch3/02.png", "ch3/03.png"}
	if got := relpaths(t, out); !sameStrings(got, want) {
		t.Errorf("after missing: members %v, want %v", got, want)
	}
	if pos := out["chain"].(map[string]any)["position"]; pos != float64(1) {
		t.Errorf("after missing: position %v, want 1", pos)
	}

	// The item card says which chain an item is a page of, numbered the same way.
	code, out = do(t, h, "GET", "/api/item?path="+url.QueryEscape("ch3/02.png"), "")
	if code != http.StatusOK {
		t.Fatalf("item: status %d", code)
	}
	ch, _ = out["chain"].(map[string]any)
	if ch == nil || ch["key"] != "ch3" || ch["position"] != float64(1) || ch["count"] != float64(3) {
		t.Errorf("item: chain %v, want ch3 page 1 of 3", out["chain"])
	}
}

func TestChainEndpointNotFound(t *testing.T) {
	_, h := chainServer(t, []string{"loose.png", "a/1.png", "a/2.png"}, []store.ChainDef{
		{Key: "a", Title: "a", Kind: "folder", Source: "a", Relpaths: []string{"a/1.png", "a/2.png"}},
	})
	for path, want := range map[string]int{
		"/api/chain":                 http.StatusBadRequest,
		"/api/chain?key=nope":        http.StatusNotFound,
		"/api/chain?path=loose.png":  http.StatusNotFound,
		"/api/chain?path=nope/1.png": http.StatusNotFound,
	} {
		if code, _ := do(t, h, "GET", path, ""); code != want {
			t.Errorf("%s: status %d, want %d", path, code, want)
		}
	}
	// An item in no chain gets no chain field at all.
	_, out := do(t, h, "GET", "/api/item?path=loose.png", "")
	if _, ok := out["chain"]; ok {
		t.Errorf("loose item: chain %v, want none", out["chain"])
	}
}
