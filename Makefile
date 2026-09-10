# Project Variables
BINARY_NAME=face-api
DOCKER_IMAGE=pi5-face-api
MODEL_FILE=arcface_w600k_mbf.onnx
MODEL_URL=https://huggingface.co/deepghs/insightface/resolve/main/buffalo_s/w600k_mbf.onnx
PORT=8080
TEST_IMAGE=test_face.jpg

# Default Load Test Config
REQUESTS ?= 500
CONCURRENCY ?= 10

# Guards against empty variable overrides
REQ_VAL = $(if $(strip $(REQUESTS)),$(REQUESTS),50)
CONC_VAL = $(if $(strip $(CONCURRENCY)),$(CONCURRENCY),5)

BOUNDARY = ------------------------heyboundary

.PHONY: all build run docker-build docker-run docker-stop download-model install-hey generate-test-image prepare-payload load-test load-test-users load-test-recognize clean help

all: build

help:
	@echo "Available Targets:"
	@echo "  make build               - Build Cgo binary locally"
	@echo "  make run                 - Download model, build, and run locally"
	@echo "  make docker-build        - Build multi-stage Docker image"
	@echo "  make docker-run          - Run Docker container with faces.db persistence"
	@echo "  make docker-stop         - Stop and remove the Docker container"
	@echo "  make download-model      - Fetch ArcFace ONNX model if missing"
	@echo "  make load-test           - Run full load test suite with 'hey'"
	@echo "  make load-test-users     - Load test GET /users endpoint"
	@echo "  make load-test-recognize - Load test POST /recognize endpoint"
	@echo "  make clean               - Remove compiled binary and temporary files"

download-model:
	@if [ ! -f $(MODEL_FILE) ]; then \
		echo "Downloading $(MODEL_FILE)..."; \
		curl -L -o $(MODEL_FILE) $(MODEL_URL); \
	else \
		echo "$(MODEL_FILE) already present."; \
	fi

build: download-model
	@echo "Building Go binary..."
	CGO_ENABLED=1 go build -ldflags="-s -w" -o $(BINARY_NAME) main.go

run: build
	@echo "Starting local server on port $(PORT)..."
	./$(BINARY_NAME)

docker-build:
	@echo "Building Docker image: $(DOCKER_IMAGE)..."
	docker build -t $(DOCKER_IMAGE) .

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

clean:
	@echo "Cleaning output binaries and temporary files..."
	rm -f $(BINARY_NAME) $(TEST_IMAGE) payload.raw