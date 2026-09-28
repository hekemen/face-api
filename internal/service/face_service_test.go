package service

import (
	"fmt"
	"testing"

	"h2hsecure.com/face/internal/domain"
)

// --- Mock implementations for testing ---

type mockProcessor struct {
	crop *domain.FaceCrop
	err  error
}

func (m *mockProcessor) DetectAndCrop(data []byte) (*domain.FaceCrop, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.crop, nil
}

func (m *mockProcessor) ExtractEmbedding(crop *domain.FaceCrop) (domain.FaceEmbedding, error) {
	if crop == nil {
		return nil, fmt.Errorf("nil crop")
	}
	return crop.Embedding, nil
}

type mockUserRepo struct {
	users []*domain.User
}

func (m *mockUserRepo) GetByName(name string) (*domain.User, error) {
	for _, u := range m.users {
		if u.Name == name {
			return u, nil
		}
	}
	return nil, fmt.Errorf("user %q not found", name)
}

func (m *mockUserRepo) Exists(name string) (bool, error) {
	for _, u := range m.users {
		if u.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func (m *mockUserRepo) Save(u *domain.User) error {
	for i, existing := range m.users {
		if existing.Name == u.Name {
			m.users[i] = u
			return nil
		}
	}
	m.users = append(m.users, u)
	return nil
}

func (m *mockUserRepo) Delete(name string) error {
	for i, u := range m.users {
		if u.Name == name {
			m.users = append(m.users[:i], m.users[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("user %q not found", name)
}

func (m *mockUserRepo) ListAll() ([]*domain.User, error) {
	return m.users, nil
}

type mockCandidateRepo struct {
	candidates []*domain.Candidate
}

func (m *mockCandidateRepo) Save(c *domain.Candidate) error {
	m.candidates = append(m.candidates, c)
	return nil
}

func (m *mockCandidateRepo) GetByID(id string) (*domain.Candidate, error) {
	for _, c := range m.candidates {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, fmt.Errorf("candidate %q not found", id)
}

func (m *mockCandidateRepo) Delete(id string) error {
	for i, c := range m.candidates {
		if c.ID == id {
			m.candidates = append(m.candidates[:i], m.candidates[i+1:]...)
			return nil
		}
	}
	return nil
}

func (m *mockCandidateRepo) ListAll() ([]*domain.Candidate, error) {
	return m.candidates, nil
}

func (m *mockCandidateRepo) DeleteByID(ids []string) error {
	filtered := make([]*domain.Candidate, 0, len(m.candidates))
	for _, c := range m.candidates {
		keep := true
		for _, id := range ids {
			if c.ID == id {
				keep = false
				break
			}
		}
		if keep {
			filtered = append(filtered, c)
		}
	}
	m.candidates = filtered
	return nil
}

func (m *mockCandidateRepo) GroupBySimilarity(threshold float32) ([]*domain.CandidateGroup, error) {
	return nil, nil
}

func (m *mockCandidateRepo) CountAll() (int, error) {
	return len(m.candidates), nil
}

type mockAuditRepo struct {
	entries []domain.AuditEntry
}

func (m *mockAuditRepo) Append(entries []domain.AuditEntry) error {
	m.entries = append(m.entries, entries...)
	return nil
}

func (m *mockAuditRepo) Recent(n int) ([]domain.AuditEntry, error) {
	return m.entries, nil
}

func (m *mockAuditRepo) ListPaginated(opts domain.ListPaginatedOpts) ([]domain.AuditEntry, int, error) {
	return m.entries, len(m.entries), nil
}

func (m *mockAuditRepo) CountAll() (int, error) {
	return len(m.entries), nil
}

func (m *mockAuditRepo) ListUnmatched(n int) ([]domain.AuditEntry, error) {
	return nil, nil
}

func (m *mockAuditRepo) ComputeStats() (domain.Stats, error) {
	return domain.Stats{}, nil
}

// --- Tests ---

func TestCheckStreamImageNoFaceWritesAuditEntry(t *testing.T) {
	audit := &mockAuditRepo{}
	users := &mockUserRepo{}
	candidates := &mockCandidateRepo{}

	// Processor returns no-face error.
	proc := &mockProcessor{
		err: fmt.Errorf("no face detected within 3 seconds"),
	}

	svc := New(users, candidates, audit, proc, 0.45)
	svc.mu.Lock() // Lock to prevent concurrent issues in test
	svc.mu.Unlock()

	fullFrame := []byte("fake-jpeg-data")
	result, err := svc.CheckStreamImage(fullFrame)
	if err != nil {
		t.Fatalf("CheckStreamImage: %v", err)
	}

	if result.Status != "not ok" {
		t.Errorf("status = %q, want %q", result.Status, "not ok")
	}

	// Verify audit entry was written.
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audit.entries))
	}

	e := audit.entries[0]
	if e.Status != domain.AuditStatusNoFace {
		t.Errorf("Status = %q, want %q", e.Status, domain.AuditStatusNoFace)
	}
	if e.Name != "" {
		t.Errorf("Name = %q, want empty", e.Name)
	}
	if e.Similarity != 0 {
		t.Errorf("Similarity = %f, want 0", e.Similarity)
	}
	// FaceImage should be the full frame (not cropped).
	if e.FaceImage != string(fullFrame) {
		t.Errorf("FaceImage = %q, want %q", e.FaceImage, string(fullFrame))
	}
}

func TestCheckStreamImageMatchedWritesAudit(t *testing.T) {
	audit := &mockAuditRepo{}
	users := &mockUserRepo{
		users: []*domain.User{
			{Name: "alice", Embeddings: []domain.FaceEmbedding{{1, 2, 3}}},
		},
	}
	candidates := &mockCandidateRepo{}

	// Processor returns a crop with a matching embedding.
	proc := &mockProcessor{
		crop: &domain.FaceCrop{
			Data:      []byte("crop-data"),
			Embedding: domain.FaceEmbedding{1, 2, 3},
		},
	}

	svc := New(users, candidates, audit, proc, 0.45)

	result, err := svc.CheckStreamImage([]byte("fake"))
	if err != nil {
		t.Fatalf("CheckStreamImage: %v", err)
	}

	if result.Status != "ok" {
		t.Errorf("status = %q, want %q", result.Status, "ok")
	}
	if result.Name != "alice" {
		t.Errorf("name = %q, want %q", result.Name, "alice")
	}

	// Verify audit entry was written with Status=matched.
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audit.entries))
	}

	e := audit.entries[0]
	if e.Status != domain.AuditStatusMatched {
		t.Errorf("Status = %q, want %q", e.Status, domain.AuditStatusMatched)
	}
	if e.Name != "alice" {
		t.Errorf("Name = %q, want %q", e.Name, "alice")
	}
	if e.Matched != true {
		t.Errorf("Matched = %v, want true", e.Matched)
	}
}

func TestCheckStreamImageNotMatchedWritesAuditAndCandidate(t *testing.T) {
	audit := &mockAuditRepo{}
	users := &mockUserRepo{
		users: []*domain.User{
			{Name: "alice", Embeddings: []domain.FaceEmbedding{{1, 2, 3}}},
		},
	}
	candidates := &mockCandidateRepo{}

	// Processor returns a crop with a non-matching embedding.
	// {-0.5, 0.9, 0.1} has ~0.41 cosine similarity to {1, 2, 3} — below 0.45 threshold.
	nonMatch := domain.FaceEmbedding{-0.5, 0.9, 0.1}
	proc := &mockProcessor{
		crop: &domain.FaceCrop{
			Data:      []byte("crop-data-not-match"),
			Embedding: nonMatch,
		},
	}

	svc := New(users, candidates, audit, proc, 0.45)

	result, err := svc.CheckStreamImage([]byte("fake"))
	if err != nil {
		t.Fatalf("CheckStreamImage: %v", err)
	}

	if result.Status != "not ok" {
		t.Errorf("status = %q, want %q", result.Status, "not ok")
	}
	if result.Reason != "Face detected but similarity below threshold" {
		t.Errorf("reason = %q, want %q", result.Reason, "Face detected but similarity below threshold")
	}

	// Verify audit entry was written with Status=not_matched.
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audit.entries))
	}

	e := audit.entries[0]
	if e.Status != domain.AuditStatusNotMatched {
		t.Errorf("Status = %q, want %q", e.Status, domain.AuditStatusNotMatched)
	}
	if e.Name != "" {
		t.Errorf("Name = %q, want empty", e.Name)
	}
	if e.Matched != false {
		t.Errorf("Matched = %v, want false", e.Matched)
	}

	// Verify candidate was auto-collected.
	if len(candidates.candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates.candidates))
	}
	if len(candidates.candidates[0].Embedding) != len(nonMatch) {
		t.Errorf("candidate embedding length = %d, want %d",
			len(candidates.candidates[0].Embedding), len(nonMatch))
	}
}

func TestCheckStreamImageConnectionErrorNoAudit(t *testing.T) {
	audit := &mockAuditRepo{}
	users := &mockUserRepo{}
	candidates := &mockCandidateRepo{}

	// Processor returns a connection error (not a no-face error).
	proc := &mockProcessor{
		err: fmt.Errorf("connection refused"),
	}

	svc := New(users, candidates, audit, proc, 0.45)

	result, err := svc.CheckStreamImage([]byte("fake"))
	if err != nil {
		t.Fatalf("CheckStreamImage: %v", err)
	}

	if result.Status != "not ok" {
		t.Errorf("status = %q, want %q", result.Status, "not ok")
	}
	if result.Reason != "Failed to connect to RTSP stream" {
		t.Errorf("reason = %q, want %q", result.Reason, "Failed to connect to RTSP stream")
	}

	// Connection errors should NOT write an audit entry.
	if len(audit.entries) != 0 {
		t.Errorf("expected 0 audit entries for connection error, got %d", len(audit.entries))
	}
}

func TestCheckStreamImageNoFaceNotConnectionError(t *testing.T) {
	audit := &mockAuditRepo{}
	users := &mockUserRepo{}
	candidates := &mockCandidateRepo{}

	// Processor returns a generic "no face" error (not connection-related).
	proc := &mockProcessor{
		err: fmt.Errorf("no face detected within 3 seconds"),
	}

	svc := New(users, candidates, audit, proc, 0.45)

	result, err := svc.CheckStreamImage([]byte("fake-frame"))
	if err != nil {
		t.Fatalf("CheckStreamImage: %v", err)
	}

	if result.Status != "not ok" {
		t.Errorf("status = %q, want %q", result.Status, "not ok")
	}

	// Should write audit entry with full frame.
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audit.entries))
	}

	e := audit.entries[0]
	if e.Status != domain.AuditStatusNoFace {
		t.Errorf("Status = %q, want %q", e.Status, domain.AuditStatusNoFace)
	}
	if e.FaceImage != "fake-frame" {
		t.Errorf("FaceImage = %q, want full frame", e.FaceImage)
	}
}
