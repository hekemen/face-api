package repository

import (
	"encoding/json"
	"fmt"
	"time"

	"h2hsecure.com/face/internal/domain"

	bolt "go.etcd.io/bbolt"
)

// Bucket name for user data in bbolt.
const facesBucket = "Faces"

// UserRepository implements domain.UserRepository using bbolt.
type UserRepository struct {
	db *bolt.DB
}

// NewUserRepository creates a new bbolt-backed user repository.
func NewUserRepository(db *bolt.DB) *UserRepository {
	return &UserRepository{db: db}
}

// EnsureBucket creates the Faces bucket if it does not exist.
func (r *UserRepository) EnsureBucket() error {
	return r.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(facesBucket))
		return err
	})
}

// GetByName returns the user with the given name, or an error if not found.
func (r *UserRepository) GetByName(name string) (*domain.User, error) {
	var u *domain.User
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(facesBucket))
		if b == nil {
			return fmt.Errorf("bucket %q not found", facesBucket)
		}
		val := b.Get([]byte(name))
		if val == nil {
			return fmt.Errorf("user %q not found", name)
		}
		var stored storedUser
		if err := json.Unmarshal(val, &stored); err != nil {
			// Legacy format: flat or nested embedding array
			lu, err2 := legacyToUser(val, name)
			if err2 != nil {
				return err2
			}
			u = lu
			return nil
		}
		updatedAt := parseTime(stored.UpdatedAt)
		u = &domain.User{
			Name:       name,
			Embeddings: stored.Embeddings,
			Pictures:   stored.Pictures,
			UpdatedAt:  updatedAt,
		}
		return nil
	})
	return u, err
}

// Exists returns true if a user with the given name already exists.
func (r *UserRepository) Exists(name string) (bool, error) {
	var exists bool
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(facesBucket))
		if b == nil {
			return nil
		}
		exists = b.Get([]byte(name)) != nil
		return nil
	})
	return exists, err
}

// Save stores or updates a user.
func (r *UserRepository) Save(u *domain.User) error {
	val, err := json.Marshal(storedUser{
		Embeddings: u.Embeddings,
		Pictures:   u.Pictures,
		UpdatedAt:  u.UpdatedAt.Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("marshal user: %w", err)
	}
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(facesBucket))
		if b == nil {
			return fmt.Errorf("bucket %q not found", facesBucket)
		}
		return b.Put([]byte(u.Name), val)
	})
}

// Delete removes a user by name.
func (r *UserRepository) Delete(name string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(facesBucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(name))
	})
}

// ListAll returns all enrolled users.
func (r *UserRepository) ListAll() ([]*domain.User, error) {
	var users []*domain.User
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(facesBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var stored storedUser
			if err := json.Unmarshal(v, &stored); err != nil {
				// Legacy format — convert
				u, err2 := legacyToUser(v, string(k))
				if err2 != nil {
					return nil // skip malformed
				}
				users = append(users, u)
				return nil
			}
			updatedAt := parseTime(stored.UpdatedAt)
			users = append(users, &domain.User{
				Name:       string(k),
				Embeddings: stored.Embeddings,
				Pictures:   stored.Pictures,
				UpdatedAt:  updatedAt,
			})
			return nil
		})
	})
	return users, err
}

// --- Internal types ---

// storedUser is the per-user value persisted in the Faces bucket.
type storedUser struct {
	Embeddings []domain.FaceEmbedding `json:"embeddings"`
	Pictures   []string               `json:"pictures,omitempty"`
	UpdatedAt  string                 `json:"updated_at,omitempty"`
}

// legacyToUser handles legacy embedding-array formats (flat or nested).
func legacyToUser(v []byte, name string) (*domain.User, error) {
	var nested [][]float32
	if !startsWith(v, "[[") {
		var flat []float32
		if err := json.Unmarshal(v, &flat); err != nil {
			return nil, err
		}
		nested = [][]float32{flat}
	} else {
		if err := json.Unmarshal(v, &nested); err != nil {
			return nil, err
		}
	}
	embeddings := make([]domain.FaceEmbedding, 0, len(nested))
	for _, e := range nested {
		embeddings = append(embeddings, domain.FaceEmbedding(e))
	}
	return &domain.User{
		Name:       name,
		Embeddings: embeddings,
	}, nil
}

func startsWith(data []byte, prefix string) bool {
	if len(data) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if data[i] != prefix[i] {
			return false
		}
	}
	return true
}
