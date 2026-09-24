package di

import (
	"net/http"
	"time"

	"github.com/rs/zerolog"
	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
	bolt "go.etcd.io/bbolt"

	fhttp "h2hsecure.com/face/internal/http"
	"h2hsecure.com/face/internal/repository"
	"h2hsecure.com/face/internal/rtsp"
	"h2hsecure.com/face/internal/service"
)

// Config holds the configuration needed to wire the face API.
type Config struct {
	DBPath       string
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
}

func NewFaceAPI(cfg Config) (*FaceAPI, error) {
	db, err := bolt.Open(cfg.DBPath, 0600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, err
	}

	if err := EnsureBuckets(db); err != nil {
		return nil, err
	}

	userRepo := repository.NewUserRepository(db)
	candidateRepo := repository.NewCandidateRepository(db)
	auditRepo := repository.NewAuditRepository(db)

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

	return &FaceAPI{
		Handler:   mux,
		MQTTReady: true,
	}, nil
}

// OpenDB opens the bbolt database with the given path.
func OpenDB(path string) (*bolt.DB, error) {
	return bolt.Open(path, 0600, &bolt.Options{Timeout: 1 * time.Second})
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
