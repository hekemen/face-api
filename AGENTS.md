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

### Request/Response Format
- All endpoints accept `multipart/form-data` with `image` field
- `/enroll` also requires `name` field
- `/stream-check` accepts `rtsp_url` form field
- Recognition returns: `{"name": "string", "similarity": float, "matched": bool}`
- Stream check returns: `{"status": "ok|not ok", "name": "string", "similarity": float, "reason": "string"}`

## Dependencies

### Go Direct Dependencies
- `github.com/shota3506/onnxruntime-purego` - Pure-Go ONNX Runtime bindings; dynamically loads `libonnxruntime.so` (no cgo in the inference path)
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

## Testing

### E2E Tests (`test/e2e/e2e_test.go`)
- Uses `testcontainers-go` to build the image from the repo Dockerfile and spin up a container
- Waits for HTTP GET /users on port 8081, then downloads Anthony Hopkins test images from GitHub
- Tests enrollment (201), listing users, recognition (matched, name, similarity ≥ 0.45), missing-name (400), and no-face (400) cases
- Run with: `make test` or `go test -v -count=1 ./test/e2e/...`

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
- Value: JSON array of float32 embedding vector

## Code Structure

```
face-api/
├── cmd/
│   └── face-api/
│       └── main.go           # Server entry point; loads libonnxruntime, creates ORT env/sessions
├── internal/
│   └── server/
│       ├── face.go           # Blob building, SCRFD/ArcFace runs, detection/crop/embedding
│       ├── server.go         # NewFaceServer, EnsureBucket, HTTP handlers, registration
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
- Cosine similarity between query embedding and enrolled embeddings
- Threshold: 0.45 (configurable in `FaceServer.threshold`)
- Returns best match if similarity >= threshold, otherwise "Unknown"

### Concurrency
- `sync.RWMutex` protects the in-memory cache (`dbMap`): read locks for lookups, write locks for enrollment
- ONNX Runtime sessions are not thread-safe: `inferMu` serializes all `Run` calls
- `ort.GetTensorData` returns a copy, so output values can be closed immediately after copying

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