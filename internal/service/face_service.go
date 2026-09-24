package service

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"time"

	"h2hsecure.com/face/internal/domain"
)

const maxPicturesPerUser = 3

// FaceServiceImpl implements domain.FaceService using repositories and a face processor.
type FaceServiceImpl struct {
	users       domain.UserRepository
	candidates  domain.CandidateRepository
	audit       domain.AuditRepository
	processor   domain.FaceProcessor
	rtspReader  domain.RTSPReader
	threshold   float32
	mu          sync.Mutex
}

// New creates a new FaceService.
func New(users domain.UserRepository, candidates domain.CandidateRepository,
	audit domain.AuditRepository, processor domain.FaceProcessor, threshold float32) *FaceServiceImpl {
	return &FaceServiceImpl{
		users:      users,
		candidates: candidates,
		audit:      audit,
		processor:  processor,
		threshold:  threshold,
	}
}

// WithRTSPReader sets the RTSP reader for stream-based operations.
func (s *FaceServiceImpl) WithRTSPReader(r domain.RTSPReader) *FaceServiceImpl {
	s.rtspReader = r
	return s
}

// --- CheckStream (URL-based, uses injected RTSP reader) ---

func (s *FaceServiceImpl) CheckStream(rtspURL string) (*domain.StreamCheckResult, error) {
	if s.rtspReader == nil {
		return nil, fmt.Errorf("RTSP reader not configured")
	}
	imageData, err := s.rtspReader.ReadFrame(rtspURL, 3*time.Second)
	if err != nil {
		reason := "Failed to connect to RTSP stream"
		if isNoFaceError(err) {
			reason = "No face detected within 3 seconds"
		}
		return &domain.StreamCheckResult{
			OperationDuration: domain.OperationDuration{DurationMs: 0},
			Status:            "not ok",
			Reason:            reason,
		}, nil
	}
	return s.CheckStreamImage(imageData)
}

// --- Enroll ---

func (s *FaceServiceImpl) EnrollImage(name string, imageData []byte) error {
	crop, err := s.processor.DetectAndCrop(imageData)
	if err != nil {
		return fmt.Errorf("face detection: %w", err)
	}

	embedding := crop.Embedding

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check max pictures limit
	u, err := s.users.GetByName(name)
	if err != nil && err.Error() != fmt.Sprintf("user %q not found", name) {
		return err
	}
	if u != nil && len(u.Embeddings) >= maxPicturesPerUser {
		return fmt.Errorf("maximum %d pictures per user", maxPicturesPerUser)
	}

	embeddings := u.Embeddings
	if embeddings == nil {
		embeddings = []domain.FaceEmbedding{}
	}
	pictures := u.Pictures
	if pictures == nil {
		pictures = []string{}
	}

	updated := &domain.User{
		Name:       name,
		Embeddings: append(embeddings, embedding),
		Pictures:   append(pictures, string(crop.Data)),
		UpdatedAt:  time.Now(),
	}

	if err := s.users.Save(updated); err != nil {
		return fmt.Errorf("save user: %w", err)
	}

	// Audit entry
	err = s.audit.Append([]domain.AuditEntry{{
		Time:       time.Now(),
		Endpoint:   "enroll",
		Name:       name,
		Similarity: 1.0,
		Matched:    true,
		DurationMs: 0,
		FaceImage:  string(crop.Data),
	}})
	if err != nil {
		return fmt.Errorf("write audit entry: %w", err)
	}

	return nil
}

// --- Recognize ---

func (s *FaceServiceImpl) RecognizeImage(imageData []byte) (*domain.RecognitionResult, error) {
	start := time.Now()

	crop, err := s.processor.DetectAndCrop(imageData)
	if err != nil {
		return nil, fmt.Errorf("face detection: %w", err)
	}

	embedding := crop.Embedding

	users, err := s.users.ListAll()
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	var bestName string
	var bestScore float32 = -1.0
	for _, u := range users {
		score := domain.BestEmbeddingScore(embedding, u.Embeddings)
		if score > bestScore {
			bestScore = score
			bestName = u.Name
		}
	}

	matched := bestScore >= s.threshold
	result := &domain.RecognitionResult{
		OperationDuration: domain.OperationDuration{DurationMs: time.Since(start).Milliseconds()},
		Name:              "Unknown",
		Similarity:        bestScore,
		Matched:           matched,
	}
	if matched {
		result.Name = bestName
	}

	// Audit entry
	_ = s.audit.Append([]domain.AuditEntry{{
		Time:       time.Now(),
		Endpoint:   "recognize",
		Name:       result.Name,
		Similarity: bestScore,
		Matched:    matched,
		DurationMs: result.DurationMs,
		FaceImage:  string(crop.Data),
	}})

	return result, nil
}

// --- Stream Check ---

// CheckStreamImage runs face detection and recognition on an image captured
// from an RTSP stream. The caller (HTTP handler / MQTT bridge) is responsible
// for reading the frame from RTSP; this method performs detection, recognition,
// and auto-collection of unmatched faces.
func (s *FaceServiceImpl) CheckStreamImage(imageData []byte) (*domain.StreamCheckResult, error) {
	start := time.Now()

	crop, err := s.processor.DetectAndCrop(imageData)
	if err != nil {
		reason := "No face detected within 3 seconds"
		// For RTSP connection errors, the caller should handle it.
		if isConnectionError(err) {
			reason = "Failed to connect to RTSP stream"
		}
		return &domain.StreamCheckResult{
			OperationDuration: domain.OperationDuration{DurationMs: time.Since(start).Milliseconds()},
			Status:            "not ok",
			Reason:            reason,
		}, nil
	}

	embedding := crop.Embedding

	users, err := s.users.ListAll()
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	var bestName string
	var highestScore float32 = -1.0
	for _, u := range users {
		score := domain.BestEmbeddingScore(embedding, u.Embeddings)
		if score > highestScore {
			highestScore = score
			bestName = u.Name
		}
	}

	matched := highestScore >= s.threshold
	dur := domain.OperationDuration{DurationMs: time.Since(start).Milliseconds()}

	// Audit entry
	_ = s.audit.Append([]domain.AuditEntry{{
		Time:       time.Now(),
		Endpoint:   "stream-check",
		Name:       bestName,
		Similarity: highestScore,
		Matched:    matched,
		DurationMs: dur.DurationMs,
		FaceImage:  string(crop.Data),
	}})

	if matched {
		return &domain.StreamCheckResult{
			OperationDuration: dur,
			Status:            "ok",
			Name:              bestName,
			Similarity:        highestScore,
			Matched:           true,
			FaceImage:         string(crop.Data),
		}, nil
	}

	// Auto-collect unmatched face as candidate
	candidate := &domain.Candidate{
		Embedding: embedding,
		FaceImage: string(crop.Data),
		Time:      time.Now(),
	}
	if err := s.CollectStreamCandidate(candidate); err != nil {
		// Log but don't fail the check
	}

	return &domain.StreamCheckResult{
		OperationDuration: dur,
		Status:            "not ok",
		Reason:            "No face detected within 3 seconds",
		Similarity:        highestScore,
		Matched:           false,
		FaceImage:         string(crop.Data),
	}, nil
}

// CollectStreamCandidate stores an unmatched face from a stream as a candidate.
func (s *FaceServiceImpl) CollectStreamCandidate(c *domain.Candidate) error {
	// Check for duplicates against existing candidates
	allCandidates, err := s.candidates.ListAll()
	if err != nil {
		return fmt.Errorf("list candidates: %w", err)
	}

	for _, existing := range allCandidates {
		if domain.CosineSimilarity(c.Embedding, existing.Embedding) >= s.threshold {
			return nil // duplicate, skip
		}
	}

	// Check against enrolled users too
	allUsers, err := s.users.ListAll()
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}

	for _, u := range allUsers {
		if domain.BestEmbeddingScore(c.Embedding, u.Embeddings) >= s.threshold {
			return nil // matched to enrolled user, skip
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.candidates.Save(c); err != nil {
		return fmt.Errorf("save candidate: %w", err)
	}

	return nil
}

// --- Users ---

func (s *FaceServiceImpl) ListUsers() ([]*domain.User, error) {
	return s.users.ListAll()
}

func (s *FaceServiceImpl) DeleteUser(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.users.Delete(name); err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	return nil
}

// --- Candidates ---

func (s *FaceServiceImpl) ListCandidates() ([]*domain.CandidateGroup, error) {
	return s.candidates.GroupBySimilarity(s.threshold)
}

func (s *FaceServiceImpl) PromoteCandidate(candidateID, name string) error {
	// Check if user already exists
	exists, err := s.users.Exists(name)
	if err != nil {
		return fmt.Errorf("check user existence: %w", err)
	}
	if exists {
		return fmt.Errorf("user %q already exists", name)
	}

	candidate, err := s.candidates.GetByID(candidateID)
	if err != nil {
		return fmt.Errorf("get candidate: %w", err)
	}

	// Find all candidates in the same group
	allCandidates, err := s.candidates.ListAll()
	if err != nil {
		return fmt.Errorf("list candidates: %w", err)
	}

	var group []*domain.Candidate
	for _, c := range allCandidates {
		if domain.CosineSimilarity(candidate.Embedding, c.Embedding) >= 0.45 {
			group = append(group, c)
		}
	}

	// Find the face most similar to the group centroid
	var centroid domain.FaceEmbedding
	for _, c := range group {
		for i, v := range c.Embedding {
			if len(centroid) == 0 {
				centroid = make(domain.FaceEmbedding, len(c.Embedding))
			}
			centroid[i] += v
		}
	}
	for i := range centroid {
		centroid[i] /= float32(len(group))
	}

	bestIdx := 0
	bestScore := float32(-1)
	for i, c := range group {
		sim := domain.CosineSimilarity(c.Embedding, centroid)
		if sim > bestScore {
			bestScore = sim
			bestIdx = i
		}
	}

	best := group[bestIdx]

	// Create the user
	user := &domain.User{
		Name:       name,
		Embeddings: []domain.FaceEmbedding{best.Embedding},
		Pictures:   []string{best.FaceImage},
		UpdatedAt:  time.Now(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.users.Save(user); err != nil {
		return fmt.Errorf("save user: %w", err)
	}

	// Delete all candidates in the group
	ids := make([]string, 0, len(group))
	for _, c := range group {
		ids = append(ids, c.ID)
	}
	if err := s.candidates.DeleteByID(ids); err != nil {
		return fmt.Errorf("delete candidates: %w", err)
	}

	return nil
}

func (s *FaceServiceImpl) BulkPromoteCandidates(name string, candidateIDs []string) error {
	// Check if user already exists
	exists, err := s.users.Exists(name)
	if err != nil {
		return fmt.Errorf("check user existence: %w", err)
	}
	if exists {
		return fmt.Errorf("user %q already exists", name)
	}

	// Load selected candidates
	allEmbeddings := make([]domain.FaceEmbedding, 0, len(candidateIDs))
	deletedIDs := make([]string, 0, len(candidateIDs))

	for _, id := range candidateIDs {
		c, err := s.candidates.GetByID(id)
		if err != nil {
			continue // skip missing
		}
		allEmbeddings = append(allEmbeddings, c.Embedding)
		deletedIDs = append(deletedIDs, id)
	}

	if len(allEmbeddings) == 0 {
		return fmt.Errorf("no valid candidates found")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	user := &domain.User{
		Name:       name,
		Embeddings: allEmbeddings,
		Pictures:   []string{},
		UpdatedAt:  time.Now(),
	}

	if err := s.users.Save(user); err != nil {
		return fmt.Errorf("save user: %w", err)
	}

	if err := s.candidates.DeleteByID(deletedIDs); err != nil {
		return fmt.Errorf("delete candidates: %w", err)
	}

	return nil
}

// --- Audit ---

func (s *FaceServiceImpl) RecentAudit(n int) ([]domain.AuditEntry, error) {
	return s.audit.Recent(n)
}

func (s *FaceServiceImpl) ListAuditPaginated(opts domain.ListPaginatedOpts) ([]domain.AuditEntry, int, error) {
	return s.audit.ListPaginated(opts)
}

func (s *FaceServiceImpl) ComputeStats() (domain.Stats, error) {
	return s.audit.ComputeStats()
}

// isConnectionError checks if an error is likely a connection issue.
func isConnectionError(err error) bool {
	e := err.Error()
	return len(e) == 0 ||
		bytes.Contains([]byte(e), []byte("connection")) ||
		bytes.Contains([]byte(e), []byte("refused")) ||
		bytes.Contains([]byte(e), []byte("timeout")) ||
		bytes.Contains([]byte(e), []byte("dial"))
}

// isNoFaceError checks if an error indicates no face was detected.
func isNoFaceError(err error) bool {
	e := err.Error()
	return strings.Contains(e, "no face detected") ||
		strings.Contains(e, "No face detected")
}
