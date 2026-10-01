package domain

import (
	"math"
	"time"
)

// FaceEmbedding is a normalized face embedding vector (typically 512-dim).
type FaceEmbedding []float32

// User represents an enrolled identity with one or more face embeddings.
type User struct {
	Name       string
	Embeddings []FaceEmbedding
	Pictures   []string // base64-encoded cropped face JPEGs (up to 3)
	UpdatedAt  time.Time
}

// AuditEntry records a single face scan attempt for the audit log.
type AuditEntry struct {
	Time       time.Time     `json:"time"`
	Endpoint   string        `json:"endpoint"`
	Name       string        `json:"name"`
	Similarity float32       `json:"similarity"`
	Matched    bool          `json:"matched"`
	Status     string        `json:"status"`
	DurationMs int64         `json:"duration_ms"`
	FaceImage  string        `json:"face_image,omitempty"` // base64-encoded image; lazy-loaded via /api/audit/face
	Embedding  FaceEmbedding `json:"embedding"` // 512-dim embedding (for promotion)
	HasFace    bool          `json:"has_face"`  // true if a face was detected (not "no_face")
}

// Audit entry status constants.
const (
	AuditStatusMatched    = "matched"
	AuditStatusNotMatched = "not_matched"
	AuditStatusNoFace     = "no_face"
)

// --- Utility functions ---

// cosineSimilarity computes the cosine similarity between two float32 vectors.
func CosineSimilarity(a, b FaceEmbedding) float32 {
	var dot, normA, normB float32
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (float32(math.Sqrt(float64(normA))) * float32(math.Sqrt(float64(normB))))
}

// BestEmbeddingScore returns the highest cosine similarity between query and
// any of the user's stored embeddings.
func BestEmbeddingScore(query FaceEmbedding, embeddings []FaceEmbedding) float32 {
	var best float32 = -1.0
	for _, vec := range embeddings {
		if s := CosineSimilarity(query, vec); s > best {
			best = s
		}
	}
	return best
}
