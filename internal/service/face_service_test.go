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

// thumbSetCall records a SetFaceThumb call on the mock.
type thumbSetCall struct {
	timestamp time.Time
	faceImage string
	thumb     string
}

type mockAuditRepo struct {
	entries   []domain.AuditEntry
	thumbSets []thumbSetCall
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
	var out []domain.AuditEntry
	for _, e := range m.entries {
		if e.Status == domain.AuditStatusNotMatched {
			out = append(out, e)
		}
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

func (m *mockAuditRepo) SetFaceThumb(timestamp time.Time, faceImage, thumb string) error {
	m.thumbSets = append(m.thumbSets, thumbSetCall{timestamp: timestamp, faceImage: faceImage, thumb: thumb})
	for i := range m.entries {
		if m.entries[i].Time.Format(time.RFC3339) == timestamp.Format(time.RFC3339) && m.entries[i].FaceImage == faceImage {
			m.entries[i].FaceThumb = thumb
			return nil
		}
	}
	return fmt.Errorf("no audit entry for time %s and face image", timestamp.Format(time.RFC3339))
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
	// The frame here is not a decodable JPEG, so no frame image can be
	// stored (graceful degradation). HasFace must stay false either way.
	if e.FaceImage != "" {
		t.Errorf("FaceImage = %q, want empty for undecodable frame", e.FaceImage)
	}
}

// TestCheckStreamImageNoFaceStoresFrameImage verifies that a no-face
// stream-check stores the captured camera frame (downscaled) in the audit
// entry so the UI can show what the camera saw. HasFace stays false because
// the status is no_face, so the listing keeps the "View frame" button.
func TestCheckStreamImageNoFaceStoresFrameImage(t *testing.T) {
	audit := &mockAuditRepo{}
	users := &mockUserRepo{}

	// Processor returns no-face error.
	proc := &mockProcessor{
		err: fmt.Errorf("no face detected within 3 seconds"),
	}

	svc := New(users, audit, proc, 0.45)

	// 800x600 gradient JPEG as the captured frame.
	frameB64 := makeTestJPEG(t, 800, 600)
	frame, err := base64.StdEncoding.DecodeString(frameB64)
	if err != nil {
		t.Fatalf("decode test frame: %v", err)
	}

	result, err := svc.CheckStreamImage(frame)
	if err != nil {
		t.Fatalf("CheckStreamImage: %v", err)
	}
	if result.Status != "not ok" {
		t.Errorf("status = %q, want %q", result.Status, "not ok")
	}

	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audit.entries))
	}
	e := audit.entries[0]
	if e.Status != domain.AuditStatusNoFace {
		t.Errorf("Status = %q, want %q", e.Status, domain.AuditStatusNoFace)
	}
	if e.FaceImage == "" {
		t.Fatalf("FaceImage = empty, want stored camera frame")
	}
	// Stored frame must be a valid JPEG downscaled so the longest side is
	// at most noFaceFrameMaxDim (800x600 -> 640x480).
	if got := maxDimOf(t, e.FaceImage); got > noFaceFrameMaxDim {
		t.Errorf("stored frame longest side = %d, want <= %d", got, noFaceFrameMaxDim)
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

	// Should write audit entry with no face image (HasFace must stay false).
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audit.entries))
	}

	e := audit.entries[0]
	if e.Status != domain.AuditStatusNoFace {
		t.Errorf("Status = %q, want %q", e.Status, domain.AuditStatusNoFace)
	}
	if e.FaceImage != "" {
		t.Errorf("FaceImage = %q, want empty", e.FaceImage)
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
				Embeddings: []domain.FaceEmbedding{{1, 1, 1}, {2, 2, 2}, {3, 3, 3}, {4, 4, 4}, {5, 5, 5}},
				Pictures:   []string{"p1", "p2", "p3", "p4", "p5"},
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
	} else if !strings.Contains(err.Error(), "maximum 5 pictures per user") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// mustDecode decodes a base64 string, failing the test on error.
func mustDecode(t *testing.T, b64 string) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	return data
}

func TestEnroll_SetsFaceThumb(t *testing.T) {
	// Large crop (over the threshold) gets a stored thumbnail.
	large := makeNoiseJPEG(t, 200, 150)
	audit := &mockAuditRepo{}
	svc := New(&mockUserRepo{}, audit, &mockProcessor{crop: &domain.FaceCrop{Data: mustDecode(t, large), Embedding: []float32{1, 0, 0}}}, 0.45)
	if err := svc.EnrollImage("alice", []byte("img")); err != nil {
		t.Fatalf("EnrollImage: %v", err)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("got %d audit entries, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.FaceImage != large {
		t.Errorf("FaceImage len %d, want full-size %d", len(e.FaceImage), len(large))
	}
	if e.FaceThumb == "" {
		t.Fatal("FaceThumb empty, want stored thumbnail for large image")
	}
	if len(e.FaceThumb) >= len(large) {
		t.Errorf("FaceThumb len %d not smaller than FaceImage %d", len(e.FaceThumb), len(large))
	}
	if m := maxDimOf(t, e.FaceThumb); m > 96 {
		t.Errorf("FaceThumb max dimension = %d, want <= 96", m)
	}

	// Small crop (under the threshold) gets no thumbnail.
	small := makeTestJPEG(t, 112, 112)
	audit2 := &mockAuditRepo{}
	svc2 := New(&mockUserRepo{}, audit2, &mockProcessor{crop: &domain.FaceCrop{Data: mustDecode(t, small), Embedding: []float32{1, 0, 0}}}, 0.45)
	if err := svc2.EnrollImage("bob", []byte("img")); err != nil {
		t.Fatalf("EnrollImage: %v", err)
	}
	if audit2.entries[0].FaceThumb != "" {
		t.Errorf("FaceThumb = %q, want empty for small image", audit2.entries[0].FaceThumb)
	}
}

func TestRecognize_SetsFaceThumb(t *testing.T) {
	large := makeNoiseJPEG(t, 200, 150)
	audit := &mockAuditRepo{}
	svc := New(&mockUserRepo{}, audit, &mockProcessor{crop: &domain.FaceCrop{Data: mustDecode(t, large), Embedding: []float32{1, 0, 0}}}, 0.45)
	if _, err := svc.RecognizeImage([]byte("img")); err != nil {
		t.Fatalf("RecognizeImage: %v", err)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("got %d audit entries, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.FaceImage != large {
		t.Errorf("FaceImage len %d, want full-size %d", len(e.FaceImage), len(large))
	}
	if e.FaceThumb == "" {
		t.Fatal("FaceThumb empty, want stored thumbnail for large image")
	}
	if m := maxDimOf(t, e.FaceThumb); m > 96 {
		t.Errorf("FaceThumb max dimension = %d, want <= 96", m)
	}
}

func TestCheckStreamImage_SetsFaceThumb(t *testing.T) {
	large := makeNoiseJPEG(t, 200, 150)
	audit := &mockAuditRepo{}
	users := &mockUserRepo{users: []*domain.User{{Name: "alice", Embeddings: []domain.FaceEmbedding{{1, 0, 0}}}}}
	svc := New(users, audit, &mockProcessor{crop: &domain.FaceCrop{Data: mustDecode(t, large), Embedding: []float32{1, 0, 0}}}, 0.45)
	res, err := svc.CheckStreamImage([]byte("img"))
	if err != nil {
		t.Fatalf("CheckStreamImage: %v", err)
	}
	if res.Status != "ok" {
		t.Fatalf("status = %q, want ok", res.Status)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("got %d audit entries, want 1", len(audit.entries))
	}
	e := audit.entries[0]
	if e.FaceImage != large {
		t.Errorf("FaceImage len %d, want full-size %d", len(e.FaceImage), len(large))
	}
	if e.FaceThumb == "" {
		t.Fatal("FaceThumb empty, want stored thumbnail for large image")
	}
}

func TestListAuditPaginated_UsesStoredThumb(t *testing.T) {
	big := strings.Repeat("A", 9000)
	audit := &mockAuditRepo{entries: []domain.AuditEntry{
		{Time: time.Now().Truncate(time.Second), Endpoint: "recognize", Name: "alice", Status: "matched", FaceImage: big, FaceThumb: "stored-thumb"},
	}}
	svc := New(&mockUserRepo{}, audit, &mockProcessor{}, 0.45)
	entries, _, _, err := svc.ListAuditPaginated(domain.ListPaginatedOpts{Page: 1, PerPage: 20})
	if err != nil {
		t.Fatalf("ListAuditPaginated: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].FaceImage != "stored-thumb" {
		t.Errorf("FaceImage not swapped to stored thumb (len %d)", len(entries[0].FaceImage))
	}
	if entries[0].FaceThumb != "" {
		t.Errorf("FaceThumb = %q, want cleared after swap", entries[0].FaceThumb)
	}
}

func TestListUnmatched_UsesStoredThumb(t *testing.T) {
	big := strings.Repeat("A", 9000)
	audit := &mockAuditRepo{entries: []domain.AuditEntry{
		{Time: time.Now().Truncate(time.Second), Endpoint: "stream-check", Status: domain.AuditStatusNotMatched, FaceImage: big, FaceThumb: "stored-thumb"},
	}}
	svc := New(&mockUserRepo{}, audit, &mockProcessor{}, 0.45)
	entries, err := svc.ListUnmatched(10)
	if err != nil {
		t.Fatalf("ListUnmatched: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].FaceImage != "stored-thumb" || entries[0].FaceThumb != "" {
		t.Errorf("stored thumb not swapped in: %+v", entries[0])
	}
}

func TestRecentAudit_UsesStoredThumb(t *testing.T) {
	big := strings.Repeat("A", 9000)
	audit := &mockAuditRepo{entries: []domain.AuditEntry{
		{Time: time.Now().Truncate(time.Second), Endpoint: "recognize", Name: "alice", Status: "matched", FaceImage: big, FaceThumb: "stored-thumb"},
	}}
	svc := New(&mockUserRepo{}, audit, &mockProcessor{}, 0.45)
	entries, err := svc.RecentAudit(10)
	if err != nil {
		t.Fatalf("RecentAudit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].FaceImage != "stored-thumb" || entries[0].FaceThumb != "" {
		t.Errorf("stored thumb not swapped in: %+v", entries[0])
	}
}

func TestBackfillAuditThumbnails(t *testing.T) {
	large := makeNoiseJPEG(t, 200, 150)
	small := makeTestJPEG(t, 50, 40)
	garbage := strings.Repeat("x", 9000) // valid base64 alphabet, not a JPEG
	base := time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)
	audit := &mockAuditRepo{entries: []domain.AuditEntry{
		{Time: base, Endpoint: "recognize", Name: "large", FaceImage: large},
		{Time: base.Add(time.Second), Endpoint: "recognize", Name: "small", FaceImage: small},
		{Time: base.Add(2 * time.Second), Endpoint: "recognize", Name: "noface"},
		{Time: base.Add(3 * time.Second), Endpoint: "recognize", Name: "already", FaceImage: large, FaceThumb: "x"},
		{Time: base.Add(4 * time.Second), Endpoint: "recognize", Name: "garbage", FaceImage: garbage},
	}}
	svc := New(&mockUserRepo{}, audit, &mockProcessor{}, 0.45)
	n, err := svc.BackfillAuditThumbnails()
	if err != nil {
		t.Fatalf("BackfillAuditThumbnails: %v", err)
	}
	if n != 1 {
		t.Fatalf("backfilled %d entries, want 1", n)
	}
	if len(audit.thumbSets) != 1 {
		t.Fatalf("SetFaceThumb called %d times, want 1", len(audit.thumbSets))
	}
	if audit.thumbSets[0].faceImage != large {
		t.Error("SetFaceThumb called for the wrong entry")
	}
	if audit.entries[0].FaceThumb == "" || len(audit.entries[0].FaceThumb) >= len(large) {
		t.Errorf("entry 0 thumb not stored: len %d", len(audit.entries[0].FaceThumb))
	}
	if audit.entries[1].FaceThumb != "" || audit.entries[2].FaceThumb != "" || audit.entries[3].FaceThumb != "x" || audit.entries[4].FaceThumb != "" {
		t.Errorf("unexpected thumbs after backfill: %+v", audit.entries)
	}

	// Idempotent: a second run updates nothing.
	if n2, err := svc.BackfillAuditThumbnails(); err != nil || n2 != 0 {
		t.Fatalf("second backfill: n=%d err=%v, want 0/nil", n2, err)
	}
}

func TestDeletePicture(t *testing.T) {
	users := &mockUserRepo{
		users: []*domain.User{
			{
				Name: "alice",
				Embeddings: []domain.FaceEmbedding{
					{1, 0, 0}, {0, 1, 0}, {0, 0, 1},
				},
				Pictures: []string{"p0", "p1", "p2"},
			},
		},
	}
	audit := &mockAuditRepo{}
	svc := New(users, audit, &mockProcessor{}, 0.45)

	// Delete the middle picture; embeddings and pictures stay aligned.
	if err := svc.DeletePicture("alice", 1); err != nil {
		t.Fatalf("DeletePicture(alice, 1): %v", err)
	}
	u, err := users.GetByName("alice")
	if err != nil {
		t.Fatalf("GetByName after delete: %v", err)
	}
	if len(u.Pictures) != 2 || u.Pictures[0] != "p0" || u.Pictures[1] != "p2" {
		t.Errorf("pictures = %v, want [p0 p2]", u.Pictures)
	}
	if len(u.Embeddings) != 2 {
		t.Errorf("embeddings len = %d, want 2 (must stay aligned with pictures)", len(u.Embeddings))
	}
	if u.UpdatedAt.IsZero() {
		t.Error("UpdatedAt not refreshed after picture delete")
	}

	// Out-of-range index is an error.
	if err := svc.DeletePicture("alice", 5); err == nil {
		t.Error("DeletePicture with out-of-range index: want error, got nil")
	}
	if err := svc.DeletePicture("alice", -1); err == nil {
		t.Error("DeletePicture with negative index: want error, got nil")
	}

	// Unknown user is an error.
	if err := svc.DeletePicture("nobody", 0); err == nil {
		t.Error("DeletePicture for unknown user: want error, got nil")
	}

	// Deleting the last picture keeps the user with empty arrays.
	if err := svc.DeletePicture("alice", 0); err != nil {
		t.Fatalf("DeletePicture(alice, 0): %v", err)
	}
	if err := svc.DeletePicture("alice", 0); err != nil {
		t.Fatalf("DeletePicture(alice, 0) on second picture: %v", err)
	}
	u, err = users.GetByName("alice")
	if err != nil {
		t.Fatalf("user must be kept after last picture delete: %v", err)
	}
	if len(u.Pictures) != 0 || len(u.Embeddings) != 0 {
		t.Errorf("user after last delete: pictures=%d embeddings=%d, want 0/0", len(u.Pictures), len(u.Embeddings))
	}
}

// --- Settings / threshold tests ---

// mockSettingsRepo implements domain.SettingsRepository for threshold tests.
type mockSettingsRepo struct {
	settings *domain.Settings // value returned by Get (nil = unset)
	saved    *domain.Settings // last value passed to Save
	saveErr  error
}

func (m *mockSettingsRepo) Get() (*domain.Settings, error) { return m.settings, nil }

func (m *mockSettingsRepo) Save(s *domain.Settings) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.saved = s
	return nil
}

// TestSetThresholdUpdatesSettings verifies SetThreshold persists the new value
// and updates the live threshold reported by GetSettings.
func TestSetThresholdUpdatesSettings(t *testing.T) {
	repo := &mockSettingsRepo{}
	svc := New(&mockUserRepo{}, &mockAuditRepo{}, &mockProcessor{}, 0.45)
	svc.WithSettingsRepo(repo)

	if err := svc.SetThreshold(0.5); err != nil {
		t.Fatalf("SetThreshold(0.5): %v", err)
	}
	if repo.saved == nil || repo.saved.Threshold != 0.5 {
		t.Errorf("expected 0.5 persisted before memory update, got %+v", repo.saved)
	}
	st, err := svc.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if st.Threshold != 0.5 {
		t.Errorf("expected live threshold 0.5, got %v", st.Threshold)
	}
}

// TestSetThresholdRejectsOutOfRange verifies values outside (0, 1) are
// rejected without persisting or changing the live threshold.
func TestSetThresholdRejectsOutOfRange(t *testing.T) {
	repo := &mockSettingsRepo{}
	svc := New(&mockUserRepo{}, &mockAuditRepo{}, &mockProcessor{}, 0.45)
	svc.WithSettingsRepo(repo)

	for _, v := range []float32{0, 1, 1.5, -0.1} {
		if err := svc.SetThreshold(v); err == nil {
			t.Errorf("SetThreshold(%v): want error, got nil", v)
		}
	}
	if repo.saved != nil {
		t.Errorf("out-of-range values must not be persisted, got %+v", repo.saved)
	}
	st, _ := svc.GetSettings()
	if st.Threshold != 0.45 {
		t.Errorf("live threshold must stay 0.45 after rejected updates, got %v", st.Threshold)
	}
}

// TestSetThresholdPersistErrorLeavesMemoryUntouched verifies a persistence
// failure leaves the live threshold unchanged.
func TestSetThresholdPersistErrorLeavesMemoryUntouched(t *testing.T) {
	repo := &mockSettingsRepo{saveErr: fmt.Errorf("db down")}
	svc := New(&mockUserRepo{}, &mockAuditRepo{}, &mockProcessor{}, 0.45)
	svc.WithSettingsRepo(repo)

	if err := svc.SetThreshold(0.5); err == nil {
		t.Fatal("SetThreshold with failing repo: want error, got nil")
	}
	st, _ := svc.GetSettings()
	if st.Threshold != 0.45 {
		t.Errorf("live threshold must stay 0.45 after persist failure, got %v", st.Threshold)
	}
}

// TestWithSettingsRepoAppliesStoredValue verifies a persisted threshold
// overrides the constructor value at startup.
func TestWithSettingsRepoAppliesStoredValue(t *testing.T) {
	repo := &mockSettingsRepo{settings: &domain.Settings{Threshold: 0.62}}
	svc := New(&mockUserRepo{}, &mockAuditRepo{}, &mockProcessor{}, 0.45)
	svc.WithSettingsRepo(repo)

	st, err := svc.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if st.Threshold != 0.62 {
		t.Errorf("expected stored threshold 0.62 to override constructor value, got %v", st.Threshold)
	}
}

// TestWithSettingsRepoNoStoredValueKeepsConstructorThreshold verifies the
// constructor threshold is kept when nothing is persisted yet.
func TestWithSettingsRepoNoStoredValueKeepsConstructorThreshold(t *testing.T) {
	repo := &mockSettingsRepo{}
	svc := New(&mockUserRepo{}, &mockAuditRepo{}, &mockProcessor{}, 0.45)
	svc.WithSettingsRepo(repo)

	st, err := svc.GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if st.Threshold != 0.45 {
		t.Errorf("expected constructor threshold 0.45, got %v", st.Threshold)
	}
}
