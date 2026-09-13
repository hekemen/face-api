# Project Variables
BINARY_NAME=face-api
DOCKER_IMAGE=pi5-face-api
DOCKER_REGISTRY=ghcr.io
DOCKER_REPO=$(DOCKER_REGISTRY)/$(shell basename $(shell pwd))
SCRFD_MODEL_FILE=models/scrfd_500m.onnx
SCRFD_MODEL_URL=https://huggingface.co/deepghs/insight-face/resolve/main/scrfd_500m.onnx
MODEL_FILE=models/arcface_w600k_mbf.onnx
MODEL_URL=https://huggingface.co/deepghs/insight-face/resolve/main/buffalo_s/w600k_mbf.onnx
ONNXRUNTIME_VERSION=1.23.0
LIBONNXRT_DIR=lib
PORT=8081
TEST_IMAGE=test_face.jpg
PLATFORMS ?= linux/amd64,linux/arm64

# Resolve the ONNX Runtime release archive for the host CPU architecture
HOST_ARCH := $(shell uname -m)
ifeq ($(HOST_ARCH),aarch64)
ONNXRUNTIME_ARCH=aarch64
else
ONNXRUNTIME_ARCH=x64
endif
ONNXRUNTIME_URL=https://github.com/microsoft/onnxruntime/releases/download/v$(ONNXRUNTIME_VERSION)/onnxruntime-linux-$(ONNXRUNTIME_ARCH)-$(ONNXRUNTIME_VERSION).tgz
LIBONNXRT=$(LIBONNXRT_DIR)/libonnxruntime.so

# Default Load Test Config
REQUESTS ?= 500
CONCURRENCY ?= 10

# Guards against empty variable overrides
REQ_VAL = $(if $(strip $(REQUESTS)),$(REQUESTS),50)
CONC_VAL = $(if $(strip $(CONCURRENCY)),$(CONCURRENCY),5)

BOUNDARY = ------------------------heyboundary

.PHONY: all build run docker-build docker-build-pi5 docker-run docker-stop docker-build-all docker-push-all download-model install-libonnxruntime download-libonnxruntime install-hey generate-test-image prepare-payload load-test load-test-users load-test-recognize test clean help

all: build

help:
	@echo "Available Targets:"
	@echo "  make build               - Build Cgo binary locally"
	@echo "  make run                 - Download model, build, and run locally"
	@echo "  make docker-build        - Build multi-stage Docker image"
	@echo "  make docker-build-pi5    - Build single-arch image for Raspberry Pi 5 (linux/arm64)"
	@echo "  make docker-run          - Run Docker container with faces.db persistence"
	@echo "  make docker-stop         - Stop and remove the Docker container"
	@echo "  make docker-build-all    - Build multi-arch Docker images (amd64 + arm64)"
	@echo "  make docker-push-all     - Push multi-arch Docker images to registry"
	@echo "  make download-model      - Fetch ONNX models if missing"
	@echo "  make download-libonnxruntime - Fetch libonnxruntime.so for local runs"
	@echo "  make load-test           - Run full load test suite with 'hey'"
	@echo "  make load-test-users     - Load test GET /users endpoint"
	@echo "  make load-test-recognize - Load test POST /recognize endpoint"
	@echo "  make clean               - Remove compiled binary and temporary files"
	@echo "  make test                - Run end-to-end tests with testcontainers"

download-libonnxruntime: install-libonnxruntime

download-model:
	@mkdir -p models
	@if [ ! -f $(SCRFD_MODEL_FILE) ]; then \
		echo "Downloading $(SCRFD_MODEL_FILE)..."; \
		curl -L -o $(SCRFD_MODEL_FILE) $(SCRFD_MODEL_URL); \
	else \
		echo "$(SCRFD_MODEL_FILE) already present."; \
	fi
	@if [ ! -f $(MODEL_FILE) ]; then \
		echo "Downloading $(MODEL_FILE)..."; \
		curl -L -o $(MODEL_FILE) $(MODEL_URL); \
	else \
		echo "$(MODEL_FILE) already present."; \
	fi

install-libonnxruntime:
	@mkdir -p $(LIBONNXRT_DIR)
	@if [ ! -f $(LIBONNXRT) ]; then \
		echo "Downloading ONNX Runtime $(ONNXRUNTIME_VERSION) for $(ONNXRUNTIME_ARCH)..."; \
		curl -sL -o /tmp/onnxruntime.tgz $(ONNXRUNTIME_URL) && \
		tar -xzf /tmp/onnxruntime.tgz -C /tmp && \
		cp /tmp/onnxruntime-linux-*/lib/libonnxruntime.so $(LIBONNXRT) && \
		rm -rf /tmp/onnxruntime-linux-* /tmp/onnxruntime.tgz; \
		echo "libonnxruntime.so installed at $(LIBONNXRT)"; \
	else \
		echo "$(LIBONNXRT) already present."; \
	fi

build: download-model
	@echo "Building Go binary..."
	CGO_ENABLED=1 go build -ldflags="-s -w" -o $(BINARY_NAME) ./cmd/face-api

run: build download-libonnxruntime
	@echo "Starting local server on port $(PORT)..."
	LD_LIBRARY_PATH=$(CURDIR)/$(LIBONNXRT_DIR) ./$(BINARY_NAME)

docker-build: download-model
	@echo "Building Docker image: $(DOCKER_IMAGE)..."
	docker build -t $(DOCKER_IMAGE) .

# Raspberry Pi 5 is arm64. --load keeps the image available locally so it can be
# saved/transferred to the Pi (e.g. docker save | ssh pi docker load).
docker-build-pi5: download-model
	@echo "Building Docker image for Raspberry Pi 5 (linux/arm64): $(DOCKER_IMAGE):arm64..."
	docker buildx build --platform linux/arm64 -t $(DOCKER_IMAGE):arm64 --load .

docker-run: docker-build
	@echo "Ensuring faces.db exists..."
	@touch faces.db
	@echo "Starting Docker container..."
	docker run --rm \
		--name $(DOCKER_IMAGE) \
		-p $(PORT):$(PORT) \
		-v $(shell pwd)/faces.db:/app/faces.db \
		--env-file .env \
		$(DOCKER_IMAGE)

docker-stop:
	@echo "Stopping container..."
	-docker stop $(DOCKER_IMAGE)
	-docker rm $(DOCKER_IMAGE)

docker-build-all: download-model
	@echo "Building multi-arch Docker images for $(PLATFORMS)..."
	docker buildx build \
		--platform $(PLATFORMS) \
		-t $(DOCKER_IMAGE) \
		-t $(DOCKER_REPO):dev \
		--load \
		.

docker-push-all: download-model
	@echo "Pushing multi-arch Docker images to $(DOCKER_REPO)..."
	docker buildx build \
		--platform $(PLATFORMS) \
		-t $(DOCKER_REPO):latest \
		-t $(DOCKER_REPO):$(shell git rev-parse --short HEAD) \
		--push \
		.

install-hey:
	@which hey > /dev/null || (echo "Installing 'hey'..." && go install github.com/rakyll/hey@latest)

generate-test-image:
	@if [ ! -f $(TEST_IMAGE) ]; then \
		echo "Downloading sample face image..."; \
		curl -L -o $(TEST_IMAGE) "https://raw.githubusercontent.com/opencv/opencv/master/samples/data/lena.jpg"; \
	fi

prepare-payload: generate-test-image
	@echo "Constructing multipart payload for 'hey'..."
	@{ \
		printf '%s\r\n' "--$(BOUNDARY)"; \
		printf '%s\r\n' 'Content-Disposition: form-data; name="image"; filename="$(TEST_IMAGE)"'; \
		printf '%s\r\n\r\n' 'Content-Type: image/jpeg'; \
		cat $(TEST_IMAGE); \
		printf '\r\n%s\r\n' "--$(BOUNDARY)--"; \
	} > payload.raw

load-test: install-hey load-test-users load-test-recognize

load-test-users: install-hey
	@echo "=== Load Testing GET /users (Requests: $(REQ_VAL), Concurrency: $(CONC_VAL)) ==="
	hey -n $(REQ_VAL) -c $(CONC_VAL) http://localhost:$(PORT)/users

load-test-recognize: install-hey prepare-payload
	@echo "=== Load Testing POST /recognize (Requests: $(REQ_VAL), Concurrency: $(CONC_VAL)) ==="
	hey -n $(REQ_VAL) -c $(CONC_VAL) -m POST \
		-T "multipart/form-data; boundary=$(BOUNDARY)" \
		-D payload.raw \
		http://localhost:$(PORT)/recognize

test:
	@echo "Running end-to-end tests with testcontainers..."
	go test -v -count=1 ./test/e2e/...

clean:
	@echo "Cleaning output binaries and temporary files..."
	rm -f $(BINARY_NAME) $(TEST_IMAGE) payload.raw
	rm -rf $(LIBONNXRT_DIR)