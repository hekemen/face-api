package service

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"strings"
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

// makeNoiseJPEG builds a w×h random-noise JPEG and returns it base64-encoded.
// Noise compresses poorly, so the output stays large even for modest sizes —
// useful for exercising the "large image" thumbnail path.
func makeNoiseJPEG(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	rnd := rand.New(rand.NewSource(1))
	for i := range img.Pix {
		img.Pix[i] = uint8(rnd.Intn(256))
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode noise jpeg: %v", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
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
	// Noise compresses poorly, so it exceeds the thumbnail threshold; a
	// smooth gradient of the same size would not.
	large := makeNoiseJPEG(t, 200, 150)
	if len(large) <= thumbnailSizeThreshold {
		t.Fatalf("test image too small to exercise the thumbnail path: %d", len(large))
	}
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

func TestThumbnailFromBase64_VeryLargeImage(t *testing.T) {
	in := makeNoiseJPEG(t, 1920, 1080)
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
}

func TestApplyThumbnails_MixedEntries(t *testing.T) {
	small := makeTestJPEG(t, 50, 40)
	large := makeNoiseJPEG(t, 200, 150)
	undecodable := strings.Repeat("x", 9000) // valid base64 alphabet, not a JPEG
	entries := []domain.AuditEntry{
		{FaceImage: strings.Repeat("A", 9000), FaceThumb: "stored-thumb"}, // stored thumb wins, no decode
		{FaceImage: large},       // large, no thumb → generated
		{FaceImage: small},       // small → untouched
		{FaceImage: undecodable}, // undecodable → untouched
		{},                       // no image → untouched
	}
	applyThumbnails(entries, 96)

	if entries[0].FaceImage != "stored-thumb" || entries[0].FaceThumb != "" {
		t.Errorf("stored thumb not swapped in: %+v", entries[0])
	}
	if entries[1].FaceImage == large || len(entries[1].FaceImage) >= len(large) {
		t.Errorf("large entry not thumbnailized: len %d vs %d", len(entries[1].FaceImage), len(large))
	}
	if m := maxDimOf(t, entries[1].FaceImage); m > 96 {
		t.Errorf("generated thumbnail max dimension = %d, want <= 96", m)
	}
	if entries[2].FaceImage != small {
		t.Error("small entry modified")
	}
	if entries[3].FaceImage != undecodable {
		t.Error("undecodable entry modified")
	}
	if entries[4].FaceImage != "" {
		t.Error("empty entry modified")
	}
}
