package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
	bolt "go.etcd.io/bbolt"
	"gocv.io/x/gocv"
)

var bucketName = []byte("Faces")

// EnsureBucket creates the faces bucket if it does not already exist.
func EnsureBucket(db *bolt.DB) error {
	return db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	})
}

// NewFaceServer creates a FaceServer with the given bbolt database and ONNX
// Runtime sessions. The caller is responsible for closing the runtime, env,
// and sessions.
func NewFaceServer(db *bolt.DB, rt *ort.Runtime, env *ort.Env, detSess, recSess *ort.Session, threshold float32) (*FaceServer, error) {
	memoryCache := make(map[string][]float32)

	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		return b.ForEach(func(k, v []byte) error {
			var vec []float32
			if err := json.Unmarshal(v, &vec); err == nil {
				memoryCache[string(k)] = vec
			}
			return nil
		})
	})
	if err != nil {
		return nil, fmt.Errorf("hydrate cache: %w", err)
	}

	return &FaceServer{
		dbMap:     memoryCache,
		boltDB:    db,
		ortRT:     rt,
		ortEnv:    env,
		detSess:   detSess,
		recSess:   recSess,
		threshold: threshold,
	}, nil
}

// RegisterHandlers attaches all FaceServer HTTP handlers to the given mux.
func (s *FaceServer) RegisterHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/enroll", s.handleEnroll)
	mux.HandleFunc("/recognize", s.handleRecognize)
	mux.HandleFunc("/stream-check", s.handleStreamCheck)
	mux.HandleFunc("/users", s.handleListUsers)
}

// decodeImageFromRequest reads a multipart form field as a file, reads all
// bytes, and decodes them into a gocv.Mat.
func decodeImageFromRequest(r *http.Request, fieldName string) (gocv.Mat, error) {
	file, _, err := r.FormFile(fieldName)
	if err != nil {
		return gocv.Mat{}, fmt.Errorf("invalid image field: %v", err)
	}
	defer file.Close()

	buf, err := io.ReadAll(file)
	if err != nil {
		return gocv.Mat{}, fmt.Errorf("failed to read image file: %v", err)
	}

	mat, err := gocv.IMDecode(buf, gocv.IMReadColor)
	if err != nil || mat.Empty() {
		return gocv.Mat{}, fmt.Errorf("unable to decode image format")
	}
	return mat, nil
}

// --- HTTP Handlers ---

func (s *FaceServer) handleEnroll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

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

	mat, err := decodeImageFromRequest(r, "image")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	defer mat.Close()

	cropped, _, err := s.detectAndCrop112(mat)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Face detection failed: " + err.Error()})
		return
	}
	defer cropped.Close()

	embedding, err := s.extractEmbedding(cropped)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Face processing failed"})
		return
	}

	vecBytes, err := json.Marshal(embedding)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to serialize embedding"})
		return
	}

	err = s.boltDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		return b.Put([]byte(name), vecBytes)
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to write to bbolt database"})
		return
	}

	s.mu.Lock()
	s.dbMap[name] = embedding
	s.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "enrolled", "name": name})
}

func (s *FaceServer) handleRecognize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method not allowed"})
		return
	}

	mat, err := decodeImageFromRequest(r, "image")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	defer mat.Close()

	cropped, _, err := s.detectAndCrop112(mat)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Face detection failed: " + err.Error()})
		return
	}
	defer cropped.Close()

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

	for name, knownVec := range s.dbMap {
		score := cosineSimilarity(queryVec, knownVec)
		if score > maxScore {
			maxScore = score
			bestMatch = name
		}
	}

	matched := maxScore >= s.threshold
	result := RecognitionResult{
		Name:       "Unknown",
		Similarity: maxScore,
		Matched:    matched,
	}
	if matched {
		result.Name = bestMatch
	}

	json.NewEncoder(w).Encode(result)
}

func (s *FaceServer) handleStreamCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(StreamCheckResponse{Status: "not ok", Reason: "Method not allowed"})
		return
	}

	rtspURL := r.FormValue("rtsp_url")
	if rtspURL == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(StreamCheckResponse{Status: "not ok", Reason: "Missing 'rtsp_url' field"})
		return
	}

	capture, err := gocv.OpenVideoCapture(rtspURL)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(StreamCheckResponse{Status: "not ok", Reason: "Failed to connect to RTSP stream"})
		return
	}
	defer capture.Close()

	timeout := time.After(3 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var bestMatch string
	var highestScore float32 = -1.0

	for {
		select {
		case <-timeout:
			reason := "No face detected within 3 seconds"
			if highestScore > -1.0 {
				reason = fmt.Sprintf("Unrecognized face detected (highest score: %.2f)", highestScore)
			}
			json.NewEncoder(w).Encode(StreamCheckResponse{
				Status:     "not ok",
				Reason:     reason,
				Similarity: highestScore,
			})
			return

		case <-ticker.C:
			img := gocv.NewMat()
			if !capture.Read(&img) || img.Empty() {
				img.Close()
				continue
			}

			cropped, _, err := s.detectAndCrop112(img)
			img.Close()
			if err != nil {
				cropped.Close()
				continue
			}

			queryVec, err := s.extractEmbedding(cropped)
			cropped.Close()
			if err != nil {
				continue
			}

			s.mu.RLock()
			for name, knownVec := range s.dbMap {
				score := cosineSimilarity(queryVec, knownVec)
				if score > highestScore {
					highestScore = score
					bestMatch = name
				}
			}
			s.mu.RUnlock()

			if highestScore >= s.threshold {
				json.NewEncoder(w).Encode(StreamCheckResponse{
					Status:     "ok",
					Name:       bestMatch,
					Similarity: highestScore,
				})
				return
			}
		}
	}
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
