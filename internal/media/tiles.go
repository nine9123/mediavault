package media

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
)

// Cutting one frame into overlapping tiles, so a detector that can only afford
// to look at N pixels can be pointed at several parts of a photograph at that
// resolution instead of at all of it shrunk down.
//
// This is the whole answer to "why does it miss faces in big photographs". A
// face is found at the resolution the detector processes, and every detector
// bounds that: give it a 6000px scan and it shrinks it to a couple of thousand
// pixels first, at which point the people at the back of a group shot are
// twenty pixels across and nothing will find them. Cutting the frame into
// quarters and detecting on each quarter spends four times the budget but looks
// at each face with twice the detail in each direction — and the same code path
// then merges the results back into whole-frame coordinates, so nothing
// downstream (the box, the crop, the embedding) knows a tile was involved.

// tileOverlap is how much of each tile is shared with its neighbour. It is what
// keeps a face that straddles a cut from being lost: at 25% of a tile, any face
// smaller than a quarter of a tile falls whole inside at least one of them, and
// the pass over the whole frame catches the larger ones anyway.
const tileOverlap = 0.25

// tileQuality is the JPEG quality tiles are re-encoded at. Higher than Go's
// default 75 on purpose: this is the second lossy pass over the same pixels
// (ffmpeg made the first), and blocking artefacts around a 40px face are
// exactly the detail a detector is working from.
const tileQuality = 90

// Tile is one piece of a frame: where it sits in that frame, as fractions, and
// the JPEG of it. The rectangle is what maps a detection made inside the tile
// back onto the whole frame.
type Tile struct {
	X, Y, W, H float64
	JPEG       []byte
}

// Project maps a box detected inside this tile onto the whole frame. Both are
// in fractions of their own image, which is the only form of coordinates the
// detection contract has (see internal/face).
func (t Tile) Project(x, y, w, h float64) (float64, float64, float64, float64) {
	return t.X + x*t.W, t.Y + y*t.H, w * t.W, h * t.H
}

// TileFrame cuts a JPEG frame into a grid x grid arrangement of overlapping
// tiles. It returns nothing — not an error — when tiling would gain nothing:
// a grid of one or less, or a frame whose longest edge is already under minEdge,
// meaning the detector was going to see it near enough whole.
//
// minEdge is the caller's judgement, not a property of the image: it is where
// the frame stops being bigger than one detection can use.
func TileFrame(frame []byte, grid, minEdge int) ([]Tile, error) {
	if grid < 2 {
		return nil, nil
	}
	cfgImg, err := jpeg.DecodeConfig(bytes.NewReader(frame))
	if err != nil {
		return nil, fmt.Errorf("tile: %w", err)
	}
	if max(cfgImg.Width, cfgImg.Height) < minEdge {
		return nil, nil
	}
	img, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		return nil, fmt.Errorf("tile: %w", err)
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("tile: empty frame")
	}

	out := make([]Tile, 0, grid*grid)
	for gy := 0; gy < grid; gy++ {
		y0, y1 := span(h, gy, grid)
		for gx := 0; gx < grid; gx++ {
			x0, x1 := span(w, gx, grid)
			buf := &bytes.Buffer{}
			if err := jpeg.Encode(buf, crop(img, b.Min.X+x0, b.Min.Y+y0, b.Min.X+x1, b.Min.Y+y1),
				&jpeg.Options{Quality: tileQuality}); err != nil {
				return nil, fmt.Errorf("tile: %w", err)
			}
			out = append(out, Tile{
				X:    float64(x0) / float64(w),
				Y:    float64(y0) / float64(h),
				W:    float64(x1-x0) / float64(w),
				H:    float64(y1-y0) / float64(h),
				JPEG: buf.Bytes(),
			})
		}
	}
	return out, nil
}

// span is the pixel range of tile i of n along an axis of length length. The
// tiles overlap by tileOverlap of their own size, so n of them still cover the
// axis exactly: n tiles of size s advancing by s*(1-overlap) each reach
// s*(1+(n-1)*(1-overlap)), which is what s is solved for.
func span(length, i, n int) (int, int) {
	size := float64(length) / (1 + float64(n-1)*(1-tileOverlap))
	start := int(float64(i) * size * (1 - tileOverlap))
	end := start + int(size+0.5)
	if i == n-1 || end > length {
		end = length // the last tile takes the rounding, so nothing is left out
	}
	if start >= end {
		start = end - 1
	}
	return start, end
}

// crop returns the given rectangle of img without copying where it can. Every
// image type jpeg.Decode produces has SubImage; the copy is there so an exotic
// decoder (or a future format) degrades to slow rather than to broken.
func crop(img image.Image, x0, y0, x1, y1 int) image.Image {
	r := image.Rect(x0, y0, x1, y1)
	if sub, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		return sub.SubImage(r)
	}
	dst := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(dst, dst.Bounds(), img, r.Min, draw.Src)
	return dst
}
