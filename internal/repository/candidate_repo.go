package repository

import (
	"encoding/json"
	"fmt"
	"sort"

	"h2hsecure.com/face/internal/domain"

	bolt "go.etcd.io/bbolt"
)

// Bucket name for candidate data in bbolt.
const candidatesBucket = "Candidates"

// CandidateRepository implements domain.CandidateRepository using bbolt.
type CandidateRepository struct {
	db *bolt.DB
}

// NewCandidateRepository creates a new bbolt-backed candidate repository.
func NewCandidateRepository(db *bolt.DB) *CandidateRepository {
	return &CandidateRepository{db: db}
}

// EnsureBucket creates the Candidates bucket if it does not exist.
func (r *CandidateRepository) EnsureBucket() error {
	return r.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(candidatesBucket))
		return err
	})
}

// Save stores a candidate face.
func (r *CandidateRepository) Save(c *domain.Candidate) error {
	val, err := json.Marshal(candidateJSON{
		ID:        c.ID,
		Embedding: c.Embedding,
		FaceImage: c.FaceImage,
		Time:      formatTime(c.Time),
		StreamURL: c.StreamURL,
	})
	if err != nil {
		return fmt.Errorf("marshal candidate: %w", err)
	}
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(candidatesBucket))
		if b == nil {
			return nil
		}
		return b.Put([]byte(c.ID), val)
	})
}

// GetByID returns a candidate by ID, or an error if not found.
func (r *CandidateRepository) GetByID(id string) (*domain.Candidate, error) {
	var c *domain.Candidate
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(candidatesBucket))
		if b == nil {
			return fmt.Errorf("bucket %q not found", candidatesBucket)
		}
		val := b.Get([]byte(id))
		if val == nil {
			return fmt.Errorf("candidate %q not found", id)
		}
		var j candidateJSON
		if err := json.Unmarshal(val, &j); err != nil {
			return err
		}
		c = &domain.Candidate{
			ID:        j.ID,
			Embedding: j.Embedding,
			FaceImage: j.FaceImage,
			Time:      parseTime(j.Time),
			StreamURL: j.StreamURL,
		}
		return nil
	})
	return c, err
}

// Delete removes a candidate by ID.
func (r *CandidateRepository) Delete(id string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(candidatesBucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(id))
	})
}

// DeleteByID removes candidates by their IDs.
func (r *CandidateRepository) DeleteByID(ids []string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(candidatesBucket))
		if b == nil {
			return nil
		}
		for _, id := range ids {
			if err := b.Delete([]byte(id)); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListAll returns all candidates.
func (r *CandidateRepository) ListAll() ([]*domain.Candidate, error) {
	var candidates []*domain.Candidate
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(candidatesBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			var j candidateJSON
			if err := json.Unmarshal(v, &j); err != nil {
				return nil // skip malformed
			}
			candidates = append(candidates, &domain.Candidate{
				ID:        j.ID,
				Embedding: j.Embedding,
				FaceImage: j.FaceImage,
				Time:      parseTime(j.Time),
				StreamURL: j.StreamURL,
			})
			return nil
		})
	})
	return candidates, err
}

// GroupBySimilarity groups candidates by pairwise cosine similarity >= threshold.
func (r *CandidateRepository) GroupBySimilarity(threshold float32) ([]*domain.CandidateGroup, error) {
	if threshold <= 0 {
		threshold = 0.45
	}

	all, err := r.ListAll()
	if err != nil {
		return nil, err
	}

	n := len(all)
	assigned := make([]bool, n)
	var groups []*domain.CandidateGroup

	for i := 0; i < n; i++ {
		if assigned[i] {
			continue
		}
		g := &domain.CandidateGroup{Faces: []domain.Candidate{*all[i]}}
		assigned[i] = true
		for j := i + 1; j < n; j++ {
			if assigned[j] {
				continue
			}
			for _, fg := range g.Faces {
				if domain.CosineSimilarity(all[j].Embedding, fg.Embedding) >= threshold {
					g.Faces = append(g.Faces, *all[j])
					assigned[j] = true
					break
				}
			}
		}
		var bestSim float32
		for a := 0; a < len(g.Faces); a++ {
			for b := a + 1; b < len(g.Faces); b++ {
				sim := domain.CosineSimilarity(g.Faces[a].Embedding, g.Faces[b].Embedding)
				if sim > bestSim {
					bestSim = sim
				}
			}
		}
		g.FaceCount = len(g.Faces)
		g.BestSimilarity = bestSim
		g.ID = generateGroupID(g.Faces)
		groups = append(groups, g)
	}

	return groups, nil
}

// --- Internal types ---

// candidateJSON is the JSON representation stored in bbolt.
type candidateJSON struct {
	ID        string               `json:"id"`
	Embedding domain.FaceEmbedding `json:"embedding"`
	FaceImage string               `json:"face_image"`
	Time      string               `json:"time"`
	StreamURL string               `json:"stream_url,omitempty"`
}

func generateGroupID(faces []domain.Candidate) string {
	if len(faces) == 0 {
		return ""
	}
	// Sort by time for deterministic grouping
	sorted := make([]domain.Candidate, len(faces))
	copy(sorted, faces)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Time.Before(sorted[j].Time)
	})
	return sorted[0].Time.Format("20060102150405")
}
