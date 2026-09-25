# Hexagonal Architecture — Design Spec

**Status**: implemented + documented
**Date**: 2026-09-25
**Related Plan**: `plans/2026-09-24-face-api-hex-refactor.md`

---

## 1. Intent

Split the monolithic `internal/server` package (~1400 lines) into a proper hexagonal (ports & adapters) architecture. Domain logic must not depend on HTTP, templates, or network protocols.

## 2. Architecture

### 2.1 Package Layout

```
face-api/
├── cmd/face-api/
│   └── main.go              # Entry point; DI wiring, startup, shutdown
├── internal/
│   ├── domain/               # Pure domain layer (no external deps)
│   │   ├── models.go         # User, Candidate, AuditEntry, FaceCrop, stats
│   │   └── ports.go          # Service interface, repository ports, response types
│   ├── service/              # Business logic implementation
│   │   ├── face_service.go   # FaceService: enroll, recognize, check, promote, stats
│   │   └── face_processor.go # FaceProcessor: ONNX detection + recognition
│   ├── repository/           # Data access (driven adapters)
│   │   ├── user_repo.go      # bbolt user persistence
│   │   ├── candidate_repo.go # bbolt candidate persistence + grouping
│   │   └── audit_repo.go     # bbolt audit persistence + stats
│   ├── http/                 # HTTP layer (driving adapters)
│   │   ├── handlers.go       # API endpoint handlers (thin: parse → call service → encode)
│   │   └── ui.go             # Web UI page handlers (template rendering + API proxying)
│   ├── rtsp/                 # RTSP transport (driven adapter)
│   │   └── reader.go         # MJPEG + H.264 frame capture
│   ├── mqtt/                 # MQTT bridge (driving adapter)
│   │   ├── bridge.go         # HA discovery, trigger subscription, result publishing
│   │   └── bridge_test.go    # MQTT integration tests
│   └── di/                   # Dependency injection
│       └── wire.go           # NewFaceAPI(config) → *FaceAPI
├── internal/server/          # OLD — monolithic package (kept for reference)
├── models/                   # ONNX weights
├── deploy/helm/face-api/     # Kubernetes deployment
└── test/
    ├── e2e/                  # End-to-end tests (testcontainers)
    └── rtsp/                 # RTSP mock tests (testcontainers)
```

### 2.2 Dependency Direction

```
HTTP handlers ──calls──▶ FaceService ──calls──▶ Repository ports
        │                              │              │
        │                              │              └──▶ bbolt (via repository impl)
        │                              │
        │                              └──▶ FaceProcessor (ONNX)
        │
        └──▶ UI templates (embedded)
        
MQTT bridge ──calls──▶ FaceService (same interface as HTTP)
```

All dependencies point **inward**. The domain layer has zero external dependencies.

### 2.3 Key Interfaces

**`domain.FaceService`** — the core business logic port:

| Method | Description |
|--------|-------------|
| `EnrollImage(name, imageData) error` | Enroll a face from image bytes |
| `RecognizeImage(imageData) (*RecognitionResult, error)` | Recognize face from image bytes |
| `CheckStream(rtspURL) (*StreamCheckResult, error)` | On-demand RTSP check |
| `CheckStreamImage(imageData) (*StreamCheckResult, error)` | Process pre-read image |
| `CollectStreamCandidate(c *Candidate) error` | Store unmatched face as candidate |
| `ListUsers() ([]*User, error)` | List all enrolled users |
| `DeleteUser(name string) error` | Delete a user |
| `ListCandidates() ([]*CandidateGroup, error)` | Grouped collected candidates |
| `PromoteCandidate(id, name) error` | Promote one candidate to user |
| `BulkPromoteCandidates(name, ids) error` | Promote multiple candidates |
| `RecentAudit(n) ([]AuditEntry, error)` | Newest audit entries |
| `ListAuditPaginated(opts) ([]AuditEntry, int, error)` | Paginated audit with filtering |
| `ComputeStats() (Stats, error)` | Aggregate statistics |

**Repository ports**: `UserRepository`, `CandidateRepository`, `AuditRepository`

**Infrastructure ports**: `FaceProcessor`, `RTSPReader`

### 2.4 Concurrency Model

| Resource | Protection |
|----------|-----------|
| In-memory cache (dbMap) | `sync.RWMutex` — read locks for lookups, write for enroll |
| ONNX inference | `sync.Mutex` (`inferMu`) — serializes all `Run` calls |
| bbolt DB | Single `*bolt.DB` handle opened once at startup |
| Template cache | `sync.Map` or singleton package-level map (read-only after init) |

### 2.5 Error Handling

| Condition | HTTP Status | Domain Behavior |
|-----------|-------------|-----------------|
| Missing image field | 400 | `http.StatusBadRequest` |
| No face detected | 400 | `http.StatusBadRequest` |
| Invalid image format | 400 | `http.StatusBadRequest` |
| Face processing failure | 500 | `http.StatusInternalServerError` |
| Database write failure | 500 | `http.StatusInternalServerError` |
| Method not allowed | 405 | `http.StatusMethodNotAllowed` |

## 3. Data Model

### 3.1 bbolt Buckets

| Bucket | Key | Value Format |
|--------|-----|--------------|
| `"Faces"` | User name (string) | JSON: `{"embeddings":[[...]],"pictures":["b64"], "updated_at":"RFC3339"}` |
| `"Audit"` | `NextSequence()` uint64 | JSON: `AuditEntry` struct |
| `"Candidates"` | UUID string | JSON: `{"id":"uuid","embedding":[...],"face_image":"b64","time":"RFC3339","stream_url":"url"}` |

### 3.2 Domain Types

```go
type User struct {
    Name       string
    Embeddings []FaceEmbedding   // 512-dim vectors
    Pictures   []string          // base64 JPEGs (up to 3)
    UpdatedAt  time.Time
}

type Candidate struct {
    ID        string
    Embedding FaceEmbedding
    FaceImage string             // base64 JPEG
    Time      time.Time
    StreamURL string
}

type AuditEntry struct {
    Time       time.Time
    Endpoint   string   // "enroll" | "recognize" | "stream-check"
    Name       string
    Similarity float32
    Matched    bool
    DurationMs int64
    FaceImage  string   // base64 JPEG (only when face detected)
}
```

## 4. On-Demand Check Flow

```
Trigger (HTTP/MQTT/UI)
    │
    ▼
RTSPReader.ReadFrame() → JPEG bytes
    │
    ▼
FaceProcessor.DetectAndCrop() → FaceCrop
    │
    ├─ no face → audit(no face) → return "not ok"
    │
    ├─ face → FaceProcessor.ExtractEmbedding() → 512-dim
    │   │
    │   ├─ matched (similarity ≥ threshold)
    │   │   → audit(matched=true) → return recognized result
    │   │
    │   └─ not matched
    │       → save Candidate → audit(matched=false) → return collected result
```

## 5. Configuration

| Env Var | Default | Description |
|---------|---------|-------------|
| `RTSP_URL` | (none) | Default RTSP URL for stream-check |
| `FACES_DB_PATH` | `faces.db` | bbolt database file path |
| `ENABLE_UI` | `false` | Enable web UI |
| `MQTT_BROKER_URL` | (none) | MQTT broker URL; empty = disabled |
| `MQTT_USERNAME` | (none) | MQTT auth username |
| `MQTT_PASSWORD` | (none) | MQTT auth password |
| `MQTT_CLIENT_ID` | `face-api` | MQTT client ID |
| `MQTT_BASE_TOPIC` | `face/scan` | MQTT topic prefix |
| `MQTT_DEVICE_NAME` | `Face API` | HA device name |
| `MQTT_QUEUE_DEPTH` | `16` | Bounded channel depth |

## 6. In-Memory Cache vs bbolt

The service layer maintains an in-memory cache (`dbMap`) that is hydrated from bbolt on startup. This cache is used for:
- **Fast lookups**: `RecognizeImage` iterates all users' embeddings (in-memory is fast)
- **Write-through**: `EnrollImage` writes to both cache and bbolt

The cache is protected by a `sync.RWMutex`. All cache writes acquire a write lock; reads acquire a read lock.

## 7. Migration Notes

- The old `internal/server/` package is kept alongside the new architecture for reference
- Old code should be removed once confident the new architecture handles all cases
- The `storedUser` JSON format supports backward-compatible migration of legacy embedding formats
