package repository

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"h2hsecure.com/face/internal/domain"

	bolt "go.etcd.io/bbolt"
)

// Bucket name for audit data in bbolt.
const auditBucket = "Audit"

// AuditRepository implements domain.AuditRepository using bbolt.
type AuditRepository struct {
	db *bolt.DB
}

// NewAuditRepository creates a new bbolt-backed audit repository.
func NewAuditRepository(db *bolt.DB) *AuditRepository {
	return &AuditRepository{db: db}
}

// EnsureBucket creates the Audit bucket if it does not exist.
func (r *AuditRepository) EnsureBucket() error {
	return r.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(auditBucket))
		return err
	})
}

// Append writes one or more audit entries.
func (r *AuditRepository) Append(entries []domain.AuditEntry) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			return fmt.Errorf("bucket %q not found", auditBucket)
		}
		for _, e := range entries {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, seq)
			val, err := json.Marshal(auditEntryJSON{
				Time:       formatTime(e.Time),
				Endpoint:   e.Endpoint,
				Name:       e.Name,
				Similarity: e.Similarity,
				Matched:    e.Matched,
				DurationMs: e.DurationMs,
				FaceImage:  e.FaceImage,
			})
			if err != nil {
				return err
			}
			if err := b.Put(key, val); err != nil {
				return err
			}
		}
		return nil
	})
}

// Recent returns the newest n entries (newest first).
func (r *AuditRepository) Recent(n int) ([]domain.AuditEntry, error) {
	if n <= 0 {
		n = 100
	}
	var entries []domain.AuditEntry
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Last(); k != nil && len(entries) < n; k, v = c.Prev() {
			var j auditEntryJSON
			if err := json.Unmarshal(v, &j); err != nil {
				continue
			}
			entries = append(entries, domain.AuditEntry{
				Time:       parseTime(j.Time),
				Endpoint:   j.Endpoint,
				Name:       j.Name,
				Similarity: j.Similarity,
				Matched:    j.Matched,
				DurationMs: j.DurationMs,
				FaceImage:  j.FaceImage,
			})
		}
		return nil
	})
	return entries, err
}

// ListPaginated returns a page of entries with filtering support.
func (r *AuditRepository) ListPaginated(opts domain.ListPaginatedOpts) ([]domain.AuditEntry, int, error) {
	if opts.PerPage <= 0 {
		opts.PerPage = 20
	}
	if opts.Page <= 0 {
		opts.Page = 1
	}

	var all []domain.AuditEntry
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var j auditEntryJSON
			if err := json.Unmarshal(v, &j); err != nil {
				continue
			}
			e := domain.AuditEntry{
				Time:       parseTime(j.Time),
				Endpoint:   j.Endpoint,
				Name:       j.Name,
				Similarity: j.Similarity,
				Matched:    j.Matched,
				DurationMs: j.DurationMs,
				FaceImage:  j.FaceImage,
			}

			// Apply filters.
			if opts.NameFilter != "" && !strings.Contains(strings.ToLower(e.Name), strings.ToLower(opts.NameFilter)) {
				continue
			}
			if opts.EndpointFilter != "" && e.Endpoint != opts.EndpointFilter {
				continue
			}
			if opts.MatchedFilter == "yes" && !e.Matched {
				continue
			}
			if opts.MatchedFilter == "no" && e.Matched {
				continue
			}
			all = append(all, e)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	totalCount := len(all)

	// Paginate (entries are already newest-first).
	start := (opts.Page - 1) * opts.PerPage
	if start >= totalCount {
		return nil, totalCount, nil
	}
	end := start + opts.PerPage
	if end > totalCount {
		end = totalCount
	}
	return all[start:end], totalCount, nil
}

// CountAll returns the total number of audit entries.
func (r *AuditRepository) CountAll() (int, error) {
	var count int
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			_ = k
			_ = v
			count++
			return nil
		})
	})
	return count, err
}

// ComputeStats returns aggregate statistics from all audit entries.
func (r *AuditRepository) ComputeStats() (domain.Stats, error) {
	var entries []domain.AuditEntry
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var j auditEntryJSON
			if err := json.Unmarshal(v, &j); err != nil {
				continue
			}
			entries = append(entries, domain.AuditEntry{
				Time:       parseTime(j.Time),
				Endpoint:   j.Endpoint,
				Name:       j.Name,
				Similarity: j.Similarity,
				Matched:    j.Matched,
				DurationMs: j.DurationMs,
				FaceImage:  j.FaceImage,
			})
		}
		return nil
	})
	if err != nil {
		return domain.Stats{}, err
	}

	stats := domain.Stats{
		TotalChecks: len(entries),
	}

	var lastMatched time.Time
	for _, e := range entries {
		if e.Matched {
			stats.TotalMatched++
			if e.Time.After(lastMatched) {
				lastMatched = e.Time
			}
		} else if e.Name == "" {
			stats.TotalNoFace++
		} else {
			stats.TotalNotMatched++
		}
	}

	if !lastMatched.IsZero() {
		stats.LastMatched = &lastMatched
	}

	return stats, nil
}

// --- Internal types ---

// auditEntryJSON is the JSON representation stored in bbolt.
type auditEntryJSON struct {
	Time       string  `json:"time"`
	Endpoint   string  `json:"endpoint"`
	Name       string  `json:"name"`
	Similarity float32 `json:"similarity"`
	Matched    bool    `json:"matched"`
	DurationMs int64   `json:"duration_ms"`
	FaceImage  string  `json:"face_image,omitempty"`
}

// Helper functions for time serialization.

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
