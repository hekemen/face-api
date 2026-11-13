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
| `RTSP_URL` | Default RTSP camera URL for stream-check and for MQTT burst scanning; used when the request omits `rtsp_url` |
| `ONNX_INTRA_OP_THREADS` | ONNX intra-op threads for both sessions (default: `4`, `0` = ONNX default) |
| `FACE_ALIGNMENT` | Crop alignment: `bbox` (default) or `arcface5` (recommended for the camera pipeline; requires re-enrollment after switching) |
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

MQTT triggers (Home Assistant button press) initiate a burst scan for up to
`TRIGGER_BURST_MS` (default 4s). Each sample is a fresh grab of a
full-resolution frame from the camera's main stream, taken no faster than
`TRIGGER_SAMPLE_MS` (default 200ms) apart. The first match at or above the
threshold is published to MQTT immediately.

For H.264 streams only keyframes are decoded: the camera repeats
SPS+PPS+IDR every couple of seconds and an IDR decodes standalone to a
complete frame, so decoding only keyframes yields the same picture quality
for a fraction of the CPU — and it keeps the decoder ahead of the packet rate,
so no RTP packets are dropped.

Key behaviours:

- **One audit row per burst.** Whether the burst finds a match, a not-matched
  face, or no face at all, exactly one audit entry is written (endpoint
  `stream-check`).
- **Embeddings stay byte-identical.** `FACE_ALIGNMENT=bbox` (default) reproduces
  today's enrolled embeddings exactly. Switching to `arcface5` produces a
  different embedding space and **requires deleting and re-enrolling all users**.
- **Connection failures are not scan outcomes.** If the camera cannot be
  reached, the burst returns `not ok` and writes no audit row.

### Switching alignment mode

`FACE_ALIGNMENT=arcface5` warps the five detected landmarks onto the ArcFace
template before embedding. Measured, this widens the gap between a genuine
match and a stranger by 52%: the same person scores 0.63 instead of 0.52,
and a stranger 0.07 instead of 0.16.

The two modes produce different embedding spaces, so **switching the mode
requires re-enrolling every user**. A user enrolled in one mode is skipped
when the other mode is active, rather than being matched on an incomparable
score: measured, a `bbox` enrollment scores 0.80 against the same photo in
`arcface5` space, which looks like a strong match but reflects the crop
rather than the face.

To re-enroll a user from the camera:

```bash
go run ./cmd/reenroll -url "$CAMERA_URL" -api "$API_URL" -token "$TOKEN" -name <user> -pictures 5
```

The command captures full-resolution frames from the camera, keeps the ones
that differ from each other, and replaces the user's record. The old record is
deleted only after a new capture has been accepted, so a failed capture cannot
leave the user with nothing.
