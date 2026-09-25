# Deployment & CI/CD — Design Spec

**Status**: implemented + documented
**Date**: 2026-09-25

---

## 1. Overview

Face-api is deployed as a Docker container running on Kubernetes (K3s) with multi-arch builds for both `linux/amd64` (development/testing) and `linux/arm64` (production on Raspberry Pi 5).

## 2. Build Pipeline

### 2.1 Multi-Stage Dockerfile

```
┌─────────────────────────────────────────────────────────┐
│ Builder Stage (golang:1.27-trixie)                      │
│  - Pure-Go build (no OpenCV, no CGO in inference)       │
│  - Downloads libonnxruntime.so (arch-aware via TARGETARCH)│
│  - Copies models/                                       │
│  - Compiles: go build -o face-api ./cmd/face-api        │
└──────────────────────┬──────────────────────────────────┘
                       │
                       ▼
┌─────────────────────────────────────────────────────────┐
│ Runtime Stage (debian:bookworm-slim)                    │
│  - Minimal ONNX Runtime runtime libraries               │
│  - Copies: face-api binary, libonnxruntime.so, models/   │
│  - Sets: LD_LIBRARY_PATH=/usr/local/lib                 │
└─────────────────────────────────────────────────────────┘
```

### 2.2 Build Targets

| Target | Description |
|--------|-------------|
| `make build` | Build Docker image (single arch) |
| `make docker-build` | Build single-arch image |
| `make docker-build-pi5` | Build arm64 image for Raspberry Pi 5 |
| `make docker-build-all` | Build multi-arch (amd64 + arm64) manifest push |
| `make download-libonnxruntime` | Fetch libonnxruntime.so into lib/ for local runs |
| `make download-model` | Download ONNX models |
| `make run` | Build + fetch lib + run on port 8081 |

### 2.3 Multi-Arch Build

Uses Docker Buildx with QEMU for cross-platform builds:

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t ghcr.io/hekemen/face-api:latest \
  --push \
  .
```

**Critical note**: `TARGETARCH` must be declared as a bare `ARG TARGETARCH` (no default value) in the Dockerfile. A plain `ARG TARGETARCH=amd64` shadows buildx's per-platform value, causing arm64 builds to produce amd64 binaries → `exec format error` on the Pi.

## 3. CI/CD Pipeline

### 3.1 GitHub Actions (`.github/workflows/build.yml`)

**Triggers:**
- Push to `main` or `release/**`
- PR to `main`
- Manual dispatch

**Job:**
1. Setup QEMU (`docker/setup-qemu-action`)
2. Setup Buildx
3. Login to GHCR (skipped for PRs)
4. Download ONNX models into `models/`
5. Build multi-arch Docker image
6. Push to `ghcr.io/hekemen/face-api`

**Tags:**
- Branch name
- PR number
- Short SHA
- `latest` (on main push only)

### 3.2 Test Suites

| Suite | Command | Tests | Coverage |
|-------|---------|-------|----------|
| Internal | `go test ./...` | 51 | Domain, service, repository, server |
| E2E | `make test-e2e` | 10 | Full Docker container + face API |
| RTSP | `make test-rtsp` | 5 | RTSP stream with mock server |

## 4. Kubernetes Deployment (K3s)

### 4.1 Helm Chart Structure

```
deploy/helm/face-api/
├── Chart.yaml              # Helm chart metadata
├── values.yaml             # Default values
├── values.pi.yaml          # Pi-specific overrides
├── templates/
│   ├── deployment.yaml     # Deployment (1 replica, Recreate strategy)
│   ├── service.yaml        # ClusterIP :8081
│   ├── ingress.yaml        # Traefik ingress (optional)
│   ├── middleware.yaml     # Traefik stripPrefix middleware
│   ├── pvc.yaml            # PersistentVolumeClaim for faces.db
│   ├── serviceaccount.yaml # Service account
│   └── _helpers.tpl        # Helper templates
```

### 4.2 Deployment Strategy

- **Strategy**: `Recreate` (not `RollingUpdate`) — bbolt takes an exclusive OS file lock
- **Replicas**: 1 (bbolt is not cluster-friendly)
- **Image pull policy**: `Always` (prevents stale image on Pi)
- **PVC**: 1-2Gi for `faces.db` (mounted at `/data`)

### 4.3 Pi Deployment Values (`values.pi.yaml`)

| Setting | Value |
|---------|-------|
| Image repo | `ghcr.io/hekemen/face-api` |
| Image pull policy | `Always` |
| Tag | `ui-fix` (current) |
| Arch | `arm64` |
| Memory limit | 512Mi |
| CPU limit | 1 |
| Ingress | Traefik at `pi.home.arpa/face` |
| stripPrefix | `/face` |

### 4.4 Deployment Order

```bash
# SSH tunnel for kubeconfig
ssh -f -N -L 16443:127.0.0.1:6443 pi@pi.home.arpa

# Helm upgrade
sed 's#https://127.0.0.1:6443#https://127.0.0.1:16443#' \
  /etc/rancher/k3s/k3s.yaml > /tmp/k3s-tunnel.yaml

KUBECONFIG=/tmp/k3s-tunnel.yaml helm upgrade face-api \
  deploy/helm/face-api \
  --namespace face-api \
  --values deploy/helm/face-api/values.yaml \
  --values deploy/helm/face-api/values.pi.yaml
```

## 5. Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `RTSP_URL` | (none) | Default RTSP stream URL |
| `FACES_DB_PATH` | `faces.db` | bbolt database file path |
| `ENABLE_UI` | `false` | Enable web UI |
| `MQTT_BROKER_URL` | (none) | MQTT broker URL; empty = disabled |
| `MQTT_USERNAME` | (none) | MQTT auth username |
| `MQTT_PASSWORD` | (none) | MQTT auth password |
| `MQTT_CLIENT_ID` | `face-api` | MQTT client ID |
| `MQTT_BASE_TOPIC` | `face/scan` | MQTT topic prefix |
| `MQTT_DEVICE_NAME` | `Face API` | HA device name |
| `MQTT_QUEUE_DEPTH` | `16` | Bounded channel depth |

## 6. Known Issues & Mitigations

| Issue | Cause | Fix |
|-------|-------|-----|
| Stale `latest` image on Pi | `IfNotPresent` + containerd digest caching | `imagePullPolicy: Always` |
| Wrong-arch content in arm64 | `ARG TARGETARCH=amd64` shadows buildx | Bare `ARG TARGETARCH` + `FROM --platform=linux/${TARGETARCH}` |
| GHCR pull 401 | K3s embedded containerd auth | `imagePullSecrets` with GHCR registry secret |
| ONNX GPU warning | Missing `/sys/class/drm` on Pi | `ort.LoggingLevelError` (not `Warning`) |
