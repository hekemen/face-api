package di

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"
	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
	bolt "go.etcd.io/bbolt"

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
	StreamURL      string
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
	// when no buffered stream is configured.
	CheckStream func(rtspURL string) (*domain.StreamCheckResult, error)
	// ScanBurst is the buffered burst scanner used by the MQTT bridge when a
	// sub-stream is configured. It scans continuously for the burst window and
	// returns the first match at or above the threshold.
	ScanBurst func(ctx context.Context) (*domain.StreamCheckResult, error)

	// source is the buffered frame source, started by Start and stopped by Stop.
	source *framesource.Source

	// startCancel cancels the source Run goroutine.
	startCancel context.CancelFunc
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

	// Build the buffered frame source when a sub-stream URL is configured.
	var src *framesource.Source
	if cfg.StreamURL != "" {
		src = framesource.New(cfg.StreamURL, cfg.Logger)
		faceService.WithFrameSource(src)
		faceService.WithBurst(cfg.BurstWindow, cfg.SampleInterval)
		cfg.Logger.Info().
			Str("stream_url", cfg.StreamURL).
			Dur("burst_window", cfg.BurstWindow).
			Dur("sample_interval", cfg.SampleInterval).
			Msg("live burst pipeline enabled")
	} else {
		cfg.Logger.Info().Msg("live burst pipeline disabled (no RTSP_STREAM_URL)")
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
		source:      src,
	}
	return api, nil
}

// Start begins running the buffered frame source (if configured). It must be
// called before Stop or Close.
func (a *FaceAPI) Start(ctx context.Context) {
	if a.source == nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	a.startCancel = cancel
	go a.source.Run(ctx)
}

// Stop cancels the source Run goroutine (if a frame source is configured).
func (a *FaceAPI) Stop() {
	if a.startCancel != nil {
		a.startCancel()
	}
}

// Close stops the source goroutine (if a frame source is configured).
// It is safe to call more than once.
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
