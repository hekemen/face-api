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

// FaceServiceImpl implements domain.FaceService using repositories and a face processor.
type FaceServiceImpl struct {
	users      domain.UserRepository
	audit      domain.AuditRepository
	processor  domain.FaceProcessor
	rtspReader domain.RTSPReader
	settings   domain.SettingsRepository
	threshold  float32
	logger     *zerolog.Logger
	mu         sync.Mutex
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

// WithSettingsRepo sets the settings repository and applies any persisted
// threshold override (a valid stored value wins over the constructor value).
func (s *FaceServiceImpl) WithSettingsRepo(repo domain.SettingsRepository) *FaceServiceImpl {
	s.settings = repo
	if st, err := repo.Get(); err == nil && st != nil && st.Threshold > 0 && st.Threshold < 1 {
		s.threshold = st.Threshold
	}
	return s
}

// GetSettings returns the current runtime settings (live values).
func (s *FaceServiceImpl) GetSettings() (*domain.Settings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &domain.Settings{Threshold: s.threshold}, nil
}

// SetThreshold updates the face-match threshold. The value must be in (0, 1).
// It is persisted before the in-memory value is updated.
func (s *FaceServiceImpl) SetThreshold(v float32) error {
	if v <= 0 || v >= 1 {
		return fmt.Errorf("threshold must be between 0 and 1")
	}
	if s.settings == nil {
		return fmt.Errorf("settings repository not configured")
	}
	if err := s.settings.Save(&domain.Settings{Threshold: v}); err != nil {
		return err
	}
	s.mu.Lock()
	s.threshold = v
	s.mu.Unlock()
	return nil
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
	if u != nil && len(u.Embeddings) >= domain.MaxPicturesPerUser {
		return fmt.Errorf("maximum %d pictures per user", domain.MaxPicturesPerUser)
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
	faceB64 := base64.StdEncoding.EncodeToString(crop.Data)
	err = s.audit.Append([]domain.AuditEntry{{
		Time:       time.Now(),
		Endpoint:   "enroll",
		Name:       name,
		Similarity: 1.0,
		Matched:    true,
		DurationMs: 0,
		FaceImage:  faceB64,
		FaceThumb:  thumbnailForEntry(faceB64),
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
	faceB64 := base64.StdEncoding.EncodeToString(crop.Data)
	_ = s.audit.Append([]domain.AuditEntry{{
		Time:       time.Now(),
		Endpoint:   "recognize",
		Name:       result.Name,
		Similarity: bestScore,
		Matched:    matched,
		DurationMs: result.DurationMs,
		FaceImage:  faceB64,
		FaceThumb:  thumbnailForEntry(faceB64),
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
		// No-face: store the captured camera frame (downscaled) so the audit
		// entry can show what the camera saw. HasFace stays false because the
		// status is no_face, so the listing keeps the "View frame" button.
		frameB64 := noFaceFrameImage(imageData)
		_ = s.audit.Append([]domain.AuditEntry{{
			Time:       time.Now(),
			Endpoint:   "stream-check",
			Name:       "",
			Similarity: 0,
			Matched:    false,
			Status:     domain.AuditStatusNoFace,
			DurationMs: time.Since(start).Milliseconds(),
			FaceImage:  frameB64,
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
		faceB64 := base64.StdEncoding.EncodeToString(crop.Data)
		_ = s.audit.Append([]domain.AuditEntry{{
			Time:       time.Now(),
			Endpoint:   "stream-check",
			Name:       bestName,
			Similarity: highestScore,
			Matched:    true,
			Status:     domain.AuditStatusMatched,
			DurationMs: dur.DurationMs,
			FaceImage:  faceB64,
			FaceThumb:  thumbnailForEntry(faceB64),
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
	faceB64 := base64.StdEncoding.EncodeToString(crop.Data)
	_ = s.audit.Append([]domain.AuditEntry{{
		Time:       time.Now(),
		Endpoint:   "stream-check",
		Name:       "",
		Similarity: highestScore,
		Matched:    false,
		Status:     domain.AuditStatusNotMatched,
		DurationMs: dur.DurationMs,
		FaceImage:  faceB64,
		FaceThumb:  thumbnailForEntry(faceB64),
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
	// Load all audit entries and find the one matching the timestamp
	allEntries, err := s.audit.Recent(10000)
	if err != nil {
		s.logf("promote: failed to list audit entries", "name", name, "audit_time", auditTime)
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
		s.logf("promote: audit entry not found", "name", name, "audit_time", auditTime)
		return fmt.Errorf("audit entry %q not found", auditTime)
	}
	if len(entry.Embedding) == 0 {
		s.logf("promote: audit entry has no embedding", "name", name, "audit_time", auditTime)
		return fmt.Errorf("audit entry has no embedding (old entry, cannot promote)")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.logf("promote: starting", "name", name, "audit_time", auditTime)

	u, err := s.users.GetByName(name)
	if err != nil && err.Error() != fmt.Sprintf("user %q not found", name) {
		s.logf("promote: failed to get user", "name", name, "error", err)
		return fmt.Errorf("get user: %w", err)
	}

	if u != nil {
		// User exists, append to them
		if len(u.Embeddings) >= domain.MaxPicturesPerUser {
			s.logf("promote: max pictures reached", "name", name, "pictures", len(u.Embeddings))
			return fmt.Errorf("maximum %d pictures per user", domain.MaxPicturesPerUser)
		}
		u.Embeddings = append(u.Embeddings, entry.Embedding)
		u.Pictures = append(u.Pictures, entry.FaceImage)
		u.UpdatedAt = time.Now()
		if err := s.users.Save(u); err != nil {
			s.logf("promote: failed to save user (update)", "name", name, "error", err)
			return fmt.Errorf("save user: %w", err)
		}
		s.logf("promote: updated existing user", "name", name, "total_pictures", len(u.Pictures))
	} else {
		// Create the user from the audit entry
		user := &domain.User{
			Name:       name,
			Embeddings: []domain.FaceEmbedding{entry.Embedding},
			Pictures:   []string{entry.FaceImage},
			UpdatedAt:  time.Now(),
		}

		if err := s.users.Save(user); err != nil {
			s.logf("promote: failed to save user (create)", "name", name, "error", err)
			return fmt.Errorf("save user: %w", err)
		}
		s.logf("promote: created new user", "name", name)
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

func (s *FaceServiceImpl) DeletePicture(name string, index int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	u, err := s.users.GetByName(name)
	if err != nil {
		return fmt.Errorf("check user: %w", err)
	}
	if index < 0 || index >= len(u.Embeddings) {
		return fmt.Errorf("picture index %d out of range (user has %d pictures)", index, len(u.Embeddings))
	}

	// Embeddings and Pictures are index-aligned; remove both at `index`.
	embeddings := make([]domain.FaceEmbedding, 0, len(u.Embeddings)-1)
	embeddings = append(embeddings, u.Embeddings[:index]...)
	embeddings = append(embeddings, u.Embeddings[index+1:]...)
	pictures := make([]string, 0, len(u.Pictures)-1)
	pictures = append(pictures, u.Pictures[:index]...)
	pictures = append(pictures, u.Pictures[index+1:]...)

	updated := &domain.User{
		Name:       u.Name,
		Embeddings: embeddings,
		Pictures:   pictures,
		UpdatedAt:  time.Now(),
	}
	if err := s.users.Save(updated); err != nil {
		return fmt.Errorf("save user: %w", err)
	}
	return nil
}

// --- Audit ---

func (s *FaceServiceImpl) RecentAudit(n int) ([]domain.AuditEntry, error) {
	entries, err := s.audit.Recent(n)
	if err != nil {
		return nil, err
	}
	applyThumbnails(entries, thumbMaxDim)
	return entries, nil
}

func (s *FaceServiceImpl) ListAuditPaginated(opts domain.ListPaginatedOpts) ([]domain.AuditEntry, int, []string, error) {
	entries, total, users, err := s.audit.ListPaginated(opts)
	if err != nil {
		return nil, 0, users, err
	}
	applyThumbnails(entries, thumbMaxDim)
	return entries, total, users, nil
}

func (s *FaceServiceImpl) ComputeStats() (domain.Stats, error) {
	return s.audit.ComputeStats()
}

func (s *FaceServiceImpl) GetAuditFace(timestamp string) (string, error) {
	return s.audit.GetAuditFace(timestamp)
}

func (s *FaceServiceImpl) ListUnmatched(n int) ([]domain.AuditEntry, error) {
	entries, err := s.audit.ListUnmatched(n)
	if err != nil {
		return nil, err
	}
	applyThumbnails(entries, thumbMaxDim)
	return entries, nil
}

// BackfillAuditThumbnails generates a stored thumbnail for audit entries
// whose FaceImage is large but has no FaceThumb (legacy data written before
// the thumbnail field existed). It is idempotent: entries that already have
// a thumb, or with a small/missing face image, are skipped. Returns the
// number of entries updated.
func (s *FaceServiceImpl) BackfillAuditThumbnails() (int, error) {
	entries, err := s.audit.Recent(100000)
	if err != nil {
		return 0, fmt.Errorf("list audit entries: %w", err)
	}
	updated := 0
	for i := range entries {
		e := &entries[i]
		if len(e.FaceImage) <= thumbnailSizeThreshold || e.FaceThumb != "" {
			continue
		}
		thumb := thumbnailForEntry(e.FaceImage)
		if thumb == "" {
			continue
		}
		if err := s.audit.SetFaceThumb(e.Time, e.FaceImage, thumb); err != nil {
			s.logf("backfill: set face thumb failed", "name", e.Name, "time", e.Time.Format(time.RFC3339), "error", err)
			continue
		}
		updated++
	}
	return updated, nil
}

func (s *FaceServiceImpl) ListRecognizeEntries(opts domain.ListRecognizeEntriesOpts) ([]domain.AuditEntry, int, error) {
	return s.audit.ListRecognizeEntries(opts)
}

func (s *FaceServiceImpl) ComputeRecognizeStats() (domain.RecognizeSummaryResponse, error) {
	summary, err := s.audit.ComputeRecognizeSummary()
	if err != nil {
		return domain.RecognizeSummaryResponse{}, err
	}
	return domain.RecognizeSummaryResponse{
		TotalRecognize:  summary.TotalRecognize,
		TotalMatched:    summary.TotalMatched,
		TotalNotMatched: summary.TotalNotMatched,
	}, nil
}

// noFaceFrameMaxDim bounds the longest side of the camera frame stored in
// no-face audit entries, keeping per-entry storage bounded while remaining
// viewable in the audit popup.
const noFaceFrameMaxDim = 640

// noFaceFrameImage downscales the captured camera frame so its longest side
// is at most noFaceFrameMaxDim and returns it as a base64 JPEG. It returns
// "" if the frame cannot be decoded or encoded (graceful degradation — the
// audit entry is still written, just without a frame).
func noFaceFrameImage(imageData []byte) string {
	img, err := decodeRGB(imageData)
	if err != nil {
		return ""
	}
	longest := img.w
	if img.h > longest {
		longest = img.h
	}
	if longest > noFaceFrameMaxDim {
		scale := float64(noFaceFrameMaxDim) / float64(longest)
		img = bilinearResize(img, round(float64(img.w)*scale), round(float64(img.h)*scale))
	}
	data, err := encodeFaceJPEG(img)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(data)
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
