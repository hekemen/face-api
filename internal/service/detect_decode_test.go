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

	// Canvas box / scale: (0,-32,64,64); y1 clamps to 0.
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
	for _, scale := range []float64{0, -0.5} {
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
