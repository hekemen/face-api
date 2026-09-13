package main

import (
	"log"
	"net/http"
	"os"
	"time"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
	bolt "go.etcd.io/bbolt"

	"h2hsecure.com/face/internal/server"
)

const (
	detModelPath = "models/scrfd_500m.onnx"
	recModelPath = "models/arcface_w600k_mbf.onnx"
)

func main() {
	// 1. Initialize bbolt Persistent Database
	dbPath := os.Getenv("FACES_DB_PATH")
	if dbPath == "" {
		dbPath = "faces.db"
	}
	kvDB, err := bolt.Open(dbPath, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		log.Fatalf("Failed to open bbolt database (%s): %v", dbPath, err)
	}
	defer kvDB.Close()

	if err := server.EnsureBucket(kvDB); err != nil {
		log.Fatalf("Failed to create bbolt bucket: %v", err)
	}

	// 2. Load ONNX Runtime and both models
	rt, err := ort.NewRuntime("", 23)
	if err != nil {
		log.Fatalf("Failed to load libonnxruntime.so: %v", err)
	}
	defer rt.Close()

	ortEnv, err := rt.NewEnv("face-api", ort.LoggingLevelWarning)
	if err != nil {
		log.Fatalf("Failed to create ONNX Runtime environment: %v", err)
	}
	defer ortEnv.Close()

	sessOpts := &ort.SessionOptions{IntraOpNumThreads: 1}

	detFile, err := os.Open(detModelPath)
	if err != nil {
		log.Fatalf("Failed to open SCRFD ONNX model: %v", err)
	}
	detSess, err := rt.NewSessionFromReader(ortEnv, detFile, sessOpts)
	detFile.Close()
	if err != nil {
		log.Fatalf("Error loading SCRFD ONNX model (%s): %v", detModelPath, err)
	}
	defer detSess.Close()

	recFile, err := os.Open(recModelPath)
	if err != nil {
		log.Fatalf("Failed to open ArcFace ONNX model: %v", err)
	}
	recSess, err := rt.NewSessionFromReader(ortEnv, recFile, sessOpts)
	recFile.Close()
	if err != nil {
		log.Fatalf("Error loading ArcFace ONNX model (%s): %v", recModelPath, err)
	}
	defer recSess.Close()

	// 3. Create server and register handlers
	srv, err := server.NewFaceServer(kvDB, rt, ortEnv, detSess, recSess, 0.45)
	if err != nil {
		log.Fatalf("Failed to create face server: %v", err)
	}

	mux := http.NewServeMux()
	srv.RegisterHandlers(mux)

	log.Println("Face API Server running on http://localhost:8081")
	log.Fatal(http.ListenAndServe(":8081", mux))
}
