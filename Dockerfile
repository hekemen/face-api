# --- Stage 1: Build ---
# Pure-Go build (CGO_ENABLED=0): no OpenCV, no gcc, no pkg-config. The binary
# is fully portable and cross-compiles natively (e.g. GOARCH=arm64) without an
# emulator. BuildKit injects TARGETARCH automatically when building with buildx
# (--platform), e.g. arm64 for the Raspberry Pi. Plain `docker build`
# (testcontainers e2e) has no --platform and leaves TARGETARCH empty, so the
# ${...:-amd64} default resolves it to the host arch. Do NOT give TARGETARCH a
# plain =amd64 default: that shadows buildx's per-platform value and makes
# cross-arch builds silently produce amd64 content.
ARG TARGETARCH
FROM --platform=linux/${TARGETARCH:-amd64} golang:1.27-trixie AS builder

WORKDIR /app

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build the pure-Go binary
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o face-api ./cmd/face-api

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