package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"h2hsecure.com/face/internal/domain"
)

// --- burst test fakes ---

type burstProcessor struct {
	crop      *domain.FaceCrop
	errs      []error
	callCount int
}

func (m *burstProcessor) DetectAndCrop(data []byte) (*domain.FaceCrop, error) {
	return m.crop, nil
}

func (m *burstProcessor) DetectAndCropFrame(w, h int, pix []byte) (*domain.FaceCrop, error) {
	m.callCount++
	idx := m.callCount - 1
	if idx < len(m.errs) && m.errs[idx] != nil {
		return nil, m.errs[idx]
	}
	return m.crop, nil
}

func (m *burstProcessor) ExtractEmbedding(crop *domain.FaceCrop) (domain.FaceEmbedding, error) {
	return crop.Embedding, nil
}

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

type burstSource struct {
	w, h     int
	seq      uint64
	fixedSeq bool
	emitErr  error
}

func (s *burstSource) Dims() (int, int) { return s.w, s.h }

func (s *burstSource) Latest(dst []byte) (int, int, uint64, time.Time, bool) {
	if !s.fixedSeq {
		s.seq++
	}
	if len(dst) < s.w*s.h*3 {
		return s.w, s.h, 0, time.Time{}, false
	}
	for i := range dst[:s.w*s.h*3] {
		dst[i] = byte(s.seq)
	}
	return s.w, s.h, s.seq, time.Now(), true
}

func TestScanBurstReturnsFirstMatch(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{emb}}
	crop := &domain.FaceCrop{Data: []byte("crop"), Embedding: emb, BBox: [4]float32{0, 0, 1, 1}}
	proc := &burstProcessor{crop: crop}
	src := &burstSource{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameSource(src).WithBurst(200*time.Millisecond, 20*time.Millisecond)

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
	src := &burstSource{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameSource(src).WithBurst(200*time.Millisecond, 20*time.Millisecond)

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

func TestScanBurstSkipsUnchangedSeq(t *testing.T) {
	aliceEmb := domain.FaceEmbedding{1, 0, 0, 0}
	cropEmb := domain.FaceEmbedding{0, 1, 0, 0}
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{aliceEmb}}
	crop := &domain.FaceCrop{Data: []byte("crop"), Embedding: cropEmb}
	proc := &burstProcessor{crop: crop}
	// Fixed sequence: every Latest returns the same seq, so the processor must
	// be invoked exactly once.
	src := &burstSource{w: 640, h: 480, fixedSeq: true, seq: 1}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameSource(src).WithBurst(100*time.Millisecond, 10*time.Millisecond)

	if _, err := s.ScanBurst(context.Background()); err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if proc.callCount != 1 {
		t.Fatalf("processor called %d times, want 1 (unchanged seq must be skipped)", proc.callCount)
	}
}

func TestScanBurstReadsUsersOnce(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{Name: "alice", Embeddings: []domain.FaceEmbedding{emb}}
	crop := &domain.FaceCrop{Data: []byte("crop"), Embedding: emb}
	proc := &burstProcessor{crop: crop}
	src := &burstSource{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameSource(src).WithBurst(100*time.Millisecond, 10*time.Millisecond)

	if _, err := s.ScanBurst(context.Background()); err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if users.listAllCalls != 1 {
		t.Fatalf("ListAll called %d times, want 1", users.listAllCalls)
	}
}

func TestScanBurstNoFrameSource(t *testing.T) {
	s := New(&mockUserRepo{}, &mockAuditRepo{}, &mockProcessor{}, 0.5)
	// No frame source configured.
	_, err := s.ScanBurst(context.Background())
	if err == nil {
		t.Fatal("expected error when frame source is not configured")
	}
}
