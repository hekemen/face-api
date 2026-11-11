package service

import (
	"fmt"
	"math"

	"h2hsecure.com/face/internal/domain"
)

// detLevelOutputs holds the raw SCRFD output tensors of one feature pyramid
// level: n confidence scores, 4n box regressions and 10n keypoint
// coordinates. The layout is the one the model emits, not a decoded form.
type detLevelOutputs struct {
	Scores []float32 // n entries, index = anchor
	Boxes  []float32 // 4n entries, anchor i owns [4i:4i+4]
	KPs    []float32 // 10n entries, anchor i owns [10i:10i+10]
}

// detOutputs holds the three SCRFD pyramid levels. Index 0, 1 and 2 are the
// strides 8, 16 and 32 levels, matching detLevels.
type detOutputs [3]detLevelOutputs

// faceHit is the best decoded detection, expressed in the coordinates of the
// original frame (not of the padded detection canvas).
type faceHit struct {
	Score float32       // raw detection confidence
	Box   [4]float32    // x1, y1, x2, y2, clamped to the frame
	KPs   [5][2]float32 // landmarks; only meaningful when HasKPs is true
	// HasKPs reports that all five landmarks are finite and inside the frame,
	// so they can be used for alignment. A false value never invalidates Box.
	HasKPs bool
}

// decodeBestFace decodes the SCRFD outputs into the highest-scoring detection
// above detThreshold. Detections are measured on the detInputSize square
// canvas, so scale (the PadSquareInto factor) maps them back to a w×h frame,
// and the box is clamped to the frame. ErrNoFace is returned when no detection
// clears the threshold; malformed outputs and a non-positive scale return a
// descriptive error instead, since those are processing failures rather than
// empty frames.
//
// The function allocates nothing and is safe to call once per camera frame.
func decodeBestFace(outs detOutputs, scale float64, w, h int) (*faceHit, error) {
	// Written as a negated > so a NaN scale is rejected too.
	if !(scale > 0) {
		return nil, fmt.Errorf("decode best face: invalid canvas scale %v", scale)
	}

	frameW := float32(w)
	frameH := float32(h)

	best := faceHit{Score: -1}

	for l, lvl := range detLevels {
		level := outs[l]
		hw := detInputSize / lvl.stride
		n := 2 * hw * hw
		if len(level.Scores) != n || len(level.Boxes) != 4*n {
			return nil, fmt.Errorf(
				"decode best face: output size mismatch for stride %d: scores=%d boxes=%d, want %d/%d",
				lvl.stride, len(level.Scores), len(level.Boxes), n, 4*n)
		}
		// KPs are optional: when absent, HasKPs stays false and alignment falls
		// back to the bbox crop. When present they must be exactly 10 per anchor.
		if len(level.KPs) != 0 && len(level.KPs) != 10*n {
			return nil, fmt.Errorf(
				"decode best face: output size mismatch for stride %d: kps=%d, want %d",
				lvl.stride, len(level.KPs), 10*n)
		}

		stride := float32(lvl.stride)
		for i := 0; i < n; i++ {
			conf := level.Scores[i]
			if conf < detThreshold {
				continue
			}
			// Negated > (not <=) so a NaN score is skipped instead of being
			// accepted: NaN comparisons are all false. An exact tie keeps the
			// earlier level, so the lowest stride (the most precise) wins.
			if !(conf > best.Score) {
				continue
			}

			cell := i / 2
			row := cell / hw
			col := cell % hw
			cx := float32(col * lvl.stride)
			cy := float32(row * lvl.stride)

			x1 := cx - level.Boxes[i*4+0]*stride
			y1 := cy - level.Boxes[i*4+1]*stride
			x2 := cx + level.Boxes[i*4+2]*stride
			y2 := cy + level.Boxes[i*4+3]*stride

			best.Score = conf
			// Reproduce the original production crop math exactly: round the
			// exact float64 canvas→frame quotient, then clamp to the frame as
			// integers. Using mapToFrame here would narrow the quotient to
			// float32 first and can flip a rare rounding edge (≈0.004 % of
			// canvas coords at 1920×1080), which would silently shift the
			// crop by 1 px and change the embedding.
			bestX1 := int(math.Round(float64(x1) / scale))
			if bestX1 < 0 {
				bestX1 = 0
			}
			if bestX1 > w {
				bestX1 = w
			}
			bestY1 := int(math.Round(float64(y1) / scale))
			if bestY1 < 0 {
				bestY1 = 0
			}
			if bestY1 > h {
				bestY1 = h
			}
			bestX2 := int(math.Round(float64(x2) / scale))
			if bestX2 < 0 {
				bestX2 = 0
			}
			if bestX2 > w {
				bestX2 = w
			}
			bestY2 := int(math.Round(float64(y2) / scale))
			if bestY2 < 0 {
				bestY2 = 0
			}
			if bestY2 > h {
				bestY2 = h
			}
			best.Box = [4]float32{float32(bestX1), float32(bestY1), float32(bestX2), float32(bestY2)}

			best.HasKPs = false
			if len(level.KPs) > 0 {
				hasKPs := true
				for j := 0; j < 5; j++ {
					kx := mapToFrame(cx+level.KPs[i*10+2*j]*stride, scale)
					ky := mapToFrame(cy+level.KPs[i*10+2*j+1]*stride, scale)
					best.KPs[j] = [2]float32{kx, ky}
					// NaN fails both comparisons, so this rejects non-finite
					// coordinates as well as ones outside the frame.
					if !(kx >= 0 && kx <= frameW && ky >= 0 && ky <= frameH) {
						hasKPs = false
					}
				}
				best.HasKPs = hasKPs
			}
		}
	}

	if best.Score < detThreshold {
		return nil, fmt.Errorf("decode best face: %w", domain.ErrNoFace)
	}
	// An inverted box is a processing failure, not an empty frame: image.Rect
	// canonicalises x1>x2, so a crop would silently use the wrong region.
	if best.Box[2] <= best.Box[0] || best.Box[3] <= best.Box[1] {
		return nil, fmt.Errorf(
			"decode best face: invalid bounding box: x1=%v y1=%v x2=%v y2=%v",
			best.Box[0], best.Box[1], best.Box[2], best.Box[3])
	}
	return &best, nil
}

// mapToFrame maps a coordinate measured on the padded detection canvas back
// to the original frame.
func mapToFrame(v float32, scale float64) float32 {
	return float32(float64(v) / scale)
}
