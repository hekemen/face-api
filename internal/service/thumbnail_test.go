package service

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"testing"
	"time"

	"h2hsecure.com/face/internal/domain"
)

// makeTestJPEG builds a w×h gradient JPEG and returns it base64-encoded.
func makeTestJPEG(t *testing.T, w, h int) string {
	t.Helper()
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.Set(x, y, color.NRGBA{
				R: uint8(x * 255 / (w - 1)),
				G: uint8(y * 255 / (h - 1)),
				B: uint8(128),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// maxDimOf returns the longer side of a base64 JPEG, or 0 on error.
func maxDimOf(t *testing.T, b64 string) int {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode jpeg: %v", err)
	}
	b := img.Bounds()
	m := b.Dx()
	if b.Dy() > m {
		m = b.Dy()
	}
	return m
}

func TestThumbnailFromBase64_ShrinksLargeImage(t *testing.T) {
	in := makeTestJPEG(t, 200, 150)
	out, err := thumbnailFromBase64(in, 96)
	if err != nil {
		t.Fatalf("thumbnailFromBase64: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty thumbnail, got empty string")
	}
	if len(out) >= len(in) {
		t.Errorf("thumbnail base64 len %d not smaller than input %d", len(out), len(in))
	}
	if m := maxDimOf(t, out); m > 96 {
		t.Errorf("thumbnail max dimension = %d, want <= 96", m)
	}
	// Aspect ratio (200:150 = 4:3) should be roughly preserved.
	data, _ := base64.StdEncoding.DecodeString(out)
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode thumbnail: %v", err)
	}
	b := img.Bounds()
	ratio := float64(b.Dx()) / float64(b.Dy())
	if ratio < 1.2 || ratio > 1.5 {
		t.Errorf("thumbnail aspect ratio = %f, want ~1.33", ratio)
	}
}

func TestThumbnailFromBase64_SmallImageStaysSmall(t *testing.T) {
	in := makeTestJPEG(t, 50, 40)
	out, err := thumbnailFromBase64(in, 96)
	if err != nil {
		t.Fatalf("thumbnailFromBase64: %v", err)
	}
	if out == "" {
		t.Fatal("expected non-empty thumbnail, got empty string")
	}
	if m := maxDimOf(t, out); m > 96 {
		t.Errorf("thumbnail max dimension = %d, want <= 96", m)
	}
}

func TestThumbnailFromBase64_InvalidInput(t *testing.T) {
	if _, err := thumbnailFromBase64("!!!not-base64!!!", 96); err == nil {
		t.Error("expected error for invalid base64, got nil")
	}
	notImage := base64.StdEncoding.EncodeToString([]byte("hello world, definitely not an image"))
	if _, err := thumbnailFromBase64(notImage, 96); err == nil {
		t.Error("expected error for non-image data, got nil")
	}
}

func TestListAuditPaginated_ThumbnailizesFaces(t *testing.T) {
	large := makeTestJPEG(t, 200, 150)
	audit := &mockAuditRepo{
		entries: []domain.AuditEntry{
			{
				Time:      time.Now().Truncate(time.Second),
				Endpoint:  "recognize",
				Name:      "alice",
				Status:    "matched",
				HasFace:   true,
				FaceImage: large,
			},
		},
	}
	svc := New(&mockUserRepo{}, audit, &mockProcessor{}, 0.45)

	entries, total, _, err := svc.ListAuditPaginated(domain.ListPaginatedOpts{Page: 1, PerPage: 20})
	if err != nil {
		t.Fatalf("ListAuditPaginated: %v", err)
	}
	if total != 1 || len(entries) != 1 {
		t.Fatalf("got total=%d entries=%d, want 1/1", total, len(entries))
	}
	e := entries[0]
	if e.Name != "alice" || e.Status != "matched" || !e.HasFace {
		t.Errorf("entry fields not preserved: %+v", e)
	}
	if e.FaceImage == "" {
		t.Fatal("FaceImage is empty, want thumbnail")
	}
	if len(e.FaceImage) >= len(large) {
		t.Errorf("FaceImage base64 len %d not smaller than original %d", len(e.FaceImage), len(large))
	}
	if m := maxDimOf(t, e.FaceImage); m > 96 {
		t.Errorf("FaceImage max dimension = %d, want <= 96", m)
	}
}
