# --- Stage 1: Build ---
FROM golang:1.27-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    libgtk-3-dev libavcodec-dev libavformat-dev libswscale-dev \
    libopencv-dev pkg-config curl && \
    rm -rf /var/lib/apt/lists/*

ARG TARGETARCH

# Fetch the ONNX Runtime shared library for the target architecture.
RUN if [ "$TARGETARCH" = "arm64" ]; then \
      ORT_URL=https://github.com/microsoft/onnxruntime/releases/download/v1.23.0/onnxruntime-linux-aarch64-1.23.0.tgz; \
    else \
      ORT_URL=https://github.com/microsoft/onnxruntime/releases/download/v1.23.0/onnxruntime-linux-x64-1.23.0.tgz; \
    fi && \
    curl -sL -o /tmp/onnxruntime.tgz "$ORT_URL" && \
    tar -xzf /tmp/onnxruntime.tgz -C /tmp && \
    cp /tmp/onnxruntime-linux-*/lib/libonnxruntime.so /usr/local/lib/ && \
    rm -rf /tmp/onnxruntime-linux-* /tmp/onnxruntime.tgz

WORKDIR /app

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build Cgo binary
COPY . .
ENV CGO_ENABLED=1
RUN go build -ldflags="-s -w" -o face-api ./cmd/face-api

# --- Stage 2: Runtime ---
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    libgtk-3-0 libavcodec59 libavformat59 libswscale6 \
    libopencv-highgui4.6 libopencv-imgproc4.6 libopencv-core4.6 \
    libopencv-videoio4.6 libopencv-imgcodecs4.6 libopencv-video4.6 \
    libopencv-objdetect4.6 libopencv-photo4.6 \
    ca-certificates && \
    rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy compiled binary, libonnxruntime, and model weights
COPY --from=builder /usr/local/lib/libonnxruntime.so /usr/local/lib/
COPY --from=builder /app/face-api .
COPY models/ /app/models/

ENV LD_LIBRARY_PATH=/usr/local/lib

EXPOSE 8081

CMD ["./face-api"]