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
	got := ClusterUnnamed(faces, 0.8)
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
	if got := ClusterUnnamed(faces, 0.6); len(got) != 1 {
		t.Errorf("threshold 0.6: got %d groups, want them merged", len(got))
	}
	if got := ClusterUnnamed(faces, 0.9); len(got) != 2 {
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
	got := ClusterUnnamed(faces, 0.8)
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
	got := ClusterUnnamed(faces, 0.8)
	if len(got) != 1 || got[0].CoverFaceID != 2 {
		t.Errorf("cover = %+v, want the bigger face 2", got)
	}
	// A real confidence beats size.
	faces[0].Score = 2
	got = ClusterUnnamed(faces, 0.8)
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
	if got := ClusterUnnamed(faces, 0.1); len(got) != 2 {
		t.Errorf("got %d groups, want vectors of different dimension kept apart", len(got))
	}
}

func TestClusterContaining(t *testing.T) {
	faces := []Face{
		face(1, 10, unitVec(1, 0, 0, 0)),
		face(2, 11, unitVec(0.99, 0.01, 0, 0)),
		face(3, 12, unitVec(0, 1, 0, 0)),
	}
	got := ClusterContaining(faces, 0.8, 2)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("got %v, want the group [1 2] that face 2 belongs to", got)
	}
	if got := ClusterContaining(faces, 0.8, 99); got != nil {
		t.Errorf("unknown face returned %v, want nil", got)
	}
	faces[0].PersonID = 5
	if got := ClusterContaining(faces, 0.8, 1); got != nil {
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
