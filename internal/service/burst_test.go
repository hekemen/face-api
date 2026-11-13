package service

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"h2hsecure.com/face/internal/domain"
)

// --- burst test fakes ---

type burstProcessor struct {
	crop      *domain.FaceCrop
	crops     []*domain.FaceCrop
	errs      []error
	callCount int32
	blockCh   chan struct{}
}

func (m *burstProcessor) DetectAndCrop(data []byte) (*domain.FaceCrop, error) {
	return m.crop, nil
}

func (m *burstProcessor) DetectAndCropFrame(w, h int, pix []byte) (*domain.FaceCrop, error) {
	idx := atomic.AddInt32(&m.callCount, 1) - 1
	if idx < int32(len(m.errs)) && m.errs[idx] != nil {
		return nil, m.errs[idx]
	}
	if idx < int32(len(m.crops)) {
		return m.crops[idx], nil
	}
	if m.blockCh != nil {
		<-m.blockCh
	}
	if m.crop != nil {
		return m.crop, nil
	}
	return nil, domain.ErrNoFace
}

func (m *burstProcessor) ExtractEmbedding(crop *domain.FaceCrop) (domain.FaceEmbedding, error) {
	return crop.Embedding, nil
}

func (m *burstProcessor) AlignmentMode() string { return "arcface5" }

type burstUserRepo struct {
	users        []*domain.User
	listAllCalls int
}

func (m *burstUserRepo) GetByName(name string) (*domain.User, error) {
	for _, u := range m.users {
		if u.Name == name {
			return u, nil
		}
	}
	return nil, fmt.Errorf("user %q not found", name)
}

func (m *burstUserRepo) Exists(name string) (bool, error) {
	for _, u := range m.users {
		if u.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (m *burstUserRepo) Save(u *domain.User) error { return nil }
func (m *burstUserRepo) Delete(name string) error  { return nil }

func (m *burstUserRepo) ListAll() ([]*domain.User, error) {
	m.listAllCalls++
	return m.users, nil
}

// burstGrabber is a fake FrameGrabber that hands out canned frames.
type burstGrabber struct {
	w, h     int
	frames   int   // number of frames to hand out before failing
	err      error // returned by every Grab
	calls    int32 // number of Grab calls
	grabHook func(call int)
}

func (g *burstGrabber) Grab(ctx context.Context) (int, int, []byte, error) {
	call := int(atomic.AddInt32(&g.calls, 1))
	if g.grabHook != nil {
		g.grabHook(call)
	}
	if g.err != nil {
		return 0, 0, nil, g.err
	}
	if g.frames > 0 && call > g.frames {
		return 0, 0, nil, errors.New("no more frames")
	}
	pix := make([]byte, g.w*g.h*3)
	for i := range pix {
		pix[i] = byte(call)
	}
	return g.w, g.h, pix, nil
}

func TestScanBurstReturnsFirstMatch(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{emb}}
	crop := &domain.FaceCrop{Data: []byte("crop"), Embedding: emb, BBox: [4]float32{0, 0, 1, 1}}
	proc := &burstProcessor{crop: crop}
	grabber := &burstGrabber{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(200*time.Millisecond, 20*time.Millisecond)

	res, err := s.ScanBurst(context.Background())
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if res.Status != "ok" {
		t.Fatalf("Status = %q, want ok", res.Status)
	}
	if res.Name != "alice" {
		t.Errorf("Name = %q, want alice", res.Name)
	}
	if !res.Matched {
		t.Error("Matched = false, want true")
	}
	if res.Similarity < 0.45 {
		t.Errorf("Similarity = %v, want >= 0.45", res.Similarity)
	}
	if res.DurationMs < 0 {
		t.Errorf("DurationMs = %v, want >= 0", res.DurationMs)
	}

	// Exactly one audit row, matched.
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Status != domain.AuditStatusMatched {
		t.Errorf("entry status = %q, want matched", e.Status)
	}
	if e.Name != "alice" {
		t.Errorf("entry name = %q, want alice", e.Name)
	}
	if e.Endpoint != "stream-check" {
		t.Errorf("entry endpoint = %q, want stream-check", e.Endpoint)
	}
}

func TestScanBurstWritesOneAuditRow(t *testing.T) {
	aliceEmb := domain.FaceEmbedding{1, 0, 0, 0}
	cropEmb := domain.FaceEmbedding{0, 1, 0, 0} // orthogonal to alice
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{aliceEmb}}
	crop := &domain.FaceCrop{Data: []byte("crop"), Embedding: cropEmb}
	proc := &burstProcessor{crop: crop}
	grabber := &burstGrabber{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(200*time.Millisecond, 20*time.Millisecond)

	res, err := s.ScanBurst(context.Background())
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if res.Status != "not ok" {
		t.Fatalf("Status = %q, want not ok", res.Status)
	}
	if res.Matched {
		t.Error("Matched = true, want false")
	}

	// Exactly one not_matched audit row.
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Status != domain.AuditStatusNotMatched {
		t.Errorf("entry status = %q, want not_matched", e.Status)
	}
	if e.Endpoint != "stream-check" {
		t.Errorf("entry endpoint = %q, want stream-check", e.Endpoint)
	}
	if e.Embedding == nil || len(e.Embedding) == 0 {
		t.Error("entry embedding should be stored for promotion")
	}
}

func TestScanBurstStopsAtFirstMatch(t *testing.T) {
	aliceEmb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{aliceEmb}}
	// The first sample matches; the burst must stop there and not keep
	// grabbing frames.
	crop := &domain.FaceCrop{Embedding: aliceEmb}
	proc := &burstProcessor{crop: crop}
	grabber := &burstGrabber{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(5*time.Second, 10*time.Millisecond)

	res, err := s.ScanBurst(context.Background())
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if res.Status != "ok" {
		t.Fatalf("Status = %q, want ok", res.Status)
	}
	if got := atomic.LoadInt32(&grabber.calls); got != 1 {
		t.Fatalf("Grab called %d times, want 1 (the burst must stop at the first match)", got)
	}
}

func TestScanBurstReadsUsersOnce(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{emb}}
	crop := &domain.FaceCrop{Data: []byte("crop"), Embedding: emb}
	proc := &burstProcessor{crop: crop}
	grabber := &burstGrabber{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(100*time.Millisecond, 10*time.Millisecond)

	if _, err := s.ScanBurst(context.Background()); err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if users.listAllCalls != 1 {
		t.Fatalf("ListAll called %d times, want 1", users.listAllCalls)
	}
}

func TestScanBurstNoFrameGrabber(t *testing.T) {
	s := New(&mockUserRepo{}, &mockAuditRepo{}, &mockProcessor{}, 0.5)
	// No frame grabber configured.
	_, err := s.ScanBurst(context.Background())
	if err == nil {
		t.Fatal("expected error when frame grabber is not configured")
	}
}

func TestScanBurstDeadlineReturnsBest(t *testing.T) {
	aliceEmb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{aliceEmb}}
	crop1 := &domain.FaceCrop{Embedding: domain.FaceEmbedding{0, 1, 0, 0}}     // score 0.0
	crop2 := &domain.FaceCrop{Embedding: domain.FaceEmbedding{0.6, 0.8, 0, 0}} // score 0.6
	crop3 := &domain.FaceCrop{Embedding: domain.FaceEmbedding{0, 0, 1, 0}}     // score 0.0
	proc := &burstProcessor{crops: []*domain.FaceCrop{crop1, crop2, crop3}}
	grabber := &burstGrabber{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.7).WithFrameGrabber(grabber).WithBurst(150*time.Millisecond, 20*time.Millisecond)

	res, err := s.ScanBurst(context.Background())
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if res.Status != "not ok" {
		t.Fatalf("Status = %q, want not ok", res.Status)
	}
	if res.Matched {
		t.Error("Matched = true, want false")
	}
	if res.Name != "alice" {
		t.Errorf("Name = %q, want alice", res.Name)
	}
	if res.Similarity < 0.55 || res.Similarity > 0.65 {
		t.Errorf("Similarity = %v, want ~0.6 (best of 0.3, 0.6, 0.2)", res.Similarity)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Status != domain.AuditStatusNotMatched {
		t.Errorf("status = %q, want not_matched", e.Status)
	}
	if len(e.Embedding) != 4 || e.Embedding[0] != 0.6 || e.Embedding[1] != 0.8 {
		t.Errorf("embedding = %v, want best sample (0.6, 0.8, 0, 0)", e.Embedding)
	}
}

func TestScanBurstAllNoFaceWritesNoFaceRow(t *testing.T) {
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{{1, 0, 0, 0}}}
	proc := &burstProcessor{errs: []error{domain.ErrNoFace, domain.ErrNoFace, domain.ErrNoFace}}
	grabber := &burstGrabber{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(100*time.Millisecond, 10*time.Millisecond)

	res, err := s.ScanBurst(context.Background())
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if res.Status != "not ok" {
		t.Fatalf("Status = %q, want not ok", res.Status)
	}
	if res.Reason != "No face detected" {
		t.Errorf("Reason = %q, want No face detected", res.Reason)
	}
	if res.Matched {
		t.Error("Matched = true, want false")
	}
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Status != domain.AuditStatusNoFace {
		t.Errorf("status = %q, want no_face", e.Status)
	}
	if e.FaceImage == "" {
		t.Error("FaceImage should be non-empty for no_face row")
	}
}

func TestScanBurstProcessorErrors(t *testing.T) {
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{{1, 0, 0, 0}}}
	errs := []error{errors.New("processing error"), errors.New("processing error"), errors.New("processing error"),
		errors.New("processing error"), errors.New("processing error"), errors.New("processing error"),
		errors.New("processing error"), errors.New("processing error"), errors.New("processing error"),
		errors.New("processing error"), errors.New("processing error"), errors.New("processing error")}
	proc := &burstProcessor{errs: errs}
	grabber := &burstGrabber{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(100*time.Millisecond, 10*time.Millisecond)

	res, err := s.ScanBurst(context.Background())
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if res.Status != "not ok" {
		t.Fatalf("Status = %q, want not ok", res.Status)
	}
	if res.Reason != "Face processing failed" {
		t.Errorf("Reason = %q, want Face processing failed", res.Reason)
	}
	if res.Matched {
		t.Error("Matched = true, want false")
	}
	if len(audit.entries) != 0 {
		t.Fatalf("expected 0 audit rows, got %d", len(audit.entries))
	}
}

// TestScanBurstGrabFailure checks that a camera that cannot be reached
// produces a not-ok result rather than an error, and writes no audit row:
// a connection failure is not a scan outcome.
func TestScanBurstGrabFailure(t *testing.T) {
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{{1, 0, 0, 0}}}
	proc := &burstProcessor{crop: &domain.FaceCrop{Embedding: alice.Embeddings[0]}}
	grabber := &burstGrabber{err: errors.New("RTSP connect: connection refused")}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(100*time.Millisecond, 10*time.Millisecond)

	res, err := s.ScanBurst(context.Background())
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if res.Status != "not ok" {
		t.Fatalf("Status = %q, want not ok", res.Status)
	}
	if res.Matched {
		t.Error("Matched = true, want false")
	}
	if len(audit.entries) != 0 {
		t.Fatalf("expected 0 audit rows for a connection failure, got %d", len(audit.entries))
	}
}

// TestScanBurstRetriesAfterGrabFailure checks that a transient grab failure
// does not end the burst: the next grab is attempted inside the window.
func TestScanBurstRetriesAfterGrabFailure(t *testing.T) {
	aliceEmb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{aliceEmb}}
	crop := &domain.FaceCrop{Embedding: aliceEmb}
	proc := &burstProcessor{crop: crop}
	// The first grab fails; the second succeeds.
	grabber := &burstGrabber{w: 640, h: 480}
	grabber.grabHook = func(call int) {
		if call == 1 {
			grabber.err = errors.New("RTSP connect: connection refused")
		} else {
			grabber.err = nil
		}
	}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(200*time.Millisecond, 10*time.Millisecond)

	res, err := s.ScanBurst(context.Background())
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if res.Status != "ok" {
		t.Fatalf("Status = %q, want ok", res.Status)
	}
	if res.Name != "alice" {
		t.Errorf("Name = %q, want alice", res.Name)
	}
}

func TestScanBurstGrabsFullResolutionFrame(t *testing.T) {
	aliceEmb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{aliceEmb}}
	crop := &domain.FaceCrop{Embedding: aliceEmb}
	proc := &burstProcessor{crop: crop}
	// A 5 MP frame, like the camera's main stream.
	grabber := &burstGrabber{w: 2560, h: 1920}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(100*time.Millisecond, 10*time.Millisecond)

	var gotW, gotH, gotPixLen int
	rec := &recordingProcessor{
		onFrame: func(w, h int, pix []byte) {
			gotW, gotH, gotPixLen = w, h, len(pix)
		},
	}
	rec.crop = crop
	s.processor = rec

	if _, err := s.ScanBurst(context.Background()); err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if gotW != 2560 || gotH != 1920 {
		t.Errorf("processor got %dx%d, want 2560x1920 (the grab must not downscale)", gotW, gotH)
	}
	if gotPixLen != 2560*1920*3 {
		t.Errorf("processor got %d pixels, want %d", gotPixLen, 2560*1920*3)
	}
}

// recordingProcessor captures the frame dimensions handed to the pipeline.
type recordingProcessor struct {
	burstProcessor
	onFrame func(w, h int, pix []byte)
}

func (r *recordingProcessor) DetectAndCropFrame(w, h int, pix []byte) (*domain.FaceCrop, error) {
	if r.onFrame != nil {
		r.onFrame(w, h, pix)
	}
	return r.burstProcessor.DetectAndCropFrame(w, h, pix)
}

func TestScanBurstSkipsUsersFromAnotherAlignmentSpace(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{
		Name:       "alice",
		Embeddings: []domain.FaceEmbedding{emb},
		Alignment:  "bbox", // enrolled in the other space
	}
	crop := &domain.FaceCrop{Data: []byte("crop"), Embedding: emb}
	proc := &burstProcessor{crop: crop}
	grabber := &burstGrabber{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(200*time.Millisecond, 20*time.Millisecond)

	res, err := s.ScanBurst(context.Background())
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if res.Matched {
		t.Fatalf("Matched = true for a user enrolled in another alignment space (similarity %v)", res.Similarity)
	}
}

func TestScanBurstMatchesUsersInTheActiveSpace(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{
		Name:       "alice",
		Embeddings: []domain.FaceEmbedding{emb},
		Alignment:  "arcface5", // same space as the processor
	}
	crop := &domain.FaceCrop{Data: []byte("crop"), Embedding: emb}
	proc := &burstProcessor{crop: crop}
	grabber := &burstGrabber{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(200*time.Millisecond, 20*time.Millisecond)

	res, err := s.ScanBurst(context.Background())
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if !res.Matched {
		t.Fatalf("Matched = false for a user enrolled in the active alignment space (similarity %v)", res.Similarity)
	}
	if res.Name != "alice" {
		t.Errorf("Name = %q, want alice", res.Name)
	}
}

func TestScanBurstCancel(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{emb}}
	crop := &domain.FaceCrop{Embedding: domain.FaceEmbedding{0, 1, 0, 0}} // score 0.0
	proc := &burstProcessor{crop: crop}
	grabber := &burstGrabber{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(5*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *domain.StreamCheckResult, 1)
	go func() {
		res, _ := s.ScanBurst(ctx)
		done <- res
	}()

	// Wait for the first sample to be processed.
	for atomic.LoadInt32(&proc.callCount) < 1 {
		time.Sleep(1 * time.Millisecond)
	}
	cancel()

	select {
	case res := <-done:
		if res.Status != "not ok" {
			t.Fatalf("Status = %q, want not ok", res.Status)
		}
		if res.Matched {
			t.Error("Matched = true, want false")
		}
		if len(audit.entries) != 0 {
			t.Fatalf("expected 0 audit rows for aborted burst, got %d", len(audit.entries))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ScanBurst did not return after cancel")
	}
}

func TestScanBurstDoesNotHoldServiceMutex(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{emb}}
	crop := &domain.FaceCrop{Embedding: emb}
	blockCh := make(chan struct{})
	proc := &burstProcessor{crop: crop, blockCh: blockCh}
	grabber := &burstGrabber{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(5*time.Second, 10*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *domain.StreamCheckResult, 1)
	go func() {
		res, _ := s.ScanBurst(ctx)
		done <- res
	}()

	// Wait for the processor to block on the first DetectAndCropFrame.
	for atomic.LoadInt32(&proc.callCount) < 1 {
		time.Sleep(1 * time.Millisecond)
	}

	// GetSettings must return quickly: ScanBurst does not hold s.mu during inference.
	start := time.Now()
	if _, err := s.GetSettings(); err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("GetSettings took %v while scan held pipeline mutex", elapsed)
	}

	cancel()
	close(blockCh)
	if res := <-done; res == nil {
		t.Fatal("ScanBurst returned nil result")
	}
}
