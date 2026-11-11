package service

import (
	"errors"
	"os"
	"testing"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"

	"h2hsecure.com/face/internal/domain"
)

// TestProcessorReusesTensorsAndCloses exercises the real ONNX Runtime path
// through the preallocated processor. It is skipped unless ORT_TEST=1 and
// LD_LIBRARY_PATH points at libonnxruntime.so, so the unit suite stays
// runnable without the ORT shared library.
func TestProcessorReusesTensorsAndCloses(t *testing.T) {
	if os.Getenv("ORT_TEST") == "" {
		t.Skip("set ORT_TEST=1 with LD_LIBRARY_PATH pointing at libonnxruntime.so")
	}

	rt, err := ort.NewRuntime("", 23)
	if err != nil {
		t.Fatalf("ort.NewRuntime: %v", err)
	}
	defer rt.Close()

	env, err := rt.NewEnv("processor-ort-test", ort.LoggingLevelError)
	if err != nil {
		t.Fatalf("rt.NewEnv: %v", err)
	}

	openSession := func(path string) *ort.Session {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		defer f.Close()
		s, err := rt.NewSessionFromReader(env, f, nil)
		if err != nil {
			t.Fatalf("NewSessionFromReader %s: %v", path, err)
		}
		return s
	}

	detS := openSession("../../models/scrfd_500m.onnx")
	recS := openSession("../../models/arcface_w600k_mbf.onnx")
	defer detS.Close()
	defer recS.Close()

	p, err := NewFaceProcessor(rt, env, detS, recS, AlignmentBBox)
	if err != nil {
		t.Fatalf("NewFaceProcessor: %v", err)
	}
	defer p.Close()

	pix := make([]byte, 640*480*3)
	for i := 0; i < 2; i++ {
		_, err := p.DetectAndCropFrame(640, 480, pix)
		if !errors.Is(err, domain.ErrNoFace) {
			t.Fatalf("sample %d: err = %v, want domain.ErrNoFace", i, err)
		}
	}

	p.Close()
	p.Close() // Close must be idempotent
}
