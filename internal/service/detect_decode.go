package service

import (
	"fmt"

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
	if scale <= 0 {
		return nil, fmt.Errorf("decode best face: invalid canvas scale %v", scale)
	}

	frameW := float32(w)
	frameH := float32(h)

	best := faceHit{Score: -1}

	for l, lvl := range detLevels {
		level := outs[l]
		hw := detInputSize / lvl.stride
		n := 2 * hw * hw
		if len(level.Scores) != n || len(level.Boxes) != 4*n || len(level.KPs) != 10*n {
			return nil, fmt.Errorf(
				"decode best face: output size mismatch for stride %d: scores=%d boxes=%d kps=%d, want %d/%d/%d",
				lvl.stride, len(level.Scores), len(level.Boxes), len(level.KPs), n, 4*n, 10*n)
		}

		stride := float32(lvl.stride)
		for i := 0; i < n; i++ {
			conf := level.Scores[i]
			if conf < detThreshold {
				continue
			}
			// Strict comparison: an exact tie keeps the earlier level, so the
			// lowest stride (the most precise one) wins.
			if conf <= best.Score {
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
			best.Box = [4]float32{
				clampToFrame(x1, scale, frameW),
				clampToFrame(y1, scale, frameH),
				clampToFrame(x2, scale, frameW),
				clampToFrame(y2, scale, frameH),
			}

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

	if best.Score < detThreshold {
		return nil, fmt.Errorf("decode best face: %w", domain.ErrNoFace)
	}
	return &best, nil
}

// mapToFrame maps a coordinate measured on the padded detection canvas back
// to the original frame.
func mapToFrame(v float32, scale float64) float32 {
	return float32(float64(v) / scale)
}

// clampToFrame maps a canvas coordinate back to the frame with the canvas
// scale and clamps the result to [0, limit].
func clampToFrame(v float32, scale float64, limit float32) float32 {
	mapped := mapToFrame(v, scale)
	if mapped < 0 {
		return 0
	}
	if mapped > limit {
		return limit
	}
	return mapped
}
