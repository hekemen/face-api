# Face API Hexagonal Architecture Refactor — Design Plan

## 1. Intent

Refactor the monolithic `internal/server` package into a proper **hexagonal (ports & adapters) architecture**, while simultaneously changing the core runtime model:

- **Remove the continuous RTSP collector** — no more 24/7 stream
- **On-demand checking** — RTSP stream opens only when triggered (Web UI recognize, MQTT trigger, stream-check API)
- **Auto-collect** — unmatched faces from on-demand checks are saved as candidates
- **Manual matching** — select from collected faces, match to existing or create new users
- **4 enroll methods** — image upload, webcam capture, RTSP stream, manual from collected
- **Preserve audit trail** — all face scans logged

## 2. Current Problems

The current codebase has a single massive `internal/server` package (~1400 lines in `server.go`) that mixes:
- Domain logic (user CRUD, face matching, candidate grouping)
- HTTP handlers (request parsing, response encoding)
- UI rendering (template parsing, HTML generation)
- Infrastructure (bbolt DB, ONNX inference, RTSP transport)
- MQTT integration (bridge callbacks)

This violates hexagonal architecture principles: domain logic should not depend on HTTP, templates, or network protocols.

## 3. Proposed Architecture

### 3.1 Package Layout

```
face-api/
├── cmd/face-api/
│   └── main.go              # Wire everything together (DI, startup)
├── internal/
│   ├── domain/               # NEW — pure domain models + interfaces
│   │   ├── models.go         # User, Candidate, AuditEntry, Enrollment
│   │   └── ports.go          # Service interface (FaceService), Repository interfaces
│   ├── service/              # NEW — business logic implementation (driven adapter)
│   │   ├── face_service.go   # Enroll, recognize, check, collect, promote logic
│   │   └── face_processor.go # ONNX inference, detection, embedding (pure computation)
│   ├── repository/           # NEW — data access (driven adapters)
│   │   ├── user_repo.go      # bbolt user persistence
│   │   ├── candidate_repo.go # bbolt candidate persistence
│   │   └── audit_repo.go     # bbolt audit persistence
│   ├── http/                 # NEW — HTTP handlers (driving adapter)
│   │   ├── handlers.go       # All API endpoint handlers
│   │   ├── ui_handlers.go    # Web UI page handlers
│   │   ├── enroll_handler.go # Multi-source enroll (upload/webcam/rtsp/collected)
│   │   └── recognize_handler.go
│   ├── mqtt/                 # EXISTING — MQTT bridge (driving adapter)
│   │   ├── bridge.go         # Unchanged
│   │   └── bridge_test.go
│   └── rtsp/                 # NEW — RTSP transport (driven adapter)
│       ├── reader.go         # RTSP frame reading
│       └── reader_test.go
├── deploy/helm/face-api/     # Helm chart (updated)
├── Makefile                   # Build targets
└── Dockerfile                 # Docker build
```

### 3.2 Hexagonal Boundaries

```
                    ┌─────────────────────────────────────────────────────────┐
                    │                       main.go                           │
                    │               (dependency injection / wiring)           │
                    └────────────────────────────┬────────────────────────────┘
                                                 │
                    ┌────────────────────────────▼────────────────────────────┐
                    │                    HTTP HANDLERS                         │
                    │  (driving adapter: parses request, calls service,        │
                    │   encodes response — NO domain knowledge here)           │
                    └────────────────────────────┬────────────────────────────┘
                                                 │
                    ┌────────────────────────────▼────────────────────────────┐
                    │                    MQTT BRIDGE                           │
                    │  (driving adapter: receives MQTT message, calls service) │
                    └────────────────────────────┬────────────────────────────┘
                                                 │
                    ═════════════════════════════╪════════════════════════════
                    │         INTERFACE LAYER    │    ← FACE SERVICE PORT
                    │  (port: domain.Port)       │
                    ═════════════════════════════╪════════════════════════════
                                                 │
                    ┌────────────────────────────▼────────────────────────────┐
                    │                   BUSINESS LOGIC                       │
                    │  (FaceService: domain knowledge, no HTTP, no DB)       │
                    │  - enroll(name, embedding, faceImage)                   │
                    │  - recognize(embedding) → User or Unknown              │
                    │  - checkRTSP(rtspURL) → Recognized | Collected         │
                    │  - promoteCandidates(ids, name)                         │
                    │  - listCandidates()                                     │
                    │  - listUsers()                                          │
                    │  - listAudit()                                          │
                    └────────────────────────────┬────────────────────────────┘
                                                 │
                    ═════════════════════════════╪════════════════════════════
                    │      PORTS (interfaces)    │    ← REPOSITORY PORTS
                    ═════════════════════════════╪════════════════════════════
                                                 │
                    ┌────────────────────────────▼────────────────────────────┐
                    │                   DRIVEN ADAPTERS                        │
                    │  (implementations of repository ports)                 │
                    │  - bboltUserRepo  - bboltCandidateRepo  - bboltAuditRepo │
                    │  - rtspFrameReader (RTSP transport)                      │
                    │  - onnxFaceProcessor (ONNX inference)                   │
                    └─────────────────────────────────────────────────────────┘
```

### 3.3 Key Interfaces (Ports)

```go
// domain/ports.go

// FaceService is the main business logic interface.
type FaceService interface {
    // Enroll adds a face to a user. Max 3 pictures per user.
    Enroll(name string, embedding []float32, faceImage string) error

    // Recognize returns the best match or "Unknown".
    Recognize(embedding []float32) (string, float32, bool)

    // CheckRTSP opens RTSP, runs one frame through detection+recognition.
    // Returns the result: matched, unknown (auto-collected), or error.
    CheckRTSP(rtspURL string) (*CheckResult, error)

    // PromoteCandidate creates a user from a collected candidate.
    PromoteCandidate(candidateID, name string) error

    // BulkPromoteCandidates creates a user from multiple candidates.
    BulkPromoteCandidates(name string, candidateIDs []string) error

    // ListUsers returns all enrolled users.
    ListUsers() ([]UserInfo, error)

    // ListCandidates returns collected candidates.
    ListCandidates() ([]Candidate, error)

    // ListAudit returns recent audit entries.
    ListAudit(limit int) ([]AuditEntry, error)

    // DeleteUser removes a user.
    DeleteUser(name string) error
}

// UserRepository handles user persistence.
type UserRepository interface {
    GetUser(name string) (*User, error)
    SaveUser(user *User) error
    DeleteUser(name string) error
    ListUsers() ([]User, error)
}

// CandidateRepository handles collected face persistence.
type CandidateRepository interface {
    Save(c *Candidate) error
    List() ([]*Candidate, error)
    Delete(id string) error
    DeleteMany(ids []string) error
    Exists(embedding []float32, threshold float32) bool
}

// AuditRepository handles audit log persistence.
type AuditRepository interface {
    Save(entries []AuditEntry) error
    List(limit int) ([]AuditEntry, error)
    Count() int
}

// FaceProcessor handles ONNX inference (pure computation, no persistence).
type FaceProcessor interface {
    DetectAndCrop(image *rgbImage) (*rgbImage, error)
    ExtractEmbedding(image *rgbImage) ([]float32, error)
    CosineSimilarity(a, b []float32) float32
    BestMatch(query []float32, embeddings [][]float32) (int, float32)
}

// RTSPReader handles RTSP transport.
type RTSPReader interface {
    ReadFrame(url string, timeout time.Duration) (*rgbImage, error)
}
```

### 3.4 On-Demand Check Flow

```
User triggers check (UI button / MQTT trigger / API POST)
    │
    ▼
HTTP Handler / MQTT worker
    │
    ▼
FaceService.CheckRTSP(rtspURL)
    │
    ├─ RTSPReader.ReadFrame() → rgbImage
    │
    ├─ FaceProcessor.DetectAndCrop() → cropped face
    │   (no face detected → return "no face" result)
    │
    ├─ FaceProcessor.ExtractEmbedding() → embedding vector
    │
    ├─ FaceService.Recognize(embedding) → (name, score, matched)
    │   │
    │   ├─ matched → save audit entry (matched=true) → return recognized result
    │   │
    │   └─ not matched → save as Candidate → save audit entry (matched=false) → return collected result
```

### 3.5 RTSP Enroll Flow

```
User opens /ui/enroll → selects "RTSP" tab
    │
    ▼
Page shows <video> element streaming from RTSP URL
    │
    ▼
User clicks "Capture Frame"
    │
    ▼
HTTP POST /enroll?rtsp=1 (or a dedicated /enroll-from-stream endpoint)
    │
    ▼
FaceService.CheckRTSP(rtspURL) → returns cropped face + embedding
    │
    ▼
If face detected + embedding extracted:
    → FaceService.Enroll(name, embedding, faceImage)
    → Return "enrolled" response
    │
    └─ If no face → return "no face detected" error
```

### 3.6 Manual Matching Flow

```
User opens /ui/candidates (Collected Faces page)
    │
    ▼
Grid of candidate face cards (same as current)
    │
    ▼
User selects one or more candidates (checkboxes)
    │
    ▼
Options appear:
    1. "Create User" → POST /candidates/bulk-promote
       → Creates NEW user from selected candidates' embeddings
    │
    2. "Match to Existing" → POST /candidates/match
       → Opens dialog with enrolled user list
       → User picks a name
       → Updates the candidate's user reference (or just logs the match)
```

## 4. Removed Components

| Current | Replacement |
|---------|-------------|
| `collector.go` (continuous loop) | Removed entirely |
| `POST /collector/start` | Removed |
| `POST /collector/stop` | Removed |
| `GET /collector/status` | Removed |
| `HandleMQTTCollect` | Simplified: MQTT trigger now does single check, not start/stop |
| MQTT switch entity | Removed; replace with single check button + sensor |

## 5. New Components

| Component | Description |
|-----------|-------------|
| `internal/domain/` | Pure models + port interfaces |
| `internal/service/` | Business logic implementation (FaceService) |
| `internal/repository/` | bbolt persistence implementations |
| `internal/http/` | All HTTP handlers (API + UI) |
| `internal/rtsp/` | RTSP frame reader (moved from server.go) |
| `POST /enroll-from-stream` | New endpoint for RTSP enroll |
| `GET /candidates/match` | UI for matching candidate to existing user |

## 6. HTTP API Changes

### Endpoints to Remove
| Endpoint | Method | Reason |
|----------|--------|--------|
| `/collector/start` | POST | No more continuous collector |
| `/collector/stop` | POST | No more continuous collector |
| `/collector/status` | GET | No more continuous collector |
| `/stream-check` | POST | Kept but simplified (no auto-collect flag) |

### Endpoints to Add
| Endpoint | Method | Description |
|----------|--------|-------------|
| `/enroll-from-stream` | POST | Enroll from RTSP stream (like stream-check but saves user) |
| `/candidates/match` | POST | Match candidate to existing user (bulk) |

### Endpoints to Keep (unchanged)
| Endpoint | Method | Description |
|----------|--------|-------------|
| `/enroll` | POST | Image upload enrollment (multipart/form-data) |
| `/recognize` | POST | Recognize from image upload |
| `/users` | GET | List enrolled users |
| `/users/:name` | DELETE | Delete user |
| `/audit` | GET | List audit entries |
| `/api/audit` | GET | Paginated audit entries |
| `/stats` | GET | Statistics |
| `/candidates` | GET | List collected candidates |
| `/candidates/promote` | POST | Promote single candidate |
| `/candidates/bulk-promote` | POST | Promote multiple candidates |
| `/healthz` | GET | Liveness |
| `/readyz` | GET | Readiness |

## 7. MQTT Changes

### Current Behavior
- `face/scan/trigger` → runs `RunStreamCheck()` → publishes result
- `face/scan/cmd` → "start"/"stop" → starts/stops continuous collector

### New Behavior
- `face/scan/trigger` → runs **single** `CheckRTSP()` → publishes result
  - If matched: `matched=true, name="X"`
  - If not matched: candidate auto-collected, `matched=false, name=""`
- `face/scan/cmd` → removed (no collector to control)
- Home Assistant entities updated:
  - Remove: Switch (collect), Sensor (collecting state)
  - Keep: Button (check), Sensor (last result), Binary sensor (matched)

## 8. UI Changes

### Pages to Add/Modify

| Page | Changes |
|------|---------|
| **Dashboard** (`/ui/`) | Remove collector status; add "Check Stream" button |
| **Enroll** (`/ui/enroll`) | Add tabs: Upload | Webcam | RTSP | Collected |
| **Recognize** (`/ui/recognize`) | Add tab: RTSP (same as stream-check) |
| **Stream-Check** (`/ui/stream-check`) | Simplified — single frame check, shows result |
| **Collected Faces** (`/ui/candidates`) | Grid + checkboxes + "Create User" + "Match to Existing" |
| **Audit** (`/ui/audit`) | Unchanged |
| **Stats** (`/ui/stats`) | Unchanged |

### Enroll Page Tabs

```
┌──────────────────────────────────────────────┐
│  [Upload] [Webcam] [RTSP] [Collected Faces]  │
├──────────────────────────────────────────────┤
│                                              │
│  Upload tab: file input + name + enroll      │
│  Webcam tab: getUserMedia → canvas → JPEG    │
│  RTSP tab:   <video> + capture button        │
│  Collected tab: select candidate → promote   │
│                                              │
└──────────────────────────────────────────────┘
```

## 9. Testing Strategy

### Unit Tests
- `domain/` — pure logic tests (cosine similarity, embedding matching, candidate grouping)
- `service/` — test FaceService with mock repositories (Ginkgo + testify mocks)
- `http/` — httptestRecorder tests for all handlers

### Integration Tests
- `internal/server/` tests moved/renamed to `internal/repository/`
- E2E tests updated for new endpoints
- RTSP mock tests unchanged (RTSP reader stays the same logic, just relocated)

### Test File Movement
| Current File | New Location |
|-------------|-------------|
| `candidates_test.go` | `repository/candidate_repo_test.go` + `service/face_service_test.go` |
| `users_test.go` | `repository/user_repo_test.go` + `service/face_service_test.go` |
| `ui_test.go` | `http/ui_handlers_test.go` |
| `rtsp_test.go` | `rtsp/reader_test.go` |
| `img_test.go` | `service/face_processor_test.go` |

## 10. Implementation Plan (Phases)

### Phase 1: Domain Layer (foundation)
- Create `internal/domain/` with models and ports
- No behavior changes, just type/interface extraction

### Phase 2: Repository Layer (data access)
- Create `internal/repository/` with bbolt implementations
- Extract bbolt code from `server.go` into separate files

### Phase 3: Service Layer (business logic)
- Create `internal/service/` with FaceService implementation
- Move business logic from `server.go` handlers
- `FaceServer` struct becomes a thin wrapper (wiring)

### Phase 4: HTTP Layer (driving adapter)
- Create `internal/http/` with handlers
- Handlers become thin — parse request, call service, encode response
- UI handlers in `ui_handlers.go`

### Phase 5: RTSP Layer (transport)
- Move `rtsp.go` to `internal/rtsp/reader.go`
- Update imports

### Phase 6: Feature Changes
- Remove collector (continuous loop)
- Add on-demand check with auto-collect
- Add RTSP enroll
- Add manual matching
- Update MQTT bridge

### Phase 7: Tests + Cleanup
- Move and update all tests
- Remove dead code
- Update Helm chart

## 11. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Large refactoring breaks existing functionality | Keep original `internal/server` as reference; migrate one module at a time |
| bbolt concurrency issues with new repo pattern | Each repo manages its own bucket access; FaceService serializes via mutex |
| ONNX inference thread safety | Keep `inferMu` in FaceProcessor; no change needed |
| UI regression | UI templates stay in `server/templates/` until Phase 4; minimal HTML changes |
| MQTT breaking Home Assistant integration | Test MQTT bridge independently; keep backward-compatible trigger behavior |

## 14. Open Questions

1. **[x] Manual matching (match to existing user)** — Should this be included in this refactor, or deferred to a follow-up? The "Create User" bulk flow is included; the single-select "match to existing" requires a new `/candidates/match` endpoint and UI dialog.

2. **[x] Helm chart updates** — Should the Helm chart be updated in this refactor (remove collector env vars, update MQTT config to remove switch, add `enroll-from-stream` endpoint config), or left unchanged and updated separately?

3. **[x] Dual-mode transition** — The plan proposes building new packages alongside the old `internal/server`, then swapping via `main.go`. Do you prefer this incremental approach, or would you rather do a complete rewrite in one pass (faster but riskier)?

4. **[x] RTSP enroll URL** — Should `/enroll-from-stream` use the configured `RTSP_URL` env var (same as current stream-check), or should it accept an `rtsp_url` form field like `/stream-check`?
   **Answer:** Use `RTSP_URL` env var only. Do not allow the user to supply a custom URL via form field.

5. **[x] MQTT check behavior** — When MQTT trigger fires and the face is **not matched**, should it auto-collect to candidates (as designed), or just log to audit and return `matched=false`? Auto-collect means candidates accumulate from both UI checks AND MQTT triggers.

## 12. Files to Create / Delete

### Create
```
internal/domain/models.go
internal/domain/ports.go
internal/service/face_service.go
internal/service/face_processor.go
internal/repository/user_repo.go
internal/repository/candidate_repo.go
internal/repository/audit_repo.go
internal/http/handlers.go
internal/http/ui_handlers.go
internal/rtsp/reader.go
internal/rtsp/reader_test.go
```

### Delete
```
internal/server/collector.go          # No more continuous collector
internal/server/face.go               # Split into service/ and domain/
internal/server/server.go             # Refactored into new packages
internal/server/types.go              # Moved to domain/models.go
internal/server/ui.go                 # Moved to http/ui_handlers.go
internal/server/img.go                # Moved to service/ (image processing)
```

### Modify
```
cmd/face-api/main.go                  # New DI wiring
internal/mqtt/bridge.go               # Simplified trigger handling
deploy/helm/face-api/                 # Remove collector env vars
Makefile                              # Update test paths
```

## 13. Migration Strategy

**Do NOT refactor in one big bang.** The safest approach:

1. **Keep `internal/server` as the current implementation** — it works, tests pass
2. **Build new packages incrementally** — domain first, then repo, then service
3. **Use a dual-mode startup** — during transition, `main.go` can choose old or new
4. **Test each phase independently** — unit tests for domain/service, httptest for http
5. **Swap the dependency injection** — once new packages pass all tests, `main.go` wires them
6. **Remove old code** — delete `internal/server` after all tests pass with new architecture

This means the codebase will temporarily have both old and new code. That's intentional — safer than a big rewrite.
