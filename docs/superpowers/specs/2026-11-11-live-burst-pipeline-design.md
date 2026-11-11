# Design: Live Frame Buffer + Trigger Burst Pipeline

**Date:** 2026-11-11
**Status:** Draft (awaiting review)
**Approach:** A — layered extraction (`internal/imaging`, `internal/framesource`, burst scanner in `service`)

---

## 1. Problem

The MQTT trigger path is the only sub-second-sensitive path in the app, and it is
the most wasteful:

1. **Connect-per-trigger.** `service.CheckStream` calls `rtsp.Reader.ReadFrame`, which
   opens an RTSP session, describes, plays, reads exactly one frame and closes. Every
   HA doorbell/person trigger pays TCP + RTSP + codec warm-up, and the frame it gets
   may be several hundred ms old by the time detection starts.
2. **Allocation churn on the hot path.** Per frame the pipeline allocates a 1.2 MB
   detection canvas, a **4.9 MB** NCHW detection blob, a decoded RGB image (~0.9 MB)
   and a per-pixel `image.At()` conversion; outputs are copied again by
   `ort.GetTensorData`. That is roughly 7 MB of garbage per frame, and a 3–5 s scan at
   ~5 fps produces ~175 MB of garbage per doorbell — GC pauses land inside the burst.
3. **Oversubscribed inference.** `main.go` builds both sessions with
   `IntraOpNumThreads: 1`, so a single inference uses one Cortex-A76 core.
4. **Bbox-only alignment.** The committed SCRFD model emits 5 facial keypoints, but
   `face_processor.go` ignores them and crops the raw bounding box, which is the
   weaker of the two standard ArcFace pre-processing paths.
5. **Scan is one frame.** A doorbell fires before the person is in frame; a single
   one-shot capture fails or matches a blurry profile.

## 2. Goals

- Keep the newest sub-stream frame in memory continuously; a trigger never connects.
- Scan continuously for a configurable window (3–5 s) and publish the **first**
  confident match to MQTT immediately.
- Remove the ~6 MB/frame of buffer churn (~7 MB/frame total today).
- Pin ONNX intra-op threads (default 4) via configuration.
- Use the SCRFD 5-point landmarks for ArcFace alignment behind an opt-in flag.
- Keep the audit log at **one row per burst** and keep every existing API contract.

## 3. Non-Goals

- No new HTTP endpoints; `/stream-check`, `/recognize`, `/enroll-from-stream` keep
  their one-shot RTSP path.
- No change to the HA discovery entities, topics or payload shape.
- No detection input downscale (320×320); see §13.
- No zero-copy output tensors (would require forking `onnxruntime-purego`); see §13.
- No multi-camera support: one buffered stream, one trigger source.
- No change to the bbolt schema.

## 4. Verified Facts (basis for the decisions below)

| # | Fact | Evidence |
|---|------|----------|
| F1 | SCRFD exposes **9** outputs: scores `443/468/493`, boxes `446/471/496`, keypoints `449/474/499` with shapes `[12800,10] [3200,10] [800,10]` | Ran the committed `models/scrfd_500m.onnx` through ORT 1.23 and dumped `Session.OutputNames()` + tensor shapes |
| F2 | Thread pinning already exists as a field: `&ort.SessionOptions{IntraOpNumThreads: 1}` in `cmd/face-api/main.go` | `main.go:54`; `SessionOptions.IntraOpNumThreads` in the binding |
| F3 | The binding is `github.com/shota3506/onnxruntime-purego`, not `onnxruntime_go` | `go.mod:11` |
| F4 | `ort.NewTensorValue` wraps the caller's slice zero-copy (`CreateTensorWithDataAsOrtValue`); reusing an input tensor only requires overwriting the Go slice | `onnxruntime/value.go:180-257` |
| F5 | `ort.GetTensorData` always returns a copy; no API reads into a caller buffer | `onnxruntime/value.go:259`, doc comment |
| F6 | Reader is one-shot: connect → describe → play → 1 frame → close | `internal/rtsp/reader.go:73-123` |
| F7 | Configured `RTSP_URL` is the **main** stream (`h264Preview_01_main`) | local `.env` |
| F8 | H.264 frames round-trip image → JPEG → bytes → decode → RGB | `reader.go:236` then `face_processor.go:65` |
| F9 | `inferMu` is held per `Session.Run`, not across a whole check | `face_processor.go:222,249` |
| F10 | MQTT queue default depth 16, drop-oldest | `bridge.go:66-68,178-198` |

## 5. Decisions

| ID | Decision | Rationale |
|----|----------|-----------|
| D1 | `ONNX_INTRA_OP_THREADS`, **default 4**, both sessions, `0` = ORT default | User decision. 4 saturates the Pi's four cores; larger hosts can raise or lower it via the knob. Invalid value → log warn + 4 |
| D2 | 5-point warp behind `FACE_ALIGNMENT=bbox\|arcface5`, **default `bbox`** | User decision. Enrolled embeddings were produced by bbox crops; switching spaces silently would break matching. Unknown value fails startup |
| D3 | Buffered sub-stream serves **MQTT triggers only** | User decision. HTTP keeps one-shot semantics, RTSP tests stay meaningful |
| D4 | **One audit row per burst** | User decision. ~15–25 sampled frames per doorbell would otherwise multiply bbolt writes |
| D5 | `MQTT_QUEUE_DEPTH` default 16 → **1** | A second trigger during a burst is meaningless; coalesce instead of queueing |
| D6 | Endpoint value stays `stream-check` | A new `mqtt-trigger` value would churn the UI endpoint filter and stats |
| D7 | Burst exits on first match ≥ threshold; threshold stays the existing live setting | One source of truth; documented recommendation is `THRESHOLD=0.50` |
| D8 | No fork of `onnxruntime-purego` | Outputs are 336 KB/frame; our own 6.1 MB/frame dominates |

## 6. Architecture

```
                 ┌─────────────────────────────────────────────┐
 RTSP sub-stream │ internal/framesource.Source (goroutine)      │
 ───────────────►│  gortsplib client → decode → imaging.RGBImage│
                 │        │                                     │
                 │        ▼  swap under mutex                   │
                 │  Buffer{cur, back *RGBImage, seq, at}        │
                 └────────┬────────────────────────────────────┘
                          │ Latest(dst) (copy under RLock)
                          ▼
 MQTT trigger ──► Bridge.queue(depth 1) ──► worker ──► FaceAPI.ScanBurst(ctx)
                                                          │
                                          deadline = TRIGGER_BURST_MS
                                                          │
                                   ┌──────────────────────┴───────────────┐
                                   │ per sample:                          │
                                   │  detect (SCRFD, 640) → best box+kps  │
                                   │  align (bbox|arcface5) → 112 crop    │
                                   │  embed  (ArcFace)                    │
                                   │  match  (users read once per burst)  │
                                   │  ≥ threshold → return matched        │
                                   └──────────────────────┬───────────────┘
                                                          ▼
                                   1 audit row + StreamCheckResult
                                                          ▼
                                   bridge publishes <base>/result, <base>/matched
```

### 6.1 Components

**`internal/imaging` (new, pure functions, no I/O, no ONNX)**

- `type RGBImage struct { W, H int; Pix []byte }` — packed 3 bytes/pixel, row-major
  (moved from `service.rgbImage`).
- `DecodeRGB(data []byte) (*RGBImage, error)` — `image.Decode` + `ToRGB`.
- `ToRGB(src image.Image) *RGBImage` — type switch with fast paths for
  `*image.YCbCr` (JPEG), `*image.RGBA`, `*image.NRGBA`, `*image.Gray`; generic
  `At()` loop only as fallback. Replaces the per-pixel interface call.
- `NewRGB(w, h int) *RGBImage`.
- `ResizeInto(dst, src *RGBImage)`, `PadSquareInto(dst, src *RGBImage) (scale float64)`,
  `CropInto(dst, src *RGBImage, r image.Rectangle)`, `NCHWInto(dst []float32, src *RGBImage, div float32)`.
  All write into caller-owned buffers and never allocate.
- `Warp5Into(dst, src *RGBImage, kps [5][2]float32)` — Umeyama similarity transform
  from detected keypoints to the ArcFace 112×112 reference points, inverse-mapped
  with bilinear sampling. Returns `false` if the transform is degenerate
  (non-finite, zero determinant, collinear points); caller falls back to bbox crop.
- `EncodeJPEG(img *RGBImage) ([]byte, error)`.
- `ArcFaceRefPoints` — the standard 5 points at 112×112:
  `(38.2946,51.6963) (73.5318,51.5014) (56.0252,71.7366) (41.5493,92.3655) (70.7299,92.2041)`.

**`internal/framesource` (new, adapter)**

- `New(url string, log zerolog.Logger) *Source`.
- `Source.Run(ctx)` — long-lived loop: `streamOnce(ctx)` then backoff 1 s → 2 → 4 → …
  capped at 30 s; backoff resets after 30 s of successful streaming. Unsupported codec
  (H.265) logs once and backs off at the cap.
- `Source.Latest(dst []byte) (w, h int, seq uint64, at time.Time, ok bool)` — copies
  the newest frame into `dst` under `RLock`.
- `Source.Dims() (w, h int)`.
- Internal `Buffer` (guarded by `sync.RWMutex`): writer fills `back` **outside** the
  lock, then swaps `cur`/`back` under `Lock()` and increments `seq`. Readers never see
  a torn frame; the lock is never held across a decode, only across the pointer swap
  and one frame copy (~0.1 ms).
- Codec handling: MJPEG → `rtpmjpeg` depacketize → JPEG → `imaging.DecodeRGB`;
  H.264 → `rtph264` depacketize → length-prefixed AVC → govid → `image.Image` →
  `imaging.ToRGB` (**no JPEG round trip**). Every packet is fed to the decoder
  (H.264 deltas need it); publishing is just a pointer swap. Frames are discarded
  until the decoder returns a full frame (replaces the `frameCount < 5` hack).

**`internal/service` (extended)**

- `WithAlignment(mode string) error` — `"bbox"` (default) or `"arcface5"`.
- `WithFrameSource(src domain.FrameSource)`.
- `ScanBurst(ctx context.Context) (*domain.StreamCheckResult, error)` — the burst
  scanner described in §7.
- `CheckStreamImage` and `EnrollImage`/`RecognizeImage` keep their signatures; they now
  call the preallocating processor path internally.

**`internal/domain` (ports)**

```go
// FrameSource exposes the newest decoded camera frame (implemented by framesource).
type FrameSource interface {
    // Dims returns the current frame size, or 0,0 before the first frame.
    Dims() (w, h int)
    // Latest copies the newest frame into dst (len >= w*h*3) and reports its
    // dimensions, sequence number and capture time.
    Latest(dst []byte) (w, h int, seq uint64, at time.Time, ok bool)
}

// StreamScanner is the MQTT burst entry point (kept out of FaceService so the
// large HTTP-facing interface and its fakes stay stable).
type StreamScanner interface {
    ScanBurst(ctx context.Context) (*StreamCheckResult, error)
}
```

`FaceProcessor` gains one method:

```go
// DetectAndCropFrame runs detection + alignment + recognition on a decoded
// RGB frame (len(pix) >= w*h*3), avoiding a JPEG decode on the hot path.
DetectAndCropFrame(w, h int, pix []byte) (*FaceCrop, error)
```

`domain.ErrNoFace` (sentinel) is returned when no detection clears
`detThreshold`, so the burst can count empty frames without building strings.

**`internal/mqtt` (changed)**

- `CheckFunc` becomes `func(ctx context.Context) (*domain.StreamCheckResult, error)`.
- `New` keeps the same call shape; `main.go` selects the implementation.
- Worker passes the bridge context to the trigger; `Stop()` therefore aborts an
  in-flight burst at its next sample instead of waiting up to 4 s.

**`internal/di` (lifecycle)**

- `FaceAPI` gains:
  - `Start(ctx context.Context)` — starts `framesource.Source.Run` when a stream URL
    is configured; no-op otherwise.
  - `Stop()` — cancels and waits for the source goroutine.
  - `ScanBurst func(ctx context.Context) (*domain.StreamCheckResult, error)`.
- `di.Config` gains `StreamURL`, `Alignment`, `Burst`, `SampleInterval`.
- `NewFaceAPI` now propagates the processor constructor error (it builds the input
  tensors eagerly).

## 7. Burst Semantics

`ScanBurst(ctx)`:

1. Derive a `context.WithDeadline` of `TRIGGER_BURST_MS` (default 4000) from the
   bridge context, so MQTT `Stop()` cancels the burst and the window is bounded.
2. Snapshot the threshold once under `s.mu` (never hold `s.mu` across inference).
3. Read `users.ListAll()` **once** and reuse for every sample.
4. If the source has no frame yet (`Dims()` = 0,0 or no `seq` newer than the burst
   start within `3 × TRIGGER_SAMPLE_MS`), fall back to the existing one-shot
   `CheckStream(streamURL)` so a cold or broken sub-stream does not disable MQTT
   scanning. Log the fallback once per burst.
5. Loop until the deadline:
   - wait for `seq` to change, polling every 20 ms (a poll is cheaper and simpler than
     a condition variable, and the burst is not a busy loop: the floor is
     `TRIGGER_SAMPLE_MS`).
   - copy the frame into a burst-owned scratch buffer sized to `Dims()`; reallocate
     only when the camera changes resolution.
   - enforce `TRIGGER_SAMPLE_MS` (default 200) between samples.
   - `DetectAndCropFrame`; `errors.Is(err, domain.ErrNoFace)` → `noFaceSamples++`,
     continue; any other error → `errors++`, continue (a decode/inference hiccup must
     not end the burst).
   - match: `best = max over users of BestEmbeddingScore`; if `best >= threshold`
     return immediately with `status "ok"`, `matched true`, name, similarity, the
     112×112 crop as `face_image`, `duration_ms` = elapsed.
   - otherwise keep the best sample (score, JPEG, embedding copy) — scratch is reused,
     so both must be copied.
6. Deadline reached:
   - ≥1 face seen → `status "not ok"`, `reason "Face detected but similarity below
     threshold"`, best similarity, best crop, `matched false`.
   - zero faces → `status "not ok"`, `reason "No face detected"`, with a downscaled
     full frame via a new `noFaceFrameFromRGB(img)` helper (the RGB sibling of the
     existing `noFaceFrameImage`, which decodes JPEG first).
7. Write **exactly one** audit row (`endpoint "stream-check"`, `duration_ms` = burst
   wall time, `embedding` for matched/not_matched so promotion works).
8. Return the `StreamCheckResult`; the MQTT worker publishes it retained as today.

Timing budget: detection + embedding is ~200–400 ms on a Pi 5 at 4 threads, so a 4 s
window yields 8–15 scored frames — enough to catch a person walking up a path.

## 8. Processor Changes

**Preallocation (per process, guarded by one mutex):**

```go
type FaceProcessorImpl struct {
    rt, env, detS, recS *ort.Session
    pipelineMu sync.Mutex // guards scratch + both sessions

    canvas  *imaging.RGBImage // 640×640×3 = 1.2 MB
    detBlob []float32         // 3×640×640 = 4.9 MB
    detTens *ort.Value        // wraps detBlob, created once
    crop    *imaging.RGBImage // 112×112×3
    recBlob []float32         // 3×112×112
    recTens *ort.Value
}
```

- `NewFaceProcessor(...) (*FaceProcessorImpl, error)` creates both tensors eagerly.
- `Close()` releases tensors; `di`/`main` defer it alongside the sessions.
- `DetectAndCropFrame` holds `pipelineMu` for the **whole** detect+align+embed pipeline:
  the scratch buffers are shared, so per-`Run` locking would let two goroutines scribble
  on the same canvas. Consequence: an HTTP request may wait for one full
  detect+embed (~200–400 ms) instead of one inference; the burst releases
  `pipelineMu` between samples, so requests are never blocked for the whole 4 s window.
- `runDetect` returns a struct with the 9 named slices instead of `map[string][]float32`
  (removes a map allocation per frame). Detection outputs are still copied by
  `GetTensorData` (F5) — documented, accepted.
- `NewTensorValue` is called once per session; `Run` reuses the same `*ort.Value`.
  Safe because `pipelineMu` serializes all inference and the processor holds the Go
  slices, keeping them alive.

**5-point decode:** for anchor index `i` (`cell = i/2`, `row = cell/hw`, `col = cell%hw`,
`cx = col*stride`, `cy = row*stride`), keypoint `j` (0..4) is
`(cx + kps[i*10+2j]*stride, cy + kps[i*10+2j+1]*stride)`, using the same anchor
mapping as the existing bbox decode and the kps outputs `449/474/499`.

**Alignment:** `bbox` = existing crop+resize. `arcface5` = `imaging.Warp5Into`; on a
degenerate transform fall back to the bbox crop for that frame (log at debug).

## 9. Configuration

| Env | Default | Range / values | Effect |
|-----|---------|----------------|--------|
| `ONNX_INTRA_OP_THREADS` | `4` | 0 = ORT default; ≥1 (no cap) | `IntraOpNumThreads` for both sessions. Invalid → warn + 4 |
| `RTSP_STREAM_URL` | (empty) | URL | Sub-stream for the buffer (e.g. `.../h264Preview_01_sub`). **Empty = buffer disabled** and MQTT triggers keep the one-shot path |
| `FACE_ALIGNMENT` | `bbox` | `bbox` \| `arcface5` | Crop strategy; unknown → fatal at startup |
| `TRIGGER_BURST_MS` | `4000` | 500..10000 (clamped) | Burst window |
| `TRIGGER_SAMPLE_MS` | `200` | 50..1000 (clamped) | Minimum gap between scored frames |
| `MQTT_QUEUE_DEPTH` | `1` (was 16) | ≥1 | Pending-trigger slots; 1 = coalesce |
| `THRESHOLD` | `0.45` | (0,1) | Unchanged; docs recommend `0.50` for burst use |

Persisted settings still win over `THRESHOLD` (unchanged precedence). The buffer
starts only when **both** `MQTT_BROKER_URL` and `RTSP_STREAM_URL` are set; there is no
separate enable switch, and unsetting `RTSP_STREAM_URL` is the rollback.

## 10. Failure Modes

| Condition | Behaviour |
|-----------|-----------|
| Sub-stream unreachable / drops | Source reconnects 1 s→30 s backoff, warns; last frame stays but its `seq` stops advancing |
| Cold buffer on first trigger | One-shot `CheckStream(streamURL)` fallback, logged |
| Sub-stream is H.265 | Source logs once, backs off at 30 s cap; every burst falls back to one-shot against the same URL (which fails with the existing codec error) |
| Camera renegotiates resolution | Buffer reallocates on the next publish; burst reallocates scratch when `Dims()` changes |
| No face for the whole window | One `no_face` audit row with a downscaled frame; result `not ok` |
| Inference error on a sample | Counted, sample skipped, burst continues; a burst with 0 successful samples returns `not ok` with `reason "Face processing failed"` and writes no audit row |
| MQTT `Stop()` mid-burst | `ctx` cancel ends the burst at the next sample; no retained publish for the aborted burst |
| ONNX thread env invalid | Warn + default 4 |
| `FACE_ALIGNMENT` invalid | `log.Fatal` (silently changing embedding semantics is worse than refusing to start) |
| Sub-stream URL missing but MQTT set | Behaviour unchanged: every trigger uses one-shot `CheckStream(RTSP_URL)` |

## 11. Compatibility & Migration

- **API contracts:** no change. `StreamCheckResult`, `/api/audit` entries and HA
  payloads keep their shapes; `duration_ms` now reports the burst length.
- **Embeddings:** `bbox` default keeps existing enrolled vectors valid. Switching to
  `arcface5` **requires deleting and re-enrolling all users** (the stored pictures are
  112×112 bbox crops, so old vectors cannot be re-derived). Documented in `AGENTS.md`
  and `README`.
- **bbolt:** no schema change; no backfill needed.
- **Helm:** `values.yaml` `extraEnv` comments gain `RTSP_STREAM_URL` and
  `ONNX_INTRA_OP_THREADS`; `values.pi.yaml` (gitignored) is updated locally with the
  sub-stream URL. No chart template change.
- **Docker image:** unchanged (no new dependencies, no CGO).
- **Rollback:** unset `RTSP_STREAM_URL` (buffer disabled → one-shot triggers) and set
  `MQTT_QUEUE_DEPTH=16` to restore today's trigger behaviour; `FACE_ALIGNMENT` already
  defaults to the old path.

## 12. Testing

### 12.1 Unit — `internal/imaging`

- Warp maps `ArcFaceRefPoints` to themselves (identity within 0.5 px).
- Warp on a synthetic translated/scaled/rotated 5-point set lands on the reference
  points; degenerate sets (collinear, NaN) return `false`.
- `NCHWInto` produces `(p-127.5)/128` in RGB plane order for known pixels.
- `DecodeRGB` fast paths agree with the generic `At()` loop for the same image.
- `testing.AllocsPerRun` = 0 for `ResizeInto`, `PadSquareInto`, `CropInto`, `NCHWInto`,
  `Warp5Into` on preallocated buffers.

### 12.2 Unit — `internal/framesource`

- Double-buffer property test under `-race`: one writer publishing 10k frames, four
  readers copying; every read is byte-identical to some published frame (no torn reads).
- `Latest` reports `ok=false` before the first publish; when the writer stops, `seq`
  and `at` freeze (the burst treats that as a stale/absent frame).
- Reconnect loop: a fake `streamOnce` failing twice then succeeding produces one
  publish and two warn logs (injected logger hook, not a real RTSP server).
- In-process gortsplib MJPEG mock (shared helper moved to `internal/rtsp/rtspmock`):
  `Source.Run(ctx)` publishes ≥3 frames, `Dims()` matches the mock resolution;
  H.264 path covered by the existing govid bridge plus the mock's MJPEG variant.

### 12.3 Unit — `internal/service`

- Burst with fake frame source + fake processor + fake repos:
  - match on sample 3 → returns matched, exactly **1** audit row, 3 processor calls,
    `duration_ms` ≥ sample cost, embedding stored.
  - no match, faces seen → `not_matched`, best score/frame retained (not the last).
  - all `ErrNoFace` → `no_face` row with a frame image.
  - no fresh frame → one-shot fallback invoked once.
  - `ctx` cancelled mid-burst → returns promptly, no audit row for the aborted run.
  - processor error on every sample → `not ok`, no audit row.
  - `s.mu` not held across inference: the fake processor blocks on a channel while a
    concurrent `GetSettings` completes, proving the burst does not hold the service
    mutex for the sample duration.
- `WithAlignment("nonsense")` returns an error.

### 12.4 Unit — `internal/mqtt`

- Queue depth 1: three triggers during one slow trigger → the third replaces the
  second, only two checks run.
- `ctx` cancellation propagates into `CheckFunc`.
- Existing discovery/availability tests unchanged.

### 12.5 E2E (`test/e2e`)

- Existing suite must pass unchanged (default `bbox`).
- New `TestFaceAlignmentWarpE2E`: container with `FACE_ALIGNMENT=arcface5`; enroll +
  recognize Anthony Hopkins; assert `matched` and `similarity ≥ 0.45`. Proves the warp
  path end-to-end without changing stored data.
- New `TestThreadsEnvE2E` (cheap): start the container with an invalid
  `ONNX_INTRA_OP_THREADS=abc` and assert it boots (warn + default).

### 12.6 Benchmarks (not CI-gated)

- `go test -bench BenchmarkDetectFrame -benchmem` with `ORT_BENCH=1`:
  report ns/op and B/op before/after. Target: the 6.1 MB canvas/blob churn and
  per-pixel `At()` calls disappear; remaining per-sample allocations are the copied
  detection outputs (~336 kB), the 112×112 JPEG (~4 kB) and the 512-float embedding.
- Manual Pi measurement: trigger → MQTT `matched` publish latency, and `top` CPU
  during a burst, recorded in the implementation plan's verification section.

## 13. Rejected Alternatives

- **B — buffer JPEG bytes, keep decoding per sample.** ~1/3 of the diff, but keeps the
  H.264→JPEG→RGB round trip and ~2 MB of garbage per sampled frame, which is the exact
  problem being solved.
- **C — fork `onnxruntime-purego` for zero-copy outputs.** Outputs are 336 kB/frame
  against 6.1 MB of our own buffers; owning a fork is not justified.
- **320×320 detection input.** 4× cheaper detection, but a 640×480 sub-stream already
  puts distant faces at ~40 px; halving the input costs recall. Revisit only with a
  measurement.
- **Warp always on.** Breaks every existing enrollment (D2).
- **Dual-embedding (bbox + warp, match = max).** No re-enrollment, but a storage-format
  migration, double match work and a higher false-accept rate from the max.
- **Row per sampled frame.** Floods bbolt on an SD card and the audit UI (D4).
- **New `mqtt-trigger` audit endpoint value.** Churns the UI endpoint filter and stats.
- **Burst inside `internal/mqtt`.** Puts inference, audit and matching in the transport
  package and makes it untestable without a broker.
- **`triggerCh chan struct{}` depth 1 as a new channel separate from `queue`.** The
  existing queue with `MQTT_QUEUE_DEPTH=1` is exactly that, already drop-oldest and
  already tested.

## 14. Files Changed

| File | Change |
|------|--------|
| `internal/imaging/rgb.go` | **New** — RGBImage, DecodeRGB/ToRGB fast paths, EncodeJPEG |
| `internal/imaging/resize.go` | **New** — ResizeInto, PadSquareInto, CropInto, NCHWInto |
| `internal/imaging/warp.go` | **New** — Umeyama + Warp5Into + ArcFaceRefPoints |
| `internal/imaging/*_test.go` | **New** — math, equivalence, allocation tests |
| `internal/framesource/source.go` | **New** — long-lived RTSP session, reconnect, Buffer |
| `internal/framesource/source_test.go` | **New** — double-buffer race test, reconnect, mock RTSP |
| `internal/domain/ports.go` | Add `FrameSource`, `StreamScanner`, `DetectAndCropFrame`, `ErrNoFace` |
| `internal/service/face_processor.go` | Preallocated scratch + tensor reuse, struct outputs, kps decode, alignment, `Close`, constructor returns error |
| `internal/service/face_service.go` | `WithAlignment`, `WithFrameSource`, `ScanBurst`, single-row audit |
| `internal/service/*_test.go` | Burst tests, alignment test, update the fake `FaceProcessor` with `DetectAndCropFrame` |
| `internal/rtsp/reader.go` | Reuse `imaging.EncodeJPEG`; keep one-shot semantics unchanged |
| `internal/rtsp/rtspmock/mock.go` | **New** — the in-process gortsplib MJPEG mock moved out of `test/rtsp/rtsp_mock_test.go` so both suites share it |
| `test/rtsp/rtsp_mock_test.go` | Use the shared mock package |
| `internal/mqtt/bridge.go` | ctx-aware `CheckFunc`, queue default 1, pass ctx to trigger |
| `internal/mqtt/bridge_test.go` | Coalescing + ctx tests |
| `internal/di/wire.go` | Processor error propagation, `Start`/`Stop`, `ScanBurst`, new config fields |
| `cmd/face-api/main.go` | Threads env, stream URL, alignment, burst/sample env, source lifecycle, trigger selection |
| `AGENTS.md`, `README.md` | Config table, alignment migration warning, burst behaviour |
| `deploy/helm/face-api/values.yaml` | `extraEnv` examples for the two new knobs |
| `test/e2e/e2e_test.go` | Warp-mode + invalid-threads tests |

## 15. Implementation Phases

1. **Extract `internal/imaging`** from `service/face_processor.go` (including the
   `At()` fast paths) with equivalence + allocation tests. No behaviour change.
2. **Preallocate the processor** (canvas, blobs, tensors, struct outputs) + wire
   `ONNX_INTRA_OP_THREADS`. Benchmark before/after.
3. **`internal/framesource`** + `domain.FrameSource` + `FaceAPI.Start/Stop`.
   Double-buffer race test + in-process mock RTSP test.
4. **Keypoint decode + `FACE_ALIGNMENT` warp** + both-mode e2e.
5. **`ScanBurst`** + single-row audit + `domain.StreamScanner` + MQTT ctx/depth-1
   wiring + burst unit tests.
6. **Docs, Helm examples, benchmark numbers**, manual Pi latency measurement.

Each phase compiles and leaves the full existing test suite green; phases 1–3 are
independently shippable.

## 16. Open Questions

None. All four forks (threads, alignment rollout, buffer consumers, audit granularity)
were decided by the operator on 2026-11-11 and are recorded in §5.
