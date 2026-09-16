package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
	bolt "go.etcd.io/bbolt"
)

var (
	bucketName      = []byte("Faces")
	auditBucketName = []byte("Audit")
)

// EnsureBucket creates the faces and audit buckets if they do not exist.
func EnsureBucket(db *bolt.DB) error {
	return db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(bucketName); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(auditBucketName)
		return err
	})
}

// unmarshalEmbeddings parses a stored value into a list of face embeddings.
// It accepts both the legacy flat 512-float JSON array (a single embedding)
// and the current nested [[...],[...]] format.
func unmarshalEmbeddings(v []byte) ([][]float32, error) {
	var nested [][]float32
	if !bytes.HasPrefix(v, []byte("[[")) {
		var flat []float32
		if err := json.Unmarshal(v, &flat); err != nil {
			return nil, err
		}
		return [][]float32{flat}, nil
	}
	if err := json.Unmarshal(v, &nested); err != nil {
		return nil, err
	}
	return nested, nil
}

// marshalEmbeddings serializes a list of face embeddings for storage.
func marshalEmbeddings(embeddings [][]float32) []byte {
	b, _ := json.Marshal(embeddings)
	return b
}

// storeAudit persists scan audit entries in a single bbolt transaction.
// Batching in one Update (rather than one fsync per frame) keeps stream-check
// writes cheap under the 100ms sampling loop.
func (s *FaceServer) storeAudit(entries []AuditEntry) error {
	return s.boltDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(auditBucketName)
		for _, e := range entries {
			seq, err := b.NextSequence()
			if err != nil {
				return err
			}
			key := make([]byte, 8)
			binary.BigEndian.PutUint64(key, seq)
			val, err := json.Marshal(e)
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

// readAudit returns up to limit audit entries, newest first.
func (s *FaceServer) readAudit(limit int) ([]AuditEntry, error) {
	res := make([]AuditEntry, 0, limit)
	err := s.boltDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(auditBucketName)
		c := b.Cursor()
		for k, v := c.Last(); k != nil && len(res) < limit; k, v = c.Prev() {
			var e AuditEntry
			if err := json.Unmarshal(v, &e); err != nil {
				continue
			}
			res = append(res, e)
		}
		return nil
	})
	return res, err
}

// NewFaceServer creates a FaceServer with the given bbolt database, logger,
// and ONNX Runtime sessions. The caller is responsible for closing the
// runtime, env, and sessions. rtspURL is the default RTSP stream URL used by
// stream-check when the request does not provide one.
func NewFaceServer(db *bolt.DB, log zerolog.Logger, rt *ort.Runtime, env *ort.Env, detSess, recSess *ort.Session, threshold float32, rtspURL string, enableUI bool) (*FaceServer, error) {
	memoryCache := make(map[string][][]float32)

	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		return b.ForEach(func(k, v []byte) error {
			embeddings, err := unmarshalEmbeddings(v)
			if err != nil {
				return err
			}
			memoryCache[string(k)] = embeddings
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("hydrate cache: %w", err)
	}

	return &FaceServer{
		dbMap:     memoryCache,
		boltDB:    db,
		log:       log,
		ortRT:     rt,
		ortEnv:    env,
		detSess:   detSess,
		recSess:   recSess,
		threshold: threshold,
		rtspURL:   rtspURL,
		enableUI:  enableUI,
	}, nil
}

// RegisterHandlers attaches all FaceServer HTTP handlers to the given mux.
func (s *FaceServer) RegisterHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/enroll", s.handleEnroll)
	mux.HandleFunc("/recognize", s.handleRecognize)
	mux.HandleFunc("/stream-check", s.handleStreamCheck)
	mux.HandleFunc("/users", s.handleListUsers)
	mux.HandleFunc("/audit", s.handleListAudit)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)

	if s.enableUI {
		s.RegisterUIHandlers(mux)
	}
}

// statusRecorder captures the response status code written by a handler so
// request logging can report it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// RequestLogging wraps a handler and emits one JSON zerolog line per request
// with method, path, status, remote address, and wall-clock duration.
// Health and readiness probes (/healthz, /readyz) are skipped so they do not
// pollute the request log stream.
func (s *FaceServer) RequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info().
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", rec.status).
			Str("remote", r.RemoteAddr).
			Int64("duration_ms", time.Since(start).Milliseconds()).
			Msg("request")
	})
}

// handleHealthz reports liveness. The process is alive if this handler runs.
func (s *FaceServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// handleReadyz reports readiness: it returns 200 only when the bbolt
// database is open and the in-memory cache is hydrated.
func (s *FaceServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	s.mu.RLock()
	dbOpen := s.boltDB != nil
	s.mu.RUnlock()

	if !dbOpen {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"status": "unavailable"})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// decodeImageFromRequest reads a multipart form field as a file, reads all
// bytes, and decodes them into an rgbImage.
func decodeImageFromRequest(r *http.Request, fieldName string) (*rgbImage, error) {
	file, _, err := r.FormFile(fieldName)
	if err != nil {
		return nil, fmt.Errorf("invalid image field: %v", err)
	}
	defer file.Close()

	buf, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read image file: %v", err)
	}

	return decodeRGB(buf)
}

// --- HTTP Handlers ---

func (s *FaceServer) handleEnroll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	start := time.Now()

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	name := r.FormValue("name")
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Missing 'name' field"})
		return
	}

	img, err := decodeImageFromRequest(r, "image")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	cropped, _, err := s.detectAndCrop112(img)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Face detection failed: " + err.Error()})
		return
	}

	faceImage, imgErr := encodeFaceToBase64(cropped)
	if imgErr != nil {
		s.log.Error().Err(imgErr).Str("name", name).Msg("failed to encode face image")
	}

	embedding, err := s.extractEmbedding(cropped)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Face processing failed"})
		return
	}

	s.mu.Lock()
	existing := s.dbMap[name]
	if len(existing) >= 3 {
		s.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Maximum 3 pictures per user"})
		return
	}
	updated := append(existing, embedding)

	err = s.boltDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		return b.Put([]byte(name), marshalEmbeddings(updated))
	})
	if err != nil {
		s.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to write to bbolt database"})
		return
	}

	s.dbMap[name] = updated
	s.mu.Unlock()

	if err := s.storeAudit([]AuditEntry{{
		Time:       time.Now(),
		Endpoint:   "enroll",
		Name:       name,
		Similarity: 1.0,
		Matched:    true,
		DurationMs: time.Since(start).Milliseconds(),
		FaceImage:  faceImage,
	}}); err != nil {
		s.log.Error().Err(err).Str("name", name).Msg("failed to write enroll audit entry")
	}

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(EnrolledResponse{
		OperationDuration: OperationDuration{DurationMs: time.Since(start).Milliseconds()},
		Status:            "enrolled",
		Name:              name,
	})
}

func (s *FaceServer) handleRecognize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	start := time.Now()

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	img, err := decodeImageFromRequest(r, "image")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	cropped, _, err := s.detectAndCrop112(img)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Face detection failed: " + err.Error()})
		return
	}

	faceImage, imgErr := encodeFaceToBase64(cropped)
	if imgErr != nil {
		s.log.Error().Err(imgErr).Msg("failed to encode face image")
	}

	queryVec, err := s.extractEmbedding(cropped)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Face processing failed"})
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var bestMatch string
	var maxScore float32 = -1.0

	for name, knownVecs := range s.dbMap {
		score := bestEmbeddingScore(queryVec, knownVecs)
		if score > maxScore {
			maxScore = score
			bestMatch = name
		}
	}

	matched := maxScore >= s.threshold
	result := RecognitionResult{
		OperationDuration: OperationDuration{DurationMs: time.Since(start).Milliseconds()},
		Name:              "Unknown",
		Similarity:        maxScore,
		Matched:           matched,
	}
	if matched {
		result.Name = bestMatch
	}

	if err := s.storeAudit([]AuditEntry{{
		Time:       time.Now(),
		Endpoint:   "recognize",
		Name:       result.Name,
		Similarity: maxScore,
		Matched:    matched,
		DurationMs: result.DurationMs,
		FaceImage:  faceImage,
	}}); err != nil {
		s.log.Error().Err(err).Msg("failed to write recognize audit entry")
	}

	json.NewEncoder(w).Encode(result)
}

func (s *FaceServer) handleStreamCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	start := time.Now()

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(StreamCheckResponse{Status: "not ok", Reason: "Method not allowed"})
		return
	}

	rtspURL := r.FormValue("rtsp_url")
	if rtspURL == "" {
		rtspURL = s.rtspURL
	}
	if rtspURL == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(StreamCheckResponse{Status: "not ok", Reason: "Missing 'rtsp_url' field"})
		return
	}

	img, err := readRTSPFrame(rtspURL, 3*time.Second)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(StreamCheckResponse{
			Status: "not ok",
			Reason: "Failed to connect to RTSP stream: " + err.Error(),
		})
		return
	}

	cropped, _, err := s.detectAndCrop112(img)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(StreamCheckResponse{
			Status: "not ok",
			Reason: "No face detected within 3 seconds",
		})
		return
	}

	queryVec, err := s.extractEmbedding(cropped)
	faceImage, imgErr := encodeFaceToBase64(cropped)
	if imgErr != nil {
		s.log.Error().Err(imgErr).Msg("failed to encode face image")
	}
	if err != nil {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(StreamCheckResponse{
			Status: "not ok",
			Reason: "No face detected within 3 seconds",
		})
		return
	}

	s.mu.RLock()
	var bestMatch string
	var highestScore float32 = -1.0
	for name, knownVecs := range s.dbMap {
		score := bestEmbeddingScore(queryVec, knownVecs)
		if score > highestScore {
			highestScore = score
			bestMatch = name
		}
	}
	s.mu.RUnlock()

	dur := OperationDuration{DurationMs: time.Since(start).Milliseconds()}

	auditEntry := AuditEntry{
		Time:       time.Now(),
		Endpoint:   "stream-check",
		Name:       bestMatch,
		Similarity: highestScore,
		Matched:    highestScore >= s.threshold,
		DurationMs: dur.DurationMs,
		FaceImage:  faceImage,
	}
	if highestScore >= s.threshold {
		if err := s.storeAudit([]AuditEntry{auditEntry}); err != nil {
			s.log.Error().Err(err).Msg("failed to write stream-check audit entry")
		}
		json.NewEncoder(w).Encode(StreamCheckResponse{
			OperationDuration: dur,
			Status:            "ok",
			Name:              bestMatch,
			Similarity:        highestScore,
		})
		return
	}

	if err := s.storeAudit([]AuditEntry{auditEntry}); err != nil {
		s.log.Error().Err(err).Msg("failed to write stream-check audit entry")
	}
	reason := "No face detected within 3 seconds"
	json.NewEncoder(w).Encode(StreamCheckResponse{
		OperationDuration: dur,
		Status:            "not ok",
		Reason:            reason,
		Similarity:        highestScore,
	})
}

func (s *FaceServer) handleListUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	s.mu.RLock()
	defer s.mu.RUnlock()

	users := make([]string, 0, len(s.dbMap))
	for name := range s.dbMap {
		users = append(users, name)
	}

	json.NewEncoder(w).Encode(map[string][]string{"users": users})
}

// handleListAudit returns the latest audit entries (face scans), newest first.
func (s *FaceServer) handleListAudit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	start := time.Now()

	entries, err := s.readAudit(100)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to read audit log"})
		return
	}

	json.NewEncoder(w).Encode(AuditListResponse{
		OperationDuration: OperationDuration{DurationMs: time.Since(start).Milliseconds()},
		Count:             len(entries),
		Entries:           entries,
	})
}
