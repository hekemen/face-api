# --- Stage 1: Build ---
# Pure-Go build (CGO_ENABLED=0): no OpenCV, no gcc, no pkg-config. The builder
# stage runs on BUILDPLATFORM (the host / native arch) and cross-compiles for
# TARGETARCH via GOARCH, so the arm64 image never needs QEMU at compile time —
# it is built natively in ~1.5s even on an amd64 machine. TARGETARCH (a
# global/--platform ARG injected by buildx) is NOT visible inside the stage
# unless redeclared; same for BUILDPLATFORM. Plain `docker build`
# (testcontainers e2e) leaves TARGETARCH empty, so ${TARGETARCH:-amd64} falls
# back to the host arch. Do NOT give TARGETARCH a plain =amd64 default: that
# shadows buildx's per-platform value.
FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.27-trixie AS builder
ARG TARGETARCH

WORKDIR /app

ARG BUILD_TIME
ARG GIT_VERSION

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and cross-compile the pure-Go binary for TARGETARCH.
COPY . .
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH:-amd64} go build -ldflags="-s -w -X h2hsecure.com/face/internal/server.buildTime=${BUILD_TIME}" -o face-api ./cmd/face-api

# --- Stage 2: Runtime ---
FROM --platform=linux/${TARGETARCH:-amd64} debian:trixie-slim
ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy compiled binary and model weights
COPY --from=builder /app/face-api .
COPY models/ /app/models/

# Download ONNX Runtime library directly in runtime stage (avoids cross-arch COPY issues)
RUN if [ "$TARGETARCH" = "arm64" ]; then \
      ORT_URL=https://github.com/microsoft/onnxruntime/releases/download/v1.23.0/onnxruntime-linux-aarch64-1.23.0.tgz; \
    else \
      ORT_URL=https://github.com/microsoft/onnxruntime/releases/download/v1.23.0/onnxruntime-linux-x64-1.23.0.tgz; \
    fi && \
    curl -sL -o /tmp/onnxruntime.tgz "$ORT_URL" && \
    tar -xzf /tmp/onnxruntime.tgz -C /tmp && \
    cp /tmp/onnxruntime-linux-*/lib/libonnxruntime.so.1* /usr/local/lib/ && \
    ln -sf libonnxruntime.so.1 /usr/local/lib/libonnxruntime.so && \
    rm -rf /tmp/onnxruntime-linux-* /tmp/onnxruntime.tgz && \
    ldconfig

ENV LD_LIBRARY_PATH=/usr/local/lib

EXPOSE 8081

CMD ["./face-api"]