# Collect Faces Feature Design

## Overview

Continuously read an RTSP stream, detect faces that don't match any enrolled user, group them by similarity, and let the operator create users from the collected face groups.

## Goals

- Collect unknown faces from a running RTSP stream without manual intervention
- Group detected faces by similarity so the operator sees "Person A (5 faces), Person B (3 faces)" instead of a raw list
- Let the operator promote a group to a real enrolled user with one click
- Retain candidates for a configurable number of days, then auto-purge

## Non-Goals

- Automatic user creation (operator must confirm)
- Real-time face clustering during collection (grouping is computed at read time)
- Multi-stream collection (one stream at a time)

## Data Model

### Candidate (stored in bbolt `"Candidates"` bucket)

```go
type Candidate struct {
    ID         string    `json:"id"`
    Embedding  []float32 `json:"embedding"`
    FaceImage  string    `json:"face_image"`
    Time       time.Time `json:"time"`
    StreamURL  string    `json:"stream_url,omitempty"`
}
```

### CandidateGroup (computed at read time, not stored)

```go
type CandidateGroup struct {
    ID            string    `json:"id"`
    FaceCount     int       `json:"face_count"`
    BestSimilarity float32  `json:"best_similarity"`
    Faces         []Candidate `json:"faces"`
}
```

### API Response

```json
{
  "groups": [
    {
      "id": "uuid",
      "face_count": 3,
      "best_similarity": 0.72,
      "faces": [
        {"id": "uuid", "face_image": "b64", "time": "2026-09-22T18:00:00Z"}
      ]
    }
  ]
}
```

## Architecture

### bbolt Buckets

Three buckets total:

| Bucket | Purpose |
|--------|---------|
| `Faces` | Enrolled users (existing) |
| `Audit` | Audit log (existing) |
| `Candidates` | Collected unknown faces (new) |

No separate group bucket — grouping is computed at read time by comparing all candidate embeddings pairwise.

### FaceServer Changes

New fields on `FaceServer`:

```go
type FaceServer struct {
    // ... existing fields ...
    collecting    bool              // whether collector is running
    collectMu     sync.Mutex        // protects collecting + collectCancel
    collectCancel context.CancelFunc
}
```

New methods:

- `startCollector(rtspURL string) error` — launches background goroutine
- `stopCollector()` — signals goroutine to stop
- `storeCandidate(c *Candidate) error` — writes to bbolt
- `readCandidates() ([]*Candidate, error)` — reads all candidates from bbolt
- `purgeExpiredCandidates(ttlDays int) error` — deletes candidates older than TTL
- `promoteCandidateToUser(candidateID, name string) error` — copies best face from candidate group to enrolled users
- `groupCandidates(candidates []*Candidate, threshold float32) ([]*CandidateGroup, error)` — O(n²) pairwise grouping

### Collector Loop

A background goroutine launched by `startCollector`:

1. Connect to RTSP stream (reconnect on failure with backoff)
2. For each frame: detect face → extract embedding → check against enrolled users (skip if matched) → check against existing candidates (group by threshold 0.45) → store new/unmatched faces
3. Runs until `stopCollector` is called or the server shuts down
4. Logs progress: frames processed, faces detected, new candidates added

### API Endpoints

**`GET /candidates`**

Returns grouped candidates. Groups are computed client-side from the raw candidate list.

**`POST /candidates/:id/promote`**

Body: `{"name": "John"}`. Promotes the candidate group containing the given candidate ID to a real enrolled user. Uses the best (highest similarity) face from the group. Returns 201 on success.

### MQTT

New Home Assistant discovery button:

- **Name**: "Collect"
- **Unique ID**: `face_api_collect`
- **Command topic**: `<base>/cmd`
- **Payload**: `{"action":"collect"}`

New `collect` case in `onCommand` — calls `b.collect()` which starts/stops the collector.

New `collect CollectFunc` field on `Bridge` struct, set by the server at startup.

### Configuration

| Env Var | Default | Description |
|---------|---------|-------------|
| `CANDIDATE_TTL_DAYS` | `7` | Days to retain candidates before auto-purge |
| `MQTT_COLLECT_BUTTON` | `true` | Whether to publish the collect button to HA |

### UI

**Dashboard card** — "Collected Faces" card on the main dashboard showing:
- Number of candidate groups
- Number of total collected faces
- Link to the full candidates page

**`/ui/candidates` page** — lists all candidate groups with:
- Group face count
- Best similarity score
- Thumbnail previews of faces in the group
- "Create User" button per group (prompts for name)

### Expiry

Candidates older than `CANDIDATE_TTL_DAYS` are purged:
- On server startup (during `hydrateCache` equivalent for candidates)
- On each `GET /candidates` call (lazy purge)

### Error Handling

- RTSP connection failure: log error, retry with exponential backoff (max 30s interval)
- Face detection failure on a frame: skip silently
- Embedding extraction failure: log warning, skip frame
- bbolt write failure: log error, skip candidate (non-fatal)
- Promote to user with duplicate name: return 409 Conflict

### Testing

**Unit tests:**
- `storeCandidate` / `readCandidates` — round-trip storage
- `groupCandidates` — verify grouping with known embeddings
- `purgeExpiredCandidates` — verify TTL enforcement
- `promoteCandidateToUser` — verify user creation from candidate

**RTSP e2e tests:**
- Start collector via MQTT, verify candidates appear in `/candidates`
- Promote candidate to user, verify user appears in `/users`
- TTL expiry — set TTL to 0 days, verify candidates are purged

**E2E tests:**
- Candidate CRUD via API
- Promote creates valid enrolled user with embeddings
- Duplicate name on promote returns 409

## Files Changed

| File | Change |
|------|--------|
| `internal/server/server.go` | Candidate storage, grouping, promotion, collector lifecycle |
| `internal/server/face.go` | `FaceServer` struct: `collecting`, `collectMu`, `collectCancel` |
| `internal/server/types.go` | `Candidate`, `CandidateGroup`, `CandidatesListResponse` |
| `internal/server/rtsp.go` | `runCollectorLoop` — shared continuous RTSP loop |
| `internal/mqtt/bridge.go` | `collect` func field, HA discovery button, `collect` command case |
| `cmd/face-api/main.go` | New env vars, wire collect func to bridge |
| `internal/server/ui.go` | `handleUICandidates`, register `/ui/candidates` |
| `internal/server/templates/index.html` | "Collected Faces" dashboard card |
| `internal/server/templates/candidates.html` | New template |
| `internal/server/candidates_test.go` | Unit tests |
| `test/e2e/e2e_test.go` | E2E tests |
| `test/rtsp/rtsp_e2e_test.go` | RTSP e2e tests |
