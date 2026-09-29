package service

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"h2hsecure.com/face/internal/domain"
)

const maxPicturesPerUser = 3

// FaceServiceImpl implements domain.FaceService using repositories and a face processor.
type FaceServiceImpl struct {
	users     domain.UserRepository
	audit     domain.AuditRepository
	processor domain.FaceProcessor
	rtspReader domain.RTSPReader
	threshold float32
	logger    *zerolog.Logger
	mu        sync.Mutex
}

// New creates a new FaceService.
func New(users domain.UserRepository, audit domain.AuditRepository,
	processor domain.FaceProcessor, threshold float32) *FaceServiceImpl {
	return &FaceServiceImpl{
		users:     users,
		audit:     audit,
		processor: processor,
		threshold: threshold,
	}
}

// WithLogger sets the logger for the service.
func (s *FaceServiceImpl) WithLogger(l *zerolog.Logger) *FaceServiceImpl {
	s.logger = l
	return s
}

// logf writes a structured log line if the logger is set.
func (s *FaceServiceImpl) logf(msg string, keysAndValues ...interface{}) {
	if s.logger == nil {
		return
	}
	s.logger.Info().Fields(toFields(keysAndValues)).Msg(msg)
}

// toFields converts a flat []interface{} pair list into a map[string]interface{}.
func toFields(kv []interface{}) map[string]interface{} {
	f := make(map[string]interface{}, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		f[kv[i].(string)] = kv[i+1]
	}
	return f
}

// WithRTSPReader sets the RTSP reader for stream-based operations.
func (s *FaceServiceImpl) WithRTSPReader(r domain.RTSPReader) *FaceServiceImpl {
	s.rtspReader = r
	return s
}

// --- CheckStream (URL-based, uses injected RTSP reader) ---

func (s *FaceServiceImpl) CheckStream(rtspURL string) (*domain.StreamCheckResult, error) {
	if rtspURL == "" {
		return nil, fmt.Errorf("missing RTSP URL")
	}
	if s.rtspReader == nil {
		return nil, fmt.Errorf("RTSP reader not configured")
	}
	imageData, err := s.rtspReader.ReadFrame(rtspURL, 10*time.Second)
	if err != nil {
		reason := "Failed to connect to RTSP stream"
		if isNoFaceError(err) {
			reason = "No face detected within 3 seconds"
		}
		s.logf("stream check failed", "reason", reason, "timestamp", time.Now().Format(time.RFC3339))
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

	embeddings := []domain.FaceEmbedding{}
	pictures := []string{}
	if u != nil {
		embeddings = u.Embeddings
		pictures = u.Pictures
	}

	updated := &domain.User{
		Name:       name,
		Embeddings: append(embeddings, embedding),
		Pictures:   append(pictures, base64.StdEncoding.EncodeToString(crop.Data)),
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
		FaceImage:  base64.StdEncoding.EncodeToString(crop.Data),
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

	s.logf("face found", "timestamp", time.Now().Format(time.RFC3339))

	embedding := crop.Embedding

	users, err := s.users.ListAll()
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	s.logf("checking users", "user_count", len(users), "timestamp", time.Now().Format(time.RFC3339))

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

	s.logf("validation result", "name", result.Name, "similarity", bestScore, "matched", matched, "timestamp", time.Now().Format(time.RFC3339))

	// Audit entry
	_ = s.audit.Append([]domain.AuditEntry{{
		Time:       time.Now(),
		Endpoint:   "recognize",
		Name:       result.Name,
		Similarity: bestScore,
		Matched:    matched,
		DurationMs: result.DurationMs,
		FaceImage:  base64.StdEncoding.EncodeToString(crop.Data),
		Embedding:  crop.Embedding,
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
		reason := "No face detected"
		// For RTSP connection errors, the caller should handle it.
		// Connection errors do NOT write an audit entry.
		if isConnectionError(err) {
			reason = "Failed to connect to RTSP stream"
			s.logf("no face detected", "reason", reason, "timestamp", time.Now().Format(time.RFC3339))
			return &domain.StreamCheckResult{
				OperationDuration: domain.OperationDuration{DurationMs: time.Since(start).Milliseconds()},
				Status:            "not ok",
				Reason:            reason,
			}, nil
		}
		// No-face: write audit entry with full frame.
		_ = s.audit.Append([]domain.AuditEntry{{
			Time:       time.Now(),
			Endpoint:   "stream-check",
			Name:       "",
			Similarity: 0,
			Matched:    false,
			Status:     domain.AuditStatusNoFace,
			DurationMs: time.Since(start).Milliseconds(),
			FaceImage:  base64.StdEncoding.EncodeToString(imageData),
		}})
		s.logf("no face detected", "timestamp", time.Now().Format(time.RFC3339))
		return &domain.StreamCheckResult{
			OperationDuration: domain.OperationDuration{DurationMs: time.Since(start).Milliseconds()},
			Status:            "not ok",
			Reason:            reason,
		}, nil
	}

	s.logf("face found", "timestamp", time.Now().Format(time.RFC3339))

	embedding := crop.Embedding

	users, err := s.users.ListAll()
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}

	s.logf("checking users", "user_count", len(users), "timestamp", time.Now().Format(time.RFC3339))

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

	s.logf("validation result", "name", bestName, "similarity", highestScore, "matched", matched, "timestamp", time.Now().Format(time.RFC3339))

	if matched {
		// Audit entry for matched face.
		_ = s.audit.Append([]domain.AuditEntry{{
			Time:       time.Now(),
			Endpoint:   "stream-check",
			Name:       bestName,
			Similarity: highestScore,
			Matched:    true,
			Status:     domain.AuditStatusMatched,
			DurationMs: dur.DurationMs,
			FaceImage:  base64.StdEncoding.EncodeToString(crop.Data),
			Embedding:  embedding,
		}})
		return &domain.StreamCheckResult{
			OperationDuration: dur,
			Status:            "ok",
			Name:              bestName,
			Similarity:        highestScore,
			Matched:           true,
			FaceImage:         base64.StdEncoding.EncodeToString(crop.Data),
		}, nil
	}

	// Audit entry for not-matched face (stores embedding for promotion).
	_ = s.audit.Append([]domain.AuditEntry{{
		Time:       time.Now(),
		Endpoint:   "stream-check",
		Name:       "",
		Similarity: highestScore,
		Matched:    false,
		Status:     domain.AuditStatusNotMatched,
		DurationMs: dur.DurationMs,
		FaceImage:  base64.StdEncoding.EncodeToString(crop.Data),
		Embedding:  embedding,
	}})

	return &domain.StreamCheckResult{
		OperationDuration: dur,
		Status:            "not ok",
		Reason:            "Face detected but similarity below threshold",
		Similarity:        highestScore,
		Matched:           false,
		FaceImage:         base64.StdEncoding.EncodeToString(crop.Data),
	}, nil
}

// PromoteFromAudit promotes an audit entry to an enrolled user.
// The auditTime is the RFC3339 timestamp of the audit entry to promote.
func (s *FaceServiceImpl) PromoteFromAudit(auditTime string, name string) error {
	// Check if user already exists
	exists, err := s.users.Exists(name)
	if err != nil {
		return fmt.Errorf("check user existence: %w", err)
	}
	if exists {
		return fmt.Errorf("user %q already exists", name)
	}

	// Load all audit entries and find the one matching the timestamp
	allEntries, err := s.audit.Recent(10000)
	if err != nil {
		return fmt.Errorf("list audit entries: %w", err)
	}

	var entry *domain.AuditEntry
	for i := range allEntries {
		if allEntries[i].Time.Format(time.RFC3339) == auditTime {
			entry = &allEntries[i]
			break
		}
	}
	if entry == nil {
		return fmt.Errorf("audit entry %q not found", auditTime)
	}
	if len(entry.Embedding) == 0 {
		return fmt.Errorf("audit entry has no embedding (old entry, cannot promote)")
	}

	// Create the user from the audit entry
	user := &domain.User{
		Name:       name,
		Embeddings: []domain.FaceEmbedding{entry.Embedding},
		Pictures:   []string{entry.FaceImage},
		UpdatedAt:  time.Now(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.users.Save(user); err != nil {
		return fmt.Errorf("save user: %w", err)
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

	exists, err := s.users.Exists(name)
	if err != nil {
		return fmt.Errorf("check user: %w", err)
	}
	if !exists {
		return fmt.Errorf("user %q not found", name)
	}

	if err := s.users.Delete(name); err != nil {
		return fmt.Errorf("delete user: %w", err)
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

func (s *FaceServiceImpl) ListUnmatched(n int) ([]domain.AuditEntry, error) {
	return s.audit.ListUnmatched(n)
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
