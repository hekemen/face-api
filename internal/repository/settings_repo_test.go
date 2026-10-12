package repository

import (
	"testing"

	"h2hsecure.com/face/internal/domain"
)

// TestSettingsRepoGetEmpty verifies Get returns nil (not an error) when no
// settings have been stored yet — a fresh database has no Settings bucket.
func TestSettingsRepoGetEmpty(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := NewSettingsRepository(db)
	s, err := repo.Get()
	if err != nil {
		t.Fatalf("Get on empty db: unexpected error: %v", err)
	}
	if s != nil {
		t.Errorf("expected nil settings on empty db, got %+v", s)
	}
}

// TestSettingsRepoSaveGetRoundTrip verifies Save persists the settings and
// Get returns them (including when the bucket had to be created).
func TestSettingsRepoSaveGetRoundTrip(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := NewSettingsRepository(db)
	if err := repo.Save(&domain.Settings{Threshold: 0.55}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	s, err := repo.Get()
	if err != nil {
		t.Fatalf("Get after Save: %v", err)
	}
	if s == nil {
		t.Fatal("expected settings after Save, got nil")
	}
	if s.Threshold != 0.55 {
		t.Errorf("expected threshold 0.55, got %v", s.Threshold)
	}
}

// TestSettingsRepoSaveOverwrites verifies a second Save replaces the stored
// value rather than appending.
func TestSettingsRepoSaveOverwrites(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	repo := NewSettingsRepository(db)
	if err := repo.Save(&domain.Settings{Threshold: 0.55}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := repo.Save(&domain.Settings{Threshold: 0.3}); err != nil {
		t.Fatalf("Save (second): %v", err)
	}

	s, err := repo.Get()
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if s == nil {
		t.Fatal("expected settings, got nil")
	}
	if s.Threshold != 0.3 {
		t.Errorf("expected threshold 0.3 after overwrite, got %v", s.Threshold)
	}
}