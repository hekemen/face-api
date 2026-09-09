# --- Stage 1: Build ---
FROM golang:1.26.5-bookworm AS builder

# Install C++ compiler, pkg-config, and OpenCV headers
RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential \
    pkg-config \
    libopencv-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Cache Go modules
COPY go.mod go.sum ./
RUN go mod download

# Copy source code and build Cgo binary
COPY . .
ENV CGO_ENABLED=1
RUN go build -ldflags="-s -w" -o face-api main.go

# --- Stage 2: Runtime ---
FROM debian:bookworm-slim

# Install OpenCV runtime shared libraries
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libopencv-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# Copy compiled binary and model weights
COPY --from=builder /app/face-api .
COPY arcface_w600k_mbf.onnx .

EXPOSE 8080

CMD ["./face-api"]