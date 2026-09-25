# Stats Feature — Design Spec

**Status**: implemented + documented
**Date**: 2026-09-25

---

## 1. Intent

Provide aggregate statistics from the audit log: total checks, matched count, no-face count, not-matched count, and last matched timestamp.

## 2. Data Source

Stats are computed from the `Audit` bbolt bucket. Every audit entry records:
- `endpoint`: The API endpoint that triggered the scan
- `matched`: Whether the face was matched to an enrolled user
- `name`: The matched user name (empty if no face or not matched)

## 3. Statistics

| Metric | Calculation | Example |
|--------|-------------|---------|
| `total_checks` | Count of all audit entries | 456 |
| `total_matched` | Count where `matched=true` | 234 |
| `total_no_face` | Count where `name=""` and `endpoint="stream-check"` | 89 |
| `total_not_matched` | Count where `matched=false` and `name!=""` | 133 |
| `last_matched` | Timestamp of most recent `matched=true` entry | `2026-09-25T10:45:00Z` |

## 4. Implementation

### 4.1 Repository Layer

`internal/repository/audit_repo.go` — `ComputeStats()` method:

```go
func (r *AuditRepository) ComputeStats() (domain.Stats, error)
```

- Opens bbolt read transaction
- Iterates the `"Audit"` bucket
- Accumulates counters in a single pass
- Tracks the most recent `matched` entry's timestamp
- Returns `domain.Stats` struct

### 4.2 Service Layer

`internal/service/face_service.go` — `ComputeStats()` method:

```go
func (s *FaceService) ComputeStats() (domain.Stats, error)
```

- Calls `auditRepo.ComputeStats()`
- Returns domain stats (no transformation needed)

### 4.3 HTTP Handler

`internal/http/handlers.go` — `handleListStats`:

```go
func (h *Handlers) handleListStats(w http.ResponseWriter, r *http.Request)
```

- Calls `svc.ComputeStats()`
- Converts `time.Time` to RFC3339 string for `last_matched`
- Returns `domain.StatsResponse` JSON

### 4.4 UI Handler

`internal/http/ui.go` — `handleUIStats`:

```go
func (h *UIHandler) handleUIStats(w http.ResponseWriter, r *http.Request)
```

- Calls `svc.ComputeStats()`
- Renders `stats.html` template with stats data

## 5. Performance

Stats computation iterates all audit entries in a single bbolt read transaction. For typical workloads (< 10K entries), this is < 5ms. The computation is not cached — it's always computed fresh from the audit log.

## 6. API Endpoint

```
GET /stats
```

Returns `domain.StatsResponse` JSON.

## 7. Dashboard Integration

The stats are displayed inline in the dashboard header (not as a separate card):
- Total checks as a badge
- Matched percentage as a badge
- Last matched time as a badge
- These appear next to the clock in the header
