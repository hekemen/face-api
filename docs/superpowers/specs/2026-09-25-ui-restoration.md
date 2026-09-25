# UI Restoration — Design Spec

**Status**: implemented + documented
**Date**: 2026-09-25

---

## 1. Intent

The hexagonal architecture refactor (Phase 4) initially lost the UI handler registration. The old `internal/server/ui.go` existed but was never wired into the new DI system. This spec documents the restored UI implementation in the hexagonal architecture.

## 2. Architecture

### 2.1 Package Location

`internal/http/ui.go` — The UI handler lives in the same `http` package as the API handlers, sharing the same dependency injection wiring.

### 2.2 Template Embedding

Templates are embedded from a local copy in `internal/http/templates/`:

```go
//go:embed templates/*.html templates/pico.min.css
var embeddedTemplates embed.FS
```

Go's `embed` directive does not support `..` paths, so templates are duplicated from `internal/server/templates/` into `internal/http/templates/`.

**Template files:**
- `shared.html` — shared layout (head/foot definitions, nav, clock, scripts)
- `index.html` — dashboard page
- `enroll.html` — enroll page
- `recognize.html` — recognize page
- `audit.html` — audit page
- `stats.html` — stats page
- `candidates.html` — collected faces page
- `pico.min.css` — vendored PicoCSS v2 (MIT license)

### 2.3 Handler Registration

The `UIHandler` is created in the DI wiring (`internal/di/wire.go`):

```go
if cfg.EnableUI {
    uiHandler := fhttp.NewUIHandler(faceService, cfg.EnableUI)
    uiHandler.RegisterUIHandlers(mux)
    fhttp.SetAPIMux(mux)
}
```

`SetAPIMux` stores the main API mux for in-process request proxying.

## 3. Request Flow

### 3.1 GET Requests (page rendering)

```
GET /ui/
    │
    ▼
handleUI() — strips /ui/ prefix
    │
    ▼
renderIndex() — calls service layer for data
    │
    ├─ svc.ListUsers() → users
    ├─ svc.ComputeStats() → stats
    └─ svc.ListCandidates() → candidates
    │
    ▼
template.ParseFS(embeddedTemplates, "templates/index.html", "templates/shared.html")
    │
    ▼
Execute template with page data
```

### 3.2 POST Requests (API proxying)

```
POST /ui/enroll (multipart/form-data)
    │
    ▼
handleUIEnroll()
    │
    ▼
proxyAPI("/enroll") — in-process HTTP dispatch
    │
    ├─ r.URL.Path = "/enroll"
    ├─ r.URL.RawQuery = ""
    ├─ apiMux.ServeHTTP(rec, r)  ← httptest.ResponseRecorder
    │
    ▼
Copy status + body to client
```

## 4. Pages

| Route | Handler | Description |
|-------|---------|-------------|
| `GET /ui/` | `renderIndex` | Dashboard with stats badges |
| `GET/POST /ui/enroll` | `handleUIEnroll` | Enroll form + user list + delete |
| `GET /ui/recognize` | `handleUIRecognize` | Recognize form with webcam |
| `GET /ui/audit` | `handleUIAudit` | Audit log page |
| `GET /ui/stats` | `handleUIStats` | Stats page |
| `GET /ui/candidates` | `handleUICandidates` | Collected faces page |

## 5. Removed Pages

| Route | Status | Reason |
|-------|--------|--------|
| `/ui/stream-check` | Removed | Template was deleted in prior commit; RTSP enroll uses `RTSP_URL` env var only |

## 6. UI Data Types

The UI handler defines its own page data struct (`uiPageData`) that wraps domain types:

```go
type uiPageData struct {
    Result             string            // result message (enroll/recognize result)
    Class              string            // CSS class ("error" or "")
    Users              []domain.UserInfo
    Entries            []domain.AuditEntry
    Count              int
    Stats              *domain.StatsResponse
    DashUsers          []domain.UserInfo
    DashStats          *domain.StatsResponse
    DashAudit          []domain.AuditEntry
    DashCandidates     int
    DashGroups         int
    Candidates         []*domain.CandidateGroup
    AllCandidates      []domain.Candidate
    CollectorRunning   bool
    CollectorStartedAt time.Time
    GitVersion         string
    GitHubURL          string
}
```

## 7. Static Assets

- `/ui/pico.min.css` — served from embedded file, `text/css; charset=utf-8`
- Templates reference CSS as relative path `href="pico.min.css"` for stripPrefix compatibility

## 8. StripPrefix Compatibility

All UI URLs (nav links, form actions, fetch calls, CSS path) are **relative**, not absolute (`/ui/...`). This ensures the UI works both:
- Direct: `http://host:8081/ui/`
- Behind Traefik: `http://host/face/ui/` (after stripPrefix strips `/face`)

## 9. ENABLE_UI

Default: `false`. Set via `ENABLE_UI=true` environment variable.

In the Helm chart:
- `values.yaml` defaults to `ENABLE_UI: "true"`
- `values.pi.yaml` sets `ENABLE_UI: "true"` for the Pi deployment
