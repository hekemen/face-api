package server

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// --- storedUser serialization ---

func TestUnmarshalUserLegacyFlat(t *testing.T) {
	// Legacy single-embedding value: a flat 512-float array.
	flat := make([]float32, 512)
	raw, _ := json.Marshal(flat)

	u, err := unmarshalUser(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(u.Embeddings) != 1 || len(u.Embeddings[0]) != 512 {
		t.Fatalf("expected 1x512 embedding, got %v", len(u.Embeddings))
	}
	if len(u.Pictures) != 0 {
		t.Fatalf("legacy has no pictures, got %d", len(u.Pictures))
	}
	if !u.UpdatedAt.IsZero() {
		t.Fatalf("legacy has no updated_at, got %v", u.UpdatedAt)
	}
}

func TestUnmarshalUserLegacyNested(t *testing.T) {
	// Legacy nested [[...],[...]] (2 pictures enrolled).
	e1 := make([]float32, 512)
	e2 := make([]float32, 512)
	for i := range e2 {
		e2[i] = 1.0
	}
	raw, _ := json.Marshal([][]float32{e1, e2})

	u, err := unmarshalUser(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(u.Embeddings) != 2 {
		t.Fatalf("expected 2 embeddings, got %d", len(u.Embeddings))
	}
	if len(u.Pictures) != 0 {
		t.Fatalf("legacy has no pictures, got %d", len(u.Pictures))
	}
}

func TestUnmarshalUserNewFormat(t *testing.T) {
	raw := []byte(`{"embeddings":[[1,2,3]],"pictures":["AAAA","BBBB"],"updated_at":"2026-09-16T12:00:00Z"}`)
	u, err := unmarshalUser(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(u.Embeddings) != 1 || len(u.Embeddings[0]) != 3 {
		t.Fatalf("bad embeddings: %v", u.Embeddings)
	}
	if len(u.Pictures) != 2 {
		t.Fatalf("expected 2 pictures, got %d", len(u.Pictures))
	}
	if u.UpdatedAt.IsZero() {
		t.Fatal("expected updated_at parsed")
	}
}

func TestMarshalUserRoundTrip(t *testing.T) {
	in := &storedUser{
		Embeddings: [][]float32{{1, 2, 3}, {4, 5, 6}},
		Pictures:   []string{"pic1", "pic2"},
		UpdatedAt:  time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
	}
	raw := marshalUser(in)

	out, err := unmarshalUser(raw)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Embeddings) != 2 || len(out.Pictures) != 2 {
		t.Fatalf("roundtrip mismatch: %+v", out)
	}
	if !out.UpdatedAt.Equal(in.UpdatedAt) {
		t.Fatalf("updated_at mismatch: %v vs %v", out.UpdatedAt, in.UpdatedAt)
	}
	if out.Pictures[0] != "pic1" || out.Pictures[1] != "pic2" {
		t.Fatalf("pictures mismatch: %v", out.Pictures)
	}
}

// --- hydrateCache + audit backfill ---

func TestHydrateBackfillsPicturesFromAudit(t *testing.T) {
	db := openUsersTestDB(t)
	defer db.Close()

	// Legacy value: 2 embeddings, no pictures, no updated_at.
	raw, _ := json.Marshal([][]float32{make([]float32, 512), make([]float32, 512)})
	t1 := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 9, 2, 11, 0, 0, 0, time.UTC)

	// Audit entries: two ENROLL rows for alice (each carries a face image).
	enrolls := []AuditEntry{
		{Time: t1, Endpoint: "enroll", Name: "alice", FaceImage: "PIC_A"},
		{Time: t1, Endpoint: "recognize", Name: "bob", FaceImage: "PIC_B_IGNORED"},
		{Time: t2, Endpoint: "enroll", Name: "alice", FaceImage: "PIC_B"},
	}
	if err := storeAuditDirect(db, enrolls); err != nil {
		t.Fatalf("store audit: %v", err)
	}

	// bob has only enroll-less rows: still a stored user (recognize target).
	bobRaw, _ := json.Marshal([][]float32{make([]float32, 512)})
	if err := db.Update(func(tx *bolt.Tx) error {
		fb := tx.Bucket(bucketName)
		if err := fb.Put([]byte("alice"), raw); err != nil {
			return err
		}
		return fb.Put([]byte("bob"), bobRaw)
	}); err != nil {
		t.Fatalf("put legacy users: %v", err)
	}

	cache, err := hydrateCache(db)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}

	alice, ok := cache["alice"]
	if !ok {
		t.Fatal("alice missing from cache")
	}
	if len(alice.Embeddings) != 2 {
		t.Fatalf("expected 2 embedded embeddings (from legacy value), got %d", len(alice.Embeddings))
	}
	if len(alice.Pictures) != 2 || alice.Pictures[0] != "PIC_A" || alice.Pictures[1] != "PIC_B" {
		t.Fatalf("pictures should be backfilled in enroll order, got %v", alice.Pictures)
	}
	if !alice.UpdatedAt.Equal(t2) {
		t.Fatalf("updated_at should be latest enroll time, got %v", alice.UpdatedAt)
	}

	// bob has recognize-only entries: no pictures, UpdatedAt = hydration time.
	bob, ok := cache["bob"]
	if !ok {
		t.Fatal("bob missing from cache")
	}
	if len(bob.Pictures) != 0 {
		t.Fatalf("recognize rows must not backfill pictures, got %v", bob.Pictures)
	}
	if bob.UpdatedAt.IsZero() {
		t.Fatal("bob should get a hydration timestamp")
	}
}

func TestHydrateKeepsNewFormatPictures(t *testing.T) {
	db := openUsersTestDB(t)
	defer db.Close()

	u := &storedUser{
		Embeddings: [][]float32{{1, 2, 3}},
		Pictures:   []string{"NEW_PIC"},
		UpdatedAt:  time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Put([]byte("alice"), marshalUser(u))
	}); err != nil {
		t.Fatalf("put user: %v", err)
	}

	cache, err := hydrateCache(db)
	if err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	if got := cache["alice"].Pictures; len(got) != 1 || got[0] != "NEW_PIC" {
		t.Fatalf("pictures should survive hydration, got %v", got)
	}
}

// --- /users handler shape ---

func TestListUsersReturnsPicturesAndDate(t *testing.T) {
	db := openUsersTestDB(t)
	defer db.Close()

	s := &FaceServer{
		boltDB: db,
		dbMap: map[string]*storedUser{
			"alice": {
				Embeddings: [][]float32{{1, 2, 3}},
				Pictures:   []string{"PIC_A", "PIC_B"},
				UpdatedAt:  time.Date(2026, 9, 2, 11, 0, 0, 0, time.UTC),
			},
			"bob": {
				Embeddings: [][]float32{{4, 5, 6}},
			},
		},
		enableUI: true,
	}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	var resp struct {
		Users []userInfoJSON `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, rec.Body.String())
	}
	if len(resp.Users) != 2 {
		t.Fatalf("expected 2 users, got %d: %s", len(resp.Users), rec.Body.String())
	}

	var alice *userInfoJSON
	for i := range resp.Users {
		if resp.Users[i].Name == "alice" {
			alice = &resp.Users[i]
		}
	}
	if alice == nil {
		t.Fatalf("alice not found: %s", rec.Body.String())
	}
	if len(alice.Pictures) != 2 || !strings.HasPrefix(alice.UpdatedAt, "2026-09-02") {
		t.Fatalf("alice pictures/date wrong: %+v", *alice)
	}
}

// --- helpers ---

type userInfoJSON struct {
	Name      string   `json:"name"`
	Pictures  []string `json:"pictures"`
	UpdatedAt string   `json:"updated_at"`
}

func openUsersTestDB(t *testing.T) *bolt.DB {
	t.Helper()
	db, err := bolt.Open(t.TempDir()+"/users.db", 0600, nil)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := EnsureBucket(db); err != nil {
		db.Close()
		t.Fatalf("ensure buckets: %v", err)
	}
	return db
}

func storeAuditDirect(db *bolt.DB, entries []AuditEntry) error {
	return db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(auditBucketName)
		for _, e := range entries {
			id, _ := b.NextSequence()
			raw, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := b.Put(itob(id), raw); err != nil {
				return err
			}
		}
		return nil
	})
}

func itob(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

// --- /stats endpoint ---

func TestStatsEmpty(t *testing.T) {
	db := openUsersTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}

	var resp StatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TotalChecks != 0 || resp.TotalMatched != 0 || resp.TotalNoFace != 0 || resp.TotalNotMatched != 0 {
		t.Fatalf("all counts should be 0: %+v", resp)
	}
	if resp.LastMatched != nil {
		t.Fatalf("last_matched should be nil, got %v", *resp.LastMatched)
	}
}

func TestStatsCounts(t *testing.T) {
	db := openUsersTestDB(t)
	defer db.Close()

	entries := []AuditEntry{
		{Time: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC), Endpoint: "enroll", Name: "alice", Matched: true},
		{Time: time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC), Endpoint: "recognize", Name: "bob", Matched: true},
		{Time: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), Endpoint: "stream-check", Name: "", Matched: false},
		{Time: time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC), Endpoint: "stream-check", Name: "", Matched: false},
		{Time: time.Date(2026, 1, 1, 14, 0, 0, 0, time.UTC), Endpoint: "recognize", Name: "charlie", Matched: false},
		{Time: time.Date(2026, 1, 1, 15, 0, 0, 0, time.UTC), Endpoint: "stream-check", Name: "alice", Matched: true},
	}
	if err := storeAuditDirect(db, entries); err != nil {
		t.Fatalf("store audit: %v", err)
	}

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var resp StatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.TotalChecks != 6 {
		t.Fatalf("total_checks: expected 6, got %d", resp.TotalChecks)
	}
	if resp.TotalMatched != 3 {
		t.Fatalf("total_matched: expected 3, got %d", resp.TotalMatched)
	}
	if resp.TotalNoFace != 2 {
		t.Fatalf("total_no_face: expected 2, got %d", resp.TotalNoFace)
	}
	if resp.TotalNotMatched != 1 {
		t.Fatalf("total_not_matched: expected 1, got %d", resp.TotalNotMatched)
	}
	if resp.LastMatched == nil || !strings.Contains(*resp.LastMatched, "2026-01-01T15:00:00") {
		t.Fatalf("last_matched should be 2026-01-01T15:00:00, got %v", resp.LastMatched)
	}
}

// --- /users/<name> delete endpoint ---

func TestDeleteUser(t *testing.T) {
	db := openUsersTestDB(t)
	defer db.Close()

	// Pre-populate users.
	if err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		alice, _ := json.Marshal(&storedUser{Embeddings: [][]float32{{1, 2, 3}}})
		bob, _ := json.Marshal(&storedUser{Embeddings: [][]float32{{4, 5, 6}}})
		if err := b.Put([]byte("alice"), alice); err != nil {
			return err
		}
		return b.Put([]byte("bob"), bob)
	}); err != nil {
		t.Fatalf("put users: %v", err)
	}

	s := &FaceServer{
		boltDB: db,
		dbMap: map[string]*storedUser{
			"alice": {Embeddings: [][]float32{{1, 2, 3}}},
			"bob":   {Embeddings: [][]float32{{4, 5, 6}}},
		},
	}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	// Delete alice.
	req := httptest.NewRequest(http.MethodDelete, "/users/alice", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "deleted" || resp["name"] != "alice" {
		t.Fatalf("unexpected response: %v", resp)
	}

	// Verify alice is gone from cache.
	if _, ok := s.dbMap["alice"]; ok {
		t.Fatal("alice should be removed from cache")
	}
	// Verify bob is still there.
	if _, ok := s.dbMap["bob"]; !ok {
		t.Fatal("bob should still be in cache")
	}

	// Verify alice is gone from bbolt.
	if err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		if b.Get([]byte("alice")) != nil {
			return fmt.Errorf("alice still in bbolt")
		}
		return nil
	}); err != nil {
		t.Fatalf("bbolt check: %v", err)
	}
}

func TestDeleteUserNonExistent(t *testing.T) {
	db := openUsersTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodDelete, "/users/nonexistent", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteUserMethodNotAllowed(t *testing.T) {
	db := openUsersTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/users/alice", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestDeleteUserMissingName(t *testing.T) {
	db := openUsersTestDB(t)
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodDelete, "/users/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}
