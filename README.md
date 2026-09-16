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

## Load Testing

```bash
make load-test              # Run full load test suite
make load-test REQUESTS=1000 CONCURRENCY=20
```

Requires [`hey`](https://github.com/rakyll/hey) load testing tool.
