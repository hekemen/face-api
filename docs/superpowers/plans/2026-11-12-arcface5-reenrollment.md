# ArcFace5 Alignment + Re-Enrollment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Switch the Pi deployment to `arcface5` crop alignment and give the operator a way to re-enroll every user in the new embedding space, so genuine matches separate cleanly from strangers.

**Architecture:** `arcface5` warps the five SCRFD landmarks onto the ArcFace template before embedding, so the face lands at a consistent scale and offset in the 112×112 crop. The measured effect is a 52% wider gap between a genuine match and a stranger. The stored enrollments are in the old (`bbox`) space, so they are replaced rather than reinterpreted: the re-enrollment tooling deletes a user's old record and re-creates it from fresh 5 MP stills, one user at a time, so the system is never in a half-migrated state.

**Tech Stack:** Go 1.27, bbolt, ONNX Runtime (purego), gortsplib/v5, Helm.

**Spec:** `docs/superpowers/analysis/2026-11-12-match-quality.md` (the measurements this plan acts on).

## Global Constraints

- The Pi deployment is only ever touched through `--kubeconfig ~/.kube/config.pi`. Never the default kubeconfig — that points at a different, production cluster.
- `FACE_ALIGNMENT` is read once at startup (`cmd/face-api/main.go`) and passed to `service.NewFaceProcessor`; there is no runtime switch. Changing it requires a restart.
- `arcface5` and `bbox` embeddings are different spaces. A `bbox` enrollment must never be compared to an `arcface5` capture and counted as a match.
- Enrollments must come from the **5 MP main stream**, not the 640×480 sub-stream. A face at 3 m is 50 px in the sub-stream and 200 px in the main stream.
- `MaxPicturesPerUser` is 5 (`internal/domain/models.go`).
- The camera's keyframe (IDR) arrives every ~2 s, so a fresh 5 MP grab takes ~1.7 s.
- Do not commit `AGENTS.md` or `deploy/helm/face-api/values.pi.yaml` (both gitignored).
- The camera's RTSP credentials live in the `face-api-secrets` secret; never write them into a committed file or a log line. Use `config.RedactURL` for anything printed.

## Review Focus

- A user whose old `bbox` enrollment still exists and who is then matched against a new `arcface5` capture — the system must not report that as a match.
- A re-enrollment that fails partway (a grab with no face in it) — the old enrollment must not be destroyed before the new one is confirmed.
- A capture with more than one face in frame — the detector picks the highest-scoring one; the enrollment must not silently become a different person than the one the operator intended.
- A user with no photos yet (deleted, then the grab failed) — the API must not report them as enrolled with zero pictures.
- Re-enrolling the same user twice — the second run must replace, not append beyond the 5-picture cap.

---

### Task 1: Mark each enrollment with the alignment mode it was made in

The stored user record has no indication of which alignment produced its embeddings. Without that, a mode switch is indistinguishable from a broken match, and a stale `bbox` enrollment is indistinguishable from a fresh `arcface5` one.

**Files:**
- Modify: `internal/domain/models.go` (add `Alignment` to `User`)
- Modify: `internal/repository/user_repo.go` (add `Alignment` to `storedUser`, read and write it)
- Create: `internal/repository/user_repo_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `domain.User.Alignment string` — the alignment mode that produced this user's embeddings (`""` for records written before this change), and `storedUser.Alignment string` with the same JSON key `alignment`.

- [ ] **Step 1: Write the failing test**

Create `internal/repository/user_repo_test.go`:

```go
package repository

import (
	"testing"

	"h2hsecure.com/face/internal/domain"

	bolt "go.etcd.io/bbolt"
)

// openUserTestDB creates a temporary bbolt database with the Faces bucket.
func openUserTestDB(t *testing.T) *bolt.DB {
	t.Helper()
	tmpFile := t.TempDir() + "/test.db"
	db, err := bolt.Open(tmpFile, 0600, nil)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfExists([]byte(facesBucket))
		return err
	}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestUserAlignmentRoundTrips verifies that the alignment mode a user was
// enrolled with survives a save and load.
func TestUserAlignmentRoundTrips(t *testing.T) {
	db := openUserTestDB(t)
	repo := NewUserRepository(db)

	err := repo.Save(&domain.User{
		Name:       "alice",
		Embeddings: []domain.FaceEmbedding{{1, 0, 0}},
		Alignment:  "arcface5",
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := repo.GetByName("alice")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.Alignment != "arcface5" {
		t.Fatalf("Alignment = %q, want arcface5", got.Alignment)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/repository/ -count=1 -run TestUserAlignmentRoundTrips -v`
Expected: FAIL with "unknown field Alignment" (compile error).

- [ ] **Step 3: Implement the field**

In `internal/domain/models.go`, add to `User`:

```go
type User struct {
	Name       string
	Embeddings []FaceEmbedding
	Pictures   []string // base64-encoded cropped face JPEGs (up to MaxPicturesPerUser)
	UpdatedAt  time.Time
	// Alignment records the crop mode that produced this user's embeddings
	// ("bbox" or "arcface5"). Embeddings from different modes are not
	// comparable, so a mismatch must be detected rather than assumed away.
	Alignment string
}
```

In `internal/repository/user_repo.go`, add to `storedUser`:

```go
type storedUser struct {
	Embeddings []domain.FaceEmbedding `json:"embeddings"`
	Pictures   []string               `json:"pictures,omitempty"`
	UpdatedAt  string                 `json:"updated_at,omitempty"`
	Alignment  string                 `json:"alignment,omitempty"`
}
```

Set `Alignment: u.Alignment` in `Save`, and read it into the returned `domain.User` in both `GetByName` and `ListAll`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/repository/ -count=1 -run TestUserAlignmentRoundTrips -v`
Expected: PASS.

- [ ] **Step 5: Run the full repository suite**

Run: `go test ./internal/repository/ -count=1`
Expected: PASS, no other test broken.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/models.go internal/repository/user_repo.go internal/repository/user_repo_test.go
git commit -m "feat(repository): record the alignment mode of each enrollment"
```

---

### Task 2: Stamp new enrollments with the active alignment mode, and refuse cross-space matches

Task 1 records the mode; this task writes it and enforces it. A user enrolled in `bbox` must not be reported as a match against an `arcface5` capture — the scores across spaces are not comparable (measured: a `bbox` enrollment scores 0.80 against the *same photo* in `arcface5` space, which would look like a strong match but means nothing).

**Files:**
- Modify: `internal/service/face_service.go` (`EnrollImage` stamps the mode; `RecognizeImage` and `ScanBurst` skip users whose mode differs)
- Modify: `internal/service/face_processor.go` (expose the processor's alignment mode)
- Modify: `internal/domain/ports.go` (add `AlignmentMode() string` to the `FaceProcessor` port)
- Test: `internal/service/burst_test.go`, `internal/service/face_service_test.go`

**Interfaces:**
- Consumes: `domain.User.Alignment` (Task 1).
- Produces: `domain.FaceProcessor.AlignmentMode() string` — the active alignment mode (`"bbox"` or `"arcface5"`); `FaceServiceImpl` reads it to stamp enrollments and to filter candidates.

- [ ] **Step 1: Write the failing test**

Add to `internal/service/burst_test.go`:

```go
// TestScanBurstSkipsUsersFromAnotherAlignmentSpace checks that a user
// enrolled in a different alignment mode is not matched. The scores across
// the two spaces are not comparable, so such a user must be skipped
// entirely rather than compared.
func TestScanBurstSkipsUsersFromAnotherAlignmentSpace(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0, 0}
	alice := &domain.User{
		Name:       "alice",
		Embeddings: []domain.FaceEmbedding{emb},
		Alignment:  "bbox", // enrolled in the old space
	}
	crop := &domain.FaceCrop{Data: []byte("crop"), Embedding: emb}
	proc := &burstProcessor{crop: crop}
	grabber := &burstGrabber{w: 640, h: 480}
	users := &burstUserRepo{users: []*domain.User{alice}}
	audit := &mockAuditRepo{}
	s := New(users, audit, proc, 0.5).WithFrameGrabber(grabber).WithBurst(200*time.Millisecond, 20*time.Millisecond)
	s.alignment = "arcface5" // the active mode

	res, err := s.ScanBurst(context.Background())
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if res.Matched {
		t.Fatalf("Matched = true for a user enrolled in another alignment space")
	}
}
```

The fake processor in `burst_test.go` must report a mode. Add to `burstProcessor`:

```go
func (m *burstProcessor) AlignmentMode() string { return "arcface5" }
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/service/ -count=1 -run TestScanBurstSkipsUsersFromAnotherAlignmentSpace -v`
Expected: FAIL — the burst matches the cross-space user.

- [ ] **Step 3: Implement the filtering**

In `internal/domain/ports.go`, add to the `FaceProcessor` port:

```go
// AlignmentMode returns the crop mode the processor embeds with ("bbox" or
// "arcface5"). Embeddings from different modes are not comparable.
AlignmentMode() string
```

In `internal/service/face_processor.go`, add:

```go
// AlignmentMode returns the crop mode this processor embeds with.
func (p *FaceProcessorImpl) AlignmentMode() string { return string(p.align) }
```

In `internal/service/face_service.go`:

- Add a field `alignment string` to `FaceServiceImpl`, set in `New` from `processor.AlignmentMode()`.
- In `EnrollImage`, set `Alignment: s.alignment` on the `domain.User` it saves.
- In `RecognizeImage` and `ScanBurst`, skip a user whose `Alignment` is non-empty and differs from `s.alignment`:

```go
// usersInActiveSpace returns the users whose embeddings are comparable with
// the active alignment mode. Enrollments made in another mode are skipped:
// their scores are not comparable and would produce meaningless matches.
func (s *FaceServiceImpl) usersInActiveSpace(users []*domain.User) []*domain.User {
	out := users[:0:0]
	for _, u := range users {
		if u.Alignment != "" && u.Alignment != s.alignment {
			continue
		}
		out = append(out, u)
	}
	return out
}
```

Apply it to the `users` slice in both `RecognizeImage` and `ScanBurst` right after `ListAll`.

The fake processors in the service tests must implement the new port method.
In `internal/service/face_service_test.go`, add to `mockProcessor`:

```go
func (m *mockProcessor) AlignmentMode() string { return "bbox" }
```

In `internal/service/burst_test.go`, add to `burstProcessor`:

```go
func (m *burstProcessor) AlignmentMode() string { return "arcface5" }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/service/ -count=1 -run TestScanBurstSkipsUsersFromAnotherAlignmentSpace -v`
Expected: PASS.

- [ ] **Step 5: Run the full service suite**

Run: `go test ./internal/service/ -count=1`
Expected: PASS. If `TestScanBurstReturnsFirstMatch` fails because its fake user has no `Alignment`, set `Alignment: "arcface5"` on that test's user — an empty mode means "unknown", which is treated as comparable.

- [ ] **Step 6: Commit**

```bash
git add internal/domain/ports.go internal/service/face_service.go internal/service/face_processor.go internal/service/burst_test.go internal/service/face_service_test.go
git commit -m "feat(service): stamp enrollments with their alignment mode and skip cross-space matches"
```

---

### Task 3: A re-enrollment command that replaces one user's pictures safely

The operator must be able to replace a user's photos with fresh 5 MP captures without a half-migrated state. The command captures from the camera, confirms a face was found, and only then replaces the user's record.

**Files:**
- Create: `cmd/reenroll/main.go`
- Test: `cmd/reenroll/main_test.go`

**Interfaces:**
- Consumes: `domain.UserRepository.Save`, `domain.UserRepository.ListAll` via the HTTP API; the camera URL; the API base URL and token.
- Produces: `reenroll -url <camera-url> -api <base-url> -token <token> -name <user> [-pictures N] [-out <dir>]`. It captures N frames from the camera, writes each to `<dir>/<name>-<n>.jpg`, and enrolls them via `POST /enroll` after deleting the user's existing record with `DELETE /users/<name>`.

- [ ] **Step 1: Write the failing test**

Create `cmd/reenroll/main_test.go` with a test for the capture-selection helper:

```go
// TestPickDistinctCaptures verifies that the command keeps the frames that
// differ from the ones already chosen, so the five photos are not five
// copies of the same moment.
func TestPickDistinctCaptures(t *testing.T) {
	frames := [][]byte{
		{1, 1, 1}, {1, 1, 1}, {2, 2, 2}, {3, 3, 3}, {3, 3, 3}, {4, 4, 4},
	}
	got := pickDistinctCaptures(frames, 3)
	if len(got) != 3 {
		t.Fatalf("kept %d frames, want 3", len(got))
	}
	if !bytes.Equal(got[0], []byte{1, 1, 1}) || !bytes.Equal(got[1], []byte{2, 2, 2}) || !bytes.Equal(got[2], []byte{3, 3, 3}) {
		t.Fatalf("kept %v, want the first of each distinct group", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/reenroll/ -count=1 -run TestPickDistinctCaptures -v`
Expected: FAIL with "undefined: pickDistinctCaptures".

- [ ] **Step 3: Implement the command**

Create `cmd/reenroll/main.go` with:

- `pickDistinctCaptures(frames [][]byte, want int) [][]byte` — keeps the first frame of each run of equal frames (the frames are compared byte-wise on their first 64 bytes, which is enough to separate moments), up to `want` frames.
- `capture(ctx, cameraURL string, n int) ([][]byte, error)` — uses `internal/framesource.NewGrabber(cameraURL, logger).Grab(ctx)` in a loop, waiting ~700 ms between grabs so consecutive grabs land on different keyframes.
- `enroll(apiURL, token, name string, frames [][]byte) error` — for each frame, `POST /enroll` as multipart with fields `name` and `image`.
- `deleteUser(apiURL, token, name string) error` — `DELETE /users/<name>`, tolerating a 404.
- `main()` — parses flags, captures, verifies a face was found by checking that `/enroll` returned 201, deletes the old record only after the first new capture succeeded, then enrolls all frames and prints the result.

The order matters: capture first, delete second, enroll third. If a capture fails, nothing is deleted.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/reenroll/ -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: no output.

- [ ] **Step 6: Commit**

```bash
git add cmd/reenroll/main.go cmd/reenroll/main_test.go
git commit -m "feat(cmd): add reenroll to replace a user's photos from the camera"
```

---

### Task 4: Switch the Pi deployment to arcface5

**Files:**
- Modify: `deploy/helm/face-api/values.pi.yaml` (add `FACE_ALIGNMENT=arcface5` to `extraEnv`)

**Interfaces:**
- Consumes: the `FACE_ALIGNMENT` env var read by `cmd/face-api/main.go` (already implemented; no code change).

- [ ] **Step 1: Add the env var to the Pi values**

In `deploy/helm/face-api/values.pi.yaml`, in `extraEnv`, add:

```yaml
  # Crop alignment: arcface5 warps the five detected landmarks onto the
  # ArcFace template, which widens the gap between a genuine match and a
  # stranger by 52% over the bbox crop. Changing this changes the embedding
  # space, so every user must be re-enrolled after the switch.
  - name: FACE_ALIGNMENT
    value: "arcface5"
```

- [ ] **Step 2: Deploy and verify the mode is active**

Run:

```bash
helm upgrade face-api deploy/helm/face-api --namespace face-api \
  --values deploy/helm/face-api/values.yaml \
  --values deploy/helm/face-api/values.pi.yaml \
  --kubeconfig ~/.kube/config.pi
kubectl --kubeconfig ~/.kube/config.pi rollout restart deploy/face-api -n face-api
kubectl --kubeconfig ~/.kube/config.pi rollout status deploy/face-api -n face-api --timeout=290s
kubectl --kubeconfig ~/.kube/config.pi logs -n face-api deploy/face-api --tail=20 | grep "face API wired"
```

Expected: the log line shows `"alignment":"arcface5"`.

- [ ] **Step 3: Verify the burst still scans at full resolution**

Run: `kubectl --kubeconfig ~/.kube/config.pi logs -n face-api deploy/face-api --tail=40 | grep "burst sample" | tail -2`
Expected: `"width":2560,"height":1920`.

- [ ] **Step 4: Verify the old enrollments are no longer matched**

Run an MQTT trigger and check the result. A user enrolled in `bbox` space must NOT be reported as matched.

Expected: `"status":"not ok"` with a reason of either "No face detected" or "Face detected but similarity below threshold".

- [ ] **Step 5: Commit**

```bash
git add deploy/helm/face-api/values.pi.yaml
git commit -m "chore(pi): switch the camera pipeline to arcface5 alignment"
```

Note: `values.pi.yaml` is gitignored, so this commit will be empty. Verify with `git status` and skip the commit if nothing is staged.

---

### Task 5: Re-enroll the users

The operator stands in front of the camera for each user. The command captures 5 MP stills and replaces that user's record.

**Files:** none (operational task).

**Interfaces:**
- Consumes: `reenroll` from Task 3; the camera URL and API token from the `face-api-secrets` secret.

- [ ] **Step 1: Confirm the camera sees a face before starting**

Run a capture and check the detector finds a face. The operator should be close enough that the face fills a good part of the frame (the same-person score rises with face size: 0.66 at 100 px, 0.81 at 200 px, 0.85 at 400 px).

- [ ] **Step 2: Re-enroll each user**

For each of `Ali`, `Cemre`, `admin`, `serap`:

```bash
CAMERA_URL=$(kubectl --kubeconfig ~/.kube/config.pi get secret face-api-secrets -n face-api -o jsonpath='{.data.rtsp-url}' | base64 -d)
TOKEN=$(kubectl --kubeconfig ~/.kube/config.pi get secret face-api-secrets -n face-api -o jsonpath='{.data.api-token}' | base64 -d)
go run ./cmd/reenroll -url "$CAMERA_URL" -api http://pi.home.arpa/face -token "$TOKEN" -name Ali -pictures 5
```

The operator stands in the frame for the duration of the capture.

- [ ] **Step 3: Verify each user has five fresh pictures**

Run:

```bash
TOKEN=$(kubectl --kubeconfig ~/.kube/config.pi get secret face-api-secrets -n face-api -o jsonpath='{.data.api-token}' | base64 -d)
curl -s -H "X-Token: $TOKEN" http://pi.home.arpa/face/users | python3 -m json.tool
```

Expected: each of the four users has `pictures` with 5 entries and an `updated_at` of today.

- [ ] **Step 4: Verify the new enrollments separate properly**

For each user, capture a still and confirm it scores high against that user and low against the others. Use the labelled sampler:

```bash
export FACE_API_TOKEN=$TOKEN
export SAMPLES_CSV=~/face-samples.csv
scripts/collect-samples.sh Cemre
```

Then check the recorded similarity is well above the stranger band.

- [ ] **Step 5: Set the threshold from the measured separation**

The strangers score `-0.060 – 0.131` and the same-person scores `0.068 – 0.375` with the old `bbox` enrollments; with `arcface5` and fresh photos the same-person band should rise well clear of the stranger band. Set the threshold to the midpoint of the measured gap:

```bash
curl -s -X POST -H "X-Token: $TOKEN" -d "threshold=0.45" http://pi.home.arpa/face/settings
```

Confirm: `curl -s -H "X-Token: $TOKEN" http://pi.home.arpa/face/settings`.

- [ ] **Step 6: Record the outcome**

Add the measured numbers to `docs/superpowers/analysis/2026-11-12-match-quality.md` under a "After re-enrollment" heading: the same-person band, the stranger band, the threshold set, and the date.

---

### Task 6: Document the alignment mode in the README and AGENTS.md

**Files:**
- Modify: `README.md` (the configuration table and the live-trigger section)
- Modify: `AGENTS.md` (gitignored, local only)

**Interfaces:**
- Consumes: nothing.

- [ ] **Step 1: Update the README configuration table**

Change the `FACE_ALIGNMENT` row to:

```
| `FACE_ALIGNMENT` | Crop alignment: `bbox` (default) or `arcface5` (recommended; requires re-enrollment after switching) |
```

- [ ] **Step 2: Explain the re-enrollment requirement in the README**

Add to the "Live Trigger Scanning" section:

```markdown
### Switching alignment mode

`FACE_ALIGNMENT=arcface5` warps the five detected landmarks onto the ArcFace
template before embedding. Measured, this widens the gap between a genuine
match and a stranger by 52%: the same person scores 0.63 instead of 0.52,
and a stranger 0.07 instead of 0.16.

The two modes produce different embedding spaces, so **switching the mode
requires re-enrolling every user**. A user enrolled in one mode is skipped
when the other mode is active, rather than being matched on an incomparable
score.

To re-enroll a user from the camera:

```bash
go run ./cmd/reenroll -url "$CAMERA_URL" -api "$API_URL" -token "$TOKEN" -name <user> -pictures 5
```
```

- [ ] **Step 3: Update AGENTS.md**

Add a note under "Environment Variables" that `FACE_ALIGNMENT` defaults to
`bbox`, that `arcface5` is the recommended mode for the camera pipeline, and
that switching requires re-enrolling every user because the embedding spaces
differ.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: describe the arcface5 alignment mode and the re-enrollment step"
```
