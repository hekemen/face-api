package di

import (
	"net/http"

	"github.com/rs/zerolog"
	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
	bolt "go.etcd.io/bbolt"

	fhttp "h2hsecure.com/face/internal/http"
	"h2hsecure.com/face/internal/repository"
	"h2hsecure.com/face/internal/rtsp"
	"h2hsecure.com/face/internal/service"
	"h2hsecure.com/face/internal/domain"
)

// Config holds the configuration needed to wire the face API.
type Config struct {
	DB           *bolt.DB
	Logger       zerolog.Logger
	RT           *ort.Runtime
	ORTEnv       *ort.Env
	DetSession   *ort.Session
	RecSession   *ort.Session
	RTSPURL      string
	EnableUI     bool
	Threshold    float32
}

// FaceAPI is the fully wired-up face API. Callers only interact with the
// http.Handler; other fields are exposed for MQTT integration.
type FaceAPI struct {
	Handler   http.Handler
	MQTTReady bool
	// CheckStream is the stream-check function used by the MQTT bridge.
	CheckStream func(rtspURL string) (*domain.StreamCheckResult, error)
}

func NewFaceAPI(cfg Config) (*FaceAPI, error) {
	if err := EnsureBuckets(cfg.DB); err != nil {
		return nil, err
	}

	userRepo := repository.NewUserRepository(cfg.DB)
	candidateRepo := repository.NewCandidateRepository(cfg.DB)
	auditRepo := repository.NewAuditRepository(cfg.DB)

	processor := service.NewFaceProcessor(cfg.RT, cfg.ORTEnv, cfg.DetSession, cfg.RecSession)
	faceService := service.New(userRepo, candidateRepo, auditRepo, processor, cfg.Threshold)
	rtspReader := rtsp.New()

	handlers := fhttp.NewHandlers(
		faceService,
		cfg.RTSPURL,
		rtspReader,
		cfg.EnableUI,
		cfg.Logger,
	)

	// Inject RTSP reader into service for stream-based operations
	faceService.WithRTSPReader(rtspReader)

	mux := http.NewServeMux()
	handlers.RegisterHandlers(mux)

	// Register UI handlers (if enabled).
	if cfg.EnableUI {
		uiHandler := fhttp.NewUIHandler(faceService, cfg.EnableUI)
		uiHandler.RegisterUIHandlers(mux)
		// Make the API mux available for in-process UI API proxying.
		fhttp.SetAPIMux(mux)
	}

	// Wrap with request logging middleware (skips /healthz and /readyz).
	handler := fhttp.RequestLogging(mux, cfg.Logger)

	return &FaceAPI{
		Handler:     handler,
		MQTTReady:   true,
		CheckStream: faceService.CheckStream,
	}, nil
}

// EnsureBuckets creates the required bbolt buckets in the given database.
func EnsureBuckets(db *bolt.DB) error {
	return db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"Faces", "Audit", "Candidates"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	})
}
