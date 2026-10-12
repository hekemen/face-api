package repository

import (
	"encoding/json"
	"fmt"

	"h2hsecure.com/face/internal/domain"

	bolt "go.etcd.io/bbolt"
)

const settingsBucket = "Settings"

// SettingsRepository persists runtime settings in the bbolt "Settings"
// bucket (single JSON blob under the "settings" key).
type SettingsRepository struct {
	db *bolt.DB
}

// NewSettingsRepository creates a SettingsRepository on the given database.
func NewSettingsRepository(db *bolt.DB) *SettingsRepository {
	return &SettingsRepository{db: db}
}

// Get returns the stored settings, or nil when none are stored yet.
func (r *SettingsRepository) Get() (*domain.Settings, error) {
	var out *domain.Settings
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(settingsBucket))
		if b == nil {
			return nil
		}
		val := b.Get([]byte("settings"))
		if val == nil {
			return nil
		}
		s := &domain.Settings{}
		if err := json.Unmarshal(val, s); err != nil {
			return err
		}
		out = s
		return nil
	})
	return out, err
}

// Save stores the settings (upsert).
func (r *SettingsRepository) Save(s *domain.Settings) error {
	val, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	return r.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(settingsBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte("settings"), val)
	})
}