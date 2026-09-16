# Face Audit Log - Baked Face Image Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Store the cropped face image as a base64-encoded string in each audit log entry when a face is detected.

**Architecture:** Add a `FaceImage` field to `AuditEntry`, modify `detectAndCrop112` to return the cropped Mat for encoding, add a base64 encoding helper, and update all 3 audit creation sites (enroll, recognize, stream-check) to capture and include the face image.

**Tech Stack:** Go, gocv (OpenCV), base64 encoding, bbolt

**Spec:** Bounded feature — audit log enhancement

## Global Constraints

- Audit entries are only written when a face is detected (no-face requests produce no entry)
- Audit entries are batched and flushed in a single bbolt transaction (never write inside per-frame loop)
- The cropped face is always 112×112 RGB (the ArcFace model input size)
- Base64 encoding must use standard (non-URL-safe) base64 with padding

---

### Task 1: Add `encodeFaceToBase64` helper

**Files:**
- Modify: `internal/server/face.go:341` (end of file)

**Interfaces:**
- Consumes: gocv.Mat (cropped 112×112 face)
- Produces: `func encodeFaceToBase64(mat gocv.Mat) (string, error)`

- [ ] **Step 1: Add `encodeFaceToBase64` function to `face.go`**

```go
// encodeFaceToBase64 encodes a gocv.Mat (BGR) to a base64-encoded JPEG string.
func encodeFaceToBase64(mat gocv.Mat) (string, error) {
	buf, err := gocv.IMEncode(gocv.JPEGFileExt, mat)
	if err != nil {
		return "", fmt.Errorf("encode face image: %w", err)
	}
	defer buf.Close()
	return base64.StdEncoding.EncodeToString(buf.ToBytes()), nil
}
```

Also add `"encoding/base64"` to the imports in `face.go`.

- [ ] **Step 2: Run `go build ./...` to verify compilation**

Run: `go build ./...`
Expected: PASS, no errors

- [ ] **Step 3: Commit**

```bash
git add internal/server/face.go
git commit -m "feat: add encodeFaceToBase64 helper for audit log"
```

---

### Task 2: Modify `detectAndCrop112` to return the cropped Mat

**Files:**
- Modify: `internal/server/face.go:198`

**Interfaces:**
- Consumes: gocv.Mat (input frame)
- Produces: `(gocv.Mat, [4]float32, error)` — cropped Mat + bbox + error (caller must Close the Mat)

- [ ] **Step 1: Change `detectAndCrop112` return signature**

Change the function signature from:
```go
func (s *FaceServer) detectAndCrop112(mat gocv.Mat) (gocv.Mat, [4]float32, error)
```
to:
```go
func (s *FaceServer) detectAndCrop112(mat gocv.Mat) (gocv.Mat, [4]float32, error)
```

Wait — the signature is already `(gocv.Mat, [4]float32, error)`. The cropped Mat is already returned. **No change needed to the function signature.** The cropped 112×112 Mat is already available to callers.

- [ ] **Step 2: Verify no change needed**

The function already returns the cropped Mat. Callers currently do `defer cropped.Close()` after receiving it. This is correct — we just need callers to encode it before closing.

- [ ] **Step 3: Commit (no-op, just confirming)**

```bash
git add -A
git commit -m "chore: confirm detectAndCrop112 already returns cropped Mat"
```

---

### Task 3: Add `FaceImage` field to `AuditEntry`

**Files:**
- Modify: `internal/server/types.go:35-42`

**Interfaces:**
- Consumes: nothing
- Produces: `AuditEntry` with new `FaceImage string` field (JSON: `"face_image"`)

- [ ] **Step 1: Add `FaceImage` field to `AuditEntry` struct**

```go
// AuditEntry records a single face scan attempt for the audit log.
type AuditEntry struct {
	Time       time.Time `json:"time"`
	Endpoint   string    `json:"endpoint"` // enroll | recognize | stream-check
	Name       string    `json:"name"`
	Similarity float32   `json:"similarity"`
	Matched    bool      `json:"matched"`
	DurationMs int64     `json:"duration_ms"`
	FaceImage  string    `json:"face_image,omitempty"`
}
```

- [ ] **Step 2: Run `go build ./...` to verify compilation**

Run: `go build ./...`
Expected: PASS, no errors

- [ ] **Step 3: Commit**

```bash
git add internal/server/types.go
git commit -m "feat: add FaceImage field to AuditEntry"
```

---

### Task 4: Update enroll handler to include face image in audit

**Files:**
- Modify: `internal/server/server.go:253-301`

**Interfaces:**
- Consumes: cropped Mat from `detectAndCrop112`
- Produces: audit entry with `FaceImage` populated

- [ ] **Step 1: Capture face image before defer-close in `handleEnroll`**

After line 259 (`defer cropped.Close()`), add encoding before the defer takes effect. The key insight: we need to encode the cropped face into a string, then the defer will close the Mat. Change the audit entry creation (lines 292-300) to include the face image:

```go
// After line 259 (defer cropped.Close()), add:
faceImage, imgErr := encodeFaceToBase64(cropped)
if imgErr != nil {
    s.log.Error().Err(imgErr).Str("name", name).Msg("failed to encode face image")
}

// Then in the audit entry (line 292-300), add FaceImage:
if err := s.storeAudit([]AuditEntry{{
    Time:       time.Now(),
    Endpoint:   "enroll",
    Name:       name,
    Similarity: 1.0,
    Matched:    true,
    DurationMs: time.Since(start).Milliseconds(),
    FaceImage:  faceImage,
}}); err != nil {
```

- [ ] **Step 2: Run `go build ./...` to verify compilation**

Run: `go build ./...`
Expected: PASS, no errors

- [ ] **Step 3: Commit**

```bash
git add internal/server/server.go
git commit -m "feat: include face image in enroll audit entry"
```

---

### Task 5: Update recognize handler to include face image in audit

**Files:**
- Modify: `internal/server/server.go:329-378`

**Interfaces:**
- Consumes: cropped Mat from `detectAndCrop112`
- Produces: audit entry with `FaceImage` populated

- [ ] **Step 1: Capture face image before defer-close in `handleRecognize`**

After line 335 (`defer cropped.Close()`), add encoding. Then add `FaceImage` to the audit entry at lines 369-377:

```go
// After line 335 (defer cropped.Close()), add:
faceImage, imgErr := encodeFaceToBase64(cropped)
if imgErr != nil {
    s.log.Error().Err(imgErr).Msg("failed to encode face image")
}

// Then in the audit entry (line 369-377), add FaceImage:
if err := s.storeAudit([]AuditEntry{{
    Time:       time.Now(),
    Endpoint:   "recognize",
    Name:       result.Name,
    Similarity: maxScore,
    Matched:    matched,
    DurationMs: result.DurationMs,
    FaceImage:  faceImage,
}}); err != nil {
```

- [ ] **Step 2: Run `go build ./...` to verify compilation**

Run: `go build ./...`
Expected: PASS, no errors

- [ ] **Step 3: Commit**

```bash
git add internal/server/server.go
git commit -m "feat: include face image in recognize audit entry"
```

---

### Task 6: Update stream-check handler to include face image in audit

**Files:**
- Modify: `internal/server/server.go:459-489`

**Interfaces:**
- Consumes: cropped Mat from `detectAndCrop112`
- Produces: audit entry with `FaceImage` populated

- [ ] **Step 1: Capture face image before cropped.Close() in stream-check loop**

In the ticker case (lines 452-500), after line 467 (`cropped.Close()`), we need to encode BEFORE closing. Change the flow:

```go
// Lines 459-470: change from:
// cropped, _, err := s.detectAndCrop112(img)
// img.Close()
// if err != nil { cropped.Close(); continue }
// queryVec, err := s.extractEmbedding(cropped)
// cropped.Close()

// To:
cropped, _, err := s.detectAndCrop112(img)
img.Close()
if err != nil {
    cropped.Close()
    continue
}

// Encode face image before extracting embedding and closing
faceImage, imgErr := encodeFaceToBase64(cropped)
if imgErr != nil {
    s.log.Error().Err(imgErr).Msg("failed to encode face image")
}

queryVec, err := s.extractEmbedding(cropped)
cropped.Close()
if err != nil {
    continue
}

// Then in the pendingAudits append (line 482-489), add FaceImage:
pendingAudits = append(pendingAudits, AuditEntry{
    Time:       time.Now(),
    Endpoint:   "stream-check",
    Name:       bestMatch,
    Similarity: highestScore,
    Matched:    highestScore >= s.threshold,
    DurationMs: dur().DurationMs,
    FaceImage:  faceImage,
})
```

- [ ] **Step 2: Run `go build ./...` to verify compilation**

Run: `go build ./...`
Expected: PASS, no errors

- [ ] **Step 3: Commit**

```bash
git add internal/server/server.go
git commit -m "feat: include face image in stream-check audit entries"
```

---

### Task 7: Update e2e tests to assert face_image presence

**Files:**
- Modify: `test/e2e/e2e_test.go`

**Interfaces:**
- Consumes: AuditEntry with FaceImage field
- Produces: test assertions that face_image is non-empty for detected-face entries

- [ ] **Step 1: Add assertion for face_image in audit entries**

In the audit test function (around line 544-579), after verifying entries exist, add:

```go
// Check that each entry with a matched face has a non-empty face_image
for _, entry := range out.Entries {
    if entry.Matched && entry.FaceImage == "" {
        t.Errorf("matched audit entry for %q has empty face_image", entry.Name)
    }
}
```

Also update the no-face test (around line 386-391) — no-face scans still produce no audit entries, so no change needed there.

- [ ] **Step 2: Run e2e tests**

Run: `go test -v -count=1 ./test/e2e/...`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add test/e2e/e2e_test.go
git commit -m "test: assert face_image is present in matched audit entries"
```

---

### Task 8: Run full test suite

**Files:**
- No file changes

**Interfaces:**
- Consumes: all previous changes
- Produces: green test suite

- [ ] **Step 1: Run all tests**

Run: `make test`
Expected: PASS (both e2e and rtsp suites)

- [ ] **Step 2: Run format**

Run: `make format`
Expected: PASS

- [ ] **Step 3: Final commit if needed**

```bash
git add -A
git commit -m "chore: format and final cleanup"
```
