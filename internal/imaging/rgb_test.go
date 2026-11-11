package imaging

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"
)

const testW, testH = 8, 8

// fillYCbCr writes a non-trivial pattern into every luma and chroma sample of
// img. Chroma values are derived from the sample index, so they are stable for
// every subsampling ratio and no two samples share a value by accident.
func fillYCbCr(t *testing.T, img *image.YCbCr) {
	t.Helper()
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			img.Y[img.YOffset(x, y)] = byte(16 + (x*37+y*53)%220)
			ci := img.COffset(x, y)
			img.Cb[ci] = byte(16 + (ci*37+11)%240)
			img.Cr[ci] = byte(16 + (ci*97+71)%240)
		}
	}
}

func fillRGBA(t *testing.T, img *image.RGBA) {
	t.Helper()
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i+0] = byte((x*31 + y*7) % 256)
			img.Pix[i+1] = byte((x*5 + y*61) % 256)
			img.Pix[i+2] = byte((x*13 + y*3 + 40) % 256)
			img.Pix[i+3] = 255
		}
	}
}

func fillNRGBA(t *testing.T, img *image.NRGBA) {
	t.Helper()
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			i := img.PixOffset(x, y)
			img.Pix[i+0] = byte((x*23 + y*47) % 256)
			img.Pix[i+1] = byte((x*59 + y*11) % 256)
			img.Pix[i+2] = byte((x*7 + y*29 + 90) % 256)
			// Non-opaque alpha: the generic path premultiplies, so the fast
			// path has to premultiply with identical rounding.
			img.Pix[i+3] = byte(60 + (x*11+y*17)%196)
		}
	}
}

func fillGray(t *testing.T, img *image.Gray) {
	t.Helper()
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			img.Pix[img.PixOffset(x, y)] = byte(16 + (x*19+y*41)%240)
		}
	}
}

func TestToRGBMatchesGenericConversion(t *testing.T) {
	ycbcrs := func(ratio image.YCbCrSubsampleRatio) *image.YCbCr {
		img := image.NewYCbCr(image.Rect(0, 0, testW, testH), ratio)
		fillYCbCr(t, img)
		return img
	}
	rgba := image.NewRGBA(image.Rect(0, 0, testW, testH))
	fillRGBA(t, rgba)
	nrgba := image.NewNRGBA(image.Rect(0, 0, testW, testH))
	fillNRGBA(t, nrgba)
	gray := image.NewGray(image.Rect(0, 0, testW, testH))
	fillGray(t, gray)

	// A sub-image with a non-zero origin exercises the Rect.Min / PixOffset
	// handling of the fast paths.
	subRGBA := rgba.SubImage(image.Rect(2, 1, 7, 6)).(*image.RGBA)

	tests := []struct {
		name string
		src  image.Image
	}{
		{name: "YCbCr420", src: ycbcrs(image.YCbCrSubsampleRatio420)},
		{name: "YCbCr422", src: ycbcrs(image.YCbCrSubsampleRatio422)},
		{name: "YCbCr444", src: ycbcrs(image.YCbCrSubsampleRatio444)},
		{name: "RGBA", src: rgba},
		{name: "NRGBA", src: nrgba},
		{name: "Gray", src: gray},
		{name: "RGBASubImage", src: subRGBA},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToRGB(tt.src)
			b := tt.src.Bounds()
			if got.W != b.Dx() || got.H != b.Dy() {
				t.Fatalf("ToRGB size = %dx%d, want %dx%d", got.W, got.H, b.Dx(), b.Dy())
			}
			if len(got.Pix) != b.Dx()*b.Dy()*3 {
				t.Fatalf("len(Pix) = %d, want %d", len(got.Pix), b.Dx()*b.Dy()*3)
			}

			for y := b.Min.Y; y < b.Max.Y; y++ {
				for x := b.Min.X; x < b.Max.X; x++ {
					gr, gg, gb := got.At(x-b.Min.X, y-b.Min.Y)
					wr, wg, wb, _ := tt.src.At(x, y).RGBA()
					wantR, wantG, wantB := uint8(wr>>8), uint8(wg>>8), uint8(wb>>8)
					if gr != wantR || gg != wantG || gb != wantB {
						t.Fatalf("pixel (%d,%d) = (%d,%d,%d), want (%d,%d,%d)",
							x, y, gr, gg, gb, wantR, wantG, wantB)
					}
				}
			}
		})
	}
}

func TestConvertIntoRejectsWrongSize(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 3))
	fillRGBA(t, src)

	t.Run("wrong dimensions", func(t *testing.T) {
		dst := NewRGB(3, 4)
		if ConvertInto(dst, src) {
			t.Fatalf("ConvertInto reported success for a %dx%d destination and a %dx%d source",
				dst.W, dst.H, 4, 3)
		}
	})

	t.Run("short Pix", func(t *testing.T) {
		dst := &RGBImage{W: 4, H: 3, Pix: make([]byte, 10)}
		if ConvertInto(dst, src) {
			t.Fatal("ConvertInto reported success for a destination with a short Pix buffer")
		}
	})

	t.Run("nil arguments", func(t *testing.T) {
		if ConvertInto(nil, src) {
			t.Fatal("ConvertInto reported success for a nil destination")
		}
		if ConvertInto(NewRGB(4, 3), nil) {
			t.Fatal("ConvertInto reported success for a nil source")
		}
	})

	t.Run("matching size", func(t *testing.T) {
		dst := NewRGB(4, 3)
		if !ConvertInto(dst, src) {
			t.Fatal("ConvertInto reported failure for a matching destination size")
		}
		for y := 0; y < 3; y++ {
			for x := 0; x < 4; x++ {
				gr, gg, gb := dst.At(x, y)
				wr, wg, wb, _ := src.At(x, y).RGBA()
				if gr != uint8(wr>>8) || gg != uint8(wg>>8) || gb != uint8(wb>>8) {
					t.Fatalf("pixel (%d,%d) = (%d,%d,%d), want (%d,%d,%d)",
						x, y, gr, gg, gb, wr>>8, wg>>8, wb>>8)
				}
			}
		}
	})
}

// jpegPattern builds a smooth, low-contrast 8x8 pattern with a constant chroma
// shift. JPEG at quality 75 with 4:2:0 chroma subsampling reproduces such a
// pattern within a couple of levels per channel.
func jpegPattern() *RGBImage {
	img := NewRGB(testW, testH)
	for y := 0; y < testH; y++ {
		for x := 0; x < testW; x++ {
			v := uint8(64 + x*5 + y*7)
			i := (y*testW + x) * 3
			img.Pix[i+0] = v
			img.Pix[i+1] = v
			img.Pix[i+2] = v - 8
		}
	}
	return img
}

func TestDecodeRGBFromJPEG(t *testing.T) {
	want := jpegPattern()
	data, err := EncodeJPEG(want)
	if err != nil {
		t.Fatalf("EncodeJPEG: %v", err)
	}

	got, err := DecodeRGB(data)
	if err != nil {
		t.Fatalf("DecodeRGB: %v", err)
	}
	if got.W != want.W || got.H != want.H {
		t.Fatalf("decoded size = %dx%d, want %dx%d", got.W, got.H, want.W, want.H)
	}

	for y := 0; y < want.H; y++ {
		for x := 0; x < want.W; x++ {
			gr, gg, gb := got.At(x, y)
			wr, wg, wb := want.At(x, y)
			if diff := absDiff(gr, wr); diff > 3 {
				t.Fatalf("pixel (%d,%d) red = %d, want %d (±3)", x, y, gr, wr)
			}
			if diff := absDiff(gg, wg); diff > 3 {
				t.Fatalf("pixel (%d,%d) green = %d, want %d (±3)", x, y, gg, wg)
			}
			if diff := absDiff(gb, wb); diff > 3 {
				t.Fatalf("pixel (%d,%d) blue = %d, want %d (±3)", x, y, gb, wb)
			}
		}
	}
}

func TestDecodeRGBRejectsGarbage(t *testing.T) {
	if _, err := DecodeRGB([]byte("not an image")); err == nil {
		t.Fatal("DecodeRGB accepted invalid data")
	}
}

func TestEncodeJPEGProducesDecodableImage(t *testing.T) {
	src := jpegPattern()
	data, err := EncodeJPEG(src)
	if err != nil {
		t.Fatalf("EncodeJPEG: %v", err)
	}
	if !bytes.HasPrefix(data, []byte{0xFF, 0xD8}) {
		t.Fatalf("encoded data does not start with a JPEG SOI marker: % x", data[:min(len(data), 4)])
	}

	decoded, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("image.Decode: %v", err)
	}
	if format != "jpeg" {
		t.Fatalf("format = %q, want %q", format, "jpeg")
	}
	if got, want := decoded.Bounds(), image.Rect(0, 0, testW, testH); got != want {
		t.Fatalf("decoded bounds = %v, want %v", got, want)
	}

	full, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("jpeg.Decode: %v", err)
	}
	for y := 0; y < src.H; y++ {
		for x := 0; x < src.W; x++ {
			wr, wg, wb := src.At(x, y)
			r, g, b, _ := full.At(x, y).RGBA()
			if absDiff(uint8(r>>8), wr) > 3 || absDiff(uint8(g>>8), wg) > 3 || absDiff(uint8(b>>8), wb) > 3 {
				t.Fatalf("pixel (%d,%d) = (%d,%d,%d), want (%d,%d,%d) ±3",
					x, y, r>>8, g>>8, b>>8, wr, wg, wb)
			}
		}
	}
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a) - int(b)
	}
	return int(b) - int(a)
}
