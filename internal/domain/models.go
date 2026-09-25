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

// Candidate represents an unknown face collected from a stream.
type Candidate struct {
	ID        string
	Embedding FaceEmbedding
	FaceImage string // base64-encoded cropped face JPEG
	Time      time.Time
	StreamURL string
}

// CandidateGroup is a computed grouping of similar candidates.
type CandidateGroup struct {
	ID             string
	FaceCount      int
	BestSimilarity float32
	Faces          []Candidate
}

// AuditEntry records a single face scan attempt for the audit log.
type AuditEntry struct {
	Time       time.Time `json:"time"`
	Endpoint   string    `json:"endpoint"`
	Name       string    `json:"name"`
	Similarity float32   `json:"similarity"`
	Matched    bool      `json:"matched"`
	DurationMs int64     `json:"duration_ms"`
	FaceImage  string    `json:"face_image"` // base64-encoded cropped face JPEG
}

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
