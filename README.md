# face-api

Face recognition REST API built with Go, OpenCV, and ArcFace ONNX model.

## Building

### Local Build

```bash
make build          # Download model and compile binary
make run            # Build and run locally on port 8090
```

### Docker Build

```bash
make docker-build   # Build single-arch Docker image
make docker-run     # Build and run with persisted faces.db
```

### Multi-Architecture Build

```bash
make docker-build-all    # Build for linux/amd64 + linux/arm64
make docker-push-all     # Push multi-arch images to registry
```

## Docker Images

Multi-architecture Docker images are built and pushed to GitHub Container Registry (GHCR) via GitHub Actions.

```bash
# Pull amd64 image
docker pull ghcr.io/<repo>/face-api:latest

# Pull arm64 image (Raspberry Pi)
docker pull ghcr.io/<repo>/face-api:latest

# Images support both x86_64 and arm64 architectures
```

## GitHub Actions

Builds are automated via GitHub Actions (`.github/workflows/build.yml`):

- **On push to main**: Builds and pushes multi-arch Docker images (amd64 + arm64) to GHCR
- **On pull requests**: Builds images for both architectures (no push)
- **Manual trigger**: Dispatch workflow manually from Actions tab

## API Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/enroll` | Enroll a face identity (name + image) |
| POST | `/recognize` | Recognize face in image |
| POST | `/stream-check` | Check RTSP camera stream for recognized faces |
| GET | `/users` | List enrolled identities |

## Configuration

Set environment variables via `.env` file or `docker run --env-file`:

| Variable | Description |
|----------|-------------|
| `RTSP_URL` | Default RTSP camera URL for stream-check; used when the request omits `rtsp_url` |
| `RTSP_STREAM_URL` | Sub-stream URL for continuous buffering (MQTT triggers only). Empty = buffer disabled, triggers use one-shot RTSP |
| `ONNX_INTRA_OP_THREADS` | ONNX intra-op threads for both sessions (default: `4`, `0` = ONNX default) |
| `FACE_ALIGNMENT` | Crop alignment: `bbox` (default) or `arcface5` (opt-in, requires re-enrollment) |
| `TRIGGER_BURST_MS` | Burst window in milliseconds (default: `4000`) |
| `TRIGGER_SAMPLE_MS` | Minimum gap between scored frames in milliseconds (default: `200`) |
| `MQTT_QUEUE_DEPTH` | Pending trigger slots (default: `1`, coalesces repeat triggers) |

## Load Testing

```bash
make load-test              # Run full load test suite
make load-test REQUESTS=1000 CONCURRENCY=20
```

Requires [`hey`](https://github.com/rakyll/hey) load testing tool.

## Live Trigger Scanning

When `RTSP_STREAM_URL` is set, the server continuously decodes the sub-stream
into an in-memory buffer. MQTT triggers (Home Assistant button press) initiate
a burst scan for up to `TRIGGER_BURST_MS` (default 4s), sampling no faster than
`TRIGGER_SAMPLE_MS` (default 200ms), and publish the first match at or above
the threshold to MQTT immediately.

Key behaviours:

- **One audit row per burst.** Whether the burst finds a match, a not-matched
  face, or no face at all, exactly one audit entry is written (endpoint
  `stream-check`).
- **Embeddings stay byte-identical.** `FACE_ALIGNMENT=bbox` (default) reproduces
  today's enrolled embeddings exactly. Switching to `arcface5` produces a
  different embedding space and **requires deleting and re-enrolling all users**.
- **Cold buffer fallback.** If the buffer has no fresh frame when a trigger
  arrives, the server falls back to a one-shot RTSP check so MQTT scanning is
  not disabled by a cold or broken sub-stream.
