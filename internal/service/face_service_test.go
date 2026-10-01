package service

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

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

func (m *mockAuditRepo) ListPaginated(opts domain.ListPaginatedOpts) ([]domain.AuditEntry, int, []string, error) {
	var names []string
	for _, e := range m.entries {
		if e.Name != "" {
			names = append(names, e.Name)
		}
	}
	return m.entries, len(m.entries), names, nil
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

func (m *mockAuditRepo) GetAuditFace(timestamp string) (string, error) {
	return "", nil
}

func (m *mockAuditRepo) ListRecognizeEntries(opts domain.ListRecognizeEntriesOpts) ([]domain.AuditEntry, int, error) {
	var filtered []domain.AuditEntry
	for _, e := range m.entries {
		if e.Endpoint != "recognize" {
			continue
		}
		if e.Status == domain.AuditStatusNoFace {
			continue
		}
		if opts.UserFilter != "" && strings.ToLower(e.Name) != strings.ToLower(opts.UserFilter) {
			continue
		}
		filtered = append(filtered, e)
	}
	if filtered == nil {
		filtered = []domain.AuditEntry{}
	}
	total := len(filtered)
	pageLower := (opts.Page - 1) * opts.PerPage
	pageUpper := opts.Page * opts.PerPage
	if opts.Page <= 0 {
		opts.Page = 1
	}
	if opts.PerPage <= 0 {
		opts.PerPage = 20
	}
	var page []domain.AuditEntry
	for i, e := range filtered {
		if i < pageLower || i >= pageUpper {
			continue
		}
		page = append(page, e)
	}
	return page, total, nil
}

func (m *mockAuditRepo) CountRecognizeEntries() (int, error) {
	count := 0
	for _, e := range m.entries {
		if e.Endpoint == "recognize" && e.Status != domain.AuditStatusNoFace {
			count++
		}
	}
	return count, nil
}

func (m *mockAuditRepo) ComputeRecognizeSummary() (domain.RecognizeSummaryResponse, error) {
	var summary domain.RecognizeSummaryResponse
	for _, e := range m.entries {
		if e.Endpoint != "recognize" {
			continue
		}
		if e.Status == domain.AuditStatusNoFace {
			continue
		}
		summary.TotalRecognize++
		switch e.Status {
		case domain.AuditStatusMatched:
			summary.TotalMatched++
		case domain.AuditStatusNotMatched:
			summary.TotalNotMatched++
		}
	}
	return summary, nil
}

// --- Tests ---

func TestCheckStreamImageNoFaceWritesAuditEntry(t *testing.T) {
	audit := &mockAuditRepo{}
	users := &mockUserRepo{}

	// Processor returns no-face error.
	proc := &mockProcessor{
		err: fmt.Errorf("no face detected within 3 seconds"),
	}

	svc := New(users, audit, proc, 0.45)
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
	if e.FaceImage != string(base64.StdEncoding.EncodeToString(fullFrame)) {
		t.Errorf("FaceImage = %q, want %q", e.FaceImage, string(base64.StdEncoding.EncodeToString(fullFrame)))
	}
}

func TestCheckStreamImageMatchedWritesAudit(t *testing.T) {
	audit := &mockAuditRepo{}
	users := &mockUserRepo{
		users: []*domain.User{
			{Name: "alice", Embeddings: []domain.FaceEmbedding{{1, 2, 3}}},
		},
	}

	// Processor returns a crop with a matching embedding.
	proc := &mockProcessor{
		crop: &domain.FaceCrop{
			Data:      []byte("crop-data"),
			Embedding: domain.FaceEmbedding{1, 2, 3},
		},
	}

	svc := New(users, audit, proc, 0.45)

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

	// Processor returns a crop with a non-matching embedding.
	// {-0.5, 0.9, 0.1} has ~0.41 cosine similarity to {1, 2, 3} — below 0.45 threshold.
	nonMatch := domain.FaceEmbedding{-0.5, 0.9, 0.1}
	proc := &mockProcessor{
		crop: &domain.FaceCrop{
			Data:      []byte("crop-data-not-match"),
			Embedding: nonMatch,
		},
	}

	svc := New(users, audit, proc, 0.45)

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
}

func TestCheckStreamImageConnectionErrorNoAudit(t *testing.T) {
	audit := &mockAuditRepo{}
	users := &mockUserRepo{}

	// Processor returns a connection error (not a no-face error).
	proc := &mockProcessor{
		err: fmt.Errorf("connection refused"),
	}

	svc := New(users, audit, proc, 0.45)

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

	// Processor returns a generic "no face" error (not connection-related).
	proc := &mockProcessor{
		err: fmt.Errorf("no face detected within 3 seconds"),
	}

	svc := New(users, audit, proc, 0.45)

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
	if e.FaceImage != string(base64.StdEncoding.EncodeToString([]byte("fake-frame"))) {
		t.Errorf("FaceImage = %q, want %q", e.FaceImage, string(base64.StdEncoding.EncodeToString([]byte("fake-frame"))))
	}
}

func TestPromoteFromAuditNewUser(t *testing.T) {
	audit := &mockAuditRepo{}
	users := &mockUserRepo{}
	proc := &mockProcessor{}
	svc := New(users, audit, proc, 0.45)

	now := time.Now()
	auditTime := now.Format(time.RFC3339)
	newEmbedding := domain.FaceEmbedding{5, 5, 5}
	newPic := "base64-new-pic"

	audit.Append([]domain.AuditEntry{{
		Time:       now,
		Endpoint:   "stream-check",
		Name:       "",
		Similarity: 0.1,
		Matched:    false,
		Status:     domain.AuditStatusNotMatched,
		FaceImage:  newPic,
		Embedding:  newEmbedding,
	}})

	err := svc.PromoteFromAudit(auditTime, "new-user")
	if err != nil {
		t.Fatalf("PromoteFromAudit: %v", err)
	}

	u, err := users.GetByName("new-user")
	if err != nil {
		t.Fatalf("could not find new user: %v", err)
	}

	if len(u.Embeddings) != 1 || domain.CosineSimilarity(u.Embeddings[0], newEmbedding) < 0.99 {
		t.Errorf("embedding mismatch: got %v, want %v", u.Embeddings[0], newEmbedding)
	}
	if len(u.Pictures) != 1 || u.Pictures[0] != newPic {
		t.Errorf("picture mismatch: got %q, want %q", u.Pictures[0], newPic)
	}
}

func TestPromoteFromAuditExistingUser(t *testing.T) {
	audit := &mockAuditRepo{}
	existingEmbed := domain.FaceEmbedding{1, 1, 1}
	existingPic := "base64-old-pic"
	users := &mockUserRepo{
		users: []*domain.User{
			{
				Name:       "alice",
				Embeddings: []domain.FaceEmbedding{existingEmbed},
				Pictures:   []string{existingPic},
				UpdatedAt:  time.Now().Add(-time.Hour),
			},
		},
	}
	proc := &mockProcessor{}
	svc := New(users, audit, proc, 0.45)

	now := time.Now()
	auditTime := now.Format(time.RFC3339)
	newEmbedding := domain.FaceEmbedding{2, 2, 2}
	newPic := "base64-new-pic"

	audit.Append([]domain.AuditEntry{{
		Time:       now,
		Endpoint:   "stream-check",
		Name:       "",
		Similarity: 0.1,
		Matched:    false,
		Status:     domain.AuditStatusNotMatched,
		FaceImage:  newPic,
		Embedding:  newEmbedding,
	}})

	err := svc.PromoteFromAudit(auditTime, "alice")
	if err != nil {
		t.Fatalf("PromoteFromAudit: %v", err)
	}

	u, err := users.GetByName("alice")
	if err != nil {
		t.Fatalf("could not find user: %v", err)
	}

	if len(u.Embeddings) != 2 || domain.CosineSimilarity(u.Embeddings[1], newEmbedding) < 0.99 {
		t.Errorf("second embedding mismatch: got %v, want %v", u.Embeddings[1], newEmbedding)
	}
	if len(u.Pictures) != 2 {
		t.Errorf("expected 2 pictures, got %d", len(u.Pictures))
	}
	if u.Pictures[1] != newPic {
		t.Errorf("second picture mismatch: got %q, want %q", u.Pictures[1], newPic)
	}
}

func TestPromoteFromAuditExceedsLimit(t *testing.T) {
	audit := &mockAuditRepo{}
	users := &mockUserRepo{
		users: []*domain.User{
			{
				Name:       "alice",
				Embeddings: []domain.FaceEmbedding{{1, 1, 1}, {2, 2, 2}, {3, 3, 3}},
				Pictures:   []string{"p1", "p2", "p3"},
				UpdatedAt:  time.Now().Add(-time.Hour),
			},
		},
	}
	proc := &mockProcessor{}
	svc := New(users, audit, proc, 0.45)

	now := time.Now()
	auditTime := now.Format(time.RFC3339)
	newEmbedding := domain.FaceEmbedding{4, 4, 4}
	newPic := "base64-new-pic"

	audit.Append([]domain.AuditEntry{{
		Time:       now,
		Endpoint:   "stream-check",
		Name:       "",
		Similarity: 0.1,
		Matched:    false,
		Status:     domain.AuditStatusNotMatched,
		FaceImage:  newPic,
		Embedding:  newEmbedding,
	}})

	err := svc.PromoteFromAudit(auditTime, "alice")
	if err == nil {
		t.Error("expected error due to max pictures limit, got nil")
	} else if !strings.Contains(err.Error(), "maximum 3 pictures per user") {
		t.Errorf("unexpected error message: %v", err)
	}
}
