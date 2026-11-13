package repository

import (
	"testing"

	"h2hsecure.com/face/internal/domain"

	bolt "go.etcd.io/bbolt"
)

// openUserTestDB creates a temporary bbolt database with the Faces bucket.
func openUserTestDB(t *testing.T) *bolt.DB {
	t.Helper()
	tmpFile := t.TempDir() + "/test.db"
	db, err := bolt.Open(tmpFile, 0600, nil)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(facesBucket))
		return err
	}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestUserAlignmentRoundTrips verifies that the alignment mode a user was
// enrolled with survives a save and load.
//
// The alignment mode decides which crop produced a user's embedding, and
// embeddings from different modes are not comparable, so the mode has to be
// stored alongside them.
func TestUserAlignmentRoundTrips(t *testing.T) {
	db := openUserTestDB(t)
	repo := NewUserRepository(db)

	err := repo.Save(&domain.User{
		Name:       "alice",
		Embeddings: []domain.FaceEmbedding{{1, 0, 0}},
		Alignment:  "arcface5",
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := repo.GetByName("alice")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.Alignment != "arcface5" {
		t.Fatalf("Alignment = %q, want arcface5", got.Alignment)
	}
}

// TestUserAlignmentInListAll verifies that ListAll reports the alignment
// mode too, since the matching path reads users through ListAll.
func TestUserAlignmentInListAll(t *testing.T) {
	db := openUserTestDB(t)
	repo := NewUserRepository(db)

	err := repo.Save(&domain.User{
		Name:       "alice",
		Embeddings: []domain.FaceEmbedding{{1, 0, 0}},
		Alignment:  "arcface5",
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	users, err := repo.ListAll()
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("ListAll returned %d users, want 1", len(users))
	}
	if users[0].Alignment != "arcface5" {
		t.Fatalf("Alignment = %q, want arcface5", users[0].Alignment)
	}
}

// TestUserAlignmentEmptyForLegacyRecords verifies that a record written
// before the alignment field existed loads with an empty mode. An empty
// mode means "unknown", which the matching path treats as comparable, so
// existing enrollments keep working until they are re-enrolled.
func TestUserAlignmentEmptyForLegacyRecords(t *testing.T) {
	db := openUserTestDB(t)
	repo := NewUserRepository(db)

	// Write a record without the alignment field, as an older build did.
	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(facesBucket))
		if b == nil {
			t.Fatal("faces bucket not found")
		}
		return b.Put([]byte("bob"), []byte(`{"embeddings":[[1,0,0]],"pictures":["x"]}`))
	})
	if err != nil {
		t.Fatalf("write legacy record: %v", err)
	}

	got, err := repo.GetByName("bob")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.Alignment != "" {
		t.Fatalf("Alignment = %q, want empty for a legacy record", got.Alignment)
	}
}
