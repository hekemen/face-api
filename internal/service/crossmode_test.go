package service_test

import (
	"os"
	"testing"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"

	"h2hsecure.com/face/internal/domain"
	"h2hsecure.com/face/internal/service"
)

// newProcessorWithMode builds a processor with a specific alignment mode.
func newProcessorWithMode(t *testing.T, mode service.AlignmentMode) *service.FaceProcessorImpl {
	t.Helper()
	for _, p := range []string{detModelPath, recModelPath} {
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
	proc, err := service.NewFaceProcessor(rt, ortEnv, detSess, recSess, mode)
	if err != nil {
		t.Fatalf("NewFaceProcessor: %v", err)
	}
	t.Cleanup(func() { proc.Close() })
	return proc
}

// TestCrossModeSimilarity measures what a bbox-space embedding scores
// against an arcface5-space embedding of the same person.
//
// This is the safety question for switching FACE_ALIGNMENT: the stored
// enrollments are in the old space, and if they still score high against
// new captures the switch would silently produce false positives rather
// than the expected misses.
func TestCrossModeSimilarity(t *testing.T) {
	raw1, err := os.ReadFile("../../test_hopkins_1.jpg")
	if err != nil {
		t.Skipf("missing fixture: %v", err)
	}
	raw2, err := os.ReadFile("../../test_hopkins_2.jpg")
	if err != nil {
		t.Skipf("missing fixture: %v", err)
	}

	bboxProc := newProcessorWithMode(t, "bbox")
	arcProc := newProcessorWithMode(t, "arcface5")

	embed := func(proc interface {
		DetectAndCrop([]byte) (*domain.FaceCrop, error)
	}, raw []byte) []float32 {
		crop, err := proc.DetectAndCrop(raw)
		if err != nil {
			t.Fatalf("detect: %v", err)
		}
		return crop.Embedding
	}

	b1 := embed(bboxProc, raw1)
	b2 := embed(bboxProc, raw2)
	a1 := embed(arcProc, raw1)
	a2 := embed(arcProc, raw2)

	t.Logf("bbox      vs bbox      (same space, same person): %.4f", domain.CosineSimilarity(b1, b2))
	t.Logf("arcface5  vs arcface5  (same space, same person): %.4f", domain.CosineSimilarity(a1, a2))
	t.Logf("bbox      vs arcface5  (CROSS space, same person): %.4f", domain.CosineSimilarity(b1, a1))
	t.Logf("bbox      vs arcface5  (CROSS space, 2nd photo): %.4f", domain.CosineSimilarity(b1, a2))

	// The safety-critical number: with OLD (bbox) enrollments still in the
	// database and NEW (arcface5) captures arriving, how do strangers score?
	// If this is high, switching the mode without re-enrolling produces
	// false positives.
	strangerPath := downloadFixture(t, strangerFixtureURL)
	strangerRaw, err := os.ReadFile(strangerPath)
	if err != nil {
		t.Skipf("missing stranger fixture: %v", err)
	}
	bS := embed(bboxProc, strangerRaw)
	aS := embed(arcProc, strangerRaw)
	t.Logf("")
	t.Logf("SAFETY: old bbox enrollment vs new arcface5 capture")
	t.Logf("  stranger  vs stranger  (same space):      %.4f", domain.CosineSimilarity(bS, aS))
	t.Logf("  same person vs stranger (same space):     %.4f", domain.CosineSimilarity(b1, bS))
	t.Logf("  stranger(bbox) vs person(arcface5) CROSS: %.4f", domain.CosineSimilarity(bS, a1))
	t.Logf("  person(bbox) vs stranger(arcface5) CROSS: %.4f", domain.CosineSimilarity(b1, aS))
}
