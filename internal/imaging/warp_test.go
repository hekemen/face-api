package imaging

import (
	"math"
	"testing"
)

// refPoints64 returns ArcFaceRefPoints widened to float64, the point set the
// warp always fits towards.
func refPoints64() [5][2]float64 {
	var out [5][2]float64
	for i, p := range ArcFaceRefPoints {
		out[i][0] = float64(p[0])
		out[i][1] = float64(p[1])
	}
	return out
}

// wantUntouched fails the test unless img still holds marker in every channel.
func wantUntouched(t *testing.T, img *RGBImage, marker byte) {
	t.Helper()
	for i, v := range img.Pix {
		if v != marker {
			t.Fatalf("dst.Pix[%d] = %#x, want untouched %#x", i, v, marker)
		}
	}
}

func TestSimilarityIdentity(t *testing.T) {
	ref := refPoints64()
	m, ok := SimilarityFromPoints(ref, ref)
	if !ok {
		t.Fatal("SimilarityFromPoints(ref, ref) reported !ok")
	}

	// Identity fit on well-conditioned points: the only error left is
	// float64 round-off (~1e-16 relative), so 1e-9 is a strict bound.
	want := [6]float64{1, 0, 0, 0, 1, 0}
	for i := range want {
		if math.Abs(m[i]-want[i]) > 1e-9 {
			t.Fatalf("m[%d] = %.17g, want %.17g", i, m[i], want[i])
		}
	}
}

func TestSimilarityRecoversKnownTransform(t *testing.T) {
	// Construct dst = scale*R(angle)*src + t in float64, then recover m.
	const (
		scale = 1.5
		angle = 10 * math.Pi / 180
		tx    = 5.0
		ty    = -3.0
	)
	cos, sin := math.Cos(angle), math.Sin(angle)

	src := refPoints64()
	var dst [5][2]float64
	for i, p := range src {
		dst[i][0] = scale*(cos*p[0]-sin*p[1]) + tx
		dst[i][1] = scale*(sin*p[0]+cos*p[1]) + ty
	}

	m, ok := SimilarityFromPoints(src, dst)
	if !ok {
		t.Fatal("SimilarityFromPoints reported !ok for a well-conditioned fit")
	}

	// The transform we applied is the expectation; nothing here re-uses the
	// implementation's formula. Construction round-off is ~1e-14, so 1e-9
	// leaves five orders of magnitude of slack.
	want := [6]float64{scale * cos, -scale * sin, tx, scale * sin, scale * cos, ty}
	for i := range want {
		if math.Abs(m[i]-want[i]) > 1e-9 {
			t.Fatalf("m[%d] = %.17g, want %.17g", i, m[i], want[i])
		}
	}
}

func TestWarpIdentity(t *testing.T) {
	t.Run("kps equal refpoints", func(t *testing.T) {
		src := NewRGB(112, 112)
		fillGrayPattern(src, func(x, y int) byte { return byte((x*3 + y*5) % 256) })

		dst := NewRGB(112, 112)
		for i := range dst.Pix {
			dst.Pix[i] = 0xAB
		}
		if !Warp5Into(dst, src, ArcFaceRefPoints) {
			t.Fatal("Warp5Into reported false for the identity keypoints")
		}
		// Bilinear sampling at the pixel centres plus rounding reproduces the
		// source exactly; anything less than exact would mean the sampling
		// position is off by a sub-pixel amount.
		for i := range src.Pix {
			if dst.Pix[i] != src.Pix[i] {
				t.Fatalf("dst.Pix[%d] = %d, want %d", i, dst.Pix[i], src.Pix[i])
			}
		}
	})

	t.Run("one-pixel shift", func(t *testing.T) {
		// kps = refpoints + (1, 0) means the fitted transform translates by -1,
		// so destination pixel x samples source pixel x+1 exactly. A
		// checkerboard makes any half-pixel convention error (which would
		// average the two neighbours to ~128) impossible to miss. Adding 1 is
		// exact in float32 for every reference coordinate.
		var kps [5][2]float32
		for i, p := range ArcFaceRefPoints {
			kps[i][0] = p[0] + 1
			kps[i][1] = p[1]
		}

		src := NewRGB(120, 112)
		fillGrayPattern(src, func(x, _ int) byte { return byte(x%2) * 255 })

		dst := NewRGB(112, 112)
		if !Warp5Into(dst, src, kps) {
			t.Fatal("Warp5Into reported false for a one-pixel translation")
		}
		for y := 0; y < dst.H; y++ {
			for x := 0; x < dst.W; x++ {
				r, g, b := dst.At(x, y)
				wr, wg, wb := src.At(x+1, y)
				if diff(r, wr) > 1 || diff(g, wg) > 1 || diff(b, wb) > 1 {
					t.Fatalf("dst(%d,%d) = (%d,%d,%d), want (%d,%d,%d)", x, y, r, g, b, wr, wg, wb)
				}
			}
		}
	})
}

func diff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

func TestWarpRecoversTransformedPattern(t *testing.T) {
	const (
		srcSize = 240
		scale   = 1.25
		angle   = -7 * math.Pi / 180
		dstMid  = 56.0  // centre of the 112x112 destination
		srcMid  = 120.0 // centre of the source
		tol     = 1.5   // quantisation (0.5) + blend (0.5) + rounding (0.5)
	)
	cos, sin := math.Cos(angle), math.Sin(angle)

	// ramp is linear, so bilinear sampling of it is exact apart from the
	// uint8 quantisation of the source.
	ramp := func(x, y float64) float64 { return 20 + 0.8*(x-42) + 0.6*(y-59) }

	src := NewRGB(srcSize, srcSize)
	fillGrayPattern(src, func(x, y int) byte {
		return byte(math.Round(ramp(float64(x), float64(y))))
	})

	// T maps destination coordinates into the source image. Setting the
	// keypoints to T(refpoints) makes the warp fit T (its inverse), so the
	// expectation is dst(x, y) == src(T(x, y)).
	toSrc := func(x, y float64) (float64, float64) {
		px, py := x-dstMid, y-dstMid
		return scale*(cos*px-sin*py) + srcMid, scale*(sin*px+cos*py) + srcMid
	}

	var kps [5][2]float32
	for i, p := range refPoints64() {
		u, v := toSrc(p[0], p[1])
		kps[i][0] = float32(u)
		kps[i][1] = float32(v)
	}

	dst := NewRGB(112, 112)
	if !Warp5Into(dst, src, kps) {
		t.Fatal("Warp5Into reported false for a well-conditioned similarity")
	}

	checked := 0
	for y := 0; y < dst.H; y++ {
		for x := 0; x < dst.W; x++ {
			u, v := toSrc(float64(x), float64(y))
			if u < 1 || v < 1 || u > srcSize-2 || v > srcSize-2 {
				t.Fatalf("destination pixel (%d,%d) maps to (%v,%v), outside the interior", x, y, u, v)
			}
			want := ramp(u, v)
			r, g, b := dst.At(x, y)
			if math.Abs(float64(r)-want) > tol || math.Abs(float64(g)-want) > tol || math.Abs(float64(b)-want) > tol {
				t.Fatalf("dst(%d,%d) = (%d,%d,%d), want %v +- %v", x, y, r, g, b, want, tol)
			}
			checked++
		}
	}
	if checked != dst.W*dst.H {
		t.Fatalf("checked %d pixels, want %d", checked, dst.W*dst.H)
	}
}

func TestWarpDegenerate(t *testing.T) {
	newDst := func() *RGBImage {
		dst := NewRGB(112, 112)
		for i := range dst.Pix {
			dst.Pix[i] = 0xAB
		}
		return dst
	}
	goodSrc := func() *RGBImage {
		src := NewRGB(240, 240)
		fillGrayPattern(src, func(x, y int) byte { return byte(x + y) })
		return src
	}
	goodKps := func() [5][2]float32 {
		var kps [5][2]float32
		for i, p := range refPoints64() {
			kps[i][0] = float32(p[0] * 2)
			kps[i][1] = float32(p[1] * 2)
		}
		return kps
	}

	t.Run("collinear keypoints", func(t *testing.T) {
		collinear := [5][2]float32{{20, 40}, {40, 60}, {60, 80}, {80, 100}, {100, 120}}
		dst := newDst()
		if Warp5Into(dst, goodSrc(), collinear) {
			t.Fatal("Warp5Into reported true for collinear keypoints")
		}
		wantUntouched(t, dst, 0xAB)
	})

	t.Run("identical keypoints", func(t *testing.T) {
		same := [5][2]float32{{50, 50}, {50, 50}, {50, 50}, {50, 50}, {50, 50}}
		if _, ok := SimilarityFromPoints([5][2]float64{{50, 50}, {50, 50}, {50, 50}, {50, 50}, {50, 50}}, refPoints64()); ok {
			t.Fatal("SimilarityFromPoints reported ok for a single distinct point")
		}
		dst := newDst()
		if Warp5Into(dst, goodSrc(), same) {
			t.Fatal("Warp5Into reported true for identical keypoints")
		}
		wantUntouched(t, dst, 0xAB)
	})

	t.Run("non-finite keypoints", func(t *testing.T) {
		for _, v := range []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))} {
			kps := goodKps()
			kps[2][1] = v
			dst := newDst()
			if Warp5Into(dst, goodSrc(), kps) {
				t.Fatalf("Warp5Into reported true for a keypoint of %v", v)
			}
			wantUntouched(t, dst, 0xAB)
		}
	})

	t.Run("non-finite fit input", func(t *testing.T) {
		src := refPoints64()
		src[0][0] = math.NaN()
		if _, ok := SimilarityFromPoints(src, refPoints64()); ok {
			t.Fatal("SimilarityFromPoints reported ok for a NaN source point")
		}
		dst := refPoints64()
		dst[4][1] = math.Inf(1)
		if _, ok := SimilarityFromPoints(refPoints64(), dst); ok {
			t.Fatal("SimilarityFromPoints reported ok for an infinite destination point")
		}
	})

	t.Run("zero-size images", func(t *testing.T) {
		dst := newDst()
		if Warp5Into(dst, &RGBImage{}, goodKps()) {
			t.Fatal("Warp5Into reported true for a zero-size source")
		}
		wantUntouched(t, dst, 0xAB)

		if Warp5Into(nil, goodSrc(), goodKps()) {
			t.Fatal("Warp5Into reported true for a nil destination")
		}
		if Warp5Into(NewRGB(0, 0), goodSrc(), goodKps()) {
			t.Fatal("Warp5Into reported true for a zero-size destination")
		}
		if Warp5Into(dst, nil, goodKps()) {
			t.Fatal("Warp5Into reported true for a nil source")
		}
		wantUntouched(t, dst, 0xAB)
	})
}

func TestWarpHasNoAllocs(t *testing.T) {
	src := NewRGB(240, 240)
	fillGrayPattern(src, func(x, y int) byte { return byte(x + y) })
	dst := NewRGB(112, 112)

	var kps [5][2]float32
	for i, p := range refPoints64() {
		kps[i][0] = float32(p[0] * 2)
		kps[i][1] = float32(p[1] * 2)
	}
	if !Warp5Into(dst, src, kps) {
		t.Fatal("Warp5Into reported false for the allocation test keypoints")
	}

	if got := testing.AllocsPerRun(50, func() { Warp5Into(dst, src, kps) }); got != 0 {
		t.Fatalf("Warp5Into allocated %v times per run, want 0", got)
	}

	srcPts := refPoints64()
	dstPts := refPoints64()
	if got := testing.AllocsPerRun(50, func() { SimilarityFromPoints(srcPts, dstPts) }); got != 0 {
		t.Fatalf("SimilarityFromPoints allocated %v times per run, want 0", got)
	}
}
