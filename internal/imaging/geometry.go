package imaging

import (
	"fmt"
	"image"
)

// This file holds the geometric and packing primitives of the detection
// pipeline in their allocation-free form: every function writes into a
// preallocated destination, so a per-frame loop can reuse its buffers.
//
// The sampling rules are ports of the historical service-side helpers
// (resizePadToSquare, bilinearResize, cropRegion, rgbToNCHW) and must stay
// bit-identical to them: same sample positions (x0 = int(x*scale)), same
// clamping of the right/bottom edge onto the last source pixel, same uint8
// truncation of the interpolated value.

// ResizeInto writes src bilinearly resized to dst.W×dst.H into dst. dst must
// already be sized to the target dimensions and must not alias src; it is left
// untouched when either image is nil, empty, or has a Pix buffer that is too
// small.
func ResizeInto(dst, src *RGBImage) {
	if !usable(dst) || !usable(src) {
		return
	}
	resampleInto(dst, src, image.Rect(0, 0, src.W, src.H), dst.W, dst.H)
}

// PadSquareInto scales src to fit a square dst and copies it at the top-left
// corner (0,0) — not centred — leaving the uncovered area black. The returned
// scale is dst.W / max(src.W, src.H), the factor a caller needs to map
// coordinates measured on the padded canvas back to src.
//
// dst must be square (dst.W == dst.H) and have a large enough Pix buffer; src
// must be non-empty. When src is empty (W or H is 0) or the scaled size
// collapses to zero, PadSquareInto returns 0 and leaves dst untouched, so 0 is
// always a failure sentinel: any success returns a positive scale. Areas of dst
// the source does not cover are cleared, which matters when dst is a reused
// frame buffer.
func PadSquareInto(dst, src *RGBImage) float64 {
	if !usable(dst) || !usable(src) || dst.W != dst.H {
		return 0
	}

	size := dst.W
	longest := src.H
	if src.W > longest {
		longest = src.W
	}
	scale := float64(size) / float64(longest)

	nw := round(float64(src.W) * scale)
	nh := round(float64(src.H) * scale)
	if nw <= 0 || nh <= 0 {
		return 0
	}

	// Clear the padding first: the bottom band, then the right band of the
	// rows the source will cover.
	if nh < size {
		clear(dst.Pix[nh*size*3 : size*size*3])
	}
	if nw < size {
		for y := 0; y < nh; y++ {
			row := dst.Pix[y*size*3 : (y+1)*size*3]
			clear(row[nw*3:])
		}
	}

	resampleInto(dst, src, image.Rect(0, 0, src.W, src.H), nw, nh)
	return scale
}

// CropInto resizes the part of src inside r into dst. r is clipped to src's
// bounds first; an empty clip is an error and leaves dst untouched. The clipped
// region is resampled to dst.W×dst.H, which is the 112×112 recognition input in
// the detection pipeline. dst must not alias src.
func CropInto(dst, src *RGBImage, r image.Rectangle) error {
	if !usable(dst) || !usable(src) {
		return fmt.Errorf("crop image: invalid source or destination")
	}
	clip := r.Intersect(image.Rect(0, 0, src.W, src.H))
	if clip.Dx() <= 0 || clip.Dy() <= 0 {
		return fmt.Errorf("invalid crop region %v", r)
	}
	resampleInto(dst, src, clip, dst.W, dst.H)
	return nil
}

// NCHWInto packs src into dst as three RGB planes in R, G, B order, row-major
// inside each plane, normalising every sample as (v-127.5)/div. div is the
// caller's: 128 for the detection model, 127.5 for the recognition model. It
// returns an error when src is empty or dst is not exactly 3*src.W*src.H long;
// div must be non-zero.
func NCHWInto(dst []float32, src *RGBImage, div float32) error {
	if !usable(src) {
		return fmt.Errorf("build nchw blob: invalid source image")
	}
	need := 3 * src.W * src.H
	if len(dst) != need {
		return fmt.Errorf("invalid nchw destination length: got %d, want %d", len(dst), need)
	}

	plane := src.W * src.H
	for y := 0; y < src.H; y++ {
		row := y * src.W * 3
		idx := y * src.W
		for x := 0; x < src.W; x++ {
			base := row + x*3
			r := float32(src.Pix[base+0])
			g := float32(src.Pix[base+1])
			b := float32(src.Pix[base+2])
			i := idx + x
			dst[i] = (r - 127.5) / div
			dst[plane+i] = (g - 127.5) / div
			dst[2*plane+i] = (b - 127.5) / div
		}
	}
	return nil
}

// resampleInto writes src's region resampled to w×h at pixel (0,0) of dst,
// using dst.W as the row stride. Values are identical to resampling into a
// w×h image and copying its rows into dst, but no intermediate buffer exists.
// region must be inside src and w, h must be positive.
func resampleInto(dst, src *RGBImage, region image.Rectangle, w, h int) {
	sw, sh := region.Dx(), region.Dy()
	scaleX := float64(sw) / float64(w)
	scaleY := float64(sh) / float64(h)

	for y := 0; y < h; y++ {
		srcY := float64(y) * scaleY
		y0 := int(srcY)
		if y0 > sh-1 {
			y0 = sh - 1
		}
		y1 := y0 + 1
		if y1 > sh-1 {
			y1 = sh - 1
		}
		fy := srcY - float64(y0)

		row0 := (region.Min.Y + y0) * src.W * 3
		row1 := (region.Min.Y + y1) * src.W * 3
		outRow := y * dst.W * 3

		for x := 0; x < w; x++ {
			srcX := float64(x) * scaleX
			x0 := int(srcX)
			if x0 > sw-1 {
				x0 = sw - 1
			}
			x1 := x0 + 1
			if x1 > sw-1 {
				x1 = sw - 1
			}
			fx := srcX - float64(x0)

			col0 := (region.Min.X + x0) * 3
			col1 := (region.Min.X + x1) * 3
			out := outRow + x*3

			for c := 0; c < 3; c++ {
				v00 := float64(src.Pix[row0+col0+c])
				v01 := float64(src.Pix[row0+col1+c])
				v10 := float64(src.Pix[row1+col0+c])
				v11 := float64(src.Pix[row1+col1+c])

				top := v00 + (v01-v00)*fx
				bot := v10 + (v11-v10)*fx
				dst.Pix[out+c] = uint8(top + (bot-top)*fy)
			}
		}
	}
}

// usable reports whether img has positive dimensions and a Pix buffer large
// enough for them.
func usable(img *RGBImage) bool {
	if img == nil || img.W <= 0 || img.H <= 0 {
		return false
	}
	return len(img.Pix) >= img.W*img.H*3
}

// round rounds to the nearest integer, halves away from zero, matching the
// historical helper the padding math was written with.
func round(v float64) int {
	if v < 0 {
		return int(v - 0.5)
	}
	return int(v + 0.5)
}
