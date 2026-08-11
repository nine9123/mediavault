package faces

// How one item is turned into a set of faces.
//
// The naive version — one frame, one detection — misses two whole categories of
// people, and both of them are the common case rather than an edge:
//
//   - In a still, everyone who is small. A detector processes a bounded number
//     of pixels (see internal/media.TileFrame), so in a 6000px group shot the
//     people at the back are twenty pixels across by the time it looks. The
//     answer is to look at the picture in pieces as well as whole.
//   - In a video, everyone who isn't on screen at second two. A film is a
//     sequence of different rooms with different people in them, and the poster
//     frame is one of them. The answer is to scrub: sample across the whole
//     duration, detect on each frame, and then collapse the results, because the
//     same person in thirty frames is one person and not thirty faces.
//
// Both of them produce several detections of the same face and have to end with
// a merge, but not the same merge, and the difference is what they mean. Two
// tiles of one photograph that both found a face at the same place found ONE
// face — the same pixels, seen twice — so overlap decides. Two frames of a video
// that both found a face found the same PERSON in two places at two times, which
// only the embeddings can say, and what survives is the frame they are clearest
// in plus the timestamp of it.

import (
	"context"
	"log"
	"math"
	"path/filepath"
	"sort"

	"mediavault/internal/config"
	"mediavault/internal/face"
	"mediavault/internal/media"
	"mediavault/internal/model"
	"mediavault/internal/store"
)

// maxStillPx bounds the frame asked of ffmpeg for a tiled still. Tiling wants a
// frame several times the size one detection can use, and this is where "several
// times" stops being useful: past it the tiles are bigger than any detector will
// look at whole, so the extra pixels are decoded, cut up and then thrown away by
// the service.
const maxStillPx = 4096

// dupIoU is how much two boxes must overlap to be called the same face. Well
// below the 0.5 an evaluation harness would use, because the duplicates here are
// not near-misses from different models but the same face seen in two
// overlapping tiles — and the ones that land lower are the halves of a face cut
// by a tile edge, which is exactly what should lose to the whole one.
const dupIoU = 0.35

// found is one detection plus where in the item it was made.
type found struct {
	det face.Detection
	t   float64 // seconds into a video; 0 for a still
	// area of the image it was detected in, as a fraction of the whole frame. A
	// face found in a tile was looked at in more detail than the same face found
	// in the whole frame, so when the two are duplicates of each other, this is
	// which one to keep.
	src float64
}

// detect finds the faces in one item, whatever kind of item it is. The bool
// reports a failure to get a frame at all (an unreadable file, a codec ffmpeg
// won't decode) as opposed to a failure to detect on one, which the caller
// treats differently: the first is about this file, the second may be about the
// service.
func (r *Recognizer) detect(ctx context.Context, it model.Item) ([]found, bool, error) {
	abs := filepath.Join(r.cfg.MediaRoot, filepath.FromSlash(it.Relpath))
	if times := sampleTimes(r.cfg, it.Type, it.Duration); len(times) > 0 {
		return r.detectVideo(ctx, abs, it, times)
	}
	return r.detectStill(ctx, abs, it)
}

// sampleTimes is where in a video to look for faces: one frame every
// FACE_VIDEO_EVERY seconds, at most FACE_VIDEO_FRAMES of them, spread evenly
// across the whole duration.
//
// The times are the midpoints of equal slices rather than a fixed cadence from
// zero, which is the same choice internal/media.Preview makes and for the same
// reasons: it skips the fade-in at the start and the credits at the end without
// special-casing either, and a long video degrades into a sparser scrub of the
// whole thing instead of a dense look at its first thirty seconds.
//
// Empty means "not a video, or nothing to scrub": the caller falls back to the
// single poster frame, which is also what happens when the duration is unknown.
func sampleTimes(cfg config.Config, typ model.MediaType, duration float64) []float64 {
	// Animated GIFs are deliberately not here. They are one short loop of one
	// scene, and — unlike video — they have no seekable timeline the face crop
	// could be cut from later (see media.PosterSeek), so a timestamp would be a
	// number nothing could act on.
	if typ != model.MediaVideo || duration <= 0 {
		return nil
	}
	every, limit := cfg.FaceVideoEvery, cfg.FaceVideoFrames
	if every <= 0 || limit < 1 {
		return nil // sampling turned off; the poster frame it is
	}
	n := int(duration / every)
	if n < 1 {
		n = 1
	}
	if n > limit {
		n = limit
	}
	out := make([]float64, 0, n)
	for i := 0; i < n; i++ {
		t := duration * (float64(i) + 0.5) / float64(n)
		out = append(out, math.Round(t*1000)/1000) // ms is finer than any seek is accurate
	}
	return out
}

// detectVideo scrubs a video and returns one face per person in it.
//
// A frame that won't come out of the file is skipped — a seek landing in a
// corrupt region or past a truncated tail costs that sample and nothing else.
// A frame the service rejects (4xx) is skipped for the same reason. Anything
// else is the service being unwell rather than this frame being bad, and that
// abandons the whole item: recording half a video's people would mark it done
// and never look at the rest.
func (r *Recognizer) detectVideo(ctx context.Context, abs string, it model.Item, times []float64) ([]found, bool, error) {
	var out []found
	var lastErr error
	ok, badFrames, rejected := 0, 0, 0
	for _, t := range times {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		jpeg, err := media.FrameAtJPEG(ctx, r.cfg, abs, t, r.cfg.FaceFramePx)
		if err != nil {
			lastErr, badFrames = err, badFrames+1
			continue
		}
		dets, err := face.Detect(ctx, r.cfg, jpeg)
		if err != nil {
			if !face.PermanentError(err) {
				return nil, false, err
			}
			lastErr, rejected = err, rejected+1
			continue
		}
		ok++
		for _, d := range dets {
			out = append(out, found{det: d, t: t, src: 1})
		}
	}
	if ok == 0 {
		// Nothing came back at all. Which kind of failure it was decides whether
		// the caller blames the file or the frame, so hand back the last one.
		return nil, rejected == 0, lastErr
	}
	if badFrames+rejected > 0 {
		// Said out loud: a video sampled at three of its twenty points has been
		// looked at, and "no faces found" from it means much less than usual.
		log.Printf("faces: %s: sampled %d of %d points (%d unreadable, %d rejected): %v",
			it.Relpath, ok, len(times), badFrames, rejected, lastErr)
	}
	return uniquePeople(out, r.Match()), false, nil
}

// detectStill detects on one frame, and — when the picture is big enough for it
// to mean anything — on a grid of overlapping tiles of that same frame as well.
//
// The tiles are a second look, not the answer: the whole-frame pass has already
// found everybody who is large in the picture, so a tile the service refuses
// costs the small faces in that corner and nothing more. That is why a failure
// here stops the tiling and keeps what is in hand, where the same failure while
// scrubbing a video abandons the item.
func (r *Recognizer) detectStill(ctx context.Context, abs string, it model.Item) ([]found, bool, error) {
	grid := r.cfg.FaceTiles
	px := r.cfg.FaceFramePx
	if grid > 1 {
		// Ask for the frame the TILES want. ffmpeg never upscales, so an image
		// that has no such detail simply arrives at its own size and TileFrame
		// declines to cut it up.
		px = min(px*grid, maxStillPx)
	}
	jpeg, err := media.FrameJPEG(ctx, r.cfg, abs, it.Type, it.Offset, px)
	if err != nil {
		return nil, true, err
	}
	dets, err := face.Detect(ctx, r.cfg, jpeg)
	if err != nil {
		return nil, false, err
	}
	out := make([]found, 0, len(dets))
	for _, d := range dets {
		out = append(out, found{det: d, src: 1})
	}

	tiles, err := media.TileFrame(jpeg, grid, tileMinEdge(r.cfg))
	if err != nil {
		// We couldn't decode what the service just detected on, so the frame is
		// fine and this is our problem. Keep the faces it found.
		log.Printf("faces: tile %s: %v (whole frame only)", it.Relpath, err)
		return dedupeBoxes(out), false, nil
	}
	for _, tl := range tiles {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		td, err := face.Detect(ctx, r.cfg, tl.JPEG)
		if err != nil {
			log.Printf("faces: tiles %s: %v (keeping the whole-frame faces)", it.Relpath, err)
			break
		}
		for _, d := range td {
			d.X, d.Y, d.W, d.H = tl.Project(d.X, d.Y, d.W, d.H)
			out = append(out, found{det: d, src: tl.W * tl.H})
		}
	}
	return dedupeBoxes(out), false, nil
}

// tileMinEdge is the frame size below which tiling is not worth the detections.
// A tile is looked at whole where the frame around it would have been shrunk, so
// the gain is the ratio between them — and at less than half as much again, four
// more calls to the service buy a resolution the whole-frame pass nearly had.
func tileMinEdge(cfg config.Config) int { return cfg.FaceFramePx * 3 / 2 }

// dedupeBoxes keeps one detection per face: the best-looking one first, then
// anything that doesn't land on top of something already kept.
func dedupeBoxes(in []found) []found {
	if len(in) < 2 {
		return in
	}
	order := make([]int, len(in))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return closerLook(in[order[a]], in[order[b]]) })
	kept := make([]found, 0, len(in))
	for _, i := range order {
		dup := false
		for _, k := range kept {
			if iou(box(in[i]), box(k)) >= dupIoU {
				dup = true
				break
			}
		}
		if !dup {
			kept = append(kept, in[i])
		}
	}
	return kept
}

// closerLook reports whether a is the copy of a duplicated face to keep.
//
// Confidence leads, because a detector that reports it is saying something about
// this face. Then size, but only when the two disagree about it by more than a
// rounding: two boxes of much the same size are the same face seen twice, while
// a box half the size of the other one is a face a tile edge cut in half, and
// what is worth keeping there is the whole face from the frame.
//
// Size settled, the closer look wins — a face cut from a tile was described from
// several times as many pixels as the same face in the shrunk-down whole frame,
// so its embedding is the better one to cluster and name with. That is the whole
// reason for tiling, and it would be thrown away by keeping either copy.
func closerLook(a, b found) bool {
	if a.det.Score != b.det.Score {
		return a.det.Score > b.det.Score
	}
	aa, ab := area(a), area(b)
	if math.Abs(aa-ab) > sameSize*math.Max(aa, ab) {
		return aa > ab
	}
	if a.src != b.src {
		return a.src < b.src
	}
	return aa > ab
}

// sameSize is how much two boxes for one face may differ in area before the
// difference is read as one of them being clipped rather than as two detectors'
// worth of disagreement about where a jaw ends.
const sameSize = 0.15

// uniquePeople collapses a video's detections to one per person: everybody who
// appears in it, once, at the point they are seen most clearly.
//
// The grouping is store's, at the same threshold the People page uses, so a
// video can never contribute two entries the page would have shown as one group
// anyway — and the twenty copies of the one person a scrub produces arrive as
// the single face they are, instead of a group of twenty that swamps everyone
// photographed twice.
//
// The surviving frame is the clearest rather than the earliest on purpose: it is
// where the face crop is cut from and where the embedding came from, so the two
// have to be the same frame, and a person who walks in from the edge of shot is
// unrecognisable in the frame they first appear in.
func uniquePeople(in []found, match float64) []found {
	if len(in) < 2 {
		return in
	}
	vecs := make([][]float32, len(in))
	for i, f := range in {
		vecs[i] = f.det.Vec
	}
	out := make([]found, 0, len(in))
	for _, g := range store.GroupVectors(vecs, match) {
		best := g[0]
		for _, i := range g[1:] {
			if better(in[i], in[best]) {
				best = i
			}
		}
		out = append(out, in[best])
	}
	// Back into the order they appear in the video, which is the order the
	// viewer lists them in and the only one that reads as a timeline.
	sort.SliceStable(out, func(i, j int) bool { return out[i].t < out[j].t })
	return out
}

// better reports whether a is the shot of a face worth keeping over b:
// the detector's confidence first, then the bigger face — closer to the camera,
// more pixels, a better crop and a better embedding.
func better(a, b found) bool {
	if a.det.Score != b.det.Score {
		return a.det.Score > b.det.Score
	}
	return area(a) > area(b)
}

func area(f found) float64 { return f.det.W * f.det.H }

func box(f found) [4]float64 { return [4]float64{f.det.X, f.det.Y, f.det.W, f.det.H} }
