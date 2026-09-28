# Design: Unified Audit + Candidate Intake Workflow

**Date:** 2026-09-28
**Status:** Draft
**Author:** AI agent

## 1. Problem

The current face-api system has two separate concerns that are hard to operate together:

1. **Audit log only records matched faces.** No-face attempts and not-matched detections are not recorded, making it impossible to review why face detection failed or to identify unknown faces captured by the camera.
2. **Collected faces (candidates) are isolated from audit.** The candidates page shows unknown faces but provides no context (what endpoint, what similarity, what the full frame looked like). Operators cannot promote faces directly from the candidates page.
3. **Misleading error messages.** When a face is detected but doesn't match any enrolled user, `CheckStreamImage` returns the reason "No face detected within 3 seconds" — which is factually wrong (a face WAS detected, it just didn't match).

## 2. Goals

- Record **every RTSP/stream scan attempt** in the audit log, including no-face and not-matched cases.
- **Unify the audit table and candidates view** so operators can see all face detections in one place and promote any unrecognized face to a user.
- Fix misleading error messages in stream-check results.
- Add a `total_collected` metric to the stats dashboard.

## 3. Non-Goals

- Change the face detection or recognition algorithms.
- Modify the MQTT bridge behavior (it continues to call `CheckStream` as before).
- Change the existing candidate auto-collection logic (it already exists).
- Add real-time face matching notifications beyond what MQTT already provides.

## 4. Data Model Changes

### 4.1 AuditEntry (domain/models.go)

Add a `Status` field to distinguish entry types:

```go
type AuditEntry struct {
    Time       time.Time `json:"time"`
    Endpoint   string    `json:"endpoint"`
    Name       string    `json:"name"`
    Similarity float32   `json:"similarity"`
    Matched    bool      `json:"matched"`
    Status     string    `json:"status"` // "matched", "not_matched", "no_face"
    DurationMs int64     `json:"duration_ms"`
    FaceImage  string    `json:"face_image"` // base64-encoded image
}
```

**Status values:**
- `"matched"` — face detected and similarity ≥ threshold (existing behavior, written implicitly via current code)
- `"not_matched"` — face detected but similarity < threshold (new)
- `"no_face"` — no face detected above detection threshold (new)

**Backward compatibility:** Existing entries in bbolt will not have a `Status` field. The repository layer will default missing/empty `Status` to `"matched"` during reads. This ensures old data doesn't break the UI.

### 4.2 Stats (domain/ports.go)

Add a new metric:

```go
type Stats struct {
    TotalChecks     int       `json:"total_checks"`
    TotalMatched    int       `json:"total_matched"`
    TotalNoFace     int       `json:"total_no_face"`
    TotalNotMatched int       `json:"total_not_matched"`
    TotalCollected  int       `json:"total_collected"` // unpromoted candidates
    LastMatched     *time.Time `json:"last_matched"`
}
```

`TotalCollected` counts candidates in the `Candidates` bucket that have not been promoted to users (i.e., the current candidate count minus any that were bulk-promoted). For simplicity, this is computed at request time from the candidate repository.

## 5. Service Layer Changes

### 5.1 CheckStreamImage (service/face_service.go)

**Current flow:**
1. `DetectAndCrop` → if error, return "No face detected" (no audit entry)
2. Recognition → if matched, write audit, return "ok"
3. If not matched, auto-collect to candidates, write audit with reason "No face detected" (incorrect)

**New flow:**
1. `DetectAndCrop` → if error:
   - Write audit entry: `Status: "no_face"`, `Name: ""`, `Similarity: 0`, `Matched: false`, `FaceImage: <full_frame_base64>`
   - Return `StreamCheckResult` with reason "No face detected"
2. Recognition → compute best similarity:
   - If matched (`similarity >= threshold`): write audit with `Status: "matched"`, return "ok" (unchanged)
   - If not matched: write audit with `Status: "not_matched"`, `Name: ""`, `Similarity: <best_score>`, `Matched: false`, `FaceImage: <crop_base64>`
   - Auto-collect to candidates (existing `CollectStreamCandidate` call, unchanged)
   - Return `StreamCheckResult` with reason "Face detected but similarity below threshold"

**Key change:** The caller (HTTP handler / MQTT bridge) passes the full frame image to `CheckStreamImage` when no face is detected, so the service can write the full frame to the audit entry. For the matched/not-matched cases, the cropped face is already available in `crop.Data`.

### 5.2 CheckStream (service/face_service.go)

No change to the signature. The caller (HTTP handler / MQTT bridge) determines what image data to pass:
- On detection error: pass the full RTSP frame
- On success: pass the cropped face

### 5.3 CollectStreamCandidate (service/face_service.go)

No change. The existing deduplication logic (check against existing candidates and enrolled users) continues to work as-is.

## 6. Repository Layer Changes

### 6.1 AuditRepository.Append (repository/audit_repo.go)

Update `auditEntryJSON` to include `Status`:

```go
type auditEntryJSON struct {
    Time       string  `json:"time"`
    Endpoint   string  `json:"endpoint"`
    Name       string  `json:"name"`
    Similarity float32 `json:"similarity"`
    Matched    bool    `json:"matched"`
    Status     string  `json:"status"` // "matched", "not_matched", "no_face"
    DurationMs int64   `json:"duration_ms"`
    FaceImage  string  `json:"face_image"`
}
```

Read paths (`Recent`, `ListPaginated`, `ComputeStats`) default empty/missing `Status` to `"matched"` for backward compatibility.

### 6.2 AuditRepository.ComputeStats (repository/audit_repo.go)

Add `TotalCollected` to the returned `Stats`. This is computed by:
1. Counting all candidates in the `Candidates` bucket
2. Subtracting candidates that were promoted (tracked by counting how many unique candidate IDs appear in promoted-user pictures)

**Simplification:** For the first iteration, `TotalCollected` returns the raw candidate count. Tracking promoted candidates would require additional bbolt state. This is acceptable because the candidates page shows the live count, and the stats dashboard is an approximation.

### 6.3 New method: AuditRepository.ListUnmatched(n int)

New method to return only not-matched audit entries (newest first), used by the unified UI:

```go
func (r *AuditRepository) ListUnmatched(n int) ([]domain.AuditEntry, error)
```

### 6.4 New method: CandidateRepository.CountAll()

Convenience method:

```go
func (r *CandidateRepository) CountAll() (int, error)
```

## 7. API Layer Changes

### 7.1 New endpoint: GET /api/audit-unmatched

Returns the latest N not-matched entries (default 100), used by the unified UI to populate the "unmatched faces" section.

**Response:**

```json
{
  "entries": [
    {
      "time": "2026-09-28T12:00:00Z",
      "endpoint": "stream-check",
      "name": "",
      "similarity": 0.312,
      "matched": false,
      "status": "not_matched",
      "duration_ms": 450,
      "face_image": "base64..."
    }
  ],
  "count": 100
}
```

### 7.2 Update: GET /api/audit

The existing paginated audit endpoint remains unchanged. The `Status` field is now included in each entry. The frontend can use this field to display status badges and filter.

### 7.3 Update: GET /stats

The `/stats` response now includes `total_collected` (count of candidates in the repository).

## 8. Web UI Changes

### 8.1 Merge candidates into audit page

**Remove:** `templates/candidates.html`
**Update:** `templates/audit.html` to show all entries with status-aware rendering.

**Table columns:**

| Time | Status | Endpoint | Name | Similarity | Face | Action |
|------|--------|----------|------|------------|------|--------|
| 2026-09-28 12:00 | <span style="color:green">● Matched</span> | stream-check | john | 0.78 | [thumb] | — |
| 2026-09-28 12:01 | <span style="color:orange">● Not matched</span> | stream-check | — | 0.31 | [thumb] | [Promote] |
| 2026-09-28 12:02 | <span style="color:red">● No face</span> | stream-check | — | — | [full frame] | [Review] |

**Status badges:**
- **Matched** (green): face detected and recognized
- **Not matched** (orange): face detected but below threshold — has a "Promote" button
- **No face** (red): no face detected — shows the full frame for review, no action button

**Promote flow:**
- Clicking "Promote" on a not_matched entry opens a small inline form (same as the candidates page bulk promote): input field for name + "Create User" button
- POST to the existing `/candidates/promote` endpoint
- On success, the entry is removed from the not_matched section (the page re-renders)

### 8.2 Navigation update (templates/shared.html)

- Remove the "Collected Faces" nav link (was pointing to `candidates.html`)
- The "Audit" nav link remains, now showing the unified view

### 8.3 Stats page update (templates/stats.html)

Add `Total Collected` to the stats display (shows count of unpromoted candidates).

### 8.4 Page data struct (http/ui.go)

Update `uiPageData` to include `UnmatchedEntries []domain.AuditEntry` for the audit template.

## 9. Error Message Fixes

### 9.1 Stream check reason (not matched)

**Before:** `"No face detected within 3 seconds"` (when face WAS detected but similarity < threshold)
**After:** `"Face detected but similarity below threshold"`

### 9.2 Stream check reason (no face)

**Before:** `"No face detected within 3 seconds"` (correct but misleading when it's a full-frame issue)
**After:** `"No face detected"` (simpler, consistent with the `Status: "no_face"` audit entry)

### 9.3 Stream check reason (RTSP error)

**Before:** `"Failed to connect to RTSP stream"` (correct)
**After:** Unchanged — this is still the right message when RTSP connection fails. No audit entry is written in this case (the caller handles it).

## 10. File Change Summary

| File | Change |
|------|--------|
| `internal/domain/models.go` | Add `Status` field to `AuditEntry` |
| `internal/domain/ports.go` | Add `TotalCollected` to `Stats` |
| `internal/service/face_service.go` | Fix `CheckStreamImage` flow: write no-face audit, write not-matched audit, fix reason messages |
| `internal/repository/audit_repo.go` | Add `Status` to JSON struct, default to `"matched"` for legacy, add `ListUnmatched()`, update `ComputeStats` |
| `internal/repository/candidate_repo.go` | Add `CountAll()` |
| `internal/http/handlers.go` | Add `GET /api/audit-unmatched`, update stream-check handler to write no-face audit, update stats handler |
| `internal/http/ui.go` | Update `uiPageData` for merged view, add `UnmatchedEntries` field |
| `internal/http/templates/audit.html` | Rewrite: status badges, promote buttons, filter by status |
| `internal/http/templates/candidates.html` | Remove (merged into audit) |
| `internal/http/templates/shared.html` | Update nav: remove candidates link |
| `internal/http/templates/stats.html` | Add `total_collected` display |

## 11. Testing Plan

### 11.1 Unit tests (service)
- `CheckStreamImage` with face detected + matched → verify audit entry has `Status: "matched"`
- `CheckStreamImage` with face detected + not matched → verify audit entry has `Status: "not_matched"`, similarity set, and candidate created
- `CheckStreamImage` with no face detected → verify audit entry has `Status: "no_face"`, full frame image stored
- `CheckStreamImage` with RTSP connection error → verify NO audit entry (caller handles)

### 11.2 Unit tests (repository)
- `Append` with `Status` field → verify round-trip read returns correct status
- `ListPaginated` with `Status` filter → verify only matching entries returned
- `ComputeStats` with mixed statuses → verify all counts correct including `TotalCollected`

### 11.3 E2E tests
- Stream-check with matched face → verify audit entry in `/api/audit` with status badge
- Stream-check with not-matched face → verify audit entry + candidate created
- Stream-check with no-face → verify audit entry with full frame, no candidate created
- `/api/audit-unmatched` → verify only not_matched entries returned
- Promote from audit table → verify candidate removed, user created

### 11.4 RTSP tests
- Existing RTSP E2E tests should pass (backward compatible status default)
- New test: stream-check with no-face → verify audit entry count increases by 1

## 12. Migration Notes

### 12.1 bbolt data
- Existing audit entries without `Status` will default to `"matched"` on read — no data migration needed.
- Existing `no_face` entries (where `Matched: false` and `Name: ""` and `Similarity: 0`) will also default to `"matched"` for backward compatibility, but the UI filter for "not matched" will still match them (since `Matched` is false). This is a soft migration.

### 12.2 UI breaking changes
- The `candidates.html` page is removed. The `/ui/candidates` URL will return a redirect to `/ui/audit`.
- The nav bar changes: "Collected Faces" link removed, "Audit" remains.
