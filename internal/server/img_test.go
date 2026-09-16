package server

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
)

// encodePNG returns a PNG-encoded image of the given size filled with the
// given color, matching what image/image/png would produce for uploads.
func encodePNG(w, h int, c color.RGBA) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestDecodeRGB(t *testing.T) {
	data := encodePNG(4, 2, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	img, err := decodeRGB(data)
	if err != nil {
		t.Fatalf("decodeRGB failed: %v", err)
	}
	if img.w != 4 || img.h != 2 {
		t.Fatalf("decodeRGB dims = %dx%d, want 4x2", img.w, img.h)
	}
	if len(img.pix) != 4*2*3 {
		t.Fatalf("decodeRGB pix len = %d, want %d", len(img.pix), 4*2*3)
	}
	if img.pix[0] != 10 || img.pix[1] != 20 || img.pix[2] != 30 {
		t.Fatalf("decodeRGB first pixel = %v, want {10,20,30}", img.pix[:3])
	}
}

func TestDecodeRGBBadData(t *testing.T) {
	if _, err := decodeRGB([]byte("not an image")); err == nil {
		t.Fatal("decodeRGB accepted garbage bytes")
	}
}

func TestResizePadToSquare(t *testing.T) {
	// 4x2 red input. Longest (4) maps to size 8 -> scale 2, resized 8x4,
	// pasted at top-left of an 8x8 black canvas (right/bottom padding).
	img := &rgbImage{w: 4, h: 2, pix: make([]uint8, 4*2*3)}
	for i := 0; i < 4*2*3; i += 3 {
		img.pix[i] = 200
		img.pix[i+1] = 10
		img.pix[i+2] = 10
	}

	canvas, scale, err := resizePadToSquare(img, 8)
	if err != nil {
		t.Fatalf("resizePadToSquare failed: %v", err)
	}
	if canvas.w != 8 || canvas.h != 8 {
		t.Fatalf("canvas dims = %dx%d, want 8x8", canvas.w, canvas.h)
	}
	if scale != 2.0 {
		t.Fatalf("scale = %v, want 2.0", scale)
	}

	// Right column of the resized area (y < 4) should be red, not black.
	for y := 0; y < 4; y++ {
		off := y*8*3 + 7*3
		if canvas.pix[off] != 200 || canvas.pix[off+1] != 10 || canvas.pix[off+2] != 10 {
			t.Fatalf("resized pixel at (7,%d) = %v, want red", y, canvas.pix[off:off+3])
		}
	}
	// Bottom padding rows (y >= 4) must be black.
	for y := 4; y < 8; y++ {
		off := y * 8 * 3
		if canvas.pix[off] != 0 || canvas.pix[off+1] != 0 || canvas.pix[off+2] != 0 {
			t.Fatalf("padding row %d = %v, want black", y, canvas.pix[off:off+3])
		}
	}
}

func TestImagToNCHW(t *testing.T) {
	// 2x2 image, pixel values (R,G,B):
	//   (10,20,30) (40,50,60)
	//   (70,80,90) (100,110,120)
	img := &rgbImage{w: 2, h: 2, pix: []uint8{
		10, 20, 30, 40, 50, 60,
		70, 80, 90, 100, 110, 120,
	}}

	blob, err := rgbToNCHW(img, 2, 128.0)
	if err != nil {
		t.Fatalf("rgbToNCHW failed: %v", err)
	}
	if len(blob) != 3*2*2 {
		t.Fatalf("blob len = %d, want %d", len(blob), 3*2*2)
	}

	want := func(v uint8) float32 {
		return (float32(v) - 127.5) / 128.0
	}
	// Channel planes: R first, then G, then B (row-major within each plane).
	rPlane := blob[0*4 : 1*4]
	if rPlane[0] != want(10) || rPlane[1] != want(40) || rPlane[2] != want(70) || rPlane[3] != want(100) {
		t.Fatalf("R plane = %v, want normalized [%v %v %v %v]", rPlane, want(10), want(40), want(70), want(100))
	}
	gPlane := blob[1*4 : 2*4]
	if gPlane[0] != want(20) || gPlane[1] != want(50) || gPlane[2] != want(80) || gPlane[3] != want(110) {
		t.Fatalf("G plane = %v, want normalized [%v %v %v %v]", gPlane, want(20), want(50), want(80), want(110))
	}
}

func TestCropRegion(t *testing.T) {
	// 4x4, all pixels distinct color (x,y) so crop bounds are verifiable.
	img := &rgbImage{w: 4, h: 4, pix: make([]uint8, 4*4*3)}
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			off := (y*4 + x) * 3
			img.pix[off] = uint8(x * 10)
			img.pix[off+1] = uint8(y * 10)
			img.pix[off+2] = 5
		}
	}

	got, err := cropRegion(img, image.Rect(1, 1, 3, 3))
	if err != nil {
		t.Fatalf("cropRegion failed: %v", err)
	}
	if got.w != 2 || got.h != 2 {
		t.Fatalf("crop dims = %dx%d, want 2x2", got.w, got.h)
	}
	// Top-left of the crop should be original (1,1): R=10,G=10.
	if got.pix[0] != 10 || got.pix[1] != 10 || got.pix[2] != 5 {
		t.Fatalf("crop (0,0) = %v, want {10,10,5}", got.pix[:3])
	}
	// Bottom-right of crop should be original (2,2): R=20,G=20.
	if got.pix[9] != 20 || got.pix[10] != 20 {
		t.Fatalf("crop (1,1) = %v, want {20,20,5}", got.pix[9:12])
	}
}

func TestEncodeJPEGRoundTrip(t *testing.T) {
	// Use a larger image — JPEG 8x8 DCT artifacts are severe on tiny images.
	w, h := 32, 32
	pix := make([]uint8, w*h*3)
	for i := 0; i < len(pix); i += 3 {
		pix[i] = 10    // R
		pix[i+1] = 200 // G
		pix[i+2] = 30  // B
	}
	img := &rgbImage{w: w, h: h, pix: pix}
	data, err := encodeJPEG(img)
	if err != nil {
		t.Fatalf("encodeJPEG failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("encodeJPEG returned empty bytes")
	}

	decoded, err := decodeRGB(data)
	if err != nil {
		t.Fatalf("decodeRGB(encoded) failed: %v", err)
	}
	if decoded.w != w || decoded.h != h {
		t.Fatalf("roundtrip dims = %dx%d, want %dx%d", decoded.w, decoded.h, w, h)
	}

	if math.Abs(float64(decoded.pix[0])-10) > 15 {
		t.Fatalf("roundtrip R[0] = %d, want ~10", decoded.pix[0])
	}
	if math.Abs(float64(decoded.pix[1])-200) > 15 {
		t.Fatalf("roundtrip G[0] = %d, want ~200", decoded.pix[1])
	}
}
