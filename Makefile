# Project Variables
BINARY_NAME=face-api
DOCKER_IMAGE=pi5-face-api
DOCKER_REGISTRY=ghcr.io/hekemen
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

# Retry config for the (flaky) QEMU-emulated arm64 build
RETRY_COUNT ?= 5
RETRY_DELAY ?= 10

# Guards against empty variable overrides
REQ_VAL = $(if $(strip $(REQUESTS)),$(REQUESTS),50)
CONC_VAL = $(if $(strip $(CONCURRENCY)),$(CONCURRENCY),5)

BOUNDARY = ------------------------heyboundary

.PHONY: all build run docker-build docker-build-pi5 docker-run docker-stop docker-build-all docker-push-all docker-push docker-push-latest docker-push-arm64 docker-push-arm64-retry docker-push-arm64-retry-all helm-deploy-pi helm-upgrade-pi download-model install-libonnxruntime download-libonnxruntime install-hey generate-test-image prepare-payload load-test load-test-users load-test-recognize test test-e2e test-rtsp clean help

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
	@echo "  make docker-push         - Build and push multi-arch image with dev tag"
	@echo "  make docker-push-latest  - Build and push multi-arch image with latest + git sha tags"
	@echo "  make docker-push-arm64    - Build and push linux/arm64 image (ghcr.io/hekemen/face-api:latest)"
	@echo "  make docker-push-arm64-retry - Build+push arm64, retrying up to RETRY_COUNT times (default 5)"
	@echo "  make docker-push-arm64-retry-all - Retry loop with --no-cache (delete-poison cache safety)"
	@echo "  make download-model      - Fetch ONNX models if missing"
	@echo "  make download-libonnxruntime - Fetch libonnxruntime.so for local runs"
	@echo "  make load-test           - Run full load test suite with 'hey'"
	@echo "  make load-test-users     - Load test GET /users endpoint"
	@echo "  make load-test-recognize - Load test POST /recognize endpoint"
	@echo "  make clean               - Remove compiled binary and temporary files"
	@echo "  make test                - Run all end-to-end tests with testcontainers (e2e + rtsp)"
	@echo "  make test-e2e            - Run face recognition end-to-end tests"
	@echo "  make test-rtsp           - Run RTSP stream-check end-to-end tests"
	@echo "  make helm-deploy-pi      - Deploy face-api to Raspberry Pi cluster (helm install)"
	@echo "  make helm-upgrade-pi     - Upgrade face-api on Raspberry Pi cluster (helm upgrade)"

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

docker-push: download-model
	@echo "Building and pushing multi-arch Docker image: $(DOCKER_REPO):dev..."
	docker buildx build \
		--platform $(PLATFORMS) \
		-t $(DOCKER_REPO):dev \
		--push \
		.

docker-push-latest: download-model
	@echo "Building and pushing multi-arch Docker image with latest + sha tags..."
	docker buildx build \
		--platform $(PLATFORMS) \
		-t $(DOCKER_REPO):latest \
		-t $(DOCKER_REPO):$(shell git rev-parse --short HEAD) \
		--push \
		.

# Build and push the linux/arm64 image to GHCR as $(DOCKER_REPO):latest
# (values.pi.yaml pins image.tag: latest). Layers shared with previous
# attempts are cached, so a retry that reuses the go-build layer is cheap.
docker-push-arm64: download-model
	@echo "Building and pushing linux/arm64 image: $(DOCKER_REPO):latest..."
	docker buildx build \
		--platform linux/arm64 \
		-t $(DOCKER_REPO):latest \
		--push \
		.

# QEMU-emulated arm64 builds are flaky (intermittent cc1 SIGSEGV in the frozen
# host emulator), so this retries the build+push until it succeeds.
docker-push-arm64-retry: download-model
	@for i in $$(seq 1 $(RETRY_COUNT)); do \
		echo "=== arm64 build+push attempt $$i/$(RETRY_COUNT) ==="; \
		if docker buildx build --platform linux/arm64 -t $(DOCKER_REPO):latest --push .; then \
			echo "=== SUCCESS on attempt $$i ==="; \
			exit 0; \
		fi; \
		echo "attempt $$i failed, retrying in $(RETRY_DELAY)s..."; \
		sleep $(RETRY_DELAY); \
	done; \
	echo "FAILED: $(RETRY_COUNT) attempts exhausted (QEMU emulation unstable)"; \
	exit 1

# Same retry loop, but rebuild every layer from scratch each attempt
# (--no-cache). Use only when a dangling/bad cache may poison the build.
docker-push-arm64-retry-all: download-model
	@for i in $$(seq 1 $(RETRY_COUNT)); do \
		echo "=== full (--no-cache) arm64 build+push attempt $$i/$(RETRY_COUNT) ==="; \
		if docker buildx build --no-cache --platform linux/arm64 -t $(DOCKER_REPO):latest --push .; then \
			echo "=== SUCCESS on attempt $$i ==="; \
			exit 0; \
		fi; \
		echo "attempt $$i failed, retrying in $(RETRY_DELAY)s..."; \
		sleep $(RETRY_DELAY); \
	done; \
	echo "FAILED: $(RETRY_COUNT) attempts exhausted (QEMU emulation unstable)"; \
	exit 1

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
	go test -v -count=1 ./test/e2e/... ./test/rtsp/...

test-e2e:
	@echo "Running face recognition end-to-end tests..."
	go test -v -count=1 ./test/e2e/...

test-rtsp:
	@echo "Running RTSP stream-check end-to-end tests..."
	go test -v -count=1 ./test/rtsp/...

clean:
	@echo "Cleaning output binaries and temporary files..."
	rm -f $(BINARY_NAME) $(TEST_IMAGE) payload.raw
	rm -rf $(LIBONNXRT_DIR)

# --- Helm Deployment Targets ---

NAMESPACE ?= face-api
RELEASE_NAME ?= face-api

helm-deploy-pi:
	@echo "Deploying face-api to Raspberry Pi cluster (namespace: $(NAMESPACE))..."
	helm upgrade --install $(RELEASE_NAME) deploy/helm/face-api \
		--create-namespace \
		--namespace $(NAMESPACE) \
		--values deploy/helm/face-api/values.yaml \
		--values deploy/helm/face-api/values.pi.yaml

helm-upgrade-pi:
	@echo "Upgrading face-api on Raspberry Pi cluster (namespace: $(NAMESPACE))..."
	helm upgrade $(RELEASE_NAME) deploy/helm/face-api \
		--namespace $(NAMESPACE) \
		--values deploy/helm/face-api/values.yaml \
		--values deploy/helm/face-api/values.pi.yaml