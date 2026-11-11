# Live Frame Buffer + Trigger Burst Pipeline Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep the Reolink sub-stream buffered in memory, scan a 3–5 s window on every MQTT trigger over preallocated ONNX buffers, publish the first match immediately, and run CI tests for the first time.

**Architecture:** Extract pure image code into `internal/imaging` (RGB conversion, allocation-free resize/NCHW, Umeyama 5-point warp), add `internal/framesource` (one long-lived RTSP session + double buffer), make `FaceProcessorImpl` reuse one canvas/blob/tensor per session, and put the burst scanner in `service` where users, thresholds and audit already live. MQTT becomes context-aware and coalesces triggers; a new `internal/config` package owns the new env parsers.

**Tech Stack:** Go 1.27, `github.com/shota3506/onnxruntime-purego` (ORT 1.23), `github.com/bluenviron/gortsplib/v5`, `github.com/liqmix/govid/h264`, `github.com/eclipse/paho.mqtt.golang`, bbolt, zerolog, `golang.org/x/image`.

**Spec:** `docs/superpowers/specs/2026-11-11-live-burst-pipeline-design.md`

## Global Constraints

- Go 1.27 (`go.mod`), no new module dependencies, no CGO in the inference path.
- Hexagonal layout: `internal/domain` stays pure (no I/O, no ONNX, no image decoding); adapters depend inward.
- ONNX Runtime sessions are **not** thread-safe: every `Session.Run` stays behind one mutex.
- bbolt: at most **one** `db.Update` transaction per request/burst; never write audit rows inside a frame loop.
- Detection input is **640×640** padded (not 320), detection padding stays **top-left** as today.
- Bbox alignment (default) must reproduce today's embeddings exactly; `arcface5` is opt-in only.
- `duration_ms` is present on every business response; audit `endpoint` stays `stream-check`.
- No new HTTP endpoints; no request-logging middleware; UI URLs stay relative (not touched by this work).
- `AGENTS.md` is gitignored — edit it locally, **never** `git add` it. Same for `deploy/helm/face-api/values.pi.yaml`.
- Tests: unit tests must pass without `libonnxruntime.so`; ORT-dependent tests skip unless `ORT_TEST=1`.
- New env defaults: `ONNX_INTRA_OP_THREADS=4`, `FACE_ALIGNMENT=bbox`, `TRIGGER_BURST_MS=4000`, `TRIGGER_SAMPLE_MS=200`, `MQTT_QUEUE_DEPTH=1`.
- Commits: conventional prefixes (`feat:`, `refactor:`, `test:`, `ci:`, `docs:`); one commit per task minimum.
- The repo has pre-existing `gofmt` drift (7 files at branch time): format the files you touch, do not reformat the rest.

## Review Focus

Inputs/conditions the spec implies but no single test naturally covers — each line has its test in the named task.

1. **A trigger arrives before the buffer is warm (or the sub-stream is down).** Must not hang for the 10 s RTSP timeout or return a bogus match; falls back to the one-shot check once, and a second trigger behaves the same. → Task 13 (`TestScanBurstFallbackWhenNoFrames`).
2. **The camera renegotiates resolution mid-stream.** Must not panic on a short `Pix` slice or return a frame labelled with the old size; readers either get a complete new-size frame or `ok=false`. → Tasks 9 and 11 (`TestPublishNewDimensions`, `TestSourceHandlesResolutionChange`).
3. **The sub-stream is H.265 (or any unsupported codec).** Must produce a readable reason and back off, not a tight reconnect loop that pegs a core. → Tasks 11 and 13 (`TestSourceBacksOffOnUnsupportedCodec`, `TestScanBurstFallbackWhenNoFrames`).
4. **`arcface5` meets a profile/occluded face with degenerate or out-of-frame keypoints.** Must fall back to the bbox crop for that frame instead of producing a NaN embedding or a zeroed crop. → Tasks 5, 8, and 18 (`TestWarpDegenerate`, `TestAlignCropFallsBackOnBadKPs`, `TestFaceAlignmentWarpE2E`).
5. **A burst overlaps an HTTP `/recognize` and an MQTT `Stop()`.** Must not deadlock, must not block HTTP for the whole window, and must not write an audit row for the aborted burst. → Tasks 13 and 14 (`TestScanBurstDoesNotHoldServiceMutex`, `TestScanBurstCancel`, `TestCheckReceivesContext`).

---

## File Structure

| Path | Responsibility |
|------|----------------|
| `internal/config/env.go` (+ test) | Parse and clamp the new env knobs; no I/O beyond a `getenv` func |
| `internal/imaging/rgb.go` (+ test) | `RGBImage`, fast `image.Image` conversion, JPEG decode/encode |
| `internal/imaging/geometry.go` (+ test) | Allocation-free resize, pad-to-square, crop, NCHW packing |
| `internal/imaging/warp.go` (+ test) | Umeyama similarity transform + 5-point warp |
| `internal/service/detect_decode.go` (+ test) | Pure SCRFD output decode: best box, keypoints, `ErrNoFace` |
| `internal/service/face_processor.go` | Preallocated scratch + tensors, alignment, `Close` |
| `internal/framesource/buffer.go` (+ test) | Double-buffered latest-frame store with `seq`/`at` |
| `internal/framesource/source.go` (+ test) | Long-lived RTSP session, MJPEG/H.264 decode, reconnect |
| `internal/rtsp/rtspmock/mock.go` (+ test) | Shared in-process M-JPEG RTSP server for tests |
| `internal/service/face_service.go` | `ScanBurst`, frame-source wiring, single-row audit |
| `internal/mqtt/bridge.go` | Context-aware trigger, queue default 1 |
| `internal/di/wire.go` | Processor error handling, source lifecycle, `ScanBurst` |
| `cmd/face-api/main.go` | Env parsing, session options, trigger selection |
| `.github/workflows/test.yml` | Unit + integration CI jobs |
| `Makefile` | `test-unit` target used by CI |
| `AGENTS.md`, `README.md`, `deploy/helm/face-api/values.yaml` | Documentation |

---

### Task 0: Branch and spec commit

**Files:**
- Create: `docs/superpowers/specs/2026-11-11-live-burst-pipeline-design.md`
- Create: `docs/superpowers/plans/2026-11-11-live-burst-pipeline.md`

**Interfaces:**
- Consumes: the approved design at `/home/hekemen/.opencode/plan/2026-11-11-live-burst-pipeline-design.md`.
- Produces: the spec and plan committed on `feat/live-burst-pipeline`.

- [ ] **Step 1: Create the branch**

```bash
git checkout -b feat/live-burst-pipeline
```

- [ ] **Step 2: Copy the spec into the repo**

```bash
cp /home/hekemen/.opencode/plan/2026-11-11-live-burst-pipeline-design.md \
   docs/superpowers/specs/2026-11-11-live-burst-pipeline-design.md
```

- [ ] **Step 3: Confirm nothing sensitive is staged**

Run: `git status --porcelain`
Expected: only the two docs files listed; `AGENTS.md` and `values.pi.yaml` must not appear.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/specs/2026-11-11-live-burst-pipeline-design.md \
        docs/superpowers/plans/2026-11-11-live-burst-pipeline.md
git commit -m "docs: add live burst pipeline design and implementation plan"
```

---

### Task 1: Env parsing for the new knobs

**Files:**
- Create: `internal/config/env.go`
- Test: `internal/config/env_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `config.IntraOpThreads(getenv func(string) string) (int, error)`
  - `config.Alignment(getenv func(string) string) (string, error)`
  - `config.BurstWindow(getenv func(string) string) time.Duration`
  - `config.SampleInterval(getenv func(string) string) time.Duration`

- [ ] **Step 1: Write the failing tests**

```go
func TestIntraOpThreads(t *testing.T) {
    // "" => 4, nil; "8" => 8, nil; "0" => 0, nil
    // "abc" => 4, error; "-2" => 4, error
}

func TestAlignment(t *testing.T) {
    // "" => "bbox", nil; "bbox" => "bbox"; "arcface5" => "arcface5"
    // "ARC" => "", error
}

func TestBurstWindow(t *testing.T) {
    // "" => 4s; "100" => 500ms; "99999" => 10s; "abc" => 4s
}

func TestSampleInterval(t *testing.T) {
    // "" => 200ms; "10" => 50ms; "5000" => 1s; "abc" => 200ms
}
```

Use a helper `getenv := func(k string) string { return m[k] }` with a map per case.

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/config/`
Expected: FAIL — `undefined: IntraOpThreads` (etc.).

- [ ] **Step 3: Implement the parsers**

`IntraOpThreads` accepts `0` and integers `>= 1`; on a non-integer or negative value it returns `(4, fmt.Errorf("invalid ONNX_INTRA_OP_THREADS %q, using 4", raw))`. `Alignment` trims/lowercases and rejects anything but `bbox`/`arcface5`. `BurstWindow` defaults 4000 ms and clamps to `[500, 10000]`; `SampleInterval` defaults 200 ms and clamps to `[50, 1000]`; both ignore unparseable values.

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test ./internal/config/ -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat(config): parse ONNX thread, alignment and burst env knobs"
```

---

### Task 2: `imaging` RGB core

**Files:**
- Create: `internal/imaging/rgb.go`
- Test: `internal/imaging/rgb_test.go`

**Interfaces:**
- Consumes: nothing (leaf package, stdlib only).
- Produces:
  - `type RGBImage struct { W, H int; Pix []byte }`
  - `func NewRGB(w, h int) *RGBImage`
  - `func (img *RGBImage) At(x, y int) (r, g, b uint8)`
  - `func ConvertInto(dst *RGBImage, src image.Image) bool` — false when `dst` is the wrong size
  - `func ToRGB(src image.Image) *RGBImage`
  - `func DecodeRGB(data []byte) (*RGBImage, error)`
  - `func EncodeJPEG(img *RGBImage) ([]byte, error)`

- [ ] **Step 1: Write the failing tests**

```go
func TestToRGBMatchesGenericConversion(t *testing.T) {
    // table over image.NewYCbCr(8,8,image.YCbCrSubsampleRatio420),
    // image.NewRGBA, image.NewNRGBA, image.NewGray
    // for every pixel: got := img2.At(x,y); want := via src.At(x,y).RGBA()>>8
}

func TestConvertIntoRejectsWrongSize(t *testing.T)
func TestDecodeRGBFromJPEG(t *testing.T) // encode 8x8 pattern, decode, each channel within ±3
func TestEncodeJPEGProducesDecodableImage(t *testing.T)
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/imaging/`
Expected: FAIL — package does not exist / undefined symbols.

- [ ] **Step 3: Implement**

Fast paths switch on `src.(type)` for `*image.YCbCr` (use `color.YCbCrToRGB(src.Y[src.YOffset(x,y)], src.Cb[src.COffset(x,y)], src.Cr[src.COffset(x,y)])`), `*image.RGBA`, `*image.NRGBA`, `*image.Gray`; the fallback uses `At().RGBA() >> 8`. `DecodeRGB` = `image.Decode` + `ToRGB`. `EncodeJPEG` uses `image/jpeg` with `nil` options (quality 75, matching today's output).

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test ./internal/imaging/ -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/imaging
git commit -m "feat(imaging): add RGB image type with fast decode/encode"
```

---

### Task 3: Allocation-free geometry

**Files:**
- Create: `internal/imaging/geometry.go`
- Test: `internal/imaging/geometry_test.go`

**Interfaces:**
- Consumes: `imaging.RGBImage`.
- Produces:
  - `func ResizeInto(dst, src *RGBImage)` — bilinear, `dst.W/H` is the target size
  - `func PadSquareInto(dst, src *RGBImage) (scale float64)` — `dst` square; source scaled to fit and copied at **(0,0)**, remainder black; `scale = dst.W / max(src.W, src.H)`
  - `func CropInto(dst, src *RGBImage, r image.Rectangle) error` — clips to bounds, error on empty
  - `func NCHWInto(dst []float32, src *RGBImage, div float32) error` — `(v-127.5)/div`, RGB plane order

- [ ] **Step 1: Write the failing tests**

```go
func TestResizeIntoKnownPixels(t *testing.T) // 2x2 -> 4x4 hand-computed bilinear values
func TestPadSquareInto(t *testing.T)         // 4x2 -> 8x8: scale 2, top-left copy, rest 0
func TestCropIntoClipsAndCopies(t *testing.T)
func TestNCHWIntoValues(t *testing.T)        // pix 0 => -127.5/128, 255 => 127.5/128
func TestGeometryHasNoAllocs(t *testing.T)   // AllocsPerRun(50, ...) == 0 for all four
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/imaging/ -run 'Resize|Pad|Crop|NCHW'`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

Port the existing loops from `service/face_processor.go` (`bilinearResize`, `resizePadToSquare`, `cropRegion`, `rgbToNCHW`) verbatim into the `*Into` forms, replacing allocations with writes into `dst`/`dst.Pix`. Keep the top-left padding placement and the `x0/y0/x1/y1` clamping — behaviour must not change.

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test ./internal/imaging/ -race -v`
Expected: PASS, all allocation assertions 0.

- [ ] **Step 5: Commit**

```bash
git add internal/imaging
git commit -m "feat(imaging): add allocation-free resize, pad, crop and NCHW packing"
```

---

### Task 4: Adopt `imaging` in `service` and `rtsp`

**Files:**
- Modify: `internal/service/face_processor.go`
- Modify: `internal/rtsp/reader.go`
- Test: existing `internal/service/*_test.go`, `internal/http/*_test.go`

**Interfaces:**
- Consumes: `imaging.DecodeRGB/ToRGB/NewRGB/ResizeInto/PadSquareInto/CropInto/NCHWInto/EncodeJPEG`.
- Produces: no API change; `service.rgbImage`, `decodeRGB`, `imageToRGB`, `resizePadToSquare`, `bilinearResize`, `cropRegion`, `rgbToNCHW`, `encodeFaceJPEG`, `encodeFaceToBase64` are deleted.

- [ ] **Step 1: Run the existing suite as the baseline**

Run: `go test ./internal/... ./cmd/...`
Expected: PASS (record the output).

- [ ] **Step 2: Replace the helpers**

In `face_processor.go`: `decodeRGB(data)` → `imaging.DecodeRGB(data)`; `p.detectAndCrop112(img *rgbImage)` → `(*imaging.RGBImage)`; `encodeFaceJPEG` → `imaging.EncodeJPEG`. In `reader.go`: `encodeToJPEG` → build an `imaging.RGBImage` via `imaging.ToRGB(frame)` then `imaging.EncodeJPEG` (delete `encodeToJPEG`). Keep `encodeFaceToBase64` as a two-line wrapper if it is still referenced.

- [ ] **Step 3: Verify behaviour is unchanged**

Run: `go test ./internal/... -race -count=1`
Expected: PASS — identical set of tests, no new failures.

- [ ] **Step 4: Confirm the old symbols are gone**

Run: `grep -rn "rgbImage\|imageToRGB\|rgbToNCHW\|bilinearResize" internal/ | grep -v "_test.go"`
Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/service internal/rtsp
git commit -m "refactor(service,rtsp): use internal/imaging helpers"
```

---

### Task 5: 5-point similarity warp

**Files:**
- Create: `internal/imaging/warp.go`
- Test: `internal/imaging/warp_test.go`

**Interfaces:**
- Consumes: `imaging.RGBImage`.
- Produces:
  - `var ArcFaceRefPoints = [5][2]float32{{38.2946, 51.6963}, {73.5318, 51.5014}, {56.0252, 71.7366}, {41.5493, 92.3655}, {70.7299, 92.2041}}`
  - `func SimilarityFromPoints(src, dst [5][2]float64) (m [6]float64, ok bool)` — `m = {a, b, tx, c, d, ty}` with `dst = [[a,b],[c,d]]·src + [tx,ty]`
  - `func Warp5Into(dst, src *RGBImage, kps [5][2]float32) bool`

- [ ] **Step 1: Write the failing tests**

```go
func TestSimilarityIdentity(t *testing.T)      // src == dst == refpoints (float64) => a,d ~= 1, b,c ~= 0, t ~= 0
func TestSimilarityRecoversKnownTransform(t *testing.T) // apply scale 1.5 + rotate 10deg + translate, then recover
func TestWarpIdentity(t *testing.T)            // 112x112 gradient, kps == refpoints => dst == src within 1e-4
func TestWarpRecoversTransformedPattern(t *testing.T) // interior pixels within tolerance
func TestWarpDegenerate(t *testing.T)          // collinear points, NaN, zero-size src => false
func TestWarpHasNoAllocs(t *testing.T)
```

For the "RecoversKnownTransform" case, compute the expected transform in the test with the same formula and assert matrix entries within `1e-4`.

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/imaging/ -run Warp`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement Umeyama (allocation-free)**

Algorithm the signature and tests do not pin down:

1. Means `μ_s`, `μ_d` over the 5 points; `var_s = Σ|s_i-μ_s|²`.
2. Covariance `H = Σ (d_i-μ_d)(s_i-μ_s)ᵀ` (2×2); compute the closed-form 2×2 SVD `H = UΣVᵀ` (rotation angle from `atan2`, singular values from `|H|`/norms).
3. `R = U·diag(1, sign(det(U·Vᵀ)))·Vᵀ`; `scale = trace(Σ·diag(1, sign))/var_s`; `t = μ_d − scale·R·μ_s`.
4. Reject if any input is non-finite, `var_s == 0`, or `|det(R)| < 1e-6`.

`Warp5Into` inverts the affine (`det = a*d − b*c`), then for each `dst` pixel `(x+0.5, y+0.5)` maps into `src` and samples bilinearly, clamping to edges. All arrays are fixed-size locals.

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test ./internal/imaging/ -race -v`
Expected: PASS; `TestWarpHasNoAllocs` reports 0.

- [ ] **Step 5: Commit**

```bash
git add internal/imaging
git commit -m "feat(imaging): add Umeyama 5-point similarity warp"
```

---

### Task 6: Pure detection decode with keypoints

**Files:**
- Create: `internal/service/detect_decode.go`
- Modify: `internal/domain/ports.go` (add `ErrNoFace`)
- Test: `internal/service/detect_decode_test.go`

**Interfaces:**
- Consumes: `domain.ErrNoFace`.
- Produces:
  - `var ErrNoFace = errors.New("no face detected")` in `domain`
  - `type detLevelOutputs struct { Scores, Boxes, KPs []float32 }` (lengths `n`, `4n`, `10n`)
  - `type detOutputs [3]detLevelOutputs` — index 0/1/2 = stride 8/16/32
  - `type faceHit struct { Score float32; Box [4]float32; KPs [5][2]float32; HasKPs bool }`
  - `func decodeBestFace(outs detOutputs, scale float64, w, h int) (*faceHit, error)`

- [ ] **Step 1: Write the failing tests**

```go
func TestDecodeBestFacePicksHighestScore(t *testing.T) // stride-8 score 0.9 at index 2, regs 1,2,3,4 => box mapped by /scale
func TestDecodeBestFaceNoFace(t *testing.T)           // all scores 0.4 => errors.Is(err, domain.ErrNoFace)
func TestDecodeBestFaceKeypoints(t *testing.T)        // kps[i*10+2j] values => cx + v*stride, HasKPs true
func TestDecodeBestFaceKeypointsOutOfFrame(t *testing.T) // NaN or x>w => HasKPs false, Box still valid
func TestDecodeBestFaceRejectsBadLengths(t *testing.T)   // scores len != n => error, not ErrNoFace
func TestDecodeBestFaceTieKeepsFirst(t *testing.T)       // equal scores across levels => lowest stride wins
```

- [ ] **Step 2: Run the tests to confirm they fail**

Run: `go test ./internal/service/ -run Decode`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

Move the anchor loop out of `detectAndCrop112`: for level index `l` with `stride`, `hw = 640/stride`, `numAnchors = 2*hw*hw`; `cell = i/2`, `row = cell/hw`, `col = cell%hw`, `cx = col*stride`, `cy = row*stride`; box as today (`x1 = cx − regs[4i]*stride` …); keypoint `j` = `(cx + kps[10i+2j]*stride, cy + kps[10i+2j+1]*stride)`. Keep the box if `score >= detThreshold`; use strict `>` for the best-so-far comparison. Map the box by `/scale`, clamp to `[0,w]×[0,h]`, return `ErrNoFace` when nothing qualifies. `HasKPs` is true only when all 10 coordinates are finite and inside the frame.

- [ ] **Step 4: Run the tests to confirm they pass**

Run: `go test ./internal/service/ -run Decode -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service/detect_decode.go internal/service/detect_decode_test.go internal/domain/ports.go
git commit -m "feat(service): decode SCRFD keypoints and map no-face to a sentinel"
```

---

### Task 7: Preallocated processor

**Files:**
- Modify: `internal/service/face_processor.go`
- Modify: `internal/domain/ports.go` (`FaceProcessor.DetectAndCropFrame`)
- Modify: `internal/di/wire.go`, `cmd/face-api/main.go` (constructor error)
- Modify: `internal/http/*_test.go`, `internal/service/*_test.go` (fakes gain the new method)
- Test: `internal/service/processor_ort_test.go` (skips unless `ORT_TEST=1`)

**Interfaces:**
- Consumes: `imaging`, `decodeBestFace`, `domain.ErrNoFace`.
- Produces:
  - `func NewFaceProcessor(rt *ort.Runtime, env *ort.Env, detS, recS *ort.Session) (*FaceProcessorImpl, error)`
  - `func (p *FaceProcessorImpl) Close()`
  - `func (p *FaceProcessorImpl) DetectAndCropFrame(w, h int, pix []byte) (*domain.FaceCrop, error)`

- [ ] **Step 1: Write the failing test**

```go
func TestProcessorReusesTensorsAndCloses(t *testing.T) {
    if os.Getenv("ORT_TEST") == "" { t.Skip("set ORT_TEST=1 with LD_LIBRARY_PATH pointing at libonnxruntime.so") }
    // open ../../models/scrfd_500m.onnx and ../../models/arcface_w600k_mbf.onnx
    // NewFaceProcessor must succeed; call DetectAndCropFrame(640,480,make([]byte,640*480*3)) twice
    // both calls must return errors.Is(err, domain.ErrNoFace); Close() twice must not panic
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `LD_LIBRARY_PATH=$PWD/lib ORT_TEST=1 go test ./internal/service/ -run TestProcessorReuses -v`
Expected: FAIL — `NewFaceProcessor` returns one value (compile error) or the test panics.

- [ ] **Step 3: Implement the processor**

Struct fields exactly as the spec's §8 (`pipelineMu sync.Mutex`, `canvas`, `detBlob`, `detTens`, `crop`, `recBlob`, `recTens`). Constructor builds `imaging.NewRGB(640,640)`, `make([]float32, 3*640*640)`, `ort.NewTensorValue(rt, detBlob, []int64{1,3,640,640})` and the 112×112 pair. `DetectAndCropFrame` takes `pipelineMu` once, wraps `pix` as `&imaging.RGBImage{W: w, H: h, Pix: pix}`, pads into `canvas`, packs into `detBlob`, runs detection with `detTens`, decodes via `decodeBestFace`, crops into `crop`, packs into `recBlob`, runs recognition with `recTens`, and returns `*domain.FaceCrop{Data: jpeg, Embedding: emb, BBox: normalized}`. `DetectAndCrop(data)` = `imaging.DecodeRGB` + `DetectAndCropFrame`. `runDetect`/`runRecognize` become lock-free helpers called only with `pipelineMu` held. `Close` closes both tensors (idempotent).

- [ ] **Step 4: Fix the call sites**

`di.NewFaceAPI`: `processor, err := service.NewFaceProcessor(...)` and return the error. `main.go` unchanged for now (DI already returns `error`). Add `DetectAndCropFrame` to every `domain.FaceProcessor` fake.

- [ ] **Step 5: Run build + unit suite**

Run: `go build ./... && go vet ./... && go test ./internal/... -race -count=1`
Expected: PASS.

- [ ] **Step 6: Run the ORT-gated test**

Run: `LD_LIBRARY_PATH=$PWD/lib ORT_TEST=1 go test ./internal/service/ -run TestProcessorReuses -v`
Expected: PASS (skipped without `ORT_TEST`).

- [ ] **Step 7: Commit**

```bash
git add internal/service internal/domain internal/di internal/http cmd
git commit -m "feat(service): preallocate detection buffers and reuse ONNX tensors"
```

---

### Task 8: Alignment mode

**Files:**
- Modify: `internal/service/face_processor.go`
- Test: `internal/service/alignment_test.go`

**Interfaces:**
- Consumes: `imaging.Warp5Into`, `faceHit.HasKPs`.
- Produces:
  - `type AlignmentMode string` with `AlignmentBBox AlignmentMode = "bbox"`, `AlignmentArcFace5 AlignmentMode = "arcface5"`
  - `func ParseAlignment(raw string) (AlignmentMode, error)` (delegates to `config.Alignment` semantics; empty → bbox)
  - `func (p *FaceProcessorImpl) alignCrop(dst, img *imaging.RGBImage, hit *faceHit) (usedWarp bool)` — warps when the mode is `arcface5` and `hit.HasKPs`; otherwise (or on a degenerate transform) bbox-crops into `dst`
  - `func NewFaceProcessor(rt, env, detS, recS, align AlignmentMode) (*FaceProcessorImpl, error)`

- [ ] **Step 1: Write the failing tests**

```go
func TestParseAlignment(t *testing.T) {
    // "" => AlignmentBBox, nil; "bbox" => AlignmentBBox; "arcface5" => AlignmentArcFace5
    // "nope" => error
}

func TestAlignCropFallsBackOnBadKPs(t *testing.T) {
    // img := gradient 200x160; hit := &faceHit{HasKPs: true, Box: [4]float32{40,30,120,110},
    //        KPs: collinear [5][2]float32{{10,10},{20,20},{30,30},{40,40},{50,50}}}
    // arcface5 processor: alignCrop must return false and produce the bbox-cropped pixels
}

func TestAlignCropUsesWarp(t *testing.T) {
    // valid kps (a real 5-point set) => alignCrop returns true and dst differs from the bbox crop
}
```

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/service/ -run 'ParseAlignment|AlignCrop'`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

Add the `align` field and pass `align` from `NewFaceProcessor`. `alignCrop` is the single decision point: bbox mode → `imaging.CropInto` + `imaging.ResizeInto`; arcface5 with `HasKPs` → `imaging.Warp5Into`, and on `false` fall through to the bbox path. `DetectAndCropFrame` calls `alignCrop` and no longer contains the branch. Bbox mode is unchanged, so default embeddings stay byte-identical.

- [ ] **Step 4: Run build + unit suite**

Run: `go build ./... && go test ./internal/service/ -race -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service
git commit -m "feat(service): add opt-in arcface5 keypoint alignment"
```

---

### Task 9: Frame buffer

**Files:**
- Create: `internal/framesource/buffer.go`
- Test: `internal/framesource/buffer_test.go`

**Interfaces:**
- Consumes: `imaging.RGBImage`.
- Produces:
  - `func NewBuffer() *Buffer`
  - `func (b *Buffer) Publish(fill func(dst *RGBImage))` — `fill` runs **without** the lock and writes into the spare buffer; the swap happens under `Lock()`
  - `func (b *Buffer) Dims() (w, h int)`
  - `func (b *Buffer) Latest(dst []byte) (w, h int, seq uint64, at time.Time, ok bool)`

- [ ] **Step 1: Write the failing tests**

```go
func TestLatestBeforeFirstFrame(t *testing.T)         // Dims 0,0; Latest ok=false
func TestPublishThenLatest(t *testing.T)             // 4x3, seq 1, content matches, at set
func TestLatestCopiesNotAliases(t *testing.T)        // mutate dst; next Latest unchanged
func TestPublishNewDimensions(t *testing.T)          // 4x3 then 320x240 => Dims 320,240; Latest needs the bigger dst
func TestLatestTooSmallDstReturnsFalse(t *testing.T)  // dst len < w*h*3 => ok=false, no panic
func TestConcurrentPublishAndLatest(t *testing.T)    // 4 readers + 1 writer, 2000 frames, -race;
                                                     // each frame fills every pixel with seq%251 and readers assert uniformity
```

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/framesource/ -race`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement**

Writer path: first call allocates both buffers to the source size; `Publish` swaps `cur`/`back` under `Lock()` and bumps `seq` + `at = time.Now()`. Reader path: `RLock`, if `len(dst) < w*h*3` return `(w, h, 0, time.Time{}, false)`, else `copy(dst, cur.Pix)`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/framesource/ -race -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/framesource
git commit -m "feat(framesource): add double-buffered latest-frame store"
```

---

### Task 10: Shared in-process RTSP mock

**Files:**
- Create: `internal/rtsp/rtspmock/mock.go`
- Test: `internal/rtsp/rtspmock/mock_test.go`
- Modify: `test/rtsp/rtsp_mock_test.go` (use the shared package)

**Interfaces:**
- Consumes: `gortsplib/v5`, `rtpmjpeg`.
- Produces:
  - `func New(addr string) (*Server, error)`
  - `func (s *Server) Start() error`
  - `func (s *Server) SetFrame(jpegBytes []byte)`
  - `func (s *Server) URL() string` — `rtsp://<addr>/stream`
  - `func (s *Server) Stop()`
  - `func SolidJPEG(w, h int) ([]byte, error)`
  - `func PadJPEGToSide(src []byte, side int) ([]byte, error)`

- [ ] **Step 1: Move the existing implementation**

Copy `mockRTSP` from `test/rtsp/rtsp_mock_test.go` into the new package with exported names and no `testing` import. Keep the 10 fps pump, the 64-byte padding rule (multiple of 8), and the `127.0.0.1:18554` / `18000` / `18001` ports.

- [ ] **Step 2: Write the failing test**

```go
func TestServerServesFrames(t *testing.T) {
    srv, err := rtspmock.New("127.0.0.1:18554")
    if err != nil { t.Skipf("port busy: %v", err) }
    defer srv.Stop()
    // Start, SetFrame(SolidJPEG(320,240)), dial URL with gortsplib, assert one M-JPEG frame decodes to 320x240
}
```

- [ ] **Step 3: Run it to confirm it fails, then passes**

Run: `go test ./internal/rtsp/rtspmock/ -v`
Expected: FAIL before the move, PASS after.

- [ ] **Step 4: Update `test/rtsp/rtsp_mock_test.go`**

Replace the local type with `rtspmock.New(mockRTSPAddr)`; keep the test names and assertions unchanged so the container suite still covers the mock.

- [ ] **Step 5: Run the RTSP suite**

Run: `make test-rtsp`
Expected: PASS (6 tests, unchanged).

- [ ] **Step 6: Commit**

```bash
git add internal/rtsp/rtspmock test/rtsp
git commit -m "test(rtsp): share the in-process M-JPEG mock server"
```

---

### Task 11: Streaming source

**Files:**
- Create: `internal/framesource/source.go`
- Test: `internal/framesource/source_test.go`

**Interfaces:**
- Consumes: `Buffer`, `imaging.ToRGB/ConvertInto`, `rtspmock` (tests only).
- Produces:
  - `func New(url string, log zerolog.Logger) *Source`
  - `func (s *Source) Run(ctx context.Context)`
  - `func (s *Source) Dims() (w, h int)`
  - `func (s *Source) Latest(dst []byte) (w, h int, seq uint64, at time.Time, ok bool)`
  - test seam: `func newSource(url string, log zerolog.Logger, stream streamFunc) *Source` with `type streamFunc func(ctx context.Context, url string, onFrame func(image.Image)) error`

- [ ] **Step 1: Write the failing tests**

```go
func TestSourcePublishesMockFrames(t *testing.T)  // rtspmock 320x240, Run in a goroutine, wait for seq>=3, dims 320,240
func TestSourceHandlesResolutionChange(t *testing.T) // fake stream emits 640x480 then 320x240 => Dims follows, Latest content matches size
func TestSourceReconnects(t *testing.T)           // fake stream: 2 x "connection refused" then blocks; assert 2 warn lines in the zerolog buffer
func TestSourceBacksOffOnUnsupportedCodec(t *testing.T) // fake stream returns "H.265 (unsupported codec)"; assert backoff >= 1s and exactly one error line
func TestSourceRunReturnsOnCancel(t *testing.T)    // cancel while the fake stream blocks; Run returns within 2s
```

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/framesource/ -race`
Expected: FAIL — undefined `Source`.

- [ ] **Step 3: Implement**

`Run`: loop calling `stream(ctx, url, s.publish)`; on return, if `ctx.Err() != nil` exit; else backoff `1s, 2s, 4s … 30s` (reset to 1 s after 30 s of successful streaming); special-case errors containing `unsupported codec` to log once per attempt and jump to the 30 s cap. `publish(img image.Image)` calls `buf.Publish(func(dst){ imaging.ConvertInto(dst, img) })` after reallocating `dst` when the size changed. `stream` performs the gortsplib dance from `rtsp.Reader` but with a single long-lived client: Describe → prefer MJPEG → `SetupAll` → `Play` → `OnPacketRTP` → decode (MJPEG: `rtpmjpeg` → `jpeg.Decode`; H.264: `rtph264` → length-prefixed AVC → `govidh264.Decoder.DecodePacket`), calling `onFrame` for every fully decoded frame (no `frameCount < 5` hack; nil frames are skipped). Return `ctx.Err()` on cancel; close the client on return.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/framesource/ -race -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/framesource
git commit -m "feat(framesource): stream the sub-stream into the buffer with backoff"
```

---

### Task 12: Burst scanner, core path

**Files:**
- Modify: `internal/domain/ports.go` (`FrameSource`, `StreamScanner`)
- Modify: `internal/service/face_service.go`
- Test: `internal/service/burst_test.go`

**Interfaces:**
- Consumes: `domain.FrameSource`, `domain.ErrNoFace`, `FaceProcessor.DetectAndCropFrame`.
- Produces:
  - `domain.FrameSource` (exact interface from the spec §6.1)
  - `domain.StreamScanner`
  - `func (s *FaceServiceImpl) WithFrameSource(src domain.FrameSource)`
  - `func (s *FaceServiceImpl) WithBurst(window, sample time.Duration)`
  - `func (s *FaceServiceImpl) ScanBurst(ctx context.Context) (*domain.StreamCheckResult, error)`

- [ ] **Step 1: Write the failing tests**

```go
func TestScanBurstReturnsFirstMatch(t *testing.T)  // 5 frames, alice sim 0.9 => matched, name alice, DurationMs >= 0
func TestScanBurstWritesOneAuditRow(t *testing.T)  // 10 no-match frames => exactly 1 Append call, status not_matched
func TestScanBurstSkipsUnchangedSeq(t *testing.T)  // same seq twice => processor called once
func TestScanBurstReadsUsersOnce(t *testing.T)     // 5 samples => fake repo ListAll called once
func TestScanBurstNoFrameSource(t *testing.T)      // WithFrameSource never called => error, no panic
```

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/service/ -run ScanBurst`
Expected: FAIL — undefined `ScanBurst`.

- [ ] **Step 3: Implement the core loop**

`ScanBurst`: derive `context.WithTimeout(ctx, window)`; read the threshold under `s.mu` (do not hold it); `users.ListAll()` once; allocate scratch from `src.Dims()`; poll `Latest` every 20 ms for a `seq` newer than the burst start; skip a sample when `seq` is unchanged; enforce `sample` between inferences; call `DetectAndCropFrame`; on `ErrNoFace` continue; on match (`>= threshold`) write **one** audit row (`time`, `endpoint "stream-check"`, name, similarity, `matched true`, `status matched`, `duration_ms`, crop `face_image`, `face_thumb`, `embedding`) and return `status "ok"`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/service/ -run ScanBurst -race -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/domain internal/service
git commit -m "feat(service): scan buffered frames and return the first match"
```

---

### Task 13: Burst scanner, deadline and failure paths

**Files:**
- Modify: `internal/service/face_service.go`
- Test: `internal/service/burst_test.go` (extend)

**Interfaces:**
- Consumes: Task 12's `ScanBurst`, `noFaceFrameMaxDim`.
- Produces:
  - `func noFaceFrameFromRGB(img *imaging.RGBImage) string` (unexported, returns base64 JPEG downscaled to `noFaceFrameMaxDim`)
  - completed deadline/fallback/error semantics from spec §7 and §10.

- [ ] **Step 1: Write the failing tests**

```go
func TestScanBurstDeadlineReturnsBest(t *testing.T)     // scores 0.3,0.6,0.2 => 0.6 + that frame's crop
func TestScanBurstAllNoFaceWritesNoFaceRow(t *testing.T) // 5 ErrNoFace => 1 row, status no_face, FaceImage non-empty
func TestScanBurstProcessorErrors(t *testing.T)          // 5 generic errors => not ok + reason "Face processing failed" + 0 rows
func TestScanBurstFallbackWhenNoFrames(t *testing.T)     // frame source ok=false => CheckStream called once
func TestScanBurstCancel(t *testing.T)                   // cancel after the first sample => returns not ok, 0 rows
func TestScanBurstDoesNotHoldServiceMutex(t *testing.T)  // processor blocks on a channel; GetSettings must return meanwhile
```

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/service/ -run 'ScanBurst' -count=1`
Expected: FAIL on the new assertions.

- [ ] **Step 3: Implement**

At deadline: faces seen → `not ok`, `reason "Face detected but similarity below threshold"`, best similarity + crop, `matched false`, one `not_matched` row with the best embedding; no faces but ≥1 successful inference → one `no_face` row with `noFaceFrameFromRGB`; zero successful samples → `not ok`, `reason "Face processing failed"`, no row. Before the loop, if no fresh frame arrives within `3 × sample`, call `s.rtspReader.ReadFrame(streamURL, 10*time.Second)` + `CheckStreamImage` exactly once (log `msg "frame buffer cold, falling back to one-shot check"`). Check `ctx.Err()` at the top of every iteration and return `not ok` with `reason "scan cancelled"` without an audit row.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/service/ -race -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/service
git commit -m "feat(service): handle burst deadlines, no-face and cold-buffer fallback"
```

---

### Task 14: Context-aware MQTT trigger and coalescing

**Files:**
- Modify: `internal/mqtt/bridge.go`
- Test: `internal/mqtt/bridge_test.go`

**Interfaces:**
- Consumes: `domain.StreamCheckResult`.
- Produces:
  - `type CheckFunc func(ctx context.Context) (*domain.StreamCheckResult, error)`
  - `Config.QueueDepth` default **1**
  - worker calls `b.check(b.ctx)`

- [ ] **Step 1: Write the failing tests**

```go
func TestQueueCoalescesAtDepthOne(t *testing.T) // default config, 1 slow check + 3 triggers => 2 checks total
func TestCheckReceivesContext(t *testing.T)     // CheckFunc captures ctx; Stop() cancels it
```

Update existing tests to the new `CheckFunc` signature.

- [ ] **Step 2: Run them to confirm they fail**

Run: `go test ./internal/mqtt/ -count=1`
Expected: FAIL — signature mismatch.

- [ ] **Step 3: Implement**

Change the signature, pass `b.ctx` to the worker's check call, and change the `QueueDepth <= 0` fallback from 16 to 1. Keep the drop-oldest logic untouched.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/mqtt/ -race -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mqtt
git commit -m "feat(mqtt): make triggers context-aware and coalesce by default"
```

---

### Task 15: DI lifecycle and main wiring

**Files:**
- Modify: `internal/di/wire.go`
- Modify: `cmd/face-api/main.go`

**Interfaces:**
- Consumes: `config.*`, `service.WithFrameSource/WithBurst`, `service.ParseAlignment`, `framesource.New`.
- Produces:
  - `di.Config{StreamURL string; Alignment service.AlignmentMode; BurstWindow, SampleInterval time.Duration; IntraOpThreads int}`
  - `FaceAPI.Start(ctx context.Context)`, `FaceAPI.Stop()`, `FaceAPI.Close()`, `FaceAPI.ScanBurst func(ctx context.Context) (*domain.StreamCheckResult, error)`
  - `main` selects the burst trigger when `RTSP_STREAM_URL` is set.

- [ ] **Step 1: Implement DI**

`NewFaceAPI` builds `framesource.New(cfg.StreamURL, cfg.Logger)` only when `cfg.StreamURL != ""`, calls `faceService.WithFrameSource(src)` and `faceService.WithBurst(cfg.BurstWindow, cfg.SampleInterval)`, and stores the source. `Start(ctx)` stores a cancel and runs `src.Run` in a goroutine; `Stop()` cancels and waits; `Close()` stops the source and closes the processor.

- [ ] **Step 2: Implement main**

```go
threads, err := config.IntraOpThreads(os.Getenv)
if err != nil { logger.Warn().Err(err).Msg("invalid ONNX_INTRA_OP_THREADS, using default") }
sessOpts := &ort.SessionOptions{IntraOpNumThreads: threads}

alignRaw, err := config.Alignment(os.Getenv)
if err != nil { log.Fatalf("FACE_ALIGNMENT: %v", err) }

trigger := func(ctx context.Context) (*domain.StreamCheckResult, error) { return faceAPI.CheckStream(rtspURL) }
if streamURL != "" { trigger = faceAPI.ScanBurst }
```

Pass `StreamURL`, `Alignment`, `BurstWindow`, `SampleInterval`, `IntraOpThreads` into `di.Config`; call `faceAPI.Start(context.Background())` and `defer faceAPI.Close()` next to the HTTP server lifecycle. Log `intra_op_threads`, `alignment` and `stream_buffer` at startup.

- [ ] **Step 3: Build and vet**

Run: `go build ./... && go vet ./...`
Expected: no output.

- [ ] **Step 4: Smoke the wiring without a camera**

Run: `LD_LIBRARY_PATH=$PWD/lib FACES_DB_PATH=/tmp/opencode/smoke.db ENABLE_UI=false go run ./cmd/face-api` in the background, then `curl -s localhost:8081/healthz`, then send SIGINT.
Expected: `ok`, clean shutdown, startup log shows `intra_op_threads=4 alignment=bbox`.

- [ ] **Step 5: Commit**

```bash
git add internal/di cmd
git commit -m "feat(di): start the frame source and select the burst trigger"
```

---

### Task 16: CI tests

**Files:**
- Create: `.github/workflows/test.yml`
- Modify: `Makefile`

**Interfaces:**
- Consumes: `go test`, `make test`.
- Produces: `make test-unit` and two CI jobs (`unit`, `integration`).

- [ ] **Step 1: Add the Makefile target**

```make
test-unit:
	@echo "Running unit tests (no Docker, no ONNX runtime)..."
	go test -race -count=1 ./internal/... ./cmd/...
```

Add `test-unit` to `.PHONY`.

- [ ] **Step 2: Verify the target**

Run: `make test-unit`
Expected: PASS, no ORT library needed.

- [ ] **Step 3: Write the workflow**

Baseline facts measured on `feat/live-burst-pipeline`: `go build ./...`, `go vet ./...` and
`go test -race ./internal/... ./cmd/...` all pass, but **7+ files are not gofmt-clean**
(`internal/http/ui.go`, `internal/mqtt/bridge.go`, `internal/service/face_processor.go`, …).
Do **not** reformat unrelated files in this branch; scope the format gate to the files the
change touches.

```yaml
name: Tests
on:
  push:
    branches: [main, 'release/**']
  pull_request:
    branches: [main]
  workflow_dispatch:

jobs:
  unit:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: gofmt (changed Go files only)
        run: |
          git fetch --no-tags --depth=1 origin main || true
          files=$(git diff --name-only origin/main...HEAD -- '*.go' || true)
          if [ -z "$files" ]; then echo "no Go changes"; exit 0; fi
          unformatted=$(gofmt -l $files) || true
          if [ -n "$unformatted" ]; then echo "$unformatted"; exit 1; fi
      - name: vet
        run: go vet ./...
      - name: unit tests
        run: make test-unit

  integration:
    if: github.event_name == 'pull_request' || github.event_name == 'workflow_dispatch'
    needs: unit
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          cache: true
      - name: e2e + rtsp
        run: make test
```

- [ ] **Step 4: Validate the YAML and the commands**

Run: `python3 -c "import yaml;yaml.safe_load(open('.github/workflows/test.yml'))" && make test-unit`
Expected: no output from the YAML check; PASS from the tests.

- [ ] **Step 5: Note the required checks (manual, not code)**

After the PR is opened, enable `Tests / unit` as a required status check on `main`. The integration job stays advisory because it builds two container images and needs Docker.

- [ ] **Step 6: Commit**

```bash
git add .github/workflows/test.yml Makefile
git commit -m "ci: run gofmt, vet, unit and integration tests"
```

---

### Task 17: Documentation and Helm examples

**Files:**
- Modify: `README.md`
- Modify: `AGENTS.md` (local only — **do not commit**)
- Modify: `deploy/helm/face-api/values.yaml`

**Interfaces:**
- Consumes: the final env names and defaults.
- Produces: operator-facing docs.

- [ ] **Step 1: README env table**

Add rows for `ONNX_INTRA_OP_THREADS`, `RTSP_STREAM_URL`, `FACE_ALIGNMENT`, `TRIGGER_BURST_MS`, `TRIGGER_SAMPLE_MS`; update the `MQTT_QUEUE_DEPTH` default to 1; add a short "Live trigger scanning" section explaining the one-audit-row-per-burst behaviour and that `arcface5` requires re-enrollment.

- [ ] **Step 2: Helm values comments**

Add to `extraEnv`:

```yaml
#   - name: RTSP_STREAM_URL
#     value: rtsp://user:pass@camera:554/h264Preview_01_sub
#   - name: ONNX_INTRA_OP_THREADS
#     value: "4"
```

- [ ] **Step 3: AGENTS.md (local, never committed)**

Update the package layout, config table, MQTT section (queue default 1, ctx trigger, burst), audit semantics (one row per burst), testing section (`make test-unit`), and the CI/CD section (the new `Tests` workflow). Add the alignment migration warning.

- [ ] **Step 4: Verify nothing sensitive is staged**

Run: `git status --porcelain`
Expected: `README.md` and `deploy/helm/face-api/values.yaml` only.

- [ ] **Step 5: Commit**

```bash
git add README.md deploy/helm/face-api/values.yaml
git commit -m "docs: document the live burst pipeline knobs"
```

---

### Task 18: Full verification, benchmark and PR

**Files:**
- No code changes expected; fix anything the verification finds.

**Interfaces:**
- Consumes: every prior task.
- Produces: a green branch and the numbers for the PR description.

- [ ] **Step 1: Unit and race**

Run: `make test-unit`
Expected: PASS.

- [ ] **Step 2: Container suites**

Run: `make test`
Expected: PASS (`test/e2e` + `test/rtsp`).

- [ ] **Step 3: ORT-gated processor tests**

Run: `LD_LIBRARY_PATH=$PWD/lib ORT_TEST=1 go test ./internal/service/ -run 'Processor|Alignment|Fallback' -v`
Expected: PASS (not skipped).

- [ ] **Step 4: Warp-mode e2e**

Add `TestFaceAlignmentWarpE2E` (container env `FACE_ALIGNMENT=arcface5`, enroll + recognize, `matched == true`, `similarity >= 0.45`) and `TestThreadsEnvE2E` (invalid `ONNX_INTRA_OP_THREADS=abc` still boots). This step is the only place those tests are added because they need the container harness.

Run: `make test-e2e`
Expected: PASS including the two new tests.

- [ ] **Step 5: Benchmark**

Run: `LD_LIBRARY_PATH=$PWD/lib ORT_BENCH=1 go test ./internal/service/ -bench BenchmarkDetectFrame -benchmem -run '^$'`
Expected: record `ns/op` and `B/op`; the 1.2 MB canvas and 4.9 MB blob no longer appear in allocations.

- [ ] **Step 6: Manual Pi check (recorded in the PR)**

Deploy `:feat-live-burst-pipeline` to the Pi with `RTSP_STREAM_URL` set to the sub-stream and `ONNX_INTRA_OP_THREADS=4`; publish to `<base>/trigger`; record trigger→`<base>/matched` latency, `top` CPU during the burst, and the new audit row. Target: match published under 1.5 s when a face is present.

- [ ] **Step 7: Commit any fixes, push, open the PR**

```bash
git push -u origin feat/live-burst-pipeline
gh pr create --title "Live frame buffer + trigger burst pipeline" \
  --body "Implements docs/superpowers/specs/2026-11-11-live-burst-pipeline-design.md. Adds imaging/framesource packages, preallocated ONNX buffers, opt-in arcface5 alignment, single-audit-row bursts, and CI tests."
```

---

## Self-Review

**Spec coverage:** every spec section maps to a task — §6.1 imaging → Tasks 2/3/5; framesource → 9/10/11; domain/service → 6/7/12/13; MQTT → 14; DI/lifecycle → 15; config table → 1/15; failure modes → 11/13; compatibility → 17; testing → each task plus 16/18; phases → task order. CI was added at the user's request (Tasks 16 and 18).

**Deviation from the spec, deliberate:** the spec listed `service.WithAlignment`; alignment is implemented as a `NewFaceProcessor` parameter because it is a preprocessing concern (`ParseAlignment` still lives in `service`). The spec's `MQTT_QUEUE_DEPTH` and audit decisions are unchanged.

**Type consistency:** `Latest(dst []byte) (w, h int, seq uint64, at time.Time, ok bool)` is identical in the domain interface (Task 12), the buffer (Task 9) and the source (Task 11). `detOutputs` (Task 6) is consumed by Task 7. `AlignmentMode` (Task 8) is consumed by `di.Config` (Task 15).

**Placeholder scan:** no TBDs; every test step names its assertions; every verification step names the command and expected result.

**Proportion:** ~18 tasks, each ≤120 lines of plan; code bodies appear only for the Umeyama algorithm and the CI workflow, both of which the tests do not determine.
