package service

import (
	"testing"

	"h2hsecure.com/face/internal/imaging"
)

func TestParseAlignment(t *testing.T) {
	tests := []struct {
		in   string
		want AlignmentMode
		ok   bool
	}{
		{"", AlignmentBBox, true},
		{"bbox", AlignmentBBox, true},
		{"  BBOX  ", AlignmentBBox, true},
		{"arcface5", AlignmentArcFace5, true},
		{"ARCFACE5", AlignmentArcFace5, true},
		{"nope", "", false},
		{" ", AlignmentBBox, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseAlignment(tt.in)
			if tt.ok {
				if err != nil {
					t.Fatalf("ParseAlignment(%q): %v, want no error", tt.in, err)
				}
				if got != tt.want {
					t.Errorf("ParseAlignment(%q) = %v, want %v", tt.in, got, tt.want)
				}
			} else {
				if err == nil {
					t.Fatalf("ParseAlignment(%q): got %v, want error", tt.in, got)
				}
			}
		})
	}
}

func TestAlignCropFallsBackOnBadKPs(t *testing.T) {
	img := imaging.NewRGB(200, 160)
	for y := 0; y < 160; y++ {
		for x := 0; x < 200; x++ {
			img.Pix[(y*200+x)*3+0] = byte(x)
			img.Pix[(y*200+x)*3+1] = byte(y)
			img.Pix[(y*200+x)*3+2] = byte((x + y) / 2)
		}
	}
	// Collinear landmarks are degenerate: Warp5Into must refuse them and the
	// caller must fall back to the bbox crop.
	hit := &faceHit{
		HasKPs: true,
		Box:    [4]float32{40, 30, 120, 110},
		KPs:    [5][2]float32{{10, 10}, {20, 20}, {30, 30}, {40, 40}, {50, 50}},
	}
	p := &FaceProcessorImpl{align: AlignmentArcFace5}
	dst := imaging.NewRGB(112, 112)
	if usedWarp := p.alignCrop(dst, img, hit); usedWarp {
		t.Error("usedWarp = true, want false (collinear KPs should fall back to bbox)")
	}
	// dst(0,0) must be the bbox corner pixel, not warp output.
	wantR, wantG, wantB := img.At(40, 30)
	gotR, gotG, gotB := dst.At(0, 0)
	if gotR != wantR || gotG != wantG || gotB != wantB {
		t.Errorf("dst(0,0) = (%d,%d,%d), want bbox corner (%d,%d,%d)", gotR, gotG, gotB, wantR, wantG, wantB)
	}
}

// TestAlignCropArcFace5WithoutKPsFallsBackToBBox pins that arcface5 with no
// usable landmarks degrades to the bbox crop instead of producing a zeroed or
// NaN crop.
func TestAlignCropArcFace5WithoutKPsFallsBackToBBox(t *testing.T) {
	img := imaging.NewRGB(200, 160)
	for y := 0; y < 160; y++ {
		for x := 0; x < 200; x++ {
			img.Pix[(y*200+x)*3+0] = byte(x)
			img.Pix[(y*200+x)*3+1] = byte(y)
			img.Pix[(y*200+x)*3+2] = byte((x + y) / 2)
		}
	}
	hit := &faceHit{
		HasKPs: false,
		Box:    [4]float32{40, 30, 120, 110},
	}
	p := &FaceProcessorImpl{align: AlignmentArcFace5}
	dst := imaging.NewRGB(112, 112)
	if usedWarp := p.alignCrop(dst, img, hit); usedWarp {
		t.Error("usedWarp = true, want false (no usable KPs -> bbox fallback)")
	}
	wantR, wantG, wantB := img.At(40, 30)
	gotR, gotG, gotB := dst.At(0, 0)
	if gotR != wantR || gotG != wantG || gotB != wantB {
		t.Errorf("dst(0,0) = (%d,%d,%d), want bbox corner (%d,%d,%d)", gotR, gotG, gotB, wantR, wantG, wantB)
	}
}

func TestAlignCropUsesWarp(t *testing.T) {
	img := imaging.NewRGB(200, 160)
	for y := 0; y < 160; y++ {
		for x := 0; x < 200; x++ {
			img.Pix[(y*200+x)*3+0] = byte(x)
			img.Pix[(y*200+x)*3+1] = byte(y)
			img.Pix[(y*200+x)*3+2] = byte((x + y) / 2)
		}
	}
	hit := &faceHit{
		HasKPs: true,
		Box:    [4]float32{40, 30, 120, 110},
		KPs:    imaging.ArcFaceRefPoints, // real 5-point set -> a valid warp
	}
	p := &FaceProcessorImpl{align: AlignmentArcFace5}
	dst := imaging.NewRGB(112, 112)
	if usedWarp := p.alignCrop(dst, img, hit); !usedWarp {
		t.Error("usedWarp = false, want true (valid KPs should warp)")
	}

	// The warp result must differ from the bbox crop; otherwise the warp is a
	// no-op and the test is vacuous.
	bboxDst := imaging.NewRGB(112, 112)
	p2 := &FaceProcessorImpl{align: AlignmentBBox}
	p2.alignCrop(bboxDst, img, hit)
	for i := 0; i < len(dst.Pix); i++ {
		if dst.Pix[i] != bboxDst.Pix[i] {
			return // they differ — the warp produced different pixels
		}
	}
	t.Error("warp crop equals bbox crop, expected different pixels")
}
