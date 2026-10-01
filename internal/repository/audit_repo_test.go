package repository

import (
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"h2hsecure.com/face/internal/domain"

	bolt "go.etcd.io/bbolt"
)

// openTestDB creates a temporary bbolt database, ensures both buckets, and returns it.
// The caller is responsible for closing and deleting the database.
func openTestDB(t *testing.T) *bolt.DB {
	t.Helper()
	tmpFile := t.TempDir() + "/test.db"
	db, err := bolt.Open(tmpFile, 0600, nil)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(auditBucket)); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("create buckets: %v", err)
	}
	return db
}

// writeLegacyEntry stores a raw JSON entry without a Status field to simulate
// pre-migration data.
func writeLegacyEntry(db *bolt.DB, t *testing.T, entry domain.AuditEntry) {
	t.Helper()
	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			t.Fatal("audit bucket not found")
		}
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, seq)
		// Marshal without Status field to simulate legacy data.
		val, err := json.Marshal(struct {
			Time       string  `json:"time"`
			Endpoint   string  `json:"endpoint"`
			Name       string  `json:"name"`
			Similarity float32 `json:"similarity"`
			Matched    bool    `json:"matched"`
			DurationMs int64   `json:"duration_ms"`
			FaceImage  string  `json:"face_image,omitempty"`
		}{
			Time:       formatTime(entry.Time),
			Endpoint:   entry.Endpoint,
			Name:       entry.Name,
			Similarity: entry.Similarity,
			Matched:    entry.Matched,
			DurationMs: entry.DurationMs,
			FaceImage:  entry.FaceImage,
		})
		if err != nil {
			return err
		}
		return b.Put(key, val)
	})
	if err != nil {
		t.Fatalf("write legacy entry: %v", err)
	}
}

func TestAuditEntryStatusDefaultsToMatched(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	// Write a legacy entry (no Status field).
	writeLegacyEntry(db, t, domain.AuditEntry{
		Endpoint: "stream-check",
		Name:     "john",
		Matched:  true,
	})

	// Read back via Recent.
	repo := NewAuditRepository(db)
	entries, err := repo.Recent(10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Status != domain.AuditStatusMatched {
		t.Errorf("expected Status=%q, got %q", domain.AuditStatusMatched, entries[0].Status)
	}
}

func TestAuditRepositoryListUnmatched(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := NewAuditRepository(db)

	now := time.Now()
	_ = repo.Append([]domain.AuditEntry{
		{Time: now.Add(-2 * time.Hour), Endpoint: "stream-check", Name: "alice", Similarity: 0.7, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 100, FaceImage: "matched.jpg"},
		{Time: now.Add(-1 * time.Hour), Endpoint: "stream-check", Name: "", Similarity: 0.3, Matched: false, Status: domain.AuditStatusNotMatched, DurationMs: 120, FaceImage: "unmatched.jpg"},
		{Time: now, Endpoint: "stream-check", Name: "", Similarity: 0, Matched: false, Status: domain.AuditStatusNoFace, DurationMs: 80},
		{Time: now.Add(-30 * time.Minute), Endpoint: "stream-check", Name: "", Similarity: 0.2, Matched: false, Status: domain.AuditStatusNotMatched, DurationMs: 110, FaceImage: "unmatched2.jpg"},
	})

	unmatched, err := repo.ListUnmatched(100)
	if err != nil {
		t.Fatalf("ListUnmatched: %v", err)
	}
	if len(unmatched) != 2 {
		t.Fatalf("expected 2 unmatched entries, got %d: %+v", len(unmatched), unmatched)
	}
	if unmatched[0].Status != domain.AuditStatusNotMatched {
		t.Errorf("entry[0] Status=%q, want %q", unmatched[0].Status, domain.AuditStatusNotMatched)
	}
	if unmatched[1].Status != domain.AuditStatusNotMatched {
		t.Errorf("entry[1] Status=%q, want %q", unmatched[1].Status, domain.AuditStatusNotMatched)
	}
}

func TestAuditRepositoryCountAll(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := NewAuditRepository(db)

	now := time.Now()
	_ = repo.Append([]domain.AuditEntry{
		{Time: now, Endpoint: "stream-check", Name: "alice", Matched: true, DurationMs: 100},
		{Time: now, Endpoint: "stream-check", Name: "", Matched: false, DurationMs: 120},
		{Time: now, Endpoint: "stream-check", Name: "", Matched: false, DurationMs: 80},
	})

	count, err := repo.CountAll()
	if err != nil {
		t.Fatalf("CountAll: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3, got %d", count)
	}
}

func TestAuditRepositoryListRecognizeEntries(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := NewAuditRepository(db)

	now := time.Now()
	_ = repo.Append([]domain.AuditEntry{
		{Time: now.Add(-3 * time.Hour), Endpoint: "recognize", Name: "alice", Similarity: 0.8, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 100, FaceImage: "alice_rec.jpg"},
		{Time: now.Add(-2 * time.Hour), Endpoint: "recognize", Name: "", Similarity: 0.3, Matched: false, Status: domain.AuditStatusNotMatched, DurationMs: 120, FaceImage: "unmatched_rec.jpg"},
		{Time: now.Add(-1 * time.Hour), Endpoint: "stream-check", Name: "bob", Similarity: 0.9, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 200, FaceImage: "bob_stream.jpg"},
		{Time: now, Endpoint: "enroll", Name: "charlie", Similarity: 1.0, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 50, FaceImage: "charlie_enroll.jpg"},
		{Time: now.Add(-30 * time.Minute), Endpoint: "recognize", Name: "alice", Similarity: 0.75, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 90, FaceImage: "alice_rec2.jpg"},
	})

	entries, total, err := repo.ListRecognizeEntries(domain.ListRecognizeEntriesOpts{Page: 1, PerPage: 20})
	if err != nil {
		t.Fatalf("ListRecognizeEntries: %v", err)
	}
	if total != 3 {
		t.Errorf("expected total 3, got %d", total)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d: %+v", len(entries), entries)
	}
	// Verify newest first order
	if entries[0].Name != "alice" || !entries[0].Matched {
		t.Errorf("entry[0] expected alice matched, got %+v", entries[0])
	}
	// Verify all are recognize-only
	for _, e := range entries {
		if e.Endpoint != "recognize" {
			t.Errorf("expected endpoint=recognize, got %q", e.Endpoint)
		}
		if e.Status == domain.AuditStatusNoFace {
			t.Errorf("expected no no_face entries, got %+v", e)
		}
	}
}

func TestAuditRepositoryListRecognizeEntries_UserFilter(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := NewAuditRepository(db)

	now := time.Now()
	_ = repo.Append([]domain.AuditEntry{
		{Time: now.Add(-2 * time.Hour), Endpoint: "recognize", Name: "alice", Similarity: 0.8, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 100, FaceImage: "alice.jpg"},
		{Time: now.Add(-1 * time.Hour), Endpoint: "recognize", Name: "bob", Similarity: 0.3, Matched: false, Status: domain.AuditStatusNotMatched, DurationMs: 120, FaceImage: "bob.jpg"},
		{Time: now, Endpoint: "recognize", Name: "Alice", Similarity: 0.75, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 90, FaceImage: "alice2.jpg"},
	})

	entries, total, err := repo.ListRecognizeEntries(domain.ListRecognizeEntriesOpts{UserFilter: "alice", Page: 1, PerPage: 20})
	if err != nil {
		t.Fatalf("ListRecognizeEntries: %v", err)
	}
	if total != 2 {
		t.Errorf("expected total 2 for user filter 'alice', got %d", total)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries for user filter, got %d", len(entries))
	}
	// Verify case-insensitive match
	for _, e := range entries {
		if e.Name != "alice" && e.Name != "Alice" {
			t.Errorf("expected name alice/Alice, got %q", e.Name)
		}
	}
}

func TestAuditRepositoryListRecognizeEntries_Pagination(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := NewAuditRepository(db)

	now := time.Now()
	_ = repo.Append([]domain.AuditEntry{
		{Time: now.Add(-5 * time.Hour), Endpoint: "recognize", Name: "alice", Similarity: 0.8, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 100, FaceImage: "a1.jpg"},
		{Time: now.Add(-4 * time.Hour), Endpoint: "recognize", Name: "alice", Similarity: 0.7, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 100, FaceImage: "a2.jpg"},
		{Time: now.Add(-3 * time.Hour), Endpoint: "recognize", Name: "bob", Similarity: 0.3, Matched: false, Status: domain.AuditStatusNotMatched, DurationMs: 120, FaceImage: "b1.jpg"},
		{Time: now.Add(-2 * time.Hour), Endpoint: "recognize", Name: "bob", Similarity: 0.6, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 100, FaceImage: "b2.jpg"},
		{Time: now.Add(-1 * time.Hour), Endpoint: "recognize", Name: "charlie", Similarity: 0.2, Matched: false, Status: domain.AuditStatusNotMatched, DurationMs: 120, FaceImage: "c1.jpg"},
	})

	// Page 1 with perPage=2
	// Page 1 with perPage=2
	entries, total, err := repo.ListRecognizeEntries(domain.ListRecognizeEntriesOpts{Page: 1, PerPage: 2})
	if err != nil {
		t.Fatalf("ListRecognizeEntries: %v", err)
	}
	if total != 5 {
		t.Errorf("expected total 5, got %d", total)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries on page 1, got %d", len(entries))
	}
	if entries[0].Name != "charlie" {
		t.Errorf("entry[0] expected charlie (newest), got %q", entries[0].Name)
	}

	// Page 2
	entries2, _, err := repo.ListRecognizeEntries(domain.ListRecognizeEntriesOpts{Page: 2, PerPage: 2})
	if err != nil {
		t.Fatalf("ListRecognizeEntries page 2: %v", err)
	}
	if len(entries2) != 2 {
		t.Fatalf("expected 2 entries on page 2, got %d", len(entries2))
	}
	if entries2[0].Name != "bob" {
		t.Errorf("entry[0] on page 2 expected bob, got %q", entries2[0].Name)
	}
}

func TestAuditRepositoryCountRecognizeEntries(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := NewAuditRepository(db)

	now := time.Now()
	_ = repo.Append([]domain.AuditEntry{
		{Time: now.Add(-3 * time.Hour), Endpoint: "recognize", Name: "alice", Similarity: 0.8, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 100, FaceImage: "alice.jpg"},
		{Time: now.Add(-2 * time.Hour), Endpoint: "recognize", Name: "", Similarity: 0.3, Matched: false, Status: domain.AuditStatusNotMatched, DurationMs: 120, FaceImage: "unmatched.jpg"},
		{Time: now.Add(-1 * time.Hour), Endpoint: "recognize", Name: "bob", Similarity: 0.7, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 90, FaceImage: "bob.jpg"},
		{Time: now.Add(-1 * time.Hour), Endpoint: "stream-check", Name: "bob", Similarity: 0.9, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 200, FaceImage: "bob_stream.jpg"},
		{Time: now, Endpoint: "enroll", Name: "charlie", Similarity: 1.0, Matched: true, Status: domain.AuditStatusMatched, DurationMs: 50, FaceImage: "charlie.jpg"},
		{Time: now.Add(-30 * time.Minute), Endpoint: "recognize", Name: "", Similarity: 0, Matched: false, Status: domain.AuditStatusNoFace, DurationMs: 80},
	})

	count, err := repo.CountRecognizeEntries()
	if err != nil {
		t.Fatalf("CountRecognizeEntries: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 recognize-only (no no_face), got %d", count)
	}
}
