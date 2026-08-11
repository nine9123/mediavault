package media

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
)

// frameJPEG builds a JPEG of the given size to cut up. The content is
// irrelevant here — what is under test is geometry — but a gradient encodes to
// something a decoder will read back at the right size, which a flat colour also
// does but less convincingly.
func frameJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	buf := &bytes.Buffer{}
	if err := jpeg.Encode(buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTileFrameDeclinesWhenThereIsNothingToGain(t *testing.T) {
	frame := frameJPEG(t, 2000, 1500)
	if tiles, err := TileFrame(frame, 1, 100); err != nil || tiles != nil {
		t.Errorf("grid of 1: got %d tiles, %v; want none", len(tiles), err)
	}
	// The frame is smaller than the caller's threshold, so the detector was
	// going to see it near enough whole and four more calls buy nothing.
	if tiles, err := TileFrame(frame, 2, 3000); err != nil || tiles != nil {
		t.Errorf("small frame: got %d tiles, %v; want none", len(tiles), err)
	}
}

func TestTileFrameCoversTheWholeFrameWithOverlap(t *testing.T) {
	const w, h = 2000, 1500
	tiles, err := TileFrame(frameJPEG(t, w, h), 2, 1920)
	if err != nil {
		t.Fatal(err)
	}
	if len(tiles) != 4 {
		t.Fatalf("got %d tiles, want a 2x2 grid", len(tiles))
	}
	// Nothing may be left out: a face in a corner of the picture has to be
	// wholly inside some tile, so the union of the tiles is the whole frame.
	if tiles[0].X != 0 || tiles[0].Y != 0 {
		t.Errorf("first tile starts at (%.3f,%.3f), want the top-left corner", tiles[0].X, tiles[0].Y)
	}
	last := tiles[3]
	if x, y := last.X+last.W, last.Y+last.H; x < 0.999 || y < 0.999 {
		t.Errorf("last tile ends at (%.3f,%.3f), want the bottom-right corner", x, y)
	}
	// ...and they have to overlap, or a face sitting on the seam is cut in half
	// in both of them and found in neither.
	if tiles[1].X >= tiles[0].X+tiles[0].W {
		t.Errorf("tiles %v and %v don't overlap", tiles[0], tiles[1])
	}

	for i, tl := range tiles {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(tl.JPEG))
		if err != nil {
			t.Fatalf("tile %d: %v", i, err)
		}
		// The pixels have to match the fractions, or every box detected in a
		// tile lands somewhere else in the frame.
		if want := int(tl.W * w); abs(cfg.Width-want) > 2 {
			t.Errorf("tile %d is %dpx wide, want ~%d for W=%.3f", i, cfg.Width, want, tl.W)
		}
		if want := int(tl.H * h); abs(cfg.Height-want) > 2 {
			t.Errorf("tile %d is %dpx tall, want ~%d for H=%.3f", i, cfg.Height, want, tl.H)
		}
		// A tile the detector will shrink as hard as it would have shrunk the
		// whole frame is a wasted call; at a 2x2 grid each one is well over half
		// the frame's edge.
		if cfg.Width < w/2 {
			t.Errorf("tile %d is only %dpx of a %dpx frame", i, cfg.Width, w)
		}
	}
}

func TestTileProjectPutsABoxBackWhereItBelongs(t *testing.T) {
	tiles, err := TileFrame(frameJPEG(t, 2000, 1500), 2, 1920)
	if err != nil {
		t.Fatal(err)
	}
	// A face filling the bottom-right tile is a face in the bottom-right of the
	// picture, and its box must not leave the frame.
	x, y, w, h := tiles[3].Project(0, 0, 1, 1)
	if x <= 0.2 || y <= 0.2 {
		t.Errorf("bottom-right tile projected to (%.3f,%.3f), want it well into the frame", x, y)
	}
	if x+w > 1.0001 || y+h > 1.0001 {
		t.Errorf("projected box (%.3f,%.3f,%.3f,%.3f) leaves the frame", x, y, w, h)
	}
	// The middle of the first tile is the middle of the part of the frame that
	// tile covers, which is nearer the top-left than the middle of the picture.
	cx, cy, _, _ := tiles[0].Project(0.5, 0.5, 0.1, 0.1)
	if cx >= 0.5 || cy >= 0.5 {
		t.Errorf("centre of the top-left tile projected to (%.3f,%.3f), want it above and left of centre", cx, cy)
	}
}

func TestTileFrameRejectsSomethingThatIsNotAFrame(t *testing.T) {
	if _, err := TileFrame([]byte("not a jpeg"), 2, 10); err == nil {
		t.Error("tiling garbage succeeded")
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
