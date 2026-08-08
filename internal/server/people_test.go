package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/faces"
	"mediavault/internal/model"
	"mediavault/internal/store"
)

// These drive the People endpoints over the real router with a real database.
// Detection is stubbed by writing faces straight into the store — the same thing
// the worker does with what the recognition service returns — so nothing here
// needs FACE_URL to actually answer.

func peopleServer(t *testing.T) (*Server, *store.Store, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		MediaRoot: dir, DataDir: dir, SidecarDir: dir,
		FaceURL:   "http://stub.invalid", // only has to be non-empty: it is never called
		FaceMatch: 0.8,
	}
	st, err := store.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(cfg, st, nil, nil, faces.New(cfg, st))
	return s, st, s.Routes()
}

// detect adds one item per relpath with one face on it, at the given direction.
func detect(t *testing.T, st *store.Store, rel string, a, b, c, d float64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	if err := st.WriteBatch(ctx, []store.ItemWrite{{Item: model.Item{
		Relpath: rel, Size: 1024, MTime: now, Type: model.MediaImage,
		Width: 800, Height: 600, IndexedAt: now, SeenAt: now,
	}}}); err != nil {
		t.Fatal(err)
	}
	n := math.Sqrt(a*a + b*b + c*c + d*d)
	v := []float32{float32(a / n), float32(b / n), float32(c / n), float32(d / n)}
	if err := st.SetFaces(ctx, rel, []store.FaceWrite{
		{X: 0.25, Y: 0.2, W: 0.2, H: 0.3, Score: 1, Vec: v},
	}); err != nil {
		t.Fatal(err)
	}
}

func do(t *testing.T, h http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// clusterRefs is the page's view: one ref per unnamed group, with its match.
func clusterRefs(t *testing.T, h http.Handler) []map[string]any {
	t.Helper()
	code, d := do(t, h, "GET", "/api/people", "")
	if code != http.StatusOK {
		t.Fatalf("GET /api/people: status %d", code)
	}
	raw, _ := d["clusters"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		m, _ := c.(map[string]any)
		out = append(out, m)
	}
	return out
}

// The whole point of the endpoint: one person shown as two groups becomes one,
// and the other person on the page is left exactly as they were.
func TestWidenGroupJoinsTheSplitHalfAndLeavesTheRestAlone(t *testing.T) {
	_, st, h := peopleServer(t)
	detect(t, st, "a.jpg", 1, 0, 0, 0)
	detect(t, st, "b.jpg", 0.99, 0.01, 0, 0)
	detect(t, st, "c.jpg", 1, 1, 0, 0) // same person, 0.71 away: split at 0.8
	detect(t, st, "d.jpg", 0, 0, 1, 0) // somebody else entirely

	before := clusterRefs(t, h)
	if len(before) != 3 {
		t.Fatalf("got %d groups, want the person split in two plus the third face", len(before))
	}

	code, d := do(t, h, "POST", "/api/people/regroup", `{"ref":"`+before[1]["ref"].(string)+`"}`)
	if code != http.StatusOK {
		t.Fatalf("widen: status %d (%v)", code, d)
	}
	if d["absorbed"].(float64) != 1 {
		t.Errorf("absorbed = %v, want the other half of the split", d["absorbed"])
	}
	if d["faces"].(float64) != 3 || d["match"].(float64) != 0.7 {
		t.Errorf("widened group = %v, want 3 faces at match 0.7", d)
	}

	after := clusterRefs(t, h)
	if len(after) != 2 {
		t.Fatalf("got %d groups after widening, want 2", len(after))
	}
	if after[0]["faces"].(float64) != 3 || after[0]["match"].(float64) != 0.7 {
		t.Errorf("joined group = %v, want 3 faces reported at match 0.7", after[0])
	}
	if _, widened := after[1]["match"]; widened {
		t.Errorf("the untouched group reports a match of its own: %v", after[1])
	}

	// Undo puts the split back, so a widening that pulled in a stranger is not a
	// one-way door.
	code, _ = do(t, h, "DELETE", "/api/people/regroup?ref="+after[0]["ref"].(string), "")
	if code != http.StatusOK {
		t.Fatalf("reset: status %d", code)
	}
	if got := clusterRefs(t, h); len(got) != 3 {
		t.Errorf("got %d groups after resetting, want the original 3", len(got))
	}
}

// Naming a widened group has to label everything the card claimed was in it.
func TestNamingAWidenedGroupLabelsAllOfIt(t *testing.T) {
	_, st, h := peopleServer(t)
	detect(t, st, "a.jpg", 1, 0, 0, 0)
	detect(t, st, "b.jpg", 1, 1, 0, 0)

	ref := clusterRefs(t, h)[0]["ref"].(string)
	if code, d := do(t, h, "POST", "/api/people/regroup", `{"ref":"`+ref+`"}`); code != http.StatusOK {
		t.Fatalf("widen: status %d (%v)", code, d)
	}
	joined := clusterRefs(t, h)
	if len(joined) != 1 {
		t.Fatalf("got %d groups, want them joined", len(joined))
	}
	code, d := do(t, h, "PUT", "/api/people", `{"ref":"`+joined[0]["ref"].(string)+`","name":"Jane"}`)
	if code != http.StatusOK {
		t.Fatalf("name: status %d (%v)", code, d)
	}
	if d["faces"].(float64) != 2 {
		t.Errorf("named %v faces, want both halves of the widened group", d["faces"])
	}
}

func TestWidenGroupRejectsBadRefsAndStopsAtTheFloor(t *testing.T) {
	_, st, h := peopleServer(t)
	detect(t, st, "a.jpg", 1, 0, 0, 0)

	if code, _ := do(t, h, "POST", "/api/people/regroup", `{"ref":"p1"}`); code != http.StatusBadRequest {
		t.Errorf("a person ref got status %d, want 400: only unnamed groups have a threshold", code)
	}
	if code, _ := do(t, h, "POST", "/api/people/regroup", `{"ref":"f9999"}`); code != http.StatusConflict {
		t.Errorf("an unknown group got status %d, want 409", code)
	}

	ref := clusterRefs(t, h)[0]["ref"].(string)
	// 0.8 down to the 0.1 floor in steps of 0.1, then it must refuse rather than
	// silently do nothing.
	for i := 0; i < 7; i++ {
		if code, d := do(t, h, "POST", "/api/people/regroup", `{"ref":"`+ref+`"}`); code != http.StatusOK {
			t.Fatalf("widen %d: status %d (%v)", i, code, d)
		}
	}
	code, _ := do(t, h, "POST", "/api/people/regroup", `{"ref":"`+ref+`"}`)
	if code != http.StatusConflict {
		t.Errorf("widening past the floor got status %d, want 409", code)
	}
}
