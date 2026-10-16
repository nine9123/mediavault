package store

import (
	"context"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"mediavault/internal/model"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testWrite(relpath string, vec []float32) ItemWrite {
	now := time.Unix(1700000000, 0)
	return ItemWrite{
		Item: model.Item{Relpath: relpath, Size: 1, MTime: now, Type: model.MediaImage,
			MIME: "image/jpeg", Width: 10, Height: 10, IndexedAt: now, SeenAt: now},
		Vector: vec,
	}
}

// topScores must return exactly what sorting everything and truncating would.
func TestTopScoresMatchesAFullSort(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, k := range []int{0, 1, 5, 50, 200} {
		var all []scored
		top := newTopScores(k)
		for i := 0; i < 100; i++ {
			sc := scored{int64(i), rng.Float64()}
			all = append(all, sc)
			top.offer(sc.id, sc.score)
		}
		sort.Slice(all, func(i, j int) bool { return all[i].score > all[j].score })
		if len(all) > k {
			all = all[:k]
		}
		got := top.sorted()
		if len(got) != len(all) {
			t.Fatalf("k=%d: got %d results, want %d", k, len(got), len(all))
		}
		for i := range all {
			if got[i] != all[i] {
				t.Fatalf("k=%d: result %d = %+v, want %+v", k, i, got[i], all[i])
			}
		}
	}
}

// Vectors written before the first similarity search are found by it (they come
// from the table), and vectors written after it are found too (from the cache).
func TestSimilarItemsSeesVectorsWrittenBeforeAndAfterLoading(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.WriteBatch(ctx, []ItemWrite{
		testWrite("a.jpg", unitVec(1, 0, 0, 0)),
		testWrite("b.jpg", unitVec(0.9, 0.1, 0, 0)),
		testWrite("c.jpg", unitVec(0, 1, 0, 0)),
	}); err != nil {
		t.Fatal(err)
	}
	if s.vecs != nil {
		t.Errorf("vectors cached before any similarity search: %d", len(s.vecs))
	}

	got, err := s.SimilarItems(ctx, "a.jpg", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Relpath != "b.jpg" || got[1].Relpath != "c.jpg" {
		t.Fatalf("similar to a.jpg = %v, want [b.jpg c.jpg]", relpaths(got))
	}

	if err := s.WriteBatch(ctx, []ItemWrite{testWrite("d.jpg", unitVec(1, 0.01, 0, 0))}); err != nil {
		t.Fatal(err)
	}
	got, err = s.SimilarItems(ctx, "a.jpg", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Relpath != "d.jpg" {
		t.Fatalf("most similar to a.jpg after a new write = %v, want [d.jpg]", relpaths(got))
	}
}

// FaceByID and FacesForItem skip the embedding; AllFaces, which clustering runs
// on, still carries it.
func TestFaceLookupsLoadEmbeddingsOnlyForClustering(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.WriteBatch(ctx, []ItemWrite{testWrite("p.jpg", nil)}); err != nil {
		t.Fatal(err)
	}
	vec := unitVec(1, 2, 3, 4)
	if err := s.SetFaces(ctx, "p.jpg", []FaceWrite{{X: 0.1, Y: 0.2, W: 0.3, H: 0.4, Score: 0.9, Vec: vec}}); err != nil {
		t.Fatal(err)
	}

	all, err := s.AllFaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || len(all[0].Vec) != len(vec) || all[0].Vec[3] != vec[3] {
		t.Fatalf("AllFaces = %+v, want one face carrying its vector", all)
	}

	f, err := s.FaceByID(ctx, all[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Relpath != "p.jpg" || f.X != 0.1 || f.H != 0.4 || f.Vec != nil {
		t.Errorf("FaceByID = %+v, want p.jpg's face without a vector", f)
	}

	list, err := s.FacesForItem(ctx, all[0].ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != all[0].ID || list[0].Vec != nil {
		t.Errorf("FacesForItem = %+v, want the one face without a vector", list)
	}
}

func relpaths(items []model.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Relpath
	}
	return out
}
