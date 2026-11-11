package imaging

import (
	"image"
	"testing"
)

// fillGrayPattern writes v(x, y) into all three channels of img, so the same
// expected value applies to red, green and blue.
func fillGrayPattern(img *RGBImage, v func(x, y int) byte) {
	for y := 0; y < img.H; y++ {
		for x := 0; x < img.W; x++ {
			i := (y*img.W + x) * 3
			c := v(x, y)
			img.Pix[i+0] = c
			img.Pix[i+1] = c
			img.Pix[i+2] = c
		}
	}
}

// wantPixel fails the test unless img holds (r, g, b) at (x, y).
func wantPixel(t *testing.T, img *RGBImage, x, y int, r, g, b uint8) {
	t.Helper()
	gotR, gotG, gotB := img.At(x, y)
	if gotR != r || gotG != g || gotB != b {
		t.Fatalf("pixel (%d,%d) = (%d,%d,%d), want (%d,%d,%d)", x, y, gotR, gotG, gotB, r, g, b)
	}
}

func TestResizeIntoKnownPixels(t *testing.T) {
	// 2x2 source, bilinear-upscaled to 4x4. scaleX = scaleY = 0.5, so fx and
	// fy are 0 or 0.5 and the right/bottom edges clamp x1/y1 onto the last
	// source pixel. uint8 conversion truncates: 138.75 -> 138, 227.5 -> 227.
	src := NewRGB(2, 2)
	copy(src.Pix, []byte{
		0, 0, 0, 100, 100, 100,
		200, 200, 200, 255, 255, 255,
	})
	dst := NewRGB(4, 4)
	ResizeInto(dst, src)

	want := [4][4]uint8{
		{0, 50, 100, 100},
		{100, 138, 177, 177},
		{200, 227, 255, 255},
		{200, 227, 255, 255},
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			v := want[y][x]
			wantPixel(t, dst, x, y, v, v, v)
		}
	}

	// A 1x1 source must smear each channel into its own destination channel.
	one := NewRGB(1, 1)
	copy(one.Pix, []byte{10, 20, 30})
	flat := NewRGB(2, 2)
	ResizeInto(flat, one)
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			wantPixel(t, flat, x, y, 10, 20, 30)
		}
	}
}

func TestPadSquareInto(t *testing.T) {
	t.Run("wide", func(t *testing.T) {
		// 4x2 source into an 8x8 canvas: scale = 8/4 = 2, so the source fills
		// the top 8x4 block at the top-left corner and the bottom four rows
		// stay black. The source is resized to 8 wide first (scaleX 0.5), so
		// odd columns average their two neighbours and the last column clamps.
		src := NewRGB(4, 2)
		fillGrayPattern(src, func(x, _ int) byte { return byte(x * 40) })

		dst := NewRGB(8, 8)
		for i := range dst.Pix {
			dst.Pix[i] = 0xFF // prove the padding is written, not left over
		}

		scale := PadSquareInto(dst, src)
		if scale != 2 {
			t.Fatalf("scale = %v, want 2", scale)
		}

		wantRow := [8]uint8{0, 20, 40, 60, 80, 100, 120, 120}
		for y := 0; y < 4; y++ {
			for x := 0; x < 8; x++ {
				v := wantRow[x]
				wantPixel(t, dst, x, y, v, v, v)
			}
		}
		for y := 4; y < 8; y++ {
			for x := 0; x < 8; x++ {
				wantPixel(t, dst, x, y, 0, 0, 0)
			}
		}
	})

	t.Run("tall", func(t *testing.T) {
		// 2x4 source: scale = 8/4 = 2 as well, but the 4x8 result is left
		// aligned, so the right half of every row is padding.
		src := NewRGB(2, 4)
		fillGrayPattern(src, func(_, _ int) byte { return 77 })

		dst := NewRGB(8, 8)
		for i := range dst.Pix {
			dst.Pix[i] = 0xFF
		}

		if scale := PadSquareInto(dst, src); scale != 2 {
			t.Fatalf("scale = %v, want 2", scale)
		}
		for y := 0; y < 8; y++ {
			for x := 0; x < 4; x++ {
				wantPixel(t, dst, x, y, 77, 77, 77)
			}
			for x := 4; x < 8; x++ {
				wantPixel(t, dst, x, y, 0, 0, 0)
			}
		}
	})

	t.Run("empty source", func(t *testing.T) {
		// A zero-sized source is reported as scale 0 and leaves dst untouched.
		dst := NewRGB(8, 8)
		for i := range dst.Pix {
			dst.Pix[i] = 0xAB
		}

		scale := PadSquareInto(dst, &RGBImage{})
		if scale != 0 {
			t.Fatalf("scale = %v, want 0", scale)
		}
		for i, v := range dst.Pix {
			if v != 0xAB {
				t.Fatalf("dst.Pix[%d] = %#x, want untouched 0xab", i, v)
			}
		}
	})
}

func TestCropIntoClipsAndCopies(t *testing.T) {
	// 4x4 source, pixel value = 40*y + 10*x.
	newSrc := func() *RGBImage {
		src := NewRGB(4, 4)
		fillGrayPattern(src, func(x, y int) byte { return byte(40*y + 10*x) })
		return src
	}

	t.Run("clips to bounds", func(t *testing.T) {
		// Rect(2,1,10,10) clips to (2,1)-(4,4): a 2x3 region that matches dst
		// exactly, so the values are copied verbatim.
		dst := NewRGB(2, 3)
		if err := CropInto(dst, newSrc(), image.Rect(2, 1, 10, 10)); err != nil {
			t.Fatalf("CropInto: %v", err)
		}
		want := [3][2]uint8{
			{60, 70},
			{100, 110},
			{140, 150},
		}
		for y := 0; y < 3; y++ {
			for x := 0; x < 2; x++ {
				v := want[y][x]
				wantPixel(t, dst, x, y, v, v, v)
			}
		}
	})

	t.Run("resamples the crop", func(t *testing.T) {
		// Downscaling the full 4x4 region to 2x2 puts the sample points on
		// integer source coordinates (scale 2, fx = fy = 0): (0,0), (2,0),
		// (0,2) and (2,2).
		dst := NewRGB(2, 2)
		if err := CropInto(dst, newSrc(), image.Rect(0, 0, 4, 4)); err != nil {
			t.Fatalf("CropInto: %v", err)
		}
		wantPixel(t, dst, 0, 0, 0, 0, 0)
		wantPixel(t, dst, 1, 0, 20, 20, 20)
		wantPixel(t, dst, 0, 1, 80, 80, 80)
		wantPixel(t, dst, 1, 1, 100, 100, 100)
	})

	t.Run("empty region", func(t *testing.T) {
		dst := NewRGB(2, 2)
		if err := CropInto(dst, newSrc(), image.Rect(10, 10, 12, 12)); err == nil {
			t.Fatal("CropInto: expected an error for a region outside the source")
		}
		// image.Rect normalises its corners, so build the inverted rectangle
		// directly.
		inverted := image.Rectangle{Min: image.Pt(3, 3), Max: image.Pt(1, 1)}
		if err := CropInto(dst, newSrc(), inverted); err == nil {
			t.Fatal("CropInto: expected an error for an inverted rectangle")
		}
	})
}

func TestNCHWIntoValues(t *testing.T) {
	// 2x2 source with distinct values per channel and plane.
	src := NewRGB(2, 2)
	copy(src.Pix, []byte{
		0, 128, 255, 255, 0, 128,
		64, 32, 16, 255, 255, 255,
	})

	dst := make([]float32, 3*2*2)
	if err := NCHWInto(dst, src, 128); err != nil {
		t.Fatalf("NCHWInto: %v", err)
	}

	// Plane order R, G, B; row-major inside each plane; (v - 127.5) / 128.
	// Every value below is an exact multiple of 1/256, so it is exactly
	// representable as a float32.
	want := []float32{
		(0 - 127.5) / 128, (255 - 127.5) / 128, (64 - 127.5) / 128, (255 - 127.5) / 128,
		(128 - 127.5) / 128, (0 - 127.5) / 128, (32 - 127.5) / 128, (255 - 127.5) / 128,
		(255 - 127.5) / 128, (128 - 127.5) / 128, (16 - 127.5) / 128, (255 - 127.5) / 128,
	}
	for i := range want {
		if dst[i] != want[i] {
			t.Fatalf("dst[%d] = %v, want %v", i, dst[i], want[i])
		}
	}

	// The divisor belongs to the caller: 127.5 maps 0 to exactly -1 and 255 to
	// exactly +1, which is the recognition path's normalisation.
	if err := NCHWInto(dst, src, 127.5); err != nil {
		t.Fatalf("NCHWInto(div=127.5): %v", err)
	}
	if dst[0] != -1 || dst[1] != 1 {
		t.Fatalf("div 127.5: dst[0], dst[1] = %v, %v, want -1, 1", dst[0], dst[1])
	}

	if err := NCHWInto(make([]float32, 11), src, 128); err == nil {
		t.Fatal("NCHWInto: expected an error for a destination of the wrong length")
	}
}

func TestGeometryHasNoAllocs(t *testing.T) {
	src := NewRGB(120, 90)
	fillGrayPattern(src, func(x, y int) byte { return byte(x*7 + y*13) })

	square := NewRGB(640, 640)
	small := NewRGB(112, 112)
	blob := make([]float32, 3*112*112)
	region := image.Rect(10, 5, 90, 70)

	cases := []struct {
		name string
		fn   func()
	}{
		{"ResizeInto", func() { ResizeInto(small, src) }},
		{"PadSquareInto", func() { PadSquareInto(square, src) }},
		{"CropInto", func() { CropInto(small, src, region) }},
		{"NCHWInto", func() { NCHWInto(blob, small, 128) }},
		{"all four", func() {
			PadSquareInto(square, src)
			CropInto(small, src, region)
			ResizeInto(small, src)
			NCHWInto(blob, small, 128)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := testing.AllocsPerRun(50, tc.fn); got != 0 {
				t.Fatalf("%s allocated %v times per run, want 0", tc.name, got)
			}
		})
	}
}
