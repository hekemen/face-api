package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
	bolt "go.etcd.io/bbolt"

	"h2hsecure.com/face/internal/domain"
	"h2hsecure.com/face/internal/di"
	"h2hsecure.com/face/internal/mqtt"
	"h2hsecure.com/face/internal/server"
)

const (
	detModelPath = "models/scrfd_500m.onnx"
	recModelPath = "models/arcface_w600k_mbf.onnx"
)

func main() {
	logger := zerolog.New(os.Stdout).With().Timestamp().Logger()

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

	// 2. Load ONNX Runtime and both models
	rt, err := ort.NewRuntime("", 23)
	if err != nil {
		log.Fatalf("Failed to load libonnxruntime.so: %v", err)
	}
	defer rt.Close()

	ortEnv, err := rt.NewEnv("face-api", ort.LoggingLevelError)
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

	enableUI := os.Getenv("ENABLE_UI") == "true"

	// Similarity threshold for face matching (default: 0.45)
	threshold := float32(0.45)
	if t := os.Getenv("THRESHOLD"); t != "" {
		if v, err := strconv.ParseFloat(t, 32); err == nil && v >= 0 && v <= 1 {
			threshold = float32(v)
		}
	}

	// 3. Wire up the hexagonal architecture
	faceAPI, err := di.NewFaceAPI(di.Config{
		DB:         kvDB,
		Logger:     logger,
		RT:         rt,
		ORTEnv:     ortEnv,
		DetSession: detSess,
		RecSession: recSess,
		RTSPURL:    os.Getenv("RTSP_URL"),
		EnableUI:   enableUI,
		Threshold:  threshold,
	})
	if err != nil {
		log.Fatalf("Failed to wire face API: %v", err)
	}

	// 4. Set up MQTT bridge if configured
	var mqttBridge *mqtt.Bridge
	if brokerURL := os.Getenv("MQTT_BROKER_URL"); brokerURL != "" {
		queueDepth := 16
		if qd := os.Getenv("MQTT_QUEUE_DEPTH"); qd != "" {
			if d, err := strconv.Atoi(qd); err == nil && d > 0 {
				queueDepth = d
			}
		}
		mqttCfg := mqtt.Config{
			BrokerURL:       brokerURL,
			Username:        os.Getenv("MQTT_USERNAME"),
			Password:        os.Getenv("MQTT_PASSWORD"),
			ClientID:        os.Getenv("MQTT_CLIENT_ID"),
			BaseTopic:       os.Getenv("MQTT_BASE_TOPIC"),
			DeviceName:      os.Getenv("MQTT_DEVICE_NAME"),
			QueueDepth:      queueDepth,
			DiscoveryPrefix: "homeassistant",
		}

		// Create a check function that uses the old server for MQTT stream checks (transition)
		// TODO: Replace with DI-wired service CheckStream
		checkFunc := func(rtspURL string) (*domain.StreamCheckResult, error) {
			srv, err := server.NewFaceServer(
				kvDB, logger, rt, ortEnv, detSess, recSess, threshold,
				rtspURL, enableUI,
			)
			if err != nil {
				return nil, err
			}
			resp, err := srv.RunStreamCheck(rtspURL)
			if err != nil {
				return nil, err
			}
			return &domain.StreamCheckResult{
				OperationDuration: domain.OperationDuration{DurationMs: resp.DurationMs},
				Status:            resp.Status,
				Name:              resp.Name,
				Similarity:        resp.Similarity,
				Reason:            resp.Reason,
				Matched:           resp.Matched,
				FaceImage:         resp.FaceImage,
			}, nil
		}

		mqttBridge, err = mqtt.New(mqttCfg, checkFunc, nil, nil)
		if err != nil {
			logger.Error().Err(err).Msg("failed to create MQTT bridge")
		}
	}

	logger.Info().Msg("Face API Server running on http://localhost:8081")

	httpServer := &http.Server{Addr: ":8081", Handler: faceAPI.Handler}

	// Start MQTT bridge if configured.
	if mqttBridge != nil {
		go func() {
			if err := mqttBridge.Run(context.Background()); err != nil {
				logger.Error().Err(err).Msg("MQTT bridge error")
			}
		}()
	}

	// Start HTTP server in a goroutine.
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- httpServer.ListenAndServe()
	}()

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	logger.Info().Str("signal", sig.String()).Msg("shutting down")

	// Stop MQTT bridge first.
	if mqttBridge != nil {
		mqttBridge.Stop()
	}

	// Stop HTTP server with timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpServer.Shutdown(ctx)

	select {
	case err := <-serverErr:
		if err != nil && err != http.ErrServerClosed {
			logger.Error().Err(err).Msg("HTTP server error")
		}
	default:
	}
}
