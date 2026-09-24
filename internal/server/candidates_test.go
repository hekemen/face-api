package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// --- groupCandidates ---

func TestGroupCandidatesEmpty(t *testing.T) {
	s := &FaceServer{}
	groups, err := s.groupCandidates(nil, 0.45)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("expected 0 groups, got %d", len(groups))
	}
}

func TestGroupCandidatesSingle(t *testing.T) {
	s := &FaceServer{}
	candidates := []*Candidate{
		{ID: "1", Embedding: makeEmbedding(512, 1.0)},
	}
	groups, err := s.groupCandidates(candidates, 0.45)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].FaceCount != 1 {
		t.Fatalf("expected 1 face, got %d", groups[0].FaceCount)
	}
}

func TestGroupCandidatesSimilar(t *testing.T) {
	s := &FaceServer{}
	base := makeEmbedding(512, 1.0)
	candidates := []*Candidate{
		{ID: "1", Embedding: base},
		{ID: "2", Embedding: base},
		{ID: "3", Embedding: base},
	}
	groups, err := s.groupCandidates(candidates, 0.45)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	if groups[0].FaceCount != 3 {
		t.Fatalf("expected 3 faces in group, got %d", groups[0].FaceCount)
	}
}

func TestGroupCandidatesDifferent(t *testing.T) {
	s := &FaceServer{}
	e1 := makeEmbedding(512, 1.0)
	e2 := makeEmbedding(512, -1.0)
	candidates := []*Candidate{
		{ID: "1", Embedding: e1},
		{ID: "2", Embedding: e2},
	}
	groups, err := s.groupCandidates(candidates, 0.45)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
}

func TestGroupCandidatesMixed(t *testing.T) {
	s := &FaceServer{}
	base := makeEmbedding(512, 1.0)
	opposite := makeEmbedding(512, -1.0)
	candidates := []*Candidate{
		{ID: "1", Embedding: base},
		{ID: "2", Embedding: base},
		{ID: "3", Embedding: opposite},
	}
	groups, err := s.groupCandidates(candidates, 0.45)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0].FaceCount != 2 {
		t.Fatalf("expected group with 2 faces, got %d", groups[0].FaceCount)
	}
}

func TestGroupCandidatesBestSimilarity(t *testing.T) {
	s := &FaceServer{}
	base := makeEmbedding(512, 1.0)
	similar := makeEmbedding(512, 0.99)
	candidates := []*Candidate{
		{ID: "1", Embedding: base},
		{ID: "2", Embedding: similar},
	}
	groups, err := s.groupCandidates(candidates, 0.45)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if groups[0].BestSimilarity < 0.9 {
		t.Fatalf("expected best_similarity >= 0.9, got %f", groups[0].BestSimilarity)
	}
}

// --- storeCandidate / readCandidates ---

func TestStoreAndReadCandidates(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db}

	c := &Candidate{
		ID:        "test-id",
		Embedding: makeEmbedding(512, 1.0),
		FaceImage: "FAKE_JPEG",
		Time:      time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC),
		StreamURL: "rtsp://test",
	}

	if err := s.storeCandidate(c); err != nil {
		t.Fatalf("store: %v", err)
	}

	read, err := s.readCandidates()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(read) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(read))
	}
	if read[0].ID != "test-id" {
		t.Fatalf("expected ID test-id, got %s", read[0].ID)
	}
	if read[0].StreamURL != "rtsp://test" {
		t.Fatalf("expected stream URL rtsp://test, got %s", read[0].StreamURL)
	}
}

func TestStoreMultipleCandidates(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db}

	for i := 0; i < 5; i++ {
		c := &Candidate{
			ID:        fmt.Sprintf("id-%d", i),
			Embedding: makeEmbedding(512, float32(i+1)),
			Time:      time.Now(),
		}
		if err := s.storeCandidate(c); err != nil {
			t.Fatalf("store %d: %v", i, err)
		}
	}

	read, err := s.readCandidates()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(read) != 5 {
		t.Fatalf("expected 5 candidates, got %d", len(read))
	}
}

// --- promoteCandidateToUser ---

func TestPromoteCandidateToUser(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	// Pre-populate a user.
	aliceRaw, _ := json.Marshal(&storedUser{Embeddings: [][]float32{{1, 2, 3}}})
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Put([]byte("alice"), aliceRaw)
	}); err != nil {
		t.Fatalf("put alice: %v", err)
	}

	s := &FaceServer{
		boltDB: db,
		dbMap:  map[string]*storedUser{"alice": {Embeddings: [][]float32{{1, 2, 3}}}},
	}

	// Store a candidate.
	c := &Candidate{
		ID:        "cand-1",
		Embedding: makeEmbedding(512, 1.0),
		FaceImage: "FAKE_JPEG",
		Time:      time.Now(),
	}
	s.storeCandidate(c)

	// Promote.
	status, err := s.promoteCandidateToUser("cand-1", "bob")
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("expected 201, got %d", status)
	}

	// Verify user was created.
	if _, ok := s.dbMap["bob"]; !ok {
		t.Fatal("bob should be in cache")
	}

	// Verify candidate was deleted.
	read, _ := s.readCandidates()
	if len(read) != 0 {
		t.Fatalf("expected 0 candidates after promote, got %d", len(read))
	}
}

func TestPromoteCandidateDuplicateName(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{
		boltDB: db,
		dbMap:  map[string]*storedUser{"alice": {Embeddings: [][]float32{{1, 2, 3}}}},
	}

	c := &Candidate{
		ID:        "cand-1",
		Embedding: makeEmbedding(512, 1.0),
		FaceImage: "FAKE_JPEG",
		Time:      time.Now(),
	}
	s.storeCandidate(c)

	status, err := s.promoteCandidateToUser("cand-1", "alice")
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
	if status != http.StatusConflict {
		t.Fatalf("expected 409, got %d", status)
	}
}

func TestPromoteCandidateNotFound(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}

	status, err := s.promoteCandidateToUser("nonexistent", "bob")
	if err == nil {
		t.Fatal("expected error for missing candidate")
	}
	if status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", status)
	}
}

// --- /candidates handler ---

func TestListCandidatesEmpty(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/candidates", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var resp CandidatesListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Groups == nil {
		t.Fatalf("expected empty groups array, got nil")
	}
}

func TestListCandidatesWithGroups(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	// Store 3 similar candidates.
	base := makeEmbedding(512, 1.0)
	for i := 0; i < 3; i++ {
		c := &Candidate{
			ID:        fmt.Sprintf("cand-%d", i),
			Embedding: base,
			Time:      time.Now(),
		}
		s.storeCandidate(c)
	}

	req := httptest.NewRequest(http.MethodGet, "/candidates", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var resp CandidatesListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(resp.Groups))
	}
	if resp.Groups[0].FaceCount != 3 {
		t.Fatalf("expected 3 faces in group, got %d", resp.Groups[0].FaceCount)
	}
}

func TestListCandidatesMethodNotAllowed(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodPost, "/candidates", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

// --- /candidates/promote handler ---

func TestPromoteCandidateHandler(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	// Store a candidate.
	c := &Candidate{
		ID:        "cand-1",
		Embedding: makeEmbedding(512, 1.0),
		FaceImage: "FAKE_JPEG",
		Time:      time.Now(),
	}
	s.storeCandidate(c)

	// Promote via HTTP.
	body := strings.NewReader(`{"name": "bob"}`)
	req := httptest.NewRequest(http.MethodPost, "/candidates/promote?id=cand-1", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp EnrolledResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "promoted" || resp.Name != "bob" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestPromoteCandidateHandlerMissingName(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	body := strings.NewReader(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/candidates/promote?id=cand-1", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestPromoteCandidateHandlerMissingID(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	body := strings.NewReader(`{"name": "bob"}`)
	req := httptest.NewRequest(http.MethodPost, "/candidates/promote", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// --- collector status handlers ---

func TestCollectorStatus(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/collector/status", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["collecting"] {
		t.Fatal("expected collecting=false")
	}
}

func TestCollectorStartNoRTSP(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodPost, "/collector/start", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// --- bulkPromoteCandidates ---

func TestBulkPromoteCandidates(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	// Pre-populate some candidates.
	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	s.storeCandidate(&Candidate{ID: "c1", Embedding: makeEmbedding(512, 1.0)})
	s.storeCandidate(&Candidate{ID: "c2", Embedding: makeEmbedding(512, 1.1)})
	s.storeCandidate(&Candidate{ID: "c3", Embedding: makeEmbedding(512, -1.0)})

	if err := s.bulkPromoteCandidates("alice", []string{"c1", "c2"}); err != nil {
		t.Fatalf("bulk promote: %v", err)
	}

	// User should exist with 2 embeddings.
	s.mu.RLock()
	alice := s.dbMap["alice"]
	s.mu.RUnlock()
	if alice == nil {
		t.Fatal("expected alice user")
	}
	if len(alice.Embeddings) != 2 {
		t.Fatalf("expected 2 embeddings, got %d", len(alice.Embeddings))
	}

	// Promoted candidates should be deleted.
	read, _ := s.readCandidates()
	if len(read) != 1 {
		t.Fatalf("expected 1 remaining candidate, got %d", len(read))
	}
	if read[0].ID != "c3" {
		t.Fatalf("expected c3, got %s", read[0].ID)
	}
}

func TestBulkPromoteDuplicateName(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{"bob": {}}}
	s.storeCandidate(&Candidate{ID: "c1", Embedding: makeEmbedding(512, 1.0)})

	err := s.bulkPromoteCandidates("bob", []string{"c1"})
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBulkPromoteEmpty(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	err := s.bulkPromoteCandidates("", nil)
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestBulkPromoteSkipsMissing(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	s.storeCandidate(&Candidate{ID: "c1", Embedding: makeEmbedding(512, 1.0)})

	// Mix of existing and non-existing IDs.
	err := s.bulkPromoteCandidates("alice", []string{"c1", "nonexistent"})
	if err != nil {
		t.Fatalf("bulk promote: %v", err)
	}

	s.mu.RLock()
	alice := s.dbMap["alice"]
	s.mu.RUnlock()
	if alice == nil || len(alice.Embeddings) != 1 {
		t.Fatalf("expected alice with 1 embedding, got %+v", alice)
	}
}

// --- bulkPromoteCandidates handler ---

func TestBulkPromoteHandler(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	s.storeCandidate(&Candidate{ID: "c1", Embedding: makeEmbedding(512, 1.0)})
	s.storeCandidate(&Candidate{ID: "c2", Embedding: makeEmbedding(512, 1.1)})

	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	body := strings.NewReader(`{"name": "alice", "candidate_ids": ["c1", "c2"]}`)
	req := httptest.NewRequest(http.MethodPost, "/candidates/bulk-promote", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp EnrolledResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "bulk-promoted" || resp.Name != "alice" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestBulkPromoteHandlerMissingName(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	body := strings.NewReader(`{"candidate_ids": ["c1"]}`)
	req := httptest.NewRequest(http.MethodPost, "/candidates/bulk-promote", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestBulkPromoteHandlerMissingIDs(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	body := strings.NewReader(`{"name": "alice"}`)
	req := httptest.NewRequest(http.MethodPost, "/candidates/bulk-promote", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestBulkPromoteHandlerMethodNotAllowed(t *testing.T) {
	db := openCandidatesTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/candidates/bulk-promote", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

// --- helpers ---

func makeEmbedding(dim int, val float32) []float32 {
	e := make([]float32, dim)
	for i := range e {
		e[i] = val
	}
	return e
}

func openCandidatesTestDB(t *testing.T) *bolt.DB {
	t.Helper()
	db, err := bolt.Open(t.TempDir()+"/candidates.db", 0600, nil)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := EnsureBucket(db); err != nil {
		db.Close()
		t.Fatalf("ensure buckets: %v", err)
	}
	return db
}
