package faces

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"mediavault/internal/config"
	"mediavault/internal/face"
	"mediavault/internal/model"
	"mediavault/internal/store"
)

func sampleCfg() config.Config {
	return config.Config{FaceVideoEvery: 1, FaceVideoFrames: 20, FaceFramePx: 1280}
}

func TestSampleTimesScrubsShortVideosEverySecond(t *testing.T) {
	got := sampleTimes(sampleCfg(), model.MediaVideo, 10)
	if len(got) != 10 {
		t.Fatalf("a 10s clip got %d sample points, want one a second: %v", len(got), got)
	}
	prev := -1.0
	for _, at := range got {
		if at <= 0 || at >= 10 {
			t.Errorf("sample at %.3fs is outside the clip", at)
		}
		if at <= prev {
			t.Errorf("sample points are not increasing: %v", got)
		}
		prev = at
	}
	// Never zero: t=0 is the fade-in, and a zero timestamp is also how a still
	// says it hasn't got one.
	if got[0] <= 0 {
		t.Errorf("first sample is at %.3fs, want it past the start", got[0])
	}
}

func TestSampleTimesSpreadsLongVideosAcrossTheWholeThing(t *testing.T) {
	const hour = 3600.0
	got := sampleTimes(sampleCfg(), model.MediaVideo, hour)
	if len(got) != 20 {
		t.Fatalf("an hour-long video got %d sample points, want the cap of 20", len(got))
	}
	// Sparser, not just the first twenty seconds — the whole point of the cap is
	// that a long video is still sampled end to end.
	if got[0] > hour/10 || got[len(got)-1] < hour*0.9 {
		t.Errorf("samples run %.0fs..%.0fs of an hour, want them spread across it", got[0], got[len(got)-1])
	}
}

func TestSampleTimesLeavesEverythingElseToThePosterFrame(t *testing.T) {
	cfg := sampleCfg()
	for _, tc := range []struct {
		name     string
		typ      model.MediaType
		duration float64
		cfg      config.Config
	}{
		{"a still", model.MediaImage, 0, cfg},
		{"an animated gif", model.MediaAnimated, 4, cfg},
		{"a video of unknown length", model.MediaVideo, 0, cfg},
		{"sampling turned off", model.MediaVideo, 60, config.Config{FaceVideoEvery: 0, FaceVideoFrames: 20}},
		{"no frames allowed", model.MediaVideo, 60, config.Config{FaceVideoEvery: 1, FaceVideoFrames: 0}},
	} {
		if got := sampleTimes(tc.cfg, tc.typ, tc.duration); got != nil {
			t.Errorf("%s: got sample points %v, want the single poster frame", tc.name, got)
		}
	}
}

// A clip shorter than one sample interval still gets looked at once.
func TestSampleTimesAlwaysSamplesSomething(t *testing.T) {
	got := sampleTimes(sampleCfg(), model.MediaVideo, 0.4)
	if len(got) != 1 || got[0] <= 0 || got[0] >= 0.4 {
		t.Errorf("a 0.4s clip got %v, want one point inside it", got)
	}
}

func det(x, y, w, h, score float64, v ...float32) face.Detection {
	if len(v) == 0 {
		v = vec(1, 0, 0, 0)
	}
	return face.Detection{X: x, Y: y, W: w, H: h, Score: score, Vec: v}
}

func TestDedupeBoxesKeepsTheClosestLookAtEachFace(t *testing.T) {
	// The same face, found in the whole frame and again in the tile that covers
	// that corner. The tile saw it in more detail, so its embedding is the one
	// worth keeping — and either way it is one face, not two.
	whole := found{det: det(0.10, 0.10, 0.06, 0.08, 1), src: 1}
	tile := found{det: det(0.11, 0.11, 0.06, 0.08, 1), src: 0.25}
	other := found{det: det(0.70, 0.60, 0.05, 0.07, 1), src: 0.25}

	got := dedupeBoxes([]found{whole, tile, other})
	if len(got) != 2 {
		t.Fatalf("got %d faces, want the duplicate collapsed: %+v", len(got), got)
	}
	if got[0].src != 0.25 || got[0].det.X != 0.11 {
		t.Errorf("kept %+v, want the copy found inside a tile", got[0].det)
	}
}

func TestDedupeBoxesKeepsTwoPeopleStandingCloseTogether(t *testing.T) {
	a := found{det: det(0.30, 0.30, 0.08, 0.10, 1), src: 1}
	b := found{det: det(0.39, 0.30, 0.08, 0.10, 1), src: 1} // shoulder to shoulder
	if got := dedupeBoxes([]found{a, b}); len(got) != 2 {
		t.Errorf("got %d faces, want both: %+v", len(got), got)
	}
}

func TestDedupeBoxesDropsHalfAFaceCutByATileEdge(t *testing.T) {
	whole := found{det: det(0.48, 0.30, 0.10, 0.12, 1), src: 1}
	half := found{det: det(0.48, 0.30, 0.05, 0.12, 1), src: 0.25} // clipped at the seam
	got := dedupeBoxes([]found{whole, half})
	if len(got) != 1 {
		t.Fatalf("got %d faces, want the clipped one dropped: %+v", len(got), got)
	}
	if got[0].det.W != 0.10 {
		t.Errorf("kept the %.2f-wide box, want the whole face", got[0].det.W)
	}
}

func TestUniquePeopleTurnsAVideoIntoWhoIsInIt(t *testing.T) {
	jane, bob := vec(1, 0, 0, 0), vec(0, 1, 0, 0)
	in := []found{
		{det: det(0.1, 0.1, 0.05, 0.06, 1, jane...), t: 1.5},
		{det: det(0.1, 0.1, 0.05, 0.06, 1, jane...), t: 2.5},
		// Jane again, closer to the camera: the shot worth remembering her by.
		{det: det(0.3, 0.2, 0.20, 0.24, 1, vec(0.99, 0.01, 0, 0)...), t: 3.5},
		{det: det(0.6, 0.2, 0.08, 0.10, 1, bob...), t: 4.5},
		{det: det(0.6, 0.2, 0.08, 0.10, 1, vec(0.02, 0.98, 0, 0)...), t: 5.5},
	}
	got := uniquePeople(in, 0.8)
	if len(got) != 2 {
		t.Fatalf("got %d faces from a clip with two people in it: %+v", len(got), got)
	}
	if got[0].t != 3.5 {
		t.Errorf("kept Jane at %.1fs, want the frame she is largest in (3.5s)", got[0].t)
	}
	if got[1].t != 4.5 {
		t.Errorf("kept Bob at %.1fs, want his first (and equally good) frame", got[1].t)
	}
	// In the order they appear, which is how the viewer lists them.
	if got[0].t > got[1].t {
		t.Errorf("faces came back out of order: %v, %v", got[0].t, got[1].t)
	}
}

func TestUniquePeopleKeepsStrangersApart(t *testing.T) {
	in := []found{
		{det: det(0.1, 0.1, 0.05, 0.06, 1, vec(1, 0, 0, 0)...), t: 1},
		{det: det(0.1, 0.1, 0.05, 0.06, 1, vec(0, 1, 0, 0)...), t: 2},
		{det: det(0.1, 0.1, 0.05, 0.06, 1, vec(0, 0, 1, 0)...), t: 3},
	}
	if got := uniquePeople(in, 0.8); len(got) != 3 {
		t.Errorf("got %d faces, want all three strangers: %+v", len(got), got)
	}
}

// The timestamp has to survive into the row, or the crop is cut from the poster
// frame and shows whoever was on screen at second two.
func TestToWritesCarriesTheTimestamp(t *testing.T) {
	got := toWrites([]found{
		{det: det(0.1, 0.1, 0.05, 0.06, 1), t: 12.25},
		{det: det(0.5, 0.1, 0.05, 0.06, 1)},
	}, 20)
	if len(got) != 2 {
		t.Fatalf("got %d writes, want 2", len(got))
	}
	if got[0].T != 12.25 {
		t.Errorf("first write is at %.2fs, want 12.25", got[0].T)
	}
	if got[1].T != 0 {
		t.Errorf("a still carries t=%.2f, want 0 (stored as NULL)", got[1].T)
	}
}

// The round trip that makes a video face findable: the timestamp goes into the
// row and comes back out as the point to cut the crop from. A still has no such
// number, and falls back to the item's poster offset the way it always did.
func TestTimestampsSurviveTheStoreAndDecideWhereToCrop(t *testing.T) {
	ctx := context.Background()
	_, st, _ := setup(t)
	addVideo(t, st, "clip.mp4", 60, 4)
	addItem(t, st, "photo.jpg")

	if err := st.SetFaces(ctx, "clip.mp4", []store.FaceWrite{
		{X: 0.1, Y: 0.1, W: 0.1, H: 0.12, Score: 1, T: 12.5, Vec: vec(1, 0, 0, 0)},
		{X: 0.6, Y: 0.1, W: 0.1, H: 0.12, Score: 1, T: 41, Vec: vec(0, 1, 0, 0)},
		// A face with no timestamp: what a video detected before this existed
		// looks like, and what one looks like when scrubbing is turned off.
		{X: 0.3, Y: 0.5, W: 0.1, H: 0.12, Score: 1, Vec: vec(0, 0, 0, 1)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetFaces(ctx, "photo.jpg", []store.FaceWrite{
		{X: 0.2, Y: 0.2, W: 0.2, H: 0.24, Score: 1, Vec: vec(0, 0, 1, 0)},
	}); err != nil {
		t.Fatal(err)
	}

	all, err := st.AllFaces(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at := map[string][]float64{}
	crop := map[string][]float64{}
	for _, f := range all {
		at[f.Relpath] = append(at[f.Relpath], f.T)
		crop[f.Relpath] = append(crop[f.Relpath], f.CropAt())
	}
	if want := []float64{12.5, 41, 0}; !equalTimes(at["clip.mp4"], want) {
		t.Errorf("clip.mp4 faces are at %v, want %v", at["clip.mp4"], want)
	}
	// Each face is cut from its own frame; the untimed one falls back to the
	// item's poster offset, which is where it was found.
	if !equalTimes(crop["clip.mp4"], []float64{12.5, 41, 4}) {
		t.Errorf("clip.mp4 crops would be cut at %v, want 12.5s, 41s and the poster offset", crop["clip.mp4"])
	}
	if !equalTimes(at["photo.jpg"], []float64{0}) {
		t.Errorf("a still carries t=%v, want none", at["photo.jpg"])
	}
	// No timestamp of its own, so the item's poster offset stands — the same
	// frame every earlier version cropped from.
	if !equalTimes(crop["photo.jpg"], []float64{0}) {
		t.Errorf("a still would be cropped at %v, want its poster offset", crop["photo.jpg"])
	}
}

func equalTimes(got, want []float64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			return false
		}
	}
	return true
}

func TestToWritesKeepsTheBestFacesWhenThereAreTooMany(t *testing.T) {
	in := []found{
		{det: det(0.1, 0.1, 0.05, 0.06, 0.3)},
		{det: det(0.2, 0.1, 0.05, 0.06, 0.9)},
		{det: det(0.3, 0.1, 0.05, 0.06, 0.6)},
	}
	got := toWrites(in, 2)
	if len(got) != 2 {
		t.Fatalf("got %d writes, want the cap of 2", len(got))
	}
	if got[0].Score != 0.9 || got[1].Score != 0.6 {
		t.Errorf("kept scores %.1f and %.1f, want the two highest", got[0].Score, got[1].Score)
	}
}

// ---- end to end, with a real ffmpeg and a stub detector ----
//
// The stub is a detector that can only see colour: it decodes the frame it is
// given and reports one face whose "identity" is the dominant colour, or no face
// at all if the frame has no dominant colour. That is enough to check the two
// things the unit tests above cannot, because both of them happen between
// processes: that the frames we ask ffmpeg for are the frames we think (the
// right point of a video, the right corner of a photograph), and that what comes
// back is mapped onto the whole item correctly.

// stubDetector serves the /detect contract, answering by dominant colour.
func stubDetector(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		img, err := jpeg.Decode(bytes.NewReader(body))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		faces := []map[string]any{}
		if v := dominant(img); v != nil {
			faces = append(faces, map[string]any{
				// Middle of whatever frame this is — a tile's middle is not the
				// picture's, which is the point.
				"box": []float64{0.4, 0.4, 0.2, 0.2}, "score": 1.0, "embedding": v,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "stub", "dim": 3, "match": 0.8, "faces": faces})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// dominant returns a one-hot vector for the colour covering more than half the
// image, or nil when no colour does.
func dominant(img image.Image) []float64 {
	b := img.Bounds()
	var count [3]int
	total := 0
	for y := b.Min.Y; y < b.Max.Y; y += 4 {
		for x := b.Min.X; x < b.Max.X; x += 4 {
			r, g, bl, _ := img.At(x, y).RGBA()
			total++
			switch {
			case r > 0x8000 && g < 0x4000 && bl < 0x4000:
				count[0]++
			case g > 0x8000 && r < 0x4000 && bl < 0x4000:
				count[1]++
			case bl > 0x8000 && r < 0x4000 && g < 0x4000:
				count[2]++
			}
		}
	}
	for i, n := range count {
		if total > 0 && n*2 > total {
			v := make([]float64, 3)
			v[i] = 1
			return v
		}
	}
	return nil
}

func ffmpegCfg(t *testing.T, url string) config.Config {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; skipping integration test")
	}
	dir := t.TempDir()
	return config.Config{
		MediaRoot: dir, DataDir: dir, SidecarDir: dir,
		FFmpeg: "ffmpeg", FaceURL: url, FaceMatch: 0.8,
		FaceFramePx: 1280, FaceTiles: 2, FaceVideoEvery: 1, FaceVideoFrames: 20,
		FaceMaxPerItem: 20, FaceTimeout: time.Minute,
	}
}

// A video of three people, one after another: scrubbing has to find all three,
// each at their own point, and each of them once however many frames they are in.
func TestDetectScrubsAVideoAndReportsEachPersonOnce(t *testing.T) {
	srv := stubDetector(t)
	cfg := ffmpegCfg(t, srv.URL)
	rec := New(cfg, nil)

	// Six seconds: red, then green, then blue, two seconds each.
	src := filepath.Join(cfg.MediaRoot, "party.mp4")
	mk := exec.Command("ffmpeg", "-nostdin", "-v", "error",
		"-f", "lavfi", "-i", "color=c=black:s=320x240:r=10:d=6",
		"-vf", "geq=r='if(lt(T,2),255,0)':g='if(between(T,2,4),255,0)':b='if(gt(T,4),255,0)'",
		"-c:v", "mpeg4", "-y", src)
	if out, err := mk.CombinedOutput(); err != nil {
		t.Fatalf("building fixture: %v: %s", err, out)
	}

	got, frameErr, err := rec.detect(context.Background(), model.Item{
		Relpath: "party.mp4", Type: model.MediaVideo, Duration: 6, Width: 320, Height: 240,
	})
	if err != nil || frameErr {
		t.Fatalf("detect: %v (frame error: %v)", err, frameErr)
	}
	if len(got) != 3 {
		t.Fatalf("found %d people in a video of three, one after another: %+v", len(got), times(got))
	}
	// One entry each, in the order they appear, and each timestamp inside the
	// stretch of video that person is actually on screen — which is what makes
	// the crop show them and the "jump to" in the viewer land on them.
	for i, want := range [][2]float64{{0, 2}, {2, 4}, {4, 6}} {
		if got[i].t < want[0] || got[i].t > want[1] {
			t.Errorf("person %d is reported at %.2fs, want them between %.0fs and %.0fs (all: %v)",
				i+1, got[i].t, want[0], want[1], times(got))
		}
	}
}

// A photograph too big for one detection: the faces are found in the tiles, and
// have to come back positioned in the picture rather than in the tile.
func TestDetectTilesAStillAndPutsTheFacesBackInTheFrame(t *testing.T) {
	srv := stubDetector(t)
	cfg := ffmpegCfg(t, srv.URL)
	rec := New(cfg, nil)

	// Four quadrants of flat colour. No colour dominates the whole picture, so
	// the stub finds nobody in the whole-frame pass and one "face" per tile —
	// the same way a real detector finds nothing in a shrunk-down group shot and
	// everybody once it is looked at in pieces.
	const w, h = 2400, 1800
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	quad := []color.RGBA{{R: 255, A: 255}, {G: 255, A: 255}, {B: 255, A: 255}, {R: 255, G: 255, A: 255}}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := 0
			if x >= w/2 {
				i++
			}
			if y >= h/2 {
				i += 2
			}
			img.Set(x, y, quad[i])
		}
	}
	src := filepath.Join(cfg.MediaRoot, "crowd.jpg")
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, frameErr, err := rec.detect(context.Background(), model.Item{
		Relpath: "crowd.jpg", Type: model.MediaImage, Width: w, Height: h,
	})
	if err != nil || frameErr {
		t.Fatalf("detect: %v (frame error: %v)", err, frameErr)
	}
	// The bottom-right quadrant is yellow, which the stub doesn't recognise, so
	// three of the four tiles report a face.
	if len(got) != 3 {
		t.Fatalf("found %d faces, want one per tile the stub recognises: %+v", len(got), got)
	}
	seen := map[[2]bool]bool{}
	for _, d := range got {
		cx, cy := d.det.X+d.det.W/2, d.det.Y+d.det.H/2
		if cx <= 0 || cx >= 1 || cy <= 0 || cy >= 1 {
			t.Errorf("face centred at (%.3f,%.3f) is outside the picture", cx, cy)
		}
		// Each tile's face has to land in that tile's quadrant. Landing all
		// three in the middle would mean the boxes were never projected.
		seen[[2]bool{cx > 0.5, cy > 0.5}] = true
	}
	if len(seen) != 3 {
		t.Errorf("the three faces landed in %d different quadrants, want 3", len(seen))
	}
	if d := got[0].det; d.W >= 0.2 {
		t.Errorf("a face found in a tile is %.3f of the picture wide, want it scaled down to the tile", d.W)
	}
}

func times(in []found) []float64 {
	out := make([]float64, 0, len(in))
	for _, f := range in {
		out = append(out, f.t)
	}
	return out
}
