package di

import (
	"net/http"

	"github.com/rs/zerolog"
	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
	bolt "go.etcd.io/bbolt"

	"h2hsecure.com/face/internal/domain"
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
	auditRepo := repository.NewAuditRepository(cfg.DB)
	settingsRepo := repository.NewSettingsRepository(cfg.DB)

	// Backfill picture data from audit log for legacy users.
	if _, err := userRepo.BackfillUsersFromAudit(); err != nil {
		cfg.Logger.Warn().Err(err).Msg("backfill users from audit failed")
	}

	processor := service.NewFaceProcessor(cfg.RT, cfg.ORTEnv, cfg.DetSession, cfg.RecSession)
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

	return &FaceAPI{
		Handler:     finalHandler,
		MQTTReady:   true,
		CheckStream: faceService.CheckStream,
	}, nil
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
