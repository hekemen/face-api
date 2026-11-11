package service

import (
	"errors"
	"math"
	"testing"

	"h2hsecure.com/face/internal/domain"
)

// newDetLevel allocates correctly sized SCRFD outputs for one pyramid level.
func newDetLevel(stride int) detLevelOutputs {
	hw := detInputSize / stride
	n := 2 * hw * hw
	return detLevelOutputs{
		Scores: make([]float32, n),
		Boxes:  make([]float32, 4*n),
		KPs:    make([]float32, 10*n),
	}
}

// validDetOutputs allocates correctly sized, all-zero outputs for all levels.
func validDetOutputs() detOutputs {
	return detOutputs{newDetLevel(8), newDetLevel(16), newDetLevel(32)}
}

func TestDecodeBestFacePicksHighestScore(t *testing.T) {
	outs := validDetOutputs()

	// Stride 8, anchor 2: cell 1 -> col 1, row 0, so cx=8, cy=0. With regs
	// {1,2,3,4} the canvas box is (0,-16,32,32).
	outs[0].Scores[2] = 0.9
	copy(outs[0].Boxes[2*4:], []float32{1, 2, 3, 4})

	// A lower score on stride 16 must not win.
	outs[1].Scores[2] = 0.6
	copy(outs[1].Boxes[2*4:], []float32{1, 1, 1, 1})

	hit, err := decodeBestFace(outs, 0.5, 1280, 960)
	if err != nil {
		t.Fatalf("decodeBestFace: %v", err)
	}
	if hit.Score != 0.9 {
		t.Errorf("Score = %v, want 0.9", hit.Score)
	}

	// Canvas box (0,-16,32,32) / 0.5 = (0,-32,64,64); y1 clamps to 0.
	want := [4]float32{0, 0, 64, 64}
	if hit.Box != want {
		t.Errorf("Box = %v, want %v", hit.Box, want)
	}
}

func TestDecodeBestFaceNoFace(t *testing.T) {
	outs := validDetOutputs()
	for i := range outs[0].Scores {
		outs[0].Scores[i] = 0.4
	}

	hit, err := decodeBestFace(outs, 1.0, 640, 640)
	if !errors.Is(err, domain.ErrNoFace) {
		t.Fatalf("err = %v, want domain.ErrNoFace", err)
	}
	if hit != nil {
		t.Fatalf("hit = %+v, want nil", hit)
	}
}

func TestDecodeBestFaceKeypoints(t *testing.T) {
	outs := validDetOutputs()

	// Stride 8, anchor 162: cell 81 -> col 1, row 1, so cx=8, cy=8.
	const idx = 162
	outs[0].Scores[idx] = 0.95
	copy(outs[0].Boxes[idx*4:], []float32{1, 1, 2, 2})
	for j := 0; j < 5; j++ {
		outs[0].KPs[idx*10+2*j] = float32(j+1) * 0.25    // kx = 8 + 2*(j+1)
		outs[0].KPs[idx*10+2*j+1] = float32(j+1) * 0.125 // ky = 8 + (j+1)
	}

	hit, err := decodeBestFace(outs, 1.0, 640, 640)
	if err != nil {
		t.Fatalf("decodeBestFace: %v", err)
	}
	if !hit.HasKPs {
		t.Fatal("HasKPs = false, want true")
	}
	wantBox := [4]float32{0, 0, 24, 24}
	if hit.Box != wantBox {
		t.Errorf("Box = %v, want %v", hit.Box, wantBox)
	}
	for j := 0; j < 5; j++ {
		want := [2]float32{8 + 2*float32(j+1), 8 + float32(j+1)}
		if math.Abs(float64(hit.KPs[j][0]-want[0])) > 1e-5 ||
			math.Abs(float64(hit.KPs[j][1]-want[1])) > 1e-5 {
			t.Errorf("KPs[%d] = %v, want %v", j, hit.KPs[j], want)
		}
	}
}

func TestDecodeBestFaceKeypointsOutOfFrame(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(kps []float32)
	}{
		{
			name: "nan",
			mutate: func(kps []float32) {
				kps[2*3] = float32(math.NaN())
			},
		},
		{
			name: "beyond frame",
			mutate: func(kps []float32) {
				kps[2*2] = 1000 // kx = 8 + 8000 > 640
			},
		},
		{
			name: "negative",
			mutate: func(kps []float32) {
				kps[2*1+1] = -1000 // ky < 0
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outs := validDetOutputs()
			const idx = 162
			outs[0].Scores[idx] = 0.95
			copy(outs[0].Boxes[idx*4:], []float32{1, 1, 2, 2})
			for j := 0; j < 5; j++ {
				outs[0].KPs[idx*10+2*j] = 0.25
				outs[0].KPs[idx*10+2*j+1] = 0.125
			}
			tt.mutate(outs[0].KPs[idx*10:])

			hit, err := decodeBestFace(outs, 1.0, 640, 640)
			if err != nil {
				t.Fatalf("decodeBestFace: %v", err)
			}
			if hit.HasKPs {
				t.Error("HasKPs = true, want false")
			}
			wantBox := [4]float32{0, 0, 24, 24}
			if hit.Box != wantBox {
				t.Errorf("Box = %v, want the valid %v", hit.Box, wantBox)
			}
		})
	}
}

func TestDecodeBestFaceRejectsBadLengths(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*detOutputs)
	}{
		{
			name: "scores",
			mutate: func(o *detOutputs) {
				o[0].Scores = o[0].Scores[:len(o[0].Scores)-1]
			},
		},
		{
			name: "boxes",
			mutate: func(o *detOutputs) {
				o[1].Boxes = o[1].Boxes[:len(o[1].Boxes)-1]
			},
		},
		{
			name: "keypoints",
			mutate: func(o *detOutputs) {
				o[2].KPs = o[2].KPs[:len(o[2].KPs)-1]
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outs := validDetOutputs()
			outs[0].Scores[0] = 0.9
			tt.mutate(&outs)

			hit, err := decodeBestFace(outs, 1.0, 640, 640)
			if err == nil {
				t.Fatal("err = nil, want a size mismatch error")
			}
			if errors.Is(err, domain.ErrNoFace) {
				t.Fatalf("err = %v, must not be domain.ErrNoFace", err)
			}
			if hit != nil {
				t.Fatalf("hit = %+v, want nil", hit)
			}
		})
	}
}

// TestDecodeBestFaceRoundHalfAwayFromZero pins the crop-rect rounding: a
// canvas coordinate of exactly 7.5 must round to 8, not truncate to 7. This
// is the round-before-clamp behaviour the preallocated processor relies on
// for byte-identical bbox alignment (Task 7 carry-forward from Task 6).
func TestDecodeBestFaceRoundHalfAwayFromZero(t *testing.T) {
	outs := validDetOutputs()

	// Stride 8, anchor 2: cell 1 -> col 1, row 0, so cx=8, cy=0.
	// x1 = 8 - 8*0.0625 = 7.5 exactly; scale 1.0 keeps it in frame coords.
	outs[0].Scores[2] = 0.9
	copy(outs[0].Boxes[2*4:], []float32{0.0625, 0, 1, 1})

	hit, err := decodeBestFace(outs, 1.0, 640, 640)
	if err != nil {
		t.Fatalf("decodeBestFace: %v", err)
	}
	wantBox := [4]float32{8, 0, 16, 8}
	if hit.Box != wantBox {
		t.Errorf("Box = %v, want %v (7.5 must round to 8, not truncate to 7)", hit.Box, wantBox)
	}
}

// TestDecodeBestFaceToleratesAbsentKPs pins that keypoints are optional:
// when a level emits no KPs (nil), decodeBestFace still returns the box and
// reports HasKPs=false instead of erroring. The arcface5 path wires real KPs
// in Task 8; until then the processor must degrade to the bbox crop.
// TestDecodeBestFaceMatchesProductionRounding pins that the box is computed
// from the exact float64 canvas→frame quotient (round half away from zero,
// then clamp to the frame as integers), exactly as the original production
// code did. A non-power-of-two scale (1920×1080 → 1/3) exercises the float64
// division path.
//
// The chosen regs make canvasX = 7.8333…, whose quotient is 23.4999… (just
// below .5). float32 narrowing rounds it up to exactly 23.5, so the old
// float32-narrowed formula yields x1=24 while the exact float64 formula
// yields x1=23. This discriminates the two.
func TestDecodeBestFaceMatchesProductionRounding(t *testing.T) {
	outs := validDetOutputs()

	// Stride 8, anchor 2: cx=8, cy=0. regs {0.020833367, 0, 1, 1} give the
	// canvas box {7.833…, 0, 16, 8}.
	outs[0].Scores[2] = 0.9
	copy(outs[0].Boxes[2*4:], []float32{0.020833367, 0, 1, 1})

	// 1920×1080 frame → PadSquareInto scale = 640/max(1920,1080) = 640/1920.
	scale := 640.0 / 1920.0
	hit, err := decodeBestFace(outs, scale, 1920, 1080)
	if err != nil {
		t.Fatalf("decodeBestFace: %v", err)
	}

	// Production formula: int(math.Round(float64(canvasX) / scale)),
	// clamped to [0,w]×[0,h]. x1 must round down to 23 (not up to 24).
	x1 := int(math.Round(float64(8-0.020833367*8) / scale)) // round(23.4999…) = 23
	y1 := 0
	x2 := int(math.Round(float64(16) / scale)) // round(48) = 48
	y2 := int(math.Round(float64(8) / scale))  // round(24) = 24
	want := [4]float32{float32(x1), float32(y1), float32(x2), float32(y2)}
	if hit.Box != want {
		t.Errorf("Box = %v, want %v (23.4999… must round to 23, not float32-narrow to 23.5→24)", hit.Box, want)
	}
	if hit.Box[0] > float32(1920) || hit.Box[2] > float32(1920) || hit.Box[1] > float32(1080) || hit.Box[3] > float32(1080) {
		t.Error("Box not clamped to frame")
	}
}

// TestDecodeBestFaceToleratesAbsentKPs pins that keypoints are optional:
// when a level emits no KPs (nil), decodeBestFace still returns the box and
// reports HasKPs=false instead of erroring. The arcface5 path wires real KPs
// in Task 8; until then the processor must degrade to the bbox crop.
func TestDecodeBestFaceToleratesAbsentKPs(t *testing.T) {
	outs := validDetOutputs()

	outs[0].Scores[162] = 0.95
	copy(outs[0].Boxes[162*4:], []float32{1, 1, 2, 2})
	outs[0].KPs = nil // absent keypoints

	hit, err := decodeBestFace(outs, 1.0, 640, 640)
	if err != nil {
		t.Fatalf("decodeBestFace: %v", err)
	}
	if hit.HasKPs {
		t.Error("HasKPs = true, want false with absent KPs")
	}
	wantBox := [4]float32{0, 0, 24, 24}
	if hit.Box != wantBox {
		t.Errorf("Box = %v, want %v", hit.Box, wantBox)
	}
}

func TestDecodeBestFaceTieKeepsFirst(t *testing.T) {
	outs := validDetOutputs()

	// Exact tie across two levels: stride 8 must win because it is scanned
	// first. anchor 2 -> cx=8, cy=0 (stride 8) and cx=16, cy=0 (stride 16).
	outs[0].Scores[2] = 0.7
	copy(outs[0].Boxes[2*4:], []float32{1, 0, 3, 4})
	outs[1].Scores[2] = 0.7
	copy(outs[1].Boxes[2*4:], []float32{1, 1, 1, 1})

	hit, err := decodeBestFace(outs, 1.0, 640, 640)
	if err != nil {
		t.Fatalf("decodeBestFace: %v", err)
	}
	if hit.Score != 0.7 {
		t.Errorf("Score = %v, want 0.7", hit.Score)
	}
	wantBox := [4]float32{0, 0, 32, 32}
	if hit.Box != wantBox {
		t.Errorf("Box = %v, want the stride-8 box %v", hit.Box, wantBox)
	}
}

func TestDecodeBestFaceRejectsBadScale(t *testing.T) {
	for _, scale := range []float64{0, -0.5, math.NaN()} {
		hit, err := decodeBestFace(validDetOutputs(), scale, 640, 640)
		if err == nil {
			t.Fatalf("scale %v: err = nil, want an error", scale)
		}
		if errors.Is(err, domain.ErrNoFace) {
			t.Fatalf("scale %v: err = %v, must not be domain.ErrNoFace", scale, err)
		}
		if hit != nil {
			t.Fatalf("scale %v: hit = %+v, want nil", scale, hit)
		}
	}
}

func TestDecodeBestFaceIgnoresNaNScore(t *testing.T) {
	t.Run("nan does not hijack a valid detection", func(t *testing.T) {
		outs := validDetOutputs()

		// anchor 162: cx=8, cy=8 -> box (0,0,24,24) at scale 1.
		outs[0].Scores[162] = 0.9
		copy(outs[0].Boxes[162*4:], []float32{1, 1, 2, 2})
		// A NaN score at a later anchor must be skipped, not selected.
		outs[0].Scores[163] = float32(math.NaN())
		copy(outs[0].Boxes[163*4:], []float32{1, 1, 1, 1})

		hit, err := decodeBestFace(outs, 1.0, 640, 640)
		if err != nil {
			t.Fatalf("decodeBestFace: %v", err)
		}
		if hit.Score != 0.9 {
			t.Fatalf("Score = %v, want 0.9", hit.Score)
		}
		want := [4]float32{0, 0, 24, 24}
		if hit.Box != want {
			t.Fatalf("Box = %v, want the finite %v", hit.Box, want)
		}
	})

	t.Run("only nan scores means no face", func(t *testing.T) {
		outs := validDetOutputs()
		for i := range outs[0].Scores {
			outs[0].Scores[i] = float32(math.NaN())
		}

		hit, err := decodeBestFace(outs, 1.0, 640, 640)
		if !errors.Is(err, domain.ErrNoFace) {
			t.Fatalf("err = %v, want domain.ErrNoFace", err)
		}
		if hit != nil {
			t.Fatalf("hit = %+v, want nil", hit)
		}
	})
}

func TestDecodeBestFaceRejectsInvertedBox(t *testing.T) {
	// anchor 162: cx=8, cy=8, stride 8.
	tests := []struct {
		name    string
		regs    [4]float32
		wantErr bool
	}{
		// x1 = 8-(-1*8) = 16, x2 = 8+(-2*8) = -8 (clamped to 0) -> inverted.
		{name: "inverted", regs: [4]float32{-1, 1, -2, 2}, wantErr: true},
		{name: "valid", regs: [4]float32{1, 1, 2, 2}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outs := validDetOutputs()
			outs[0].Scores[162] = 0.9
			copy(outs[0].Boxes[162*4:], tt.regs[:])

			hit, err := decodeBestFace(outs, 1.0, 640, 640)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("hit = %+v, want an error", hit)
				}
				if errors.Is(err, domain.ErrNoFace) {
					t.Fatalf("err = %v, must not be domain.ErrNoFace", err)
				}
				if hit != nil {
					t.Fatalf("hit = %+v, want nil", hit)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeBestFace: %v", err)
			}
			want := [4]float32{0, 0, 24, 24}
			if hit.Box != want {
				t.Errorf("Box = %v, want %v", hit.Box, want)
			}
		})
	}
}

func TestDecodeBestFaceStride32(t *testing.T) {
	outs := validDetOutputs()

	// Stride 32, anchor 42: cell 21 -> row 1, col 1, so cx=32, cy=32. With
	// regs {1,1,2,2} the canvas box is (0,0,96,96) and maps 1:1 at scale 1.
	const idx = 42
	outs[2].Scores[idx] = 0.8
	copy(outs[2].Boxes[idx*4:], []float32{1, 1, 2, 2})

	hit, err := decodeBestFace(outs, 1.0, 640, 640)
	if err != nil {
		t.Fatalf("decodeBestFace: %v", err)
	}
	if hit.Score != 0.8 {
		t.Errorf("Score = %v, want 0.8", hit.Score)
	}
	wantBox := [4]float32{0, 0, 96, 96}
	if hit.Box != wantBox {
		t.Errorf("Box = %v, want the stride-32 box %v", hit.Box, wantBox)
	}
	if !hit.HasKPs {
		t.Fatal("HasKPs = false, want true")
	}
	wantKP := [2]float32{32, 32}
	if hit.KPs[0] != wantKP {
		t.Errorf("KPs[0] = %v, want %v", hit.KPs[0], wantKP)
	}
}
