package repository

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"h2hsecure.com/face/internal/domain"

	bolt "go.etcd.io/bbolt"
)

// statsCacheTTL is how long a computed stats cache remains valid.
const statsCacheTTL = 30 * time.Second

// Bucket name for audit data in bbolt.
const auditBucket = "Audit"

// AuditRepository implements domain.AuditRepository using bbolt.
type AuditRepository struct {
	db         *bolt.DB
	statsCache struct {
		sync.RWMutex
		stats    domain.Stats
		cachedAt time.Time
	}
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
	// Clear stats cache so the next ComputeStats recomputes.
	r.statsCache.Lock()
	r.statsCache.stats = domain.Stats{}
	r.statsCache.cachedAt = time.Time{}
	r.statsCache.Unlock()

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
				Status:     e.Status,
				DurationMs: e.DurationMs,
				FaceImage:  e.FaceImage,
				Embedding:  e.Embedding,
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
			if j.Status == "" {
				j.Status = "matched"
			}
			entries = append(entries, domain.AuditEntry{
				Time:       parseTime(j.Time),
				Endpoint:   j.Endpoint,
				Name:       j.Name,
				Similarity: j.Similarity,
				Matched:    j.Matched,
				Status:     j.Status,
				DurationMs: j.DurationMs,
				FaceImage:  j.FaceImage,
				Embedding:  j.Embedding,
				HasFace:    j.FaceImage != "",
			})
		}
		return nil
	})
	return entries, err
}

// ListPaginated returns a page of entries with filtering support.
func (r *AuditRepository) ListPaginated(opts domain.ListPaginatedOpts) ([]domain.AuditEntry, int, []string, error) {
	if opts.PerPage <= 0 {
		opts.PerPage = 20
	}
	if opts.Page <= 0 {
		opts.Page = 1
	}

	var all []domain.AuditEntry
	totalCount := 0
	nameSet := make(map[string]struct{})

	// Compute page boundaries (1-based indices).
	pageLower := (opts.Page - 1) * opts.PerPage
	pageUpper := opts.Page * opts.PerPage

	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()

		// Collect distinct non-empty names from ALL entries (full scan, no early exit).
		for _, v := c.Last(); v != nil; _, v = c.Prev() {
			var j auditEntryJSON
			if err := json.Unmarshal(v, &j); err != nil {
				continue
			}
			if j.Name != "" {
				nameSet[j.Name] = struct{}{}
			}
		}

		// Re-scan: iterate newest-first, count matching entries and collect the requested page.
		// Early exit: once we've passed the requested page (totalCount > pageUpper),
		// stop iterating—no need to process older entries we'll never return.
		for _, v := c.Last(); v != nil; _, v = c.Prev() {
			var j auditEntryJSON
			if err := json.Unmarshal(v, &j); err != nil {
				continue
			}
			if j.Status == "" {
				j.Status = "matched"
			}

			// Apply filters before counting.
			if opts.NameFilter != "" && !strings.Contains(strings.ToLower(j.Name), strings.ToLower(opts.NameFilter)) {
				continue
			}
			if opts.EndpointFilter != "" && j.Endpoint != opts.EndpointFilter {
				continue
			}
			if opts.MatchedFilter == "yes" && !j.Matched {
				continue
			}
			if opts.MatchedFilter == "no" && j.Matched {
				continue
			}
			if opts.StatusFilter != "" && j.Status != opts.StatusFilter {
				continue
			}

			totalCount++
			// Only build and keep entries within the requested page.
			// Do not include FaceImage or Embedding to keep the response lean.
			if totalCount <= pageUpper && totalCount > pageLower {
				all = append(all, domain.AuditEntry{
					Time:       parseTime(j.Time),
					Endpoint:   j.Endpoint,
					Name:       j.Name,
					Similarity: j.Similarity,
					Matched:    j.Matched,
					Status:     j.Status,
					DurationMs: j.DurationMs,
					HasFace:    j.Status != domain.AuditStatusNoFace,
				})
			}
			// Past the requested page: stop iterating.
			if totalCount > pageUpper {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, nil, err
	}

	// Convert name set to sorted slice.
	var names []string
	for n := range nameSet {
		names = append(names, n)
	}
	// Sort names for consistent ordering.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[i] > names[j] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}

	return all, totalCount, names, nil
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

// CountRecognizeEntries returns the count of recognize-only audit entries
// (endpoint == "recognize", status != "no_face").
func (r *AuditRepository) CountRecognizeEntries() (int, error) {
	var count int
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for _, v := c.Last(); v != nil; _, v = c.Prev() {
			var j auditEntryJSON
			if err := json.Unmarshal(v, &j); err != nil {
				continue
			}
			if j.Status == "" {
				j.Status = "matched"
			}
			if j.Endpoint != "recognize" || j.Status == domain.AuditStatusNoFace {
				continue
			}
			count++
		}
		return nil
	})
	return count, err
}

// ComputeRecognizeSummary returns aggregate statistics for recognize-only entries
// (endpoint == "recognize", status != "no_face"), in a single pass.
func (r *AuditRepository) ComputeRecognizeSummary() (domain.RecognizeSummaryResponse, error) {
	var result domain.RecognizeSummaryResponse
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for _, v := c.Last(); v != nil; _, v = c.Prev() {
			var j auditEntryJSON
			if err := json.Unmarshal(v, &j); err != nil {
				continue
			}
			if j.Status == "" {
				j.Status = "matched"
			}
			if j.Endpoint != "recognize" || j.Status == domain.AuditStatusNoFace {
				continue
			}
			result.TotalRecognize++
			switch j.Status {
			case domain.AuditStatusMatched:
				result.TotalMatched++
			case domain.AuditStatusNotMatched:
				result.TotalNotMatched++
			}
		}
		return nil
	})
	return result, err
}

// ListRecognizeEntries returns a page of recognize-only audit entries
// (endpoint == "recognize", status != "no_face"), optionally filtered by
// user name (case-insensitive exact match), sorted newest first.
func (r *AuditRepository) ListRecognizeEntries(opts domain.ListRecognizeEntriesOpts) ([]domain.AuditEntry, int, error) {
	if opts.PerPage <= 0 {
		opts.PerPage = 20
	}
	if opts.Page <= 0 {
		opts.Page = 1
	}

	var all []domain.AuditEntry
	var totalCount int

	// First pass: count all matching entries.
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for _, v := c.Last(); v != nil; _, v = c.Prev() {
			var j auditEntryJSON
			if err := json.Unmarshal(v, &j); err != nil {
				continue
			}
			if j.Status == "" {
				j.Status = "matched"
			}
			if j.Endpoint != "recognize" || j.Status == domain.AuditStatusNoFace {
				continue
			}
			if opts.UserFilter != "" && strings.ToLower(j.Name) != strings.ToLower(opts.UserFilter) {
				continue
			}
			totalCount++
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	// Second pass: collect entries for the requested page.
	pageLower := (opts.Page - 1) * opts.PerPage
	pageUpper := opts.Page * opts.PerPage
	err = r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		seen := 0
		for _, v := c.Last(); v != nil; _, v = c.Prev() {
			var j auditEntryJSON
			if err := json.Unmarshal(v, &j); err != nil {
				continue
			}
			if j.Status == "" {
				j.Status = "matched"
			}
			if j.Endpoint != "recognize" || j.Status == domain.AuditStatusNoFace {
				continue
			}
			if opts.UserFilter != "" && strings.ToLower(j.Name) != strings.ToLower(opts.UserFilter) {
				continue
			}
			seen++
			if seen <= pageUpper && seen > pageLower {
				all = append(all, domain.AuditEntry{
					Time:       parseTime(j.Time),
					Endpoint:   j.Endpoint,
					Name:       j.Name,
					Similarity: j.Similarity,
					Matched:    j.Matched,
					Status:     j.Status,
					DurationMs: j.DurationMs,
					FaceImage:  j.FaceImage,
				})
			}
			// Past the requested page: stop early.
			if seen > pageUpper {
				break
			}
		}
		return nil
	})
	return all, totalCount, err
}

// ListUnmatched returns the newest n entries that are not matched
// (status == "not_matched"). Returns newest first.
func (r *AuditRepository) ListUnmatched(n int) ([]domain.AuditEntry, error) {
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
			if j.Status == "" {
				j.Status = "matched"
			}
			if j.Status != domain.AuditStatusNotMatched {
				continue
			}
			entries = append(entries, domain.AuditEntry{
				Time:       parseTime(j.Time),
				Endpoint:   j.Endpoint,
				Name:       j.Name,
				Similarity: j.Similarity,
				Matched:    j.Matched,
				Status:     j.Status,
				DurationMs: j.DurationMs,
				FaceImage:  j.FaceImage,
				Embedding:  j.Embedding,
			})
		}
		return nil
	})
	return entries, err
}

// GetAuditFace returns the face_image (base64 JPEG) for the audit entry
// with the given RFC3339 timestamp.
func (r *AuditRepository) GetAuditFace(timestamp string) (string, error) {
	var faceImage string
	err := r.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(auditBucket))
		if b == nil {
			return fmt.Errorf("bucket %q not found", auditBucket)
		}
		c := b.Cursor()
		for _, v := c.Last(); v != nil; _, v = c.Prev() {
			var j auditEntryJSON
			if err := json.Unmarshal(v, &j); err != nil {
				continue
			}
			if j.Time == timestamp {
				faceImage = j.FaceImage
				return nil
			}
		}
		return fmt.Errorf("audit entry %q not found", timestamp)
	})
	return faceImage, err
}

// ComputeStats returns aggregate statistics from all audit entries.
// It uses an in-memory cache to avoid full-scan on every call.
func (r *AuditRepository) ComputeStats() (domain.Stats, error) {
	// Try cache first.
	r.statsCache.RLock()
	cached := r.statsCache.stats
	cachedAt := r.statsCache.cachedAt
	r.statsCache.RUnlock()

	if !cachedAt.IsZero() && time.Since(cachedAt) < statsCacheTTL {
		return cached, nil
	}

	// Cache miss or expired — recompute.
	var stats domain.Stats
	var lastMatched time.Time

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
			if j.Status == "" {
				j.Status = "matched"
			}
			stats.TotalChecks++
			switch j.Status {
			case domain.AuditStatusMatched:
				stats.TotalMatched++
				if j.Time != "" {
					t, err := time.Parse(time.RFC3339, j.Time)
					if err == nil && t.After(lastMatched) {
						lastMatched = t
					}
				}
			case domain.AuditStatusNoFace:
				stats.TotalNoFace++
			case domain.AuditStatusNotMatched:
				stats.TotalNotMatched++
			}
		}
		return nil
	})
	if err != nil {
		return domain.Stats{}, err
	}

	if !lastMatched.IsZero() {
		stats.LastMatched = &lastMatched
	}

	// Store in cache.
	r.statsCache.Lock()
	r.statsCache.stats = stats
	r.statsCache.cachedAt = time.Now()
	r.statsCache.Unlock()

	return stats, nil
}

// --- Internal types ---

// auditEntryJSON is the JSON representation stored in bbolt.
type auditEntryJSON struct {
	Time       string               `json:"time"`
	Endpoint   string               `json:"endpoint"`
	Name       string               `json:"name"`
	Similarity float32              `json:"similarity"`
	Matched    bool                 `json:"matched"`
	Status     string               `json:"status"`
	DurationMs int64                `json:"duration_ms"`
	FaceImage  string               `json:"face_image,omitempty"`
	Embedding  domain.FaceEmbedding `json:"embedding,omitempty"`
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
