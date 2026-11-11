package service_test

import (
	"bytes"
	"context"
	"image"
	"os"
	"testing"
	"time"

	"github.com/rs/zerolog"
	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"

	"h2hsecure.com/face/internal/domain"
	"h2hsecure.com/face/internal/framesource"
	"h2hsecure.com/face/internal/imaging"
	"h2hsecure.com/face/internal/rtsp/rtspmock"
	"h2hsecure.com/face/internal/service"
)

// These tests exercise the real ONNX pipeline (SCRFD + ArcFace) against a
// mock RTSP camera serving a real photograph, so the burst scanner is
// verified against genuine pixels rather than fakes.
//
// They need libonnxruntime.so and the two ONNX models, so they skip when
// either is unavailable.

const (
	detModelPath = "../../models/scrfd_500m.onnx"
	recModelPath = "../../models/arcface_w600k_mbf.onnx"
	faceFixture  = "../../test_hopkins_1.jpg"
	// samePersonSecondFixture is the same person as faceFixture, in a
	// different photograph.
	samePersonSecondFixture = "../../test_hopkins_2.jpg"
	// strangerFixture is a different person entirely.
	strangerFixture = "../../test_stranger.jpg"
)

// newRealProcessor builds a processor backed by the real ONNX models, or
// skips the test when they are not available.
func newRealProcessor(t *testing.T) *service.FaceProcessorImpl {
	t.Helper()
	for _, p := range []string{detModelPath, recModelPath, faceFixture} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("missing %s: %v", p, err)
		}
	}
	rt, err := ort.NewRuntime("", 23)
	if err != nil {
		t.Skipf("libonnxruntime.so unavailable: %v", err)
	}
	t.Cleanup(func() { rt.Close() })

	ortEnv, err := rt.NewEnv("face-api-test", ort.LoggingLevelError)
	if err != nil {
		t.Skipf("create ONNX env: %v", err)
	}
	t.Cleanup(func() { ortEnv.Close() })

	opts := &ort.SessionOptions{IntraOpNumThreads: 4}
	detSess := newSession(t, rt, ortEnv, detModelPath, opts)
	recSess := newSession(t, rt, ortEnv, recModelPath, opts)

	proc, err := service.NewFaceProcessor(rt, ortEnv, detSess, recSess, service.AlignmentBBox)
	if err != nil {
		t.Fatalf("NewFaceProcessor: %v", err)
	}
	t.Cleanup(func() { proc.Close() })
	return proc
}

// newSession loads an ONNX model into a session.
func newSession(t *testing.T, rt *ort.Runtime, env *ort.Env, path string, opts *ort.SessionOptions) *ort.Session {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("open model %s: %v", path, err)
	}
	defer f.Close()
	sess, err := rt.NewSessionFromReader(env, f, opts)
	if err != nil {
		t.Skipf("load model %s: %v", path, err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// faceJPEGCrop returns a JPEG of src downscaled so the frame is a size the
// mock camera's MJPEG encoder can keep up with, at a scale where the face is
// still detectable.
//
// The stranger fixture is a full-body photograph, so the face is small within
// it. Downscaling the whole frame (rather than cropping to the face) keeps
// the detector's context, which it needs to find the face at all.
func faceJPEGCrop(t *testing.T, src []byte, scale float64) []byte {
	t.Helper()
	img, err := imaging.DecodeRGB(src)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	// Keep both sides a multiple of 8: the MJPEG RTP encoder requires it.
	w := (int(float64(img.W)*scale) / 8) * 8
	h := (int(float64(img.H)*scale) / 8) * 8
	if w < 64 || h < 64 {
		t.Fatalf("degenerate scale %.2f -> %dx%d", scale, w, h)
	}
	out := imaging.NewRGB(w, h)
	if err := imaging.CropInto(out, img, image.Rect(0, 0, img.W, img.H)); err != nil {
		t.Fatalf("downscale fixture: %v", err)
	}
	data, err := imaging.EncodeJPEG(out)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return data
}

// padForMock pads a JPEG onto a square canvas whose side is a multiple of
// 8, which the MJPEG RTP encoder requires.
func padForMock(t *testing.T, jpegBytes []byte) []byte {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	b := img.Bounds()
	side := b.Dx()
	if b.Dy() > side {
		side = b.Dy()
	}
	side = (side + 7) / 8 * 8
	padded, err := rtspmock.PadJPEGToSide(jpegBytes, side)
	if err != nil {
		t.Fatalf("pad fixture: %v", err)
	}
	return padded
}

// TestScanBurstMatchesRealFaceFromMockCamera is the end-to-end proof that
// the burst scanner works: it grabs a frame from an RTSP camera, detects the
// face, and matches it against an enrollment made from the same photo.
func TestScanBurstMatchesRealFaceFromMockCamera(t *testing.T) {
	proc := newRealProcessor(t)

	// Enroll from the fixture so the enrollment is a real embedding.
	photo, err := os.ReadFile(faceFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	crop, err := proc.DetectAndCrop(photo)
	if err != nil {
		t.Fatalf("DetectAndCrop (enroll): %v", err)
	}

	users := &e2eUserRepo{users: []*domain.User{{
		Name:       "anthony",
		Embeddings: []domain.FaceEmbedding{crop.Embedding},
	}}}
	audit := &e2eAuditRepo{}
	svc := service.New(users, audit, proc, 0.45)

	// Serve the same photo from a mock camera.
	srv, err := rtspmock.New("127.0.0.1:18572")
	if err != nil {
		t.Skipf("rtspmock port busy: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("rtspmock Start: %v", err)
	}
	defer srv.Stop()
	srv.SetFrame(padForMock(t, photo))

	svc.WithFrameGrabber(framesource.NewGrabber(srv.URL(), zerolog.Nop()))
	svc.WithBurst(5*time.Second, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := svc.ScanBurst(ctx)
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}

	t.Logf("result: status=%q name=%q similarity=%.4f matched=%v reason=%q",
		res.Status, res.Name, res.Similarity, res.Matched, res.Reason)

	if res.Status != "ok" {
		t.Fatalf("Status = %q, want ok (reason %q)", res.Status, res.Reason)
	}
	if res.Name != "anthony" {
		t.Errorf("Name = %q, want anthony", res.Name)
	}
	if !res.Matched {
		t.Error("Matched = false, want true")
	}
	if res.Similarity < 0.45 {
		t.Errorf("Similarity = %v, want >= 0.45", res.Similarity)
	}

	// Exactly one audit row for the burst.
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(audit.entries))
	}
	if audit.entries[0].Status != domain.AuditStatusMatched {
		t.Errorf("audit status = %q, want matched", audit.entries[0].Status)
	}
	if len(audit.entries[0].Embedding) == 0 {
		t.Error("audit row should carry the embedding for promotion")
	}
}

// TestScanBurstRejectsStrangerFromMockCamera checks that a genuinely
// different person scores below the threshold and is not reported as a match.
//
// The two hopkins fixtures are the same person, so they cannot be used here:
// the same person in a different photo legitimately scores ~0.52. This test
// uses a different identity.
func TestScanBurstRejectsStrangerFromMockCamera(t *testing.T) {
	proc := newRealProcessor(t)

	// Enroll Anthony; serve a different person.
	photo, err := os.ReadFile(faceFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	other, err := os.ReadFile(strangerFixture)
	if err != nil {
		t.Skipf("missing stranger fixture: %v", err)
	}
	crop, err := proc.DetectAndCrop(photo)
	if err != nil {
		t.Fatalf("DetectAndCrop (enroll): %v", err)
	}

	users := &e2eUserRepo{users: []*domain.User{{
		Name:       "anthony",
		Embeddings: []domain.FaceEmbedding{crop.Embedding},
	}}}
	audit := &e2eAuditRepo{}
	svc := service.New(users, audit, proc, 0.45)

	srv, err := rtspmock.New("127.0.0.1:18574")
	if err != nil {
		t.Skipf("rtspmock port busy: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("rtspmock Start: %v", err)
	}
	defer srv.Stop()
	// Crop to the face so it stays detectable once the frame is padded.
	srv.SetFrame(padForMock(t, faceJPEGCrop(t, other, 0.35)))

	svc.WithFrameGrabber(framesource.NewGrabber(srv.URL(), zerolog.Nop()))
	svc.WithBurst(5*time.Second, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := svc.ScanBurst(ctx)
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}

	t.Logf("result: status=%q name=%q similarity=%.4f matched=%v reason=%q",
		res.Status, res.Name, res.Similarity, res.Matched, res.Reason)

	if res.Reason == "No face detected" {
		t.Fatal("the stranger fixture produced no face; the test would not prove anything")
	}
	if res.Matched {
		t.Errorf("Matched = true for a different person (similarity %v)", res.Similarity)
	}
	if res.Status != "not ok" {
		t.Errorf("Status = %q, want not ok", res.Status)
	}
}

// TestSamePersonDifferentPhotoScoresHigh records the similarity the same
// person scores in a different photograph.
//
// This is the number that decides how low the threshold can go: a genuine
// match must clear it, so it is an upper bound on the threshold, not a
// lower one.
func TestSamePersonDifferentPhotoScoresHigh(t *testing.T) {
	proc := newRealProcessor(t)

	photo, err := os.ReadFile(faceFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	second, err := os.ReadFile(samePersonSecondFixture)
	if err != nil {
		t.Skipf("missing second fixture: %v", err)
	}
	crop, err := proc.DetectAndCrop(photo)
	if err != nil {
		t.Fatalf("DetectAndCrop (enroll): %v", err)
	}

	users := &e2eUserRepo{users: []*domain.User{{
		Name:       "anthony",
		Embeddings: []domain.FaceEmbedding{crop.Embedding},
	}}}
	audit := &e2eAuditRepo{}
	svc := service.New(users, audit, proc, 0.45)

	srv, err := rtspmock.New("127.0.0.1:18576")
	if err != nil {
		t.Skipf("rtspmock port busy: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("rtspmock Start: %v", err)
	}
	defer srv.Stop()
	srv.SetFrame(padForMock(t, second))

	svc.WithFrameGrabber(framesource.NewGrabber(srv.URL(), zerolog.Nop()))
	svc.WithBurst(5*time.Second, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := svc.ScanBurst(ctx)
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}

	t.Logf("same person, different photo: similarity=%.4f matched=%v", res.Similarity, res.Matched)
	if res.Similarity < 0.45 {
		t.Errorf("similarity = %v, want >= 0.45 (the same person must match)", res.Similarity)
	}
}

// --- fakes ---

type e2eUserRepo struct {
	users []*domain.User
}

func (m *e2eUserRepo) GetByName(name string) (*domain.User, error) {
	for _, u := range m.users {
		if u.Name == name {
			return u, nil
		}
	}
	return nil, os.ErrNotExist
}
func (m *e2eUserRepo) Exists(name string) (bool, error) {
	_, err := m.GetByName(name)
	return err == nil, nil
}
func (m *e2eUserRepo) Save(u *domain.User) error        { return nil }
func (m *e2eUserRepo) Delete(name string) error         { return nil }
func (m *e2eUserRepo) ListAll() ([]*domain.User, error) { return m.users, nil }

type e2eAuditRepo struct {
	entries []domain.AuditEntry
}

func (m *e2eAuditRepo) Append(entries []domain.AuditEntry) error {
	m.entries = append(m.entries, entries...)
	return nil
}
func (m *e2eAuditRepo) Recent(n int) ([]domain.AuditEntry, error) { return m.entries, nil }
func (m *e2eAuditRepo) ListPaginated(opts domain.ListPaginatedOpts) ([]domain.AuditEntry, int, []string, error) {
	return m.entries, len(m.entries), nil, nil
}
func (m *e2eAuditRepo) CountAll() (int, error) { return len(m.entries), nil }
func (m *e2eAuditRepo) ListUnmatched(n int) ([]domain.AuditEntry, error) {
	return m.entries, nil
}
func (m *e2eAuditRepo) ComputeStats() (domain.Stats, error) { return domain.Stats{}, nil }
func (m *e2eAuditRepo) GetAuditFace(timestamp string) (string, error) {
	return "", nil
}
func (m *e2eAuditRepo) ListRecognizeEntries(opts domain.ListRecognizeEntriesOpts) ([]domain.AuditEntry, int, error) {
	return nil, 0, nil
}
func (m *e2eAuditRepo) CountRecognizeEntries() (int, error) { return 0, nil }
func (m *e2eAuditRepo) ComputeRecognizeSummary() (domain.RecognizeSummaryResponse, error) {
	return domain.RecognizeSummaryResponse{}, nil
}
func (m *e2eAuditRepo) SetFaceThumb(t time.Time, faceImage, thumb string) error { return nil }
func (m *e2eAuditRepo) BackfillFaceThumbs(thumbFor func(string) string, maxUpdates int) (int, error) {
	return 0, nil
}
