# Audit + Candidate Unification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Record every RTSP/stream scan in the audit log, unify the audit table with candidate collection, and add promote-from-audit capability.

**Architecture:** Extend the existing `AuditEntry` model with a `Status` field, update the service layer to write audit entries for no-face and not-matched scans, add new API endpoints for the unified view, and merge the candidates page into the audit table with status badges and inline promote buttons.

**Tech Stack:** Go, bbolt, Go templates, PicoCSS v2 (vendored)

**Spec:** `docs/superpowers/specs/2026-09-28-audit-candidate-unification-design.md`

## Global Constraints

- Add `Status` field to `AuditEntry` with values `"matched"`, `"not_matched"`, `"no_face"`
- Backward compatible: entries without `Status` default to `"matched"` during reads
- No-face entries store the full RTSP frame image; not-matched entries store the cropped face image
- `TotalCollected` in stats = raw candidate count (not subtracting promoted)
- Merge candidates.html into audit.html; remove candidates page entirely
- Follow existing code patterns: `writeJSON`/`writeError`, zerolog, bbolt JSON serialization

## Review Focus

- **Migration of existing audit data**: entries without Status field must render as "matched" in the UI, not break the filter or table
- **Full frame storage for no-face**: the full frame can be large (base64 encoded); ensure the bbolt value doesn't exceed bbolt's 1MB limit gracefully
- **Promote from audit table**: the inline promote form must POST to the correct endpoint and handle the same success/failure paths as the candidates page
- **Filter by status**: the new `ListUnmatched` method must correctly filter only `not_matched` entries, excluding `matched` and `no_face`
- **Backward compat on stats**: `ComputeStats` must still count old entries correctly (treating them as `matched`) while also counting new entries by their Status field

---

### Task 1: Domain Model & Repository Status Support

**Files:**
- Modify: `internal/domain/models.go:36-45`
- Modify: `internal/domain/ports.go:60-67`
- Modify: `internal/repository/audit_repo.go:243-252`
- Test: `internal/repository/audit_repo_test.go` (new file)

**Interfaces:**
- Consumes: `domain.AuditEntry` struct (existing fields)
- Produces: `AuditEntry.Status string` field, `Stats.TotalCollected int` field, `AuditRepository.ListUnmatched(n int) ([]domain.AuditEntry, error)` method, `AuditRepository.CountAll()` method on domain models and repository

**Code constants:**
- `const auditStatusMatched = "matched"`
- `const auditStatusNotMatched = "not_matched"`
- `const auditStatusNoFace = "no_face"`

- [ ] **Step 1: Add `Status` field to `AuditEntry` and `Status` constants in `internal/domain/models.go`**

Add a `Status` string field after `Matched` on `AuditEntry`:
```go
type AuditEntry struct {
    Time       time.Time `json:"time"`
    Endpoint   string    `json:"endpoint"`
    Name       string    `json:"name"`
    Similarity float32   `json:"similarity"`
    Matched    bool      `json:"matched"`
    Status     string    `json:"status"`
    DurationMs int64     `json:"duration_ms"`
    FaceImage  string    `json:"face_image"`
}
```

Add constants:
```go
const AuditStatusMatched = "matched"
const AuditStatusNotMatched = "not_matched"
const AuditStatusNoFace = "no_face"
```

- [ ] **Step 2: Add `TotalCollected` to `Stats` in `internal/domain/ports.go`**

Add after `TotalNotMatched`:
```go
type Stats struct {
    TotalChecks     int       `json:"total_checks"`
    TotalMatched    int       `json:"total_matched"`
    TotalNoFace     int       `json:"total_no_face"`
    TotalNotMatched int       `json:"total_not_matched"`
    TotalCollected  int       `json:"total_collected"`
    LastMatched     *time.Time `json:"last_matched"`
}
```

- [ ] **Step 3: Write failing test for backward-compatible status default**

Create `internal/repository/audit_repo_test.go`:
```go
func TestAuditEntryStatusDefaultsToMatched(t *testing.T) {
    // Write an entry without Status field (simulate legacy data)
    // Read it back and verify Status defaults to "matched"
}

func TestAuditRepositoryListUnmatched(t *testing.T) {
    // Write entries with mixed statuses
    // Call ListUnmatched(10)
    // Verify only not_matched entries returned
}

func TestAuditRepositoryCountAll(t *testing.T) {
    // Write 3 entries
    // Call CountAll()
    // Verify returns 3
}
```

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./internal/repository/ -run TestAuditEntryStatusDefaultsToMatched -v`
Expected: FAIL — `Status` field not defined on `AuditEntry`

- [ ] **Step 5: Implement status support in `internal/repository/audit_repo.go`**

Update `auditEntryJSON` struct to include `Status` field:
```go
type auditEntryJSON struct {
    Time       string  `json:"time"`
    Endpoint   string  `json:"endpoint"`
    Name       string  `json:"name"`
    Similarity float32 `json:"similarity"`
    Matched    bool    `json:"matched"`
    Status     string  `json:"status"`
    DurationMs int64   `json:"duration_ms"`
    FaceImage  string  `json:"face_image"`
}
```

In all read paths (`Recent`, `ListPaginated`, `ComputeStats`), after unmarshaling, default empty/missing `Status` to `"matched"`:
```go
if j.Status == "" {
    j.Status = "matched"
}
```

Add `ListUnmatched` method:
```go
func (r *AuditRepository) ListUnmatched(n int) ([]domain.AuditEntry, error)
```
- Returns newest-first entries where `Status == "not_matched"`
- `n` defaults to 100 if ≤ 0

Add `CountAll` method to `CandidateRepository` in `internal/repository/candidate_repo.go`:
```go
func (r *CandidateRepository) CountAll() (int, error)
```
- Iterates bbolt `Candidates` bucket, returns count

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/repository/ -v`
Expected: all 3 tests PASS

- [ ] **Step 7: Commit**

```bash
git add internal/domain/models.go internal/domain/ports.go internal/repository/audit_repo.go internal/repository/candidate_repo.go
git commit -m "domain: add Status field to AuditEntry and TotalCollected to Stats"
```

---

### Task 2: Service Layer — Audit Writing for All Scan Types

**Files:**
- Modify: `internal/service/face_service.go:208-307`

**Interfaces:**
- Consumes: `AuditEntry.Status` constants from Task 1, `domain.RTSPReader` (existing)
- Produces: `CheckStreamImage` writes no-face audit entries with full frame, writes not-matched audit entries with cropped face, returns correct reason messages

- [ ] **Step 1: Write failing test for no-face audit entry**

Add test to `internal/service/face_service_test.go` (create if not exists):
```go
func TestCheckStreamImageNoFaceWritesAuditEntry(t *testing.T) {
    // Mock processor that returns no-face error with full frame image
    // Call CheckStreamImage(fullFrame)
    // Verify audit.Append was called with entry having Status="no_face"
    // Verify entry has full frame in FaceImage field
    // Verify entry has Name="" and Similarity=0
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/service/ -run TestCheckStreamImageNoFaceWritesAuditEntry -v`
Expected: FAIL — `AuditStatusNoFace` constant not defined / method doesn't write audit

- [ ] **Step 3: Update `CheckStreamImage` for no-face case**

In the `DetectAndCrop` error branch (around line 218-230):
```go
crop, err := s.processor.DetectAndCrop(imageData)
if err != nil {
    // ... existing connection error check ...
    
    // Write no-face audit entry with the full frame
    _ = s.audit.Append([]domain.AuditEntry{{
        Time:       time.Now(),
        Endpoint:   "stream-check",
        Name:       "",
        Similarity: 0,
        Matched:    false,
        Status:     domain.AuditStatusNoFace,
        DurationMs: time.Since(start).Milliseconds(),
        FaceImage:  string(imageData), // FULL FRAME for no-face
    }})
    
    s.logf("no face detected", "timestamp", time.Now().Format(time.RFC3339))
    return &domain.StreamCheckResult{
        OperationDuration: domain.OperationDuration{DurationMs: time.Since(start).Milliseconds()},
        Status:            "not ok",
        Reason:            "No face detected", // simpler message
    }, nil
}
```

- [ ] **Step 4: Write failing test for not-matched audit entry**

```go
func TestCheckStreamImageNotMatchedWritesAuditAndCandidate(t *testing.T) {
    // Mock processor that returns a crop with embedding
    // Mock user repo with users whose embeddings don't match
    // Call CheckStreamImage(cropData)
    // Verify audit.Append called with Status="not_matched", similarity set
    // Verify CollectStreamCandidate called with the crop
    // Verify reason is "Face detected but similarity below threshold"
}
```

- [ ] **Step 5: Run test to verify it fails**

Run: `go test ./internal/service/ -run TestCheckStreamImageNotMatchedWritesAuditAndCandidate -v`
Expected: FAIL — new audit writing and reason message not implemented

- [ ] **Step 6: Update `CheckStreamImage` for not-matched case**

Around line 270-298, change the not-matched return:
```go
// Audit entry (unchanged — already written above)
_ = s.audit.Append([]domain.AuditEntry{{
    Time:       time.Now(),
    Endpoint:   "stream-check",
    Name:       bestName,
    Similarity: highestScore,
    Matched:    matched,
    Status:     domain.AuditStatusMatched,
    DurationMs: dur.DurationMs,
    FaceImage:  string(crop.Data),
}})

if matched {
    return &domain.StreamCheckResult{...}, nil
}

// Auto-collect unmatched face as candidate (existing code, unchanged)
candidate := &domain.Candidate{...}
if err := s.CollectStreamCandidate(candidate); err != nil {
    // Log but don't fail the check
}

// Fix the reason message — was "No face detected within 3 seconds"
return &domain.StreamCheckResult{
    OperationDuration: dur,
    Status:            "not ok",
    Reason:            "Face detected but similarity below threshold",
    Similarity:        highestScore,
    Matched:           false,
    FaceImage:         string(crop.Data),
}, nil
```

Also update the matched-path audit entry to include `Status: domain.AuditStatusMatched`.

- [ ] **Step 7: Update `ComputeStats` to count by Status**

In `repository/audit_repo.go`, update the loop in `ComputeStats` to use the new `Status` field:
```go
for _, e := range entries {
    switch e.Status {
    case domain.AuditStatusMatched:
        stats.TotalMatched++
        if e.Time.After(lastMatched) {
            lastMatched = e.Time
        }
    case domain.AuditStatusNoFace:
        stats.TotalNoFace++
    case domain.AuditStatusNotMatched:
        stats.TotalNotMatched++
    }
    stats.TotalCollected++ // placeholder: will be candidate count
}
```

For `TotalCollected`, compute from candidate count (add in Task 3 when `CountAll` is available). For now, leave it as 0 in `ComputeStats` — it will be set in the API handler from `candidateRepo.CountAll()`.

- [ ] **Step 8: Run all tests to verify**

Run: `go test ./internal/service/ -v`
Expected: all tests PASS (including new + any existing)

- [ ] **Step 9: Commit**

```bash
git add internal/service/face_service.go internal/repository/audit_repo.go
git commit -m "service: write audit entries for no-face and not-matched scans"
```

---

### Task 3: API Layer — New Endpoints & Updated Stats

**Files:**
- Modify: `internal/http/handlers.go`
- Modify: `internal/domain/ports.go` (StatsResponse if needed)
- Modify: `internal/service/face_service.go` (Wire candidate count into stats)

**Interfaces:**
- Consumes: `AuditRepository.ListUnmatched(n int)` from Task 1, `CandidateRepository.CountAll()` from Task 1
- Produces: `GET /api/audit-unmatched` endpoint, updated `/api/stats` with `total_collected`, updated `/api/audit` response includes `status` field

- [ ] **Step 1: Add `CountAll` to the FaceService interface**

In `internal/domain/ports.go`, add to `FaceService`:
```go
CandidateCount() int
```

Implement in `face_service.go`:
```go
func (s *FaceServiceImpl) CandidateCount() int {
    count, err := s.candidates.CountAll()
    if err != nil {
        return 0
    }
    return count
}
```

- [ ] **Step 2: Write failing test for GET /api/audit-unmatched**

In `internal/http/handlers_test.go` (create if not exists):
```go
func TestGetAuditUnmatched(t *testing.T) {
    // Set up mock: 2 not_matched entries, 1 matched entry, 1 no_face entry
    // Call GET /api/audit-unmatched
    // Verify response contains only the 2 not_matched entries
    // Verify response has count field
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/http/ -run TestGetAuditUnmatched -v`
Expected: FAIL — endpoint not registered

- [ ] **Step 4: Implement `GET /api/audit-unmatched` endpoint**

In `handlers.go`, add handler and registration:
```go
func (h *Handlers) handleAuditUnmatched(w http.ResponseWriter, r *http.Request) {
    if r.Method != http.MethodGet {
        writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
        return
    }
    
    entries, err := h.svc.ListUnmatched(100)
    if err != nil {
        writeError(w, http.StatusInternalServerError, err.Error())
        return
    }
    
    writeJSON(w, http.StatusOK, map[string]interface{}{
        "entries": entries,
        "count":   len(entries),
    })
}
```

Register in `RegisterHandlers`:
```go
mux.HandleFunc("GET /api/audit-unmatched", h.handleAuditUnmatched)
```

- [ ] **Step 5: Update `/api/stats` to include `total_collected`**

In the stats handler (around line 407-422), add `total_collected` from `CandidateCount()`:
```go
writeJSON(w, http.StatusOK, map[string]interface{}{
    "total_checks":      stats.TotalChecks,
    "total_matched":     stats.TotalMatched,
    "total_no_face":     stats.TotalNoFace,
    "total_not_matched": stats.TotalNotMatched,
    "total_collected":   h.svc.CandidateCount(),
    "last_matched":      lastMatched,
})
```

- [ ] **Step 6: Run tests to verify**

Run: `go test ./internal/http/ -v`
Expected: all tests PASS

- [ ] **Step 7: Commit**

```bash
git add internal/http/handlers.go internal/domain/ports.go internal/service/face_service.go
git commit -m "api: add audit-unmatched endpoint and total_collected stats"
```

---

### Task 4: Web UI — Unified Audit Table

**Files:**
- Create: `internal/http/templates/audit.html` (rewrite)
- Remove: `internal/http/templates/candidates.html`
- Modify: `internal/http/templates/shared.html` (nav)
- Modify: `internal/http/templates/stats.html` (add total_collected)
- Modify: `internal/http/ui.go` (page data struct)
- Modify: `internal/http/handlers.go` (page data population)

**Interfaces:**
- Consumes: `GET /api/audit` endpoint (existing, returns entries with `status` field)
- Consumes: `GET /api/audit-unmatched` endpoint from Task 3
- Consumes: `GET /stats` endpoint (updated with `total_collected`)
- Produces: Unified audit table with status badges, inline promote form, status filter dropdown

- [ ] **Step 1: Update `uiPageData` in `internal/http/ui.go`**

Add field for unmatched entries:
```go
type uiPageData struct {
    // ... existing fields ...
    UnmatchedEntries []domain.AuditEntry
    TotalCollected   int
}
```

- [ ] **Step 2: Update the audit page handler to populate new fields**

In the audit handler (around where page data is set), add:
```go
unmatched, _ := s.audit.ListUnmatched(100)
data := uiPageData{
    // ... existing fields ...
    UnmatchedEntries: unmatched,
    TotalCollected:   candidateCount,
}
```

- [ ] **Step 3: Rewrite `templates/audit.html`**

The rewritten template should:
1. Keep existing filter UI (user, endpoint, matched radio)
2. Add a **Status filter**: a select with options: All, Matched, Not matched, No face
3. Render table rows with status badges:
   - Green `● Matched` for `status == "matched"`
   - Orange `● Not matched` for `status == "not_matched"` — with a "Promote" button
   - Red `● No face` for `status == "no_face"` — no action button, full frame preview
4. The "Promote" button opens an inline form (same pattern as candidates page bulk promote):
   ```html
   <button onclick="showPromoteForm(this)" data-id="{{.ID}}">Promote</button>
   <div class="promote-form" style="display:none" data-id="{{.ID}}">
       <input type="text" placeholder="User name" class="promote-input">
       <button class="btn-primary" onclick="promoteFace(this)">Create</button>
   </div>
   ```
5. JavaScript for promote: POST to `/candidates/promote` with `{name, candidate_id}`
6. Keep existing pagination and auto-refresh

- [ ] **Step 4: Update `templates/shared.html` navigation**

Remove the candidates nav link. Keep audit. The nav should look like:
```html
<a href="./">Dashboard</a>
<a href="audit">Audit</a>
<!-- other links -->
```

- [ ] **Step 5: Update `templates/stats.html`**

Add `Total Collected` to the stats display:
```html
<strong>Total Collected:</strong> {{.TotalCollected}}
```

- [ ] **Step 6: Remove `templates/candidates.html`**

```bash
rm internal/http/templates/candidates.html
```

- [ ] **Step 7: Update UI handler to remove candidates page reference**

Remove or redirect the candidates handler. Since candidates.html is deleted, the `/ui/candidates` route should either:
- Return 404, OR
- Redirect to `/ui/audit` (preferred)

- [ ] **Step 8: Build and test**

Run: `go build ./...`
Expected: clean build, no references to candidates.html

- [ ] **Step 9: Run full test suite**

Run: `go test ./... -count=1`
Expected: all tests PASS

- [ ] **Step 10: Commit**

```bash
git add internal/http/templates/audit.html internal/http/templates/candidates.html internal/http/templates/shared.html internal/http/templates/stats.html internal/http/ui.go internal/http/handlers.go
git commit -m "ui: merge candidates into audit table with status badges and promote"
```

---

### Task 5: E2E Verification & Integration Tests

**Files:**
- Modify: `test/e2e/e2e_test.go`
- Modify: `test/rtsp/rtsp_e2e_test.go`

**Interfaces:**
- Consumes: All API endpoints from Tasks 1-4
- Produces: Verified end-to-end flow: stream-check → no-face audit entry, stream-check → not-matched audit entry + candidate, promote from audit, stats with total_collected

- [ ] **Step 1: Add E2E test for no-face audit entry**

In `test/e2e/e2e_test.go`:
```go
func TestNoFaceAuditEntry_E2E(t *testing.T) {
    // POST /stream-check with solid-color image (no face)
    // Verify response status "not ok", reason "No face detected"
    // GET /api/audit
    // Verify entry exists with status="no_face", face_image present
}
```

- [ ] **Step 2: Add E2E test for not-matched audit entry + candidate**

```go
func TestNotMatchedAuditAndCandidate_E2E(t *testing.T) {
    // Enroll user A
    // POST /stream-check with image of person B (not enrolled)
    // Verify response status "not ok", reason contains "below threshold"
    // GET /api/audit — verify entry with status="not_matched"
    // GET /candidates — verify candidate created
    // GET /api/stats — verify total_collected >= 1
}
```

- [ ] **Step 3: Add E2E test for audit-unmatched endpoint**

```go
func TestAuditUnmatched_E2E(t *testing.T) {
    // Create 2 not_matched entries (via stream-check with unknown faces)
    // GET /api/audit-unmatched
    // Verify exactly 2 entries returned, both status="not_matched"
}
```

- [ ] **Step 4: Add RTSP E2E test for no-face audit**

In `test/rtsp/rtsp_e2e_test.go`:
```go
func TestRTSPNoFaceAuditEntry_E2E(t *testing.T) {
    // Stream solid-color frame via mock RTSP
    // POST /stream-check
    // Verify audit entry with status="no_face" and full frame image
}
```

- [ ] **Step 5: Run all E2E tests**

Run: `go test -v -count=1 ./test/e2e/... ./test/rtsp/...`
Expected: all tests PASS

- [ ] **Step 6: Commit**

```bash
git add test/e2e/e2e_test.go test/rtsp/rtsp_e2e_test.go
git commit -m "e2e: add tests for no-face audit, not-matched audit+candidate, audit-unmatched"
```

---

## Self-Review Checklist

1. **Spec coverage:**
   - Every RTSP scan → audit entry ✓ (Task 2)
   - Unified audit table ✓ (Task 4)
   - Status field with backward compat ✓ (Task 1)
   - Full frame for no-face ✓ (Task 2)
   - Cropped face for not-matched ✓ (Task 2)
   - Promote from audit table ✓ (Task 4)
   - Stats with total_collected ✓ (Task 3)
   - Error message fixes ✓ (Task 2)
   - Remove candidates page ✓ (Task 4)

2. **Step scan:** Each step has exactly one action with a checkable result.

3. **Type consistency:** All method signatures match between spec and tasks.

4. **Review Focus:** Each failure mode has a test:
   - Migration: Task 1 Step 3 test
   - Full frame storage: Task 2 Step 3 + Task 5 Step 1
   - Promote from audit: Task 4 Step 3 inline form
   - Filter by status: Task 5 Step 3
   - Backward compat stats: Task 2 Step 7

5. **Proportion:** Plan is ~3x spec length — appropriate for detailed task decomposition.
