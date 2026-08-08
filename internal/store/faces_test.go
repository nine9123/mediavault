package store

import (
	"math"
	"testing"
)

// unitVec builds a normalized 4-d vector, so the tests can talk in angles rather
// than in float noise. Clustering compares by cosine, so length must be 1.
func unitVec(a, b, c, d float64) []float32 {
	n := math.Sqrt(a*a + b*b + c*c + d*d)
	return []float32{float32(a / n), float32(b / n), float32(c / n), float32(d / n)}
}

func face(id int64, itemID int64, vec []float32) Face {
	return Face{ID: id, ItemID: itemID, Relpath: "x.jpg", W: 0.2, H: 0.2, Score: 1, Vec: vec}
}

func TestClusterUnnamedGroupsAlikeFacesAndSeparatesOthers(t *testing.T) {
	// Two tight neighbourhoods, plus a face on its own.
	faces := []Face{
		face(1, 10, unitVec(1, 0, 0, 0)),
		face(2, 11, unitVec(0.98, 0.02, 0, 0)),
		face(3, 12, unitVec(0.95, 0.05, 0, 0)),
		face(4, 13, unitVec(0, 1, 0, 0)),
		face(5, 14, unitVec(0.02, 0.98, 0, 0)),
		face(6, 15, unitVec(0, 0, 1, 0)),
	}
	got := ClusterUnnamed(faces, 0.8, nil)
	if len(got) != 3 {
		t.Fatalf("got %d groups, want 3: %+v", len(got), got)
	}
	// Biggest group first, so the page leads with the faces worth naming.
	if got[0].Faces != 3 || got[0].KeyFaceID != 1 {
		t.Errorf("first group = %+v, want the 3 faces keyed on face 1", got[0])
	}
	if got[0].Items != 3 {
		t.Errorf("first group spans %d items, want 3", got[0].Items)
	}
	if got[1].Faces != 2 || got[1].KeyFaceID != 4 {
		t.Errorf("second group = %+v, want 2 faces keyed on face 4", got[1])
	}
	if got[2].Faces != 1 || got[2].KeyFaceID != 6 {
		t.Errorf("third group = %+v, want the lone face", got[2])
	}
}

// The threshold is the whole behaviour: too low merges two people, too high
// splits one. It comes from the recognition service or FACE_MATCH, so this pins
// down that changing it actually changes the grouping.
func TestClusterUnnamedRespectsTheThreshold(t *testing.T) {
	faces := []Face{
		face(1, 10, unitVec(1, 0, 0, 0)),
		face(2, 11, unitVec(1, 1, 0, 0)), // cosine 0.707 from face 1
	}
	if got := ClusterUnnamed(faces, 0.6, nil); len(got) != 1 {
		t.Errorf("threshold 0.6: got %d groups, want them merged", len(got))
	}
	if got := ClusterUnnamed(faces, 0.9, nil); len(got) != 2 {
		t.Errorf("threshold 0.9: got %d groups, want them apart", len(got))
	}
}

func TestClusterUnnamedSkipsNamedFaces(t *testing.T) {
	faces := []Face{
		face(1, 10, unitVec(1, 0, 0, 0)),
		face(2, 11, unitVec(1, 0, 0, 0)),
		face(3, 12, unitVec(1, 0, 0, 0)),
	}
	faces[0].PersonID, faces[0].PersonName = 7, "Jane"
	got := ClusterUnnamed(faces, 0.8, nil)
	if len(got) != 1 {
		t.Fatalf("got %d groups, want 1", len(got))
	}
	if got[0].Faces != 2 || got[0].KeyFaceID != 2 {
		t.Errorf("group = %+v, want only the two unnamed faces", got[0])
	}
}

// A cover has to be pickable even from a detector that reports no confidence
// (facerec/ is one), so ties fall back to the largest face in the group.
func TestClusterUnnamedPicksTheBestCover(t *testing.T) {
	faces := []Face{
		face(1, 10, unitVec(1, 0, 0, 0)),
		face(2, 11, unitVec(1, 0, 0, 0)),
	}
	faces[1].W, faces[1].H = 0.5, 0.5
	got := ClusterUnnamed(faces, 0.8, nil)
	if len(got) != 1 || got[0].CoverFaceID != 2 {
		t.Errorf("cover = %+v, want the bigger face 2", got)
	}
	// A real confidence beats size.
	faces[0].Score = 2
	got = ClusterUnnamed(faces, 0.8, nil)
	if got[0].CoverFaceID != 1 {
		t.Errorf("cover = %d, want the higher-scoring face 1", got[0].CoverFaceID)
	}
}

// Vectors of different lengths come from different models. Comparing them is
// meaningless, so they must never land in the same group.
func TestClusterUnnamedNeverMixesDimensions(t *testing.T) {
	faces := []Face{
		face(1, 10, unitVec(1, 0, 0, 0)),
		face(2, 11, []float32{1, 0}),
	}
	if got := ClusterUnnamed(faces, 0.1, nil); len(got) != 2 {
		t.Errorf("got %d groups, want vectors of different dimension kept apart", len(got))
	}
}

// split is the situation widening exists for: one person (faces 1-4) whose
// photographs vary enough to land in two groups at 0.8, and a second person
// (faces 5-6) split the same way but orthogonal to the first. cos between the
// two halves of each pair is ~0.72, so 0.8 keeps them apart and 0.7 joins them.
func split() []Face {
	return []Face{
		face(1, 10, unitVec(1, 0, 0, 0)),
		face(2, 11, unitVec(0.99, 0.01, 0, 0)),
		face(3, 12, unitVec(1, 1, 0, 0)),
		face(4, 13, unitVec(1.02, 0.98, 0, 0)),
		face(5, 14, unitVec(0, 0, 1, 0)),
		face(6, 15, unitVec(0, 0, 1, 1)),
	}
}

func TestWidenJoinsASplitGroupFromEitherHalf(t *testing.T) {
	// Which half the user clicks must not matter: the greedy pass walks faces in
	// id order, so a threshold applied during it would only ever pull in later
	// faces — widening the second half would silently do nothing.
	for _, key := range []int64{1, 3} {
		faces := split()
		got := ClusterUnnamed(faces, 0.8, Widened{key: 0.7})
		if len(got) != 3 {
			t.Fatalf("widening on face %d: got %d groups, want 3 (the joined pair + the other two)", key, len(got))
		}
		if got[0].Faces != 4 || got[0].KeyFaceID != 1 {
			t.Errorf("widening on face %d: joined group = %+v, want 4 faces keyed on face 1", key, got[0])
		}
		if got[0].Match != 0.7 {
			t.Errorf("widening on face %d: group match = %v, want 0.7", key, got[0].Match)
		}
		if got[0].Items != 4 {
			t.Errorf("widening on face %d: group spans %d items, want 4", key, got[0].Items)
		}
	}
}

// The point of widening one group rather than lowering FACE_MATCH: everyone
// else stays grouped the way they were.
func TestWidenLeavesEveryOtherGroupAlone(t *testing.T) {
	faces := split()
	if got := ClusterUnnamed(faces, 0.8, nil); len(got) != 4 {
		t.Fatalf("baseline: got %d groups, want 4", len(got))
	}
	got := ClusterUnnamed(faces, 0.8, Widened{1: 0.7})
	if len(got) != 3 {
		t.Fatalf("got %d groups, want 3", len(got))
	}
	for _, c := range got[1:] {
		if c.Faces != 1 {
			t.Errorf("untouched group %+v gained faces; only the widened one should regroup", c)
		}
		if c.Match != 0.8 {
			t.Errorf("untouched group %+v reports match %v, want the library's 0.8", c, c.Match)
		}
	}
	// The same faces at a library-wide 0.7 would join the second person too —
	// which is exactly the collateral damage widening avoids.
	if all := ClusterUnnamed(faces, 0.7, nil); len(all) != 2 {
		t.Errorf("sanity: at a global 0.7 got %d groups, want both pairs joined", len(all))
	}
}

func TestWidenIgnoresOverridesOnFacesThatAreGone(t *testing.T) {
	faces := split()
	got := ClusterUnnamed(faces, 0.8, Widened{99: 0.1})
	if len(got) != 4 {
		t.Errorf("got %d groups, want the ungrouped 4: an override on an unknown face means nothing", len(got))
	}
}

// Naming a widened group has to label everything the page showed in it, not the
// subset the library-wide threshold would have found.
func TestClusterContainingFollowsAWidening(t *testing.T) {
	faces := split()
	got := ClusterContaining(faces, 0.8, Widened{3: 0.7}, 3)
	if len(got) != 4 {
		t.Errorf("got %v, want all four faces of the widened group", got)
	}
}

func TestClusterContaining(t *testing.T) {
	faces := []Face{
		face(1, 10, unitVec(1, 0, 0, 0)),
		face(2, 11, unitVec(0.99, 0.01, 0, 0)),
		face(3, 12, unitVec(0, 1, 0, 0)),
	}
	got := ClusterContaining(faces, 0.8, nil, 2)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("got %v, want the group [1 2] that face 2 belongs to", got)
	}
	if got := ClusterContaining(faces, 0.8, nil, 99); got != nil {
		t.Errorf("unknown face returned %v, want nil", got)
	}
	faces[0].PersonID = 5
	if got := ClusterContaining(faces, 0.8, nil, 1); got != nil {
		t.Errorf("an already-named face returned %v, want nil", got)
	}
}

func TestMatchPerson(t *testing.T) {
	protos := map[int64][][]float32{
		1: {unitVec(1, 0, 0, 0), unitVec(0, 0, 1, 0)}, // one person, two looks
		2: {unitVec(0, 1, 0, 0)},
	}
	for _, tc := range []struct {
		name string
		vec  []float32
		want int64
	}{
		{"matches the first example", unitVec(0.99, 0.01, 0, 0), 1},
		// Examples are compared individually, not averaged: a person's photos
		// legitimately form several tight groups, and their mean is close to none.
		{"matches the second example of the same person", unitVec(0, 0.02, 0.98, 0), 1},
		{"matches the other person", unitVec(0.02, 0.98, 0, 0), 2},
		{"matches nobody", unitVec(0, 0, 0, 1), 0},
		{"wrong dimension matches nobody", []float32{1, 0}, 0},
	} {
		if got := MatchPerson(protos, tc.vec, 0.8); got != tc.want {
			t.Errorf("%s: MatchPerson = %d, want %d", tc.name, got, tc.want)
		}
	}
}
