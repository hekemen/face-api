# Face API - Agent Documentation

## Project Overview
Face API is a Go-based face recognition server that runs two ONNX models (SCRFD face detection + ArcFace recognition) through the ONNX Runtime shared library via pure-Go bindings. OpenCV (via gocv) is used for image I/O and processing only. The server provides REST API endpoints for enrolling faces, recognizing faces in images, checking RTSP streams, and listing enrolled users.

## Architecture

### Core Components
- **FaceServer** (`internal/server/face.go`): Main struct holding the in-memory cache, the persistent bbolt datastore, and the two ONNX Runtime inference sessions (detection + recognition).
- **HTTP Server** (`cmd/face-api/main.go`, `internal/server/server.go`): Standard Go HTTP server on port 8081 with handler registration and model loading.
- **Inference**: `onnxruntime-purego` loads `libonnxruntime.so` and runs both ONNX models on the CPU. gocv is used only for image decode, resize, crop, blob layout, and RTSP capture.
- **Data Persistence**: bbolt database (`faces.db`) for storing face embeddings with an in-memory cache for fast lookups.

### API Endpoints
- `POST /enroll` - Enroll a face with a name and image
- `POST /recognize` - Recognize a face in an image
- `POST /stream-check` - Check RTSP stream for recognized faces (3-second timeout)
- `GET /users` - List all enrolled users
- `GET /audit` - List recent face-scan audit entries (newest first)
- `GET /healthz` - Liveness check (200 while the process runs)
- `GET /readyz` - Readiness check (200 once bbolt is open and the cache is hydrated)

### Request/Response Format
- All endpoints accept `multipart/form-data` with `image` field
- `/enroll` also requires `name` field; repeat calls with the same `name` append pictures up to a max of **3** per user (4th returns 400). Stored embeddings are a JSON array-of-arrays; legacy single-embedding values are migrated on startup.
- `/stream-check` accepts an optional `rtsp_url` form field; if omitted it falls back to the `RTSP_URL` env var / config value (400 only if neither is set)
- Every success/business response includes a `duration_ms` integer (operation wall-clock time)
- Recognition returns: `{"name": "string", "similarity": float, "matched": bool, "duration_ms": int}`
- Stream check returns: `{"status": "ok|not ok", "name": "string", "similarity": float, "reason": "string", "duration_ms": int}`
- Enroll returns: `{"status": "enrolled", "name": "string", "duration_ms": int}`
- `/audit` returns: `{"count": int, "duration_ms": int, "entries": [{"time": RFC3339, "endpoint": "enroll|recognize|stream-check", "name": "string", "similarity": float, "matched": bool, "duration_ms": int}]}` (newest first, up to 100 entries)

### Request Logging & Audit Trail
- **Request logging**: every HTTP request is logged to stdout as a JSON zerolog line with `method`, `path`, `status`, `remote`, and `duration_ms` via `FaceServer.RequestLogging` middleware (`internal/server/server.go`, `statusRecorder` wrapper). **`/healthz` and `/readyz` probes are skipped** so they do not pollute the log stream.
- **Audit trail**: every face scan (enroll, recognize, and each stream-check frame that produced an embedding) is persisted to a bbolt `"Audit"` bucket with a timestamp. **Audit entries are only written when a face is detected** — a no-face request (e.g. solid-color image) produces no entry. Keys are `NextSequence()` (big-endian uint64), values are JSON `AuditEntry` structs.
- **bbolt write constraint**: audit entries are accumulated during a request and flushed in a **single `db.Update` transaction** (`storeAudit`) — stream-check samples ~30 frames in 3 seconds, so writing one transaction per frame would cost 30 fsyncs/pages and stall the 100ms sampling loop. Never write audit rows inside the per-frame loop; batch and flush once at request end.

## Dependencies

### Go Direct Dependencies
- `github.com/shota3506/onnxruntime-purego` - Pure-Go ONNX Runtime bindings; dynamically loads `libonnxruntime.so` (no cgo in the inference path)
- `github.com/rs/zerolog` - structured JSON logging (request logging + error logging)
- `gocv.io/x/gocv v0.31.0` - OpenCV Go bindings for **image processing** (decode, resize, crop, RTSP capture) — not inference
- `go.etcd.io/bbolt v1.3.11` - Persistent key-value storage for face embeddings

### Runtime Library
- `libonnxruntime.so` (ONNX Runtime 1.23.0) is required at runtime: bundled in the Docker image, or fetched locally by `make download-libonnxruntime` into `lib/` and made visible via `LD_LIBRARY_PATH`.

### System Dependencies (Dockerfile)
- **Build stage**: `libgtk-3-dev`, `libavcodec-dev`, `libavformat-dev`, `libswscale-dev`, `libopencv-dev`, `pkg-config` (OpenCV headers needed only to compile gocv) plus `curl` to fetch `libonnxruntime.so`
- **Runtime stage**: `libgtk-3-0`, `libavcodec59`, `libavformat59`, `libswscale6`, `libopencv-highgui4.6`, `libopencv-imgproc4.6`, `libopencv-core4.6`, `libopencv-videoio4.6`, `libopencv-imgcodecs4.6`, `libopencv-video4.6`, `libopencv-objdetect4.6`, `libopencv-photo4.6`

## Build Process

### Local Build
```bash
make build                    # Downloads models, builds Go binary with CGO_ENABLED=1
make download-libonnxruntime  # Fetches libonnxruntime.so (host arch) into lib/
make run                      # Build + fetch lib + run on port 8081 (sets LD_LIBRARY_PATH)
```
Local compilation requires OpenCV dev headers and pkg-config matching gocv v0.31.0. If the host lacks them, build and test inside Docker (`make test`).

### Docker Build
```bash
make docker-build       # Build single-arch image
make docker-build-pi5   # Build arm64 image for Raspberry Pi 5 (buildx + qemu)
make docker-build-all   # Build multi-arch (amd64 + arm64)
make docker-run         # Build and run with volume-mounted faces.db
```

### Multi-Stage Dockerfile
1. **Builder stage**: `golang:1.27-bookworm` with OpenCV dev dependencies; downloads `libonnxruntime.so` (1.23.0, arch-aware via `TARGETARCH`); compiles the Go binary (`go build .`).
2. **Runtime stage**: `debian:bookworm-slim` with minimal OpenCV runtime libraries; includes the binary, `libonnxruntime.so` at `/usr/local/lib` (with `LD_LIBRARY_PATH` set), and the two dynamic ONNX models.

## ONNX Models & Inference

### Detection Model (SCRFD 500M)
- **File**: `models/scrfd_500m.onnx` (dynamic input shape `[1,3,H,W]`)
- **Source**: `https://huggingface.co/deepghs/insight-face/resolve/main/scrfd_500m.onnx`
- **Run size**: the input image is padded to a 640×640 square (aspect preserved, centered on black)
- **Blob**: `(pixel - 127.5) / 128.0`, RGB channel order (NCHW float32)
- **Input name**: `input.1`; **outputs** (classification scores + bbox regs per FPN level):

  | stride | score output | reg output |
  |--------|--------------|------------|
  | 8      | `443`        | `446`      |
  | 16     | `468`        | `471`      |
  | 32     | `493`        | `496`      |

  Each level exposes `2 × (640/stride)²` anchors in cell-major layout.
- **Decode** (insightface `distance2bbox`): anchor center at `(col*stride, row*stride)`; index `i` → cell `i/2`, anchor `i%2`; box = `x1 = cx − reg0*stride`, `y1 = cy − reg1*stride`, `x2 = cx + reg2*stride`, `y2 = cy + reg3*stride`.
- Keep detections with score ≥ 0.5, take the highest-scoring box, divide by the canvas scale to map back to the original frame, and clamp to frame bounds.

### Recognition Model (ArcFace MBF W600K)
- **File**: `models/arcface_w600k_mbf.onnx`
- **Source**: `https://huggingface.co/deepghs/insight-face/resolve/main/buffalo_s/w600k_mbf.onnx`
- **Input**: 112×112 face crop; **blob**: `(pixel - 127.5) / 127.5`, RGB channel order (NCHW float32)
- **Input name**: `input.1`; **output**: `516` → 512-dim embedding

### Why ONNX Runtime (not OpenCV DNN)
OpenCV 4.6 DNN cannot run the SCRFD ONNX model: its dynamic-shape FPN `Add` layers trigger an assertion failure and **SIGABRT inside cgo, killing the whole process** during any inference, and `Forward("")` returns only the first output tensor. ONNX Runtime executes the full multi-output model correctly. Do not reintroduce `gocv.ReadNetFromONNX`/`Net.Forward` for these models.

### ONNX Logging Level (GPU device discovery warning)
The ORT env is created with `ort.LoggingLevelError`, NOT `Warning`. On scaffolds without `/sys/class/drm/card*/device/vendor` (e.g. the Pi, the e2e container), ORT's internal EP registration logs `GPU device discovery failed: device_discovery.cc:89 ReadFileContents` at WARNING during **env creation** (`Environment::CreateAndRegisterInternalEps` → `SortDevicesByType` → `DeviceDiscovery::GetDevices()`). It is benign (the app is CPU-only), but noisy. Keeping the env at `Error` suppresses it. Do not revert to `Warning` without also verifying container logs stay clean.

## Testing

### E2E Tests (`test/e2e/e2e_test.go`)
- Uses `testcontainers-go` to build the image from the repo Dockerfile and spin up a container
- Waits for HTTP GET /users on port 8081, then downloads Anthony Hopkins test images from GitHub
- Tests enrollment (201), listing users, recognition (matched, name, similarity ≥ 0.45), missing-name (400), and no-face (400) cases
- Also asserts: request-log JSON lines in container logs (via testcontainers `Logs(ctx)`), `/audit` returns newest-first entries with parseable RFC3339 times, no-face scans create **no** audit entries, `/healthz` and `/readyz` requests are **not** logged (while `/users` still is), and the container logs do **not** contain the ONNX `GPU device discovery failed` warning
- Run with: `make test-e2e` or `go test -v -count=1 ./test/e2e/...`

### RTSP Stream-Check Tests (`test/rtsp/`)
- Uses `testcontainers-go` to build the image and run it with **host networking** (`NetworkMode: "host"`)
- Runs an **in-process gortsplib MJPEG-over-RTP mock server** (`test/rtsp/rtsp_mock_test.go`) on `127.0.0.1:18554`, so no external RTSP source or wifi camera is needed
- The mock streams at 10 fps; frames are padded to a multiple-of-8 JPEG size (RTP/M-JPEG header constraint, `width/8`)
- Tests: matched-face stream (status ok, name, similarity ≥ 0.45), config-fallback (request omits `rtsp_url`, uses `RTSP_URL` env), solid-color no-face stream (3s timeout → not ok), unreachable endpoint (not ok), and stream-check entries appearing in the audit log
- Run with: `make test-rtsp` or `go test -v -count=1 ./test/rtsp/...`
- `make test` runs both suites

### Load Testing
```bash
make load-test              # Full load test suite
make load-test-users        # Load test GET /users
make load-test-recognize    # Load test POST /recognize
```
Configurable via `REQUESTS` (default: 500) and `CONCURRENCY` (default: 10) environment variables. Uses `hey` for HTTP load testing.

## CI/CD Pipeline (`.github/workflows/build.yml`)
- Triggers: push to main/release/**, PR to main, manual dispatch
- Single Docker Buildx job builds both `linux/amd64` and `linux/arm64` in one manifest push
- Uses QEMU (`docker/setup-qemu-action`) for the arm64 build, Buildx caching via GHA cache
- Downloads both ONNX models into `models/` before building (weights are gitignored)
- Pushes to GHCR (`ghcr.io/hekemen/face-api`); login skipped for PRs
- Tags: branch, PR, sha, latest (on main push)

## Kubernetes Deployment (Helm)
- Chart: `deploy/helm/face-api`
- Installs Deployment (1 replica), Service (ClusterIP :8081), and a PersistentVolumeClaim for `faces.db`
- The app reads its database path from `FACES_DB_PATH` (default `faces.db`); the chart mounts the PVC at `/data` and sets `FACES_DB_PATH=/data/faces.db` — bbolt creates the file on first boot, no init container needed
- Optional: ingress (disabled by default), `existingClaim`/`storageClassName` for the PVC, extra env via `extraEnv`
- Usage: `helm install face-api deploy/helm/face-api --set image.tag=<tag>`

## Configuration

### Environment Variables
- `RTSP_URL` - RTSP stream URL for stream-check endpoint (in `.env`)
- `FACES_DB_PATH` - bbolt database file path (default `faces.db`); set to `/data/faces.db` by the Helm chart

### Database
- `faces.db` - bbolt database file, persisted via Docker volume mount
- Bucket name: `"Faces"` (byte slice)
- Key: user name (string)
- Value: JSON array-of-arrays of float32 embeddings (legacy flat single-embedding values are migrated on startup)
- Audit bucket name: `"Audit"` (byte slice); keys are `NextSequence()` big-endian uint64, values are JSON audit entries

## Code Structure

```
face-api/
├── cmd/
│   └── face-api/
│       └── main.go           # Server entry point; loads libonnxruntime, creates ORT env/sessions
├── internal/
│   └── server/
│       ├── face.go           # Blob building, SCRFD/ArcFace runs, detection/crop/embedding
│       ├── server.go         # NewFaceServer, EnsureBucket, request logging + audit handlers
│       └── types.go          # API response structs
├── models/                   # ONNX weights (downloaded by make download-model, gitignored)
│   ├── scrfd_500m.onnx
│   └── arcface_w600k_mbf.onnx
├── test/
│   └── e2e/
│       └── e2e_test.go       # End-to-end tests
├── deploy/
│   └── helm/
│       └── face-api/         # Helm chart (Deployment, Service, PVC, ingress)
├── Makefile                  # Build, test, load test, model + libonnxruntime download
├── Dockerfile                # Multi-stage build, bundles libonnxruntime.so + models
├── go.mod                    # Go module dependencies
├── .env                      # Environment configuration
├── lib/                      # (local) downloaded libonnxruntime.so, gitignored
└── .github/
    └── workflows/
        └── build.yml         # CI/CD pipeline
```

## Key Implementation Details

### Face Detection Pipeline (`detectAndCrop112`)
1. Pad input to a 640×640 square, keeping aspect ratio (`resizePadToSquare` returns the canvas + scale).
2. Build an NCHW float32 blob `(pixel-127.5)/128`, RGB order (`matToNCHW`).
3. `runDetect`: run SCRFD via the ORT session; collect score/reg tensors for all three FPN levels.
4. Decode boxes with insightface distance decode; keep score ≥ 0.5; take the highest-confidence box.
5. Divide by the canvas scale to map back to the original frame; clamp to frame bounds.
6. Crop the ROI and bilinear-resize to 112×112 (the ArcFace input).

### Embedding Extraction (`extractEmbedding`)
1. Build an NCHW blob from the 112×112 crop, `(pixel-127.5)/127.5`, RGB order.
2. `runRecognize`: run ArcFace; read output `516` → 512-dim float32 vector.

### Face Matching
- Cosine similarity between query embedding and enrolled embeddings; for users with multiple pictures the best (max) score across their embeddings is used
- Threshold: 0.45 (configurable in `FaceServer.threshold`)
- Returns best match if similarity >= threshold, otherwise "Unknown"

### Concurrency
- `sync.RWMutex` protects the in-memory cache (`dbMap`): read locks for lookups, write locks for enrollment
- ONNX Runtime sessions are not thread-safe: `inferMu` serializes all `Run` calls
- `ort.GetTensorData` returns a copy, so output values can be closed immediately after copying
- Deployment uses `Recreate` strategy (not RollingUpdate) because bbolt takes an exclusive OS file lock — two pods cannot open the same `faces.db` simultaneously

## Operational Notes

### Model Files
- Only the dynamic models `models/scrfd_500m.onnx` and `models/arcface_w600k_mbf.onnx` are used (local, Docker, and CI). No static/simplified variants exist.
- The Makefile checks for model existence before downloading; the CI workflow downloads both models into `models/` before building.
- `libonnxruntime.so` 1.23.0 (x64/aarch64): bundled in the Docker image; `make install-libonnxruntime` fetches it into `lib/` for local runs.

### Docker Volume Persistence
- `faces.db` is volume-mounted to preserve enrolled users across container restarts
- On startup, embeddings are hydrated from bbolt into memory cache
- In Kubernetes the Helm chart mounts a PVC at `/data` and sets `FACES_DB_PATH=/data/faces.db`

### RTSP Stream Check
- Connects to RTSP URL, samples frames every 100ms
- 3-second timeout for recognition
- Returns first recognized face above threshold, or "not ok" with reason

### Error Handling
- Missing image field: 400 Bad Request
- Face detection failure / no face detected: 400 Bad Request
- Invalid image format: 400 Bad Request
- Face processing (embedding) failure: 500 Internal Server Error
- Database write failure: 500 Internal Server Error
- Method not allowed: 405 Method Not Allowed

## Raspberry Pi (pi.home.arpa)

### SSH Access
```bash
ssh -l pi pi.home.arpa
```

### Pi Setup
- **OS**: Raspberry Pi OS (Linux)
- **Runtime**: Both Docker and K3s (Kubernetes) installed
- **Docker**: Standalone containers (used for local testing)
- **K3s**: Production deployment via Helm charts
- **K3s kubeconfig**: `sudo cat /etc/rancher/k3s/k3s.yaml` on the Pi

### K3s Namespace
- `face-api` — face-api Deployment + Service (port 8081)
- Other namespaces: `ftp`, `homeassistant`, `homebridge`, `kube-system`, `mqtt`, `smartmeter`, `zigbee`

### External Access (Ingress)
- The chart ships a Traefik ingress + `stripPrefix` Middleware. With `values.pi.yaml` the API is reachable at **`http://pi.home.arpa/face/...`** (e.g. `GET /face/users`, `POST /face/enroll`, `POST /face/recognize`).
- The app only registers handlers at root paths, so the Middleware strips the `/face` prefix before the request reaches the service. Do not change the app routes when serving under a path prefix.
- Traefik is the ingress controller (`ingressclass.kubernetes.io/is-default-class: "true"`); its LoadBalancer is on `192.168.178.35`, port 80/443.
- No TLS/cert-manager currently installed on the cluster — HTTP only. Add a cert issuer + `ingress.tls` if HTTPS is needed.

### K3s Kubeconfig Access from this Machine
The API server cert is only valid for `raspberrypi`/`localhost`, so `~/.kube/config.pi` (which points at `pi.home.arpa:6443`) fails TLS verification. Either:
1. SSH-tunnel and rewrite the server address (used for `helm upgrade`):
   ```bash
   ssh -f -N -L 16443:127.0.0.1:6443 pi@pi.home.arpa
   sed 's#https://127.0.0.1:6443#https://127.0.0.1:16443#' /etc/rancher/k3s/k3s.yaml > /tmp/k3s-tunnel.yaml
   KUBECONFIG=/tmp/k3s-tunnel.yaml helm upgrade face-api deploy/helm/face-api \
     --namespace face-api \
     --values deploy/helm/face-api/values.yaml \
     --values deploy/helm/face-api/values.pi.yaml
   ```
2. Or run `kubectl`/`helm` on the Pi itself via SSH with the chart copied over.

### Known Issues
- **Wrong-arch content in arm64 images (`exec format error`, or silent x86-64 binaries)**: If the Dockerfile `ARG TARGETARCH` has a plain default (`ARG TARGETARCH=amd64`), that default **shadows** buildx's per-platform `TARGETARCH`, so every `--platform linux/arm64` leg actually builds amd64 content — the Go binary AND `libonnxruntime.so` come out x86-64, and the Pi pod fails with `exec ./face-api: exec format error`. Also, `TARGETARCH` (a global/`FROM`-level ARG) is NOT visible to shell commands inside a stage unless redeclared. Fix (current Dockerfile): declare bare `ARG TARGETARCH` and use `FROM --platform=linux/${TARGETARCH:-amd64}` so plain `docker build` (testcontainers e2e, no `--platform`) still resolves while buildx overrides the value per-arch. Verify the pushed arm64 manifest with `docker buildx imagetools inspect <repo> --raw` + inspecting the arm64 sub-manifest's `/app/face-api` and `/usr/local/lib/libonnxruntime.so.1` (`file` must say "ARM aarch64"). Also ensure QEMU is registered (`docker run --rm --privileged multiarch/qemu-user-static --reset -p yes`) and `docker buildx ls` lists `linux/arm64`; if a `docker-container` builder was created before QEMU existed, **recreate it** (`docker buildx rm` + `docker buildx create`) or it may reuse poisoned arm64 layers.
- **Stale `latest` image on the Pi**: with `imagePullPolicy: IfNotPresent`, containerd keeps the previously-pulled `latest` by digest and K3s will NOT re-pull a new push. `helm upgrade` swaps the pod fine, but it runs the old image. Fix: `values.pi.yaml` uses `image.pullPolicy: Always`. If a rollout still runs the old digest, `kubectl rollout restart deploy/face-api -n face-api`.
- **K3s cannot pull from GHCR (401)**: K3s's embedded containerd fails to get an anonymous GHCR token even though the image is public and `docker pull` works. Fix: create `kubectl -n face-api create secret docker-registry ghcr-pull --from-file=.dockerconfigjson=...` from ~/.docker/config.json and set `imagePullSecrets: [{name: ghcr-pull}]` in `values.pi.yaml`.