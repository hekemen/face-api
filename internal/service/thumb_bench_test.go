package service

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"math/rand"
	"testing"
)

// benchNoiseJPEG renders a w×h random-noise image as a base64 JPEG. Noise
// content maximizes JPEG entropy so the encode/decode cost is representative
// of a real (non-flat) photo rather than a solid-color frame.
func benchNoiseJPEG(b *testing.B, w, h int) string {
	b.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(1))
	for i := range img.Pix {
		img.Pix[i] = uint8(rnd.Intn(256))
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		b.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// BenchmarkThumbnailFromBase64_New times the current implementation: decode
// via image.Decode and downscale with draw.BiLinear.Scale (sampled, so the
// cost is proportional to the destination size rather than the source's full
// resolution).
func BenchmarkThumbnailFromBase64_New(b *testing.B) {
	b64 := benchNoiseJPEG(b, 1920, 1080)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := thumbnailFromBase64(b64, thumbMaxDim); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkThumbnailFromBase64_OldPath times the previous implementation:
// decode to a packed rgbImage, then a manual bilinear resize driven by the
// full source, then re-encode. It exists only to document the speedup from
// the sampled-scaling rewrite.
func BenchmarkThumbnailFromBase64_OldPath(b *testing.B) {
	b64 := benchNoiseJPEG(b, 1920, 1080)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			b.Fatal(err)
		}
		img, err := decodeRGB(data)
		if err != nil {
			b.Fatal(err)
		}
		longest := img.w
		if img.h > longest {
			longest = img.h
		}
		scale := float64(thumbMaxDim) / float64(longest)
		nw := round(float64(img.w) * scale)
		nh := round(float64(img.h) * scale)
		if nw < 1 {
			nw = 1
		}
		if nh < 1 {
			nh = 1
		}
		resized := bilinearResize(img, nw, nh)
		if _, err := encodeFaceToBase64(resized); err != nil {
			b.Fatal(err)
		}
	}
}