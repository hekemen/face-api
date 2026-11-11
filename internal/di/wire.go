package di

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
	bolt "go.etcd.io/bbolt"

	"h2hsecure.com/face/internal/config"
	"h2hsecure.com/face/internal/domain"
	"h2hsecure.com/face/internal/framesource"
	fhttp "h2hsecure.com/face/internal/http"
	"h2hsecure.com/face/internal/repository"
	"h2hsecure.com/face/internal/rtsp"
	"h2hsecure.com/face/internal/service"
)

// Config holds the configuration needed to wire the face API.
type Config struct {
	DB             *bolt.DB
	Logger         zerolog.Logger
	RT             *ort.Runtime
	ORTEnv         *ort.Env
	DetSession     *ort.Session
	RecSession     *ort.Session
	RTSPURL        string
	EnableUI       bool
	BasePath       string
	Threshold      float32
	AuthToken      string
	MaxUploadBytes int64

	// Live burst pipeline knobs.
	Alignment      service.AlignmentMode
	BurstWindow    time.Duration
	SampleInterval time.Duration
	IntraOpThreads int
}

// FaceAPI is the fully wired-up face API. Callers only interact with the
// http.Handler; other fields are exposed for MQTT integration.
type FaceAPI struct {
	Handler   http.Handler
	MQTTReady bool
	// CheckStream is the one-shot stream-check function used by the MQTT bridge
	// when no camera URL is configured.
	CheckStream func(rtspURL string) (*domain.StreamCheckResult, error)
	// ScanBurst is the burst scanner used by the MQTT bridge when a camera URL
	// is configured. It grabs full-resolution frames for the burst window and
	// returns the first match at or above the threshold.
	ScanBurst func(ctx context.Context) (*domain.StreamCheckResult, error)
}

// NewFaceAPI builds all components and wires them together.
func NewFaceAPI(cfg Config) (*FaceAPI, error) {
	if err := EnsureBuckets(cfg.DB); err != nil {
		return nil, err
	}

	userRepo := repository.NewUserRepository(cfg.DB)
	auditRepo := repository.NewAuditRepository(cfg.DB)
	settingsRepo := repository.NewSettingsRepository(cfg.DB)

	// Backfill picture data from audit log for legacy users.
	if _, err := userRepo.BackfillUsersFromAudit(); err != nil {
		cfg.Logger.Warn().Err(err).Msg("backfill users from audit failed")
	}

	processor, err := service.NewFaceProcessor(cfg.RT, cfg.ORTEnv, cfg.DetSession, cfg.RecSession, cfg.Alignment)
	if err != nil {
		return nil, fmt.Errorf("create face processor: %w", err)
	}
	faceService := service.New(userRepo, auditRepo, processor, cfg.Threshold)
	// Apply any persisted threshold override (stored value wins over env/ctor).
	faceService.WithSettingsRepo(settingsRepo)
	rtspReader := rtsp.New()

	handlers := fhttp.NewHandlers(
		faceService,
		cfg.RTSPURL,
		rtspReader,
		cfg.EnableUI,
		cfg.BasePath,
		cfg.AuthToken,
		cfg.MaxUploadBytes,
		cfg.Logger,
	)

	// Inject RTSP reader into service for stream-based operations
	faceService.WithRTSPReader(rtspReader)
	faceService.WithLogger(&cfg.Logger)

	// Backfill thumbnails for legacy audit entries (one-time, at startup).
	if n, err := faceService.BackfillAuditThumbnails(); err != nil {
		cfg.Logger.Warn().Err(err).Msg("backfill audit thumbnails failed")
	} else if n > 0 {
		cfg.Logger.Info().Int("entries", n).Msg("backfilled audit thumbnails")
	}

	// The burst scanner grabs full-resolution frames straight from the
	// camera's main stream. The sub-stream was unusable for matching: at
	// 640x480 a face at 3 m is 50 px and scores ~0.44, while the same face
	// in the 2560x1920 main stream is 200 px and scores ~0.90.
	if cfg.RTSPURL != "" {
		faceService.WithFrameGrabber(framesource.NewGrabber(cfg.RTSPURL, cfg.Logger))
		faceService.WithBurst(cfg.BurstWindow, cfg.SampleInterval)
		cfg.Logger.Info().
			Str("stream_url", config.RedactURL(cfg.RTSPURL)).
			Dur("burst_window", cfg.BurstWindow).
			Dur("sample_interval", cfg.SampleInterval).
			Msg("live burst pipeline enabled")
	} else {
		cfg.Logger.Info().Msg("live burst pipeline disabled (no RTSP_URL)")
	}

	mux := http.NewServeMux()
	handlers.RegisterHandlers(mux)

	// Register UI handlers (if enabled).
	if cfg.EnableUI {
		uiHandler := fhttp.NewUIHandler(faceService, cfg.EnableUI, cfg.RTSPURL)
		uiHandler.RegisterUIHandlers(mux)
		// Make the API mux available for in-process UI API proxying.
		fhttp.SetAPIMux(mux)
	}

	// Wrap with middleware: auth first (outer), size limit (inner).
	var finalHandler http.Handler
	finalHandler = fhttp.NewAuthMiddleware(mux, cfg.AuthToken, cfg.EnableUI, cfg.BasePath)
	finalHandler = fhttp.NewMaxSizeMiddleware(finalHandler, cfg.MaxUploadBytes)

	api := &FaceAPI{
		Handler:     finalHandler,
		MQTTReady:   true,
		CheckStream: faceService.CheckStream,
		ScanBurst:   faceService.ScanBurst,
	}
	return api, nil
}

// Start is a no-op: the burst scanner grabs frames on demand, so there is no
// background stream to run.
func (a *FaceAPI) Start(ctx context.Context) {}

// Stop is a no-op; see Start.
func (a *FaceAPI) Stop() {}

// Close releases the API resources. It is safe to call more than once.
func (a *FaceAPI) Close() {
	a.Stop()
}

// EnsureBuckets creates the required bbolt buckets in the given database.
func EnsureBuckets(db *bolt.DB) error {
	return db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"Faces", "Audit", "Settings"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		return nil
	})
}
