package main

import (
	"encoding/json"
	"fmt"
	"image"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"gocv.io/x/gocv"
)

var bucketName = []byte("Faces")

type FaceServer struct {
	mu        sync.RWMutex
	dbMap     map[string][]float32 // In-memory cache for high-speed lookups
	boltDB    *bolt.DB             // Persistent bbolt datastore
	recNet    gocv.Net             // ArcFace ONNX execution graph
	threshold float32              // Match confidence threshold (MobileFaceNet ~0.45)
}

type RecognitionResult struct {
	Name       string  `json:"name"`
	Similarity float32 `json:"similarity"`
	Matched    bool    `json:"matched"`
}

type StreamCheckResponse struct {
	Status     string  `json:"status"` // "ok" or "not ok"
	Name       string  `json:"name,omitempty"`
	Similarity float32 `json:"similarity,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

func main() {
	runtime.GOMAXPROCS(1)
	// 1. Initialize bbolt Persistent Database
	kvDB, err := bolt.Open("faces.db", 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		log.Fatalf("Failed to open bbolt database: %v", err)
	}
	defer kvDB.Close()

	// Ensure target bucket exists
	err = kvDB.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	})
	if err != nil {
		log.Fatalf("Failed to create bbolt bucket: %v", err)
	}

	// 2. Hydrate In-Memory Cache from Disk
	memoryCache := make(map[string][]float32)
	err = kvDB.View(func(tx *bolt.Tx) error {
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
		log.Fatalf("Failed to hydrate memory cache from bbolt: %v", err)
	}
	log.Printf("Loaded %d enrolled identity(ies) from bbolt datastore.", len(memoryCache))

	// 3. Load ArcFace ONNX Model
	net := gocv.ReadNetFromONNX("arcface_w600k_mbf.onnx")
	if net.Empty() {
		log.Fatal("Error loading ArcFace ONNX model (arcface_w600k_mbf.onnx)")
	}
	defer net.Close()

	net.SetPreferableBackend(gocv.NetBackendDefault)
	net.SetPreferableTarget(gocv.NetTargetCPU)

	server := &FaceServer{
		dbMap:     memoryCache,
		boltDB:    kvDB,
		recNet:    net,
		threshold: 0.45,
	}

	// 4. Register REST Endpoints
	http.HandleFunc("/enroll", server.handleEnroll)
	http.HandleFunc("/recognize", server.handleRecognize)
	http.HandleFunc("/stream-check", server.handleStreamCheck)
	http.HandleFunc("/users", server.handleListUsers)

	log.Println("Face API Server running on http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// Extract 512-dimensional embedding using ArcFace
func (s *FaceServer) extractEmbedding(mat gocv.Mat) ([]float32, error) {
	// ArcFace expects 112x112 RGB input with (x - 127.5) / 127.5 normalization
	blob := gocv.BlobFromImage(mat, 1.0/127.5, image.Pt(112, 112),
		gocv.NewScalar(127.5, 127.5, 127.5, 0), true, false)
	defer blob.Close()

	s.mu.Lock()
	s.recNet.SetInput(blob, "")
	prob := s.recNet.Forward("")
	s.mu.Unlock()
	defer prob.Close()

	if prob.Empty() || prob.Total() == 0 {
		return nil, fmt.Errorf("failed to generate embedding")
	}

	data, err := prob.DataPtrFloat32()
	if err != nil {
		return nil, err
	}

	embedding := make([]float32, len(data))
	copy(embedding, data)
	return embedding, nil
}

// POST /enroll (Form: "name", "image")
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

	embedding, err := s.extractEmbedding(mat)
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

	// Persist to bbolt
	err = s.boltDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketName)
		return b.Put([]byte(name), vecBytes)
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to write to bbolt database"})
		return
	}

	// Update memory cache
	s.mu.Lock()
	s.dbMap[name] = embedding
	s.mu.Unlock()

	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"status": "enrolled", "name": name})
}

// POST /recognize (Form: "image")
func (s *FaceServer) handleRecognize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	log.Printf("request: %v", *r)

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

	queryVec, err := s.extractEmbedding(mat)
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

// POST /stream-check
func (s *FaceServer) handleStreamCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(StreamCheckResponse{Status: "not ok", Reason: "Method not allowed"})
		return
	}

	rtspURL := os.Getenv("RTSP_URL")
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

			queryVec, err := s.extractEmbedding(img)
			img.Close()
			if err != nil {
				continue // Retry on next frame tick
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

// GET /users
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

// Helper: Read multipart HTTP image into gocv.Mat
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

// Helper: Cosine Similarity between vector slices
func cosineSimilarity(a, b []float32) float32 {
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
