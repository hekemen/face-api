# MQTT Integration & Home Assistant Discovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add MQTT support to the face-api server so Home Assistant automations can trigger a stream check and receive structured results via MQTT Discovery.

**Architecture:** Dedicated `internal/mqtt` package with a `Bridge` type that owns the MQTT connection, HA discovery, trigger subscription, and result publishing. The HTTP stream-check handler is refactored to call a shared `RunStreamCheck` method, which the MQTT bridge also uses. Main.go gains signal handling for graceful shutdown.

**Tech Stack:** Go 1.27, `github.com/eclipse/paho.mqtt.golang` (MQTT 3.1.1), testcontainers-go for integration tests, Home Assistant MQTT Discovery protocol.

**Spec:** `docs/superpowers/specs/2026-09-17-mqtt-design.md`

## Global Constraints

- **Go version:** 1.27.0 (from go.mod)
- **MQTT library:** `github.com/eclipse/paho.mqtt.golang` (MQTT 3.1.1, pure Go, no CGO)
- **No CGO dependencies** (Dockerfile must remain CGO-free for ONNX Runtime)
- **Config via env vars only** (consistent with existing app pattern)
- **MQTT disabled when `MQTT_BROKER_URL` is empty** (non-fatal, app runs as HTTP-only)
- **Broker reachability verified:** `mosquitto.mqtt.svc.cluster.local:1883` is reachable from the face-api pod

---

### Task 1: Extract `RunStreamCheck` from HTTP handler

**Files:**
- Modify: `internal/server/server.go` (lines 535-641)
- Modify: `internal/server/face.go` (lines 42-60)

**Interfaces:**
- Consumes: existing `handleStreamCheck`, `readRTSPFrame`, `detectAndCrop112`, `extractEmbedding`, `storeAudit`, `bestEmbeddingScore`
- Produces: `func (s *FaceServer) RunStreamCheck(rtspURL string) (StreamCheckResponse, error)` — shared core logic

**Step 1: Write the failing test**

Create a test that calls `RunStreamCheck` directly. Since it needs an RTSP connection, use the existing mock RTSP infrastructure. The test will fail because `RunStreamCheck` doesn't exist yet.

```go
// Append to internal/server/server_test.go (or create if needed):
func TestRunStreamCheckReturnsResult(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/test.db", 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rt, err := ort.NewRuntime("", 23)
	if err != nil {
		t.Skip("ORT not available")
	}
	defer rt.Close()

	ortEnv, err := rt.NewEnv("test", ort.LoggingLevelError)
	if err != nil {
		t.Fatal(err)
	}
	defer ortEnv.Close()

	sessOpts := &ort.SessionOptions{IntraOpNumThreads: 1}
	detFile, err := os.Open("models/scrfd_500m.onnx")
	if err != nil {
		t.Skip("model not found")
	}
	detSess, err := rt.NewSessionFromReader(ortEnv, detFile, sessOpts)
	detFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer detSess.Close()

	recFile, err := os.Open("models/arcface_w600k_mbf.onnx")
	if err != nil {
		t.Skip("model not found")
	}
	recSess, err := rt.NewSessionFromReader(ortEnv, recFile, sessOpts)
	recFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer recSess.Close()

	s := &FaceServer{
		boltDB:    db,
		dbMap:     map[string]*storedUser{},
		log:       zerolog.New(os.Stdout),
		ortRT:     rt,
		ortEnv:    ortEnv,
		detSess:   detSess,
		recSess:   recSess,
		threshold: 0.45,
	}

	// RunStreamCheck doesn't exist yet — this test fails to compile.
	result, err := s.RunStreamCheck("rtsp://127.0.0.1:18554/test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Status != "not ok" {
		t.Fatalf("expected not ok (no face), got %s", result.Status)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test -count=1 -run TestRunStreamCheckReturnsResult ./internal/server/ -v`
Expected: FAIL with `s.RunStreamCheck undefined` (method doesn't exist)

**Step 3: Write minimal implementation**

Extract the core logic from `handleStreamCheck` (server.go:535-641) into a new method `RunStreamCheck`. The HTTP handler becomes a thin wrapper.

```go
// In internal/server/server.go, add after handleStreamCheck:

// RunStreamCheck performs a single stream check: reads an RTSP frame,
// detects a face, extracts an embedding, matches against enrolled users,
// and stores an audit entry. It returns the result and any error.
func (s *FaceServer) RunStreamCheck(rtspURL string) (StreamCheckResponse, error) {
	start := time.Now()

	if rtspURL == "" {
		rtspURL = s.rtspURL
	}
	if rtspURL == "" {
		return StreamCheckResponse{
			Status: "not ok",
			Reason: "Missing RTSP URL",
		}, fmt.Errorf("missing RTSP URL")
	}

	img, err := readRTSPFrame(rtspURL, 3*time.Second)
	if err != nil {
		reason := "Failed to connect to RTSP stream"
		if strings.Contains(err.Error(), "codec") {
			reason = err.Error()
		}
		return StreamCheckResponse{
			Status: "not ok",
			Reason: reason,
		}, err
	}

	cropped, _, err := s.detectAndCrop112(img)
	if err != nil {
		return StreamCheckResponse{
			Status: "not ok",
			Reason: "No face detected within 3 seconds",
		}, nil
	}

	queryVec, err := s.extractEmbedding(cropped)
	if err != nil {
		return StreamCheckResponse{
			Status: "not ok",
			Reason: "No face detected within 3 seconds",
		}, nil
	}

	s.mu.RLock()
	var bestMatch string
	var highestScore float32 = -1.0
	for name, user := range s.dbMap {
		score := bestEmbeddingScore(queryVec, user.Embeddings)
		if score > highestScore {
			highestScore = score
			bestMatch = name
		}
	}
	s.mu.RUnlock()

	dur := OperationDuration{DurationMs: time.Since(start).Milliseconds()}

	faceImage, _ := encodeFaceToBase64(cropped)
	auditEntry := AuditEntry{
		Time:       time.Now(),
		Endpoint:   "stream-check",
		Name:       bestMatch,
		Similarity: highestScore,
		Matched:    highestScore >= s.threshold,
		DurationMs: dur.DurationMs,
		FaceImage:  faceImage,
	}
	if err := s.storeAudit([]AuditEntry{auditEntry}); err != nil {
		s.log.Error().Err(err).Msg("failed to write stream-check audit entry")
	}

	if highestScore >= s.threshold {
		return StreamCheckResponse{
			OperationDuration: dur,
			Status:            "ok",
			Name:              bestMatch,
			Similarity:        highestScore,
		}, nil
	}

	return StreamCheckResponse{
		OperationDuration: dur,
		Status:            "not ok",
		Reason:            "No face detected within 3 seconds",
		Similarity:        highestScore,
	}, nil
}
```

Then refactor `handleStreamCheck` to call `RunStreamCheck`:

```go
func (s *FaceServer) handleStreamCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(StreamCheckResponse{Status: "not ok", Reason: "Method not allowed"})
		return
	}

	rtspURL := r.FormValue("rtsp_url")
	if rtspURL == "" {
		rtspURL = s.rtspURL
	}
	if rtspURL == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(StreamCheckResponse{Status: "not ok", Reason: "Missing 'rtsp_url' field"})
		return
	}

	result, err := s.RunStreamCheck(rtspURL)
	if err != nil {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(result)
		return
	}
	json.NewEncoder(w).Encode(result)
}
```

**Step 4: Run test to verify it passes**

Run: `go test -count=1 -run TestRunStreamCheckReturnsResult ./internal/server/ -v`
Expected: PASS (or SKIP if models not found — that's acceptable for this test)

Also run the full suite to ensure `handleStreamCheck` still works:
Run: `go test -count=1 ./internal/server/ -v`
Expected: PASS (all existing tests still pass)

**Step 5: Commit**

```bash
git add internal/server/server.go internal/server/face.go
git commit -m "refactor: extract RunStreamCheck from HTTP handler for MQTT reuse"
```

---

### Task 2: Create `internal/mqtt` package — Bridge type

**Files:**
- Create: `internal/mqtt/bridge.go`
- Create: `internal/mqtt/bridge_test.go`

**Interfaces:**
- Consumes: `internal/server.StreamCheckResponse` (as return type from CheckFunc)
- Produces: `type Bridge struct`, `type Config struct`, `type CheckFunc func() (StreamCheckResponse, error)`, `func New(cfg Config, check CheckFunc) (*Bridge, error)`, `func (b *Bridge) Run(ctx context.Context) error`, `func (b *Bridge) Stop()`

**Step 1: Write the failing test**

```go
// internal/mqtt/bridge_test.go
package mqtt

import (
	"context"
	"testing"
	"time"

	"h2hsecure.com/face/internal/server"
)

func TestBridgeDisabledWhenNoBroker(t *testing.T) {
	cfg := Config{
		BrokerURL:       "",
		BaseTopic:       "face/scan",
		ClientID:        "test",
		QueueDepth:      16,
		DeviceName:      "Test",
		DiscoveryPrefix: "homeassistant",
	}
	// New should return nil bridge when broker URL is empty.
	b, err := New(cfg, func() (server.StreamCheckResponse, error) {
		return server.StreamCheckResponse{Status: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b != nil {
		t.Fatal("expected nil bridge when broker URL is empty")
	}
}

func TestBridgePublishesDiscovery(t *testing.T) {
	// Use an in-memory mock broker (mochi-mqtt) or a testcontainer.
	// For now, this test verifies the bridge compiles and discovers
	// the correct config topics.
	cfg := Config{
		BrokerURL:       "tcp://127.0.0.1:1883",
		BaseTopic:       "face/scan",
		ClientID:        "test",
		QueueDepth:      16,
		DeviceName:      "Test",
		DiscoveryPrefix: "homeassistant",
	}
	b, err := New(cfg, func() (server.StreamCheckResponse, error) {
		return server.StreamCheckResponse{Status: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil bridge")
	}
	// Run in a goroutine with a timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = b.Run(ctx) }()

	// Wait for discovery configs to be published.
	time.Sleep(2 * time.Second)
	cancel()
	b.Stop()
}
```

**Step 2: Run test to verify it fails**

Run: `go test -count=1 -run TestBridge ./internal/mqtt/ -v`
Expected: FAIL — package `internal/mqtt` does not exist

**Step 3: Write minimal implementation**

Create `internal/mqtt/bridge.go`:

```go
package mqtt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/eclipse/paho.mqtt.golang"
	"github.com/rs/zerolog"

	"h2hsecure.com/face/internal/server"
)

// CheckFunc is the stream-check function that the Bridge calls when
// a trigger message arrives. It returns the result of the check.
type CheckFunc func() (server.StreamCheckResponse, error)

// Config holds MQTT configuration.
type Config struct {
	BrokerURL       string
	Username        string
	Password        string
	ClientID        string
	BaseTopic       string
	DeviceName      string
	QueueDepth      int
	DiscoveryPrefix string
}

// Bridge manages the MQTT connection, Home Assistant discovery, trigger
// subscription, and result publishing.
type Bridge struct {
	cfg    Config
	check  CheckFunc
	client mqtt.Client
	log    zerolog.Logger
	queue  chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a new Bridge. Returns nil if cfg.BrokerURL is empty (MQTT disabled).
func New(cfg Config, check CheckFunc) (*Bridge, error) {
	if cfg.BrokerURL == "" {
		return nil, nil
	}
	if cfg.ClientID == "" {
		cfg.ClientID = "face-api"
	}
	if cfg.BaseTopic == "" {
		cfg.BaseTopic = "face/scan"
	}
	if cfg.DeviceName == "" {
		cfg.DeviceName = "Face API"
	}
	if cfg.DiscoveryPrefix == "" {
		cfg.DiscoveryPrefix = "homeassistant"
	}
	if cfg.QueueDepth <= 0 {
		cfg.QueueDepth = 16
	}

	b := &Bridge{
		cfg:   cfg,
		check: check,
		queue: make(chan struct{}, cfg.QueueDepth),
	}

	// Set up MQTT client options.
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.BrokerURL)
	opts.SetClientID(cfg.ClientID)
	opts.SetAutoReconnect(true)
	opts.SetMaxReconnectInterval(30 * time.Second)
	opts.SetOrderMatters(false)

	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
		opts.SetPassword(cfg.Password)
	}

	// LWT: publish "offline" on unexpected disconnect.
	lwtTopic := cfg.BaseTopic + "/availability"
	opts.SetWill(lwtTopic, "offline", 1, true)

	opts.SetOnConnectHandler(func(c mqtt.Client) {
		// Publish "online" on connect.
		c.Publish(lwtTopic, 1, true, "online")
		// Subscribe to trigger topic.
		triggerTopic := cfg.BaseTopic + "/trigger"
		c.Subscribe(triggerTopic, 1, b.onMessage)
		// Publish HA discovery configs.
		b.publishDiscovery(c)
	})

	opts.SetConnectionLostHandler(func(c mqtt.Client, err error) {
		// LWT will be published automatically by the broker.
		b.log.Warn().Err(err).Msg("MQTT connection lost")
	})

	b.client = mqtt.NewClient(opts)

	return b, nil
}

// Run starts the MQTT connection and blocks until ctx is cancelled.
func (b *Bridge) Run(ctx context.Context) error {
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.log = zerolog.New(os.Stdout).With().Str("module", "mqtt").Logger()

	token := b.client.Connect()
	token.Wait()
	if token.Error() != nil {
		b.log.Error().Err(token.Error()).Msg("MQTT connect failed")
		// Retry with backoff.
		for i := 0; i < 10; i++ {
			time.Sleep(time.Duration(i+1) * time.Second)
			token = b.client.Connect()
			token.Wait()
			if token.Error() == nil {
				break
			}
			b.log.Error().Err(token.Error()).Msg("MQTT reconnect attempt failed")
		}
		if token.Error() != nil {
			return fmt.Errorf("MQTT connect failed after retries: %w", token.Error())
		}
	}

	// Start worker goroutine.
	b.wg.Add(1)
	go b.worker()

	// Block until context cancelled.
	<-b.ctx.Done()
	return nil
}

// Stop gracefully shuts down the Bridge.
func (b *Bridge) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()

	// Publish availability offline.
	lwtTopic := b.cfg.BaseTopic + "/availability"
	b.client.Publish(lwtTopic, 1, true, "offline")
	b.client.Disconnect(250)
}

// onMessage handles incoming trigger messages.
func (b *Bridge) onMessage(_ mqtt.Client, msg mqtt.Message) {
	payload := string(msg.Payload())
	if payload == "" {
		return // skip empty messages
	}

	select {
	case b.queue <- struct{}{}:
		// Enqueued.
	default:
		// Queue full — drop oldest.
		select {
		case <-b.queue:
			b.log.Warn().Msg("trigger queue full, dropped oldest")
		default:
			// Race: another goroutine also full; drop this one.
		}
	}
}

// worker dequeues triggers and runs the check function.
func (b *Bridge) worker() {
	defer b.wg.Done()
	for range b.queue {
		result, err := b.check()
		if err != nil {
			result = server.StreamCheckResponse{
				Status: "not ok",
				Reason: err.Error(),
			}
		}

		// Publish result JSON.
		resultJSON, _ := json.Marshal(result)
		resultTopic := b.cfg.BaseTopic + "/result"
		b.client.Publish(resultTopic, 1, true, resultJSON)

		// Publish matched state.
		matched := result.Matched
		matchedJSON, _ := json.Marshal(map[string]any{
			"matched":   matched,
			"last_scan": time.Now().UTC().Format(time.RFC3339),
		})
		matchedTopic := b.cfg.BaseTopic + "/matched"
		b.client.Publish(matchedTopic, 1, true, matchedJSON)
	}
}

// publishDiscovery publishes Home Assistant MQTT Discovery configs.
func (b *Bridge) publishDiscovery(c mqtt.Client) {
	device := map[string]any{
		"name":        b.cfg.DeviceName,
		"identifiers": []string{"face_api"},
		"model":       "face-api",
		"sw_version":  b.swVersion(),
	}
	availTopic := b.cfg.BaseTopic + "/availability"
	availMode := "all"

	// Sensor: last result.
	sensorConfig := map[string]any{
		"name":                  "Last Result",
		"unique_id":             "face_api_last_result",
		"state_topic":           b.cfg.BaseTopic + "/result",
		"value_template":        "{{ value_json.name if value_json.name else 'unknown' }}",
		"json_attributes_topic": b.cfg.BaseTopic + "/result",
		"device":                device,
		"availability_topic":    availTopic,
		"payload_available":     "online",
		"payload_not_available": "offline",
		"availability_mode":     availMode,
	}
	c.Publish(discoveryTopic("sensor", "face_api_last_result"), 1, true, mustJSON(sensorConfig))

	// Binary sensor: matched.
	binaryConfig := map[string]any{
		"name":                  "Matched",
		"unique_id":             "face_api_matched",
		"state_topic":           b.cfg.BaseTopic + "/matched",
		"value_template":        "{{ value_json.matched }}",
		"device":                device,
		"availability_topic":    availTopic,
		"payload_available":     "online",
		"payload_not_available": "offline",
		"availability_mode":     availMode,
	}
	c.Publish(discoveryTopic("binary_sensor", "face_api_matched"), 1, true, mustJSON(binaryConfig))

	// Button: check.
	buttonConfig := map[string]any{
		"name":              "Check",
		"unique_id":         "face_api_check",
		"command_topic":     b.cfg.BaseTopic + "/trigger",
		"payload_press":     "on",
		"device":            device,
		"availability_topic": availTopic,
		"payload_available": "online",
		"payload_not_available": "offline",
		"availability_mode": availMode,
	}
	c.Publish(discoveryTopic("button", "face_api_check"), 1, true, mustJSON(buttonConfig))
}

func discoveryTopic(component, objectID string) string {
	return fmt.Sprintf("%s/%s/%s/config", b.cfg.DiscoveryPrefix, component, objectID)
}

func (b *Bridge) swVersion() string {
	// Build-time version injected via ldflags.
	if swVersion != "" {
		return swVersion
	}
	return "dev"
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// swVersion is set at build time via -ldflags.
var swVersion = ""
```

**Step 4: Run test to verify it passes**

Run: `go test -count=1 -run TestBridgeDisabledWhenNoBroker ./internal/mqtt/ -v`
Expected: PASS

Run: `go test -count=1 -run TestBridgePublishesDiscovery ./internal/mqtt/ -v`
Expected: PASS (bridge starts, publishes discovery, then stops)

**Step 5: Commit**

```bash
git add internal/mqtt/bridge.go internal/mqtt/bridge_test.go go.mod go.sum
git commit -m "feat: add MQTT bridge with HA discovery and trigger subscription"
```

---

### Task 3: Integration tests for MQTT bridge

**Files:**
- Create: `internal/mqtt/bridge_integration_test.go`

**Interfaces:**
- Consumes: `internal/mqtt.Bridge`, `internal/mqtt.Config`, `internal/mqtt.New`
- Produces: integration tests that verify full flow with a real MQTT broker

**Step 1: Write the integration test**

```go
// internal/mqtt/bridge_integration_test.go
package mqtt

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mosquitto"
	"github.com/testcontainers/testcontainers-go/wait"

	"h2hsecure.com/face/internal/server"
)

func TestBridgeTriggerToResult(t *testing.T) {
	ctx := context.Background()

	// Start a Mosquitto testcontainer.
	broker, err := mosquitto.Run(ctx, "eclipse-mosquitto:2.0",
		testcontainers.WithWaitStrategy(wait.ForLog("Configuring logging")),
	)
	if err != nil {
		t.Fatalf("failed to start mosquitto: %v", err)
	}
	defer broker.Terminate(ctx)

	host, err := broker.Host(ctx)
	if err != nil {
		t.Fatalf("failed to get broker host: %v", err)
	}
	port, err := broker.MappedPort(ctx, "1883/tcp")
	if err != nil {
		t.Fatalf("failed to get broker port: %v", err)
	}
	brokerURL := "tcp://" + host + ":" + port.Port()

	// Create a bridge with a mock check function.
	var checkCount int
	var mu sync.Mutex
	checkFunc := func() (server.StreamCheckResponse, error) {
		mu.Lock()
		checkCount++
		mu.Unlock()
		return server.StreamCheckResponse{
			Status:     "ok",
			Name:       "TestUser",
			Similarity: 0.87,
			Matched:    true,
		}, nil
	}

	cfg := Config{
		BrokerURL:       brokerURL,
		BaseTopic:       "test/scan",
		ClientID:        "test-bridge",
		QueueDepth:      16,
		DeviceName:      "Test",
		DiscoveryPrefix: "homeassistant",
	}

	b, err := New(cfg, checkFunc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil bridge")
	}

	// Run the bridge in a goroutine.
	runCtx, runCancel := context.WithTimeout(ctx, 10*time.Second)
	defer runCancel()
	go func() { _ = b.Run(runCtx) }()

	// Wait for connection and discovery.
	time.Sleep(2 * time.Second)

	// Subscribe to result topic to verify publishing.
	resultCh := make(chan string, 1)
	opts := mqtt.NewClientOptions().AddBroker(brokerURL).SetClientID("test-sub")
	opts.SetDefaultPublishHandler(func(c mqtt.Client, msg mqtt.Message) {
		resultCh <- string(msg.Payload())
	})
	subClient := mqtt.NewClient(opts)
	subToken := subClient.Connect()
	subToken.Wait()
	defer subClient.Disconnect(250)

	subToken = subClient.Subscribe("test/scan/result", 1, func(_ mqtt.Client, msg mqtt.Message) {
		resultCh <- string(msg.Payload())
	})
	subToken.Wait()

	// Publish a trigger.
	pubClient := mqtt.NewClient(opts)
	pubToken := pubClient.Connect()
	pubToken.Wait()
	defer pubClient.Disconnect(250)
	pubToken = pubClient.Publish("test/scan/trigger", 1, false, []byte("on"))
	pubToken.Wait()

	// Wait for result.
	select {
	case resultJSON := <-resultCh:
		var result server.StreamCheckResponse
		if err := json.Unmarshal([]byte(resultJSON), &result); err != nil {
			t.Fatalf("failed to unmarshal result: %v", err)
		}
		if result.Status != "ok" {
			t.Fatalf("expected status ok, got %s", result.Status)
		}
		if result.Name != "TestUser" {
			t.Fatalf("expected name TestUser, got %s", result.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for result")
	}

	// Verify check was called.
	mu.Lock()
	if checkCount != 1 {
		t.Fatalf("expected 1 check call, got %d", checkCount)
	}
	mu.Unlock()

	runCancel()
	b.Stop()
}

func TestBridgeAvailability(t *testing.T) {
	ctx := context.Background()

	broker, err := mosquitto.Run(ctx, "eclipse-mosquitto:2.0",
		testcontainers.WithWaitStrategy(wait.ForLog("Configuring logging")),
	)
	if err != nil {
		t.Fatalf("failed to start mosquitto: %v", err)
	}
	defer broker.Terminate(ctx)

	host, err := broker.Host(ctx)
	if err != nil {
		t.Fatalf("failed to get broker host: %v", err)
	}
	port, err := broker.MappedPort(ctx, "1883/tcp")
	if err != nil {
		t.Fatalf("failed to get broker port: %v", err)
	}
	brokerURL := "tcp://" + host + ":" + port.Port()

	cfg := Config{
		BrokerURL:   brokerURL,
		BaseTopic:   "test/avail",
		ClientID:    "test-avail",
		DeviceName:  "Test",
	}

	b, err := New(cfg, func() (server.StreamCheckResponse, error) {
		return server.StreamCheckResponse{Status: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Subscribe to availability topic.
	availCh := make(chan string, 2)
	opts := mqtt.NewClientOptions().AddBroker(brokerURL).SetClientID("avail-sub")
	opts.SetDefaultPublishHandler(func(c mqtt.Client, msg mqtt.Message) {
		if string(msg.Topic()) == "test/avail/availability" {
			availCh <- string(msg.Payload())
		}
	})
	subClient := mqtt.NewClient(opts)
	subToken := subClient.Connect()
	subToken.Wait()
	defer subClient.Disconnect(250)
	subToken = subClient.Subscribe("test/avail/availability", 1, nil)
	subToken.Wait()

	// Run bridge — should publish "online".
	runCtx, runCancel := context.WithTimeout(ctx, 5*time.Second)
	defer runCancel()
	go func() { _ = b.Run(runCtx) }()

	// Wait for "online".
	select {
	case payload := <-availCh:
		if payload != "online" {
			t.Fatalf("expected 'online', got %q", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for 'online' availability")
	}

	// Stop bridge — should publish "offline".
	b.Stop()

	select {
	case payload := <-availCh:
		if payload != "offline" {
			t.Fatalf("expected 'offline', got %q", payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for 'offline' availability")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test -count=1 -run TestBridgeTriggerToResult ./internal/mqtt/ -v`
Expected: FAIL — `mosquitto` package not found (testcontainers-go modules not imported yet)

**Step 3: Add testcontainer dependency and run**

The test imports `github.com/testcontainers/testcontainers-go/modules/mosquitto`. Add it to go.mod:
Run: `go get github.com/testcontainers/testcontainers-go/modules/mosquitto`

Then run the test:
Run: `go test -count=1 -run TestBridgeTriggerToResult ./internal/mqtt/ -v`
Expected: PASS (testcontainer starts Mosquitto, bridge connects, trigger → result verified)

Run: `go test -count=1 -run TestBridgeAvailability ./internal/mqtt/ -v`
Expected: PASS

**Step 4: Commit**

```bash
git add internal/mqtt/bridge_integration_test.go go.mod go.sum
git commit -m "test: add MQTT bridge integration tests with Mosquitto testcontainer"
```

---

### Task 4: Wire MQTT bridge into FaceServer and main.go

**Files:**
- Modify: `internal/server/face.go` (lines 42-60)
- Modify: `internal/server/server.go`
- Modify: `cmd/face-api/main.go`

**Interfaces:**
- Consumes: `internal/mqtt.Bridge`, `internal/mqtt.Config`, `internal/mqtt.New`
- Produces: `FaceServer.mqttBridge` field, signal handling in main, graceful shutdown

**Step 1: Add mqttBridge field to FaceServer**

In `internal/server/face.go`, add to the `FaceServer` struct:

```go
type FaceServer struct {
	// ... existing fields ...
	mqttBridge *mqtt.Bridge // nil when MQTT is disabled
}
```

Add import: `"h2hsecure.com/face/internal/mqtt"`

**Step 2: Add MQTT bridge setup to NewFaceServer**

In `internal/server/server.go`, add a setter:

```go
// In server.go, add after NewFaceServer:

// SetMQTTBridge attaches an MQTT bridge to the server.
func (s *FaceServer) SetMQTTBridge(b *mqtt.Bridge) {
	s.mqttBridge = b
}
```

**Step 3: Wire into main.go**

In `cmd/face-api/main.go`, after creating the FaceServer:

```go
// After srv creation, add MQTT bridge setup:
var mqttBridge *mqtt.Bridge
if brokerURL := os.Getenv("MQTT_BROKER_URL"); brokerURL != "" {
	mqttCfg := mqtt.Config{
		BrokerURL:       brokerURL,
		Username:        os.Getenv("MQTT_USERNAME"),
		Password:        os.Getenv("MQTT_PASSWORD"),
		ClientID:        os.Getenv("MQTT_CLIENT_ID"),
		BaseTopic:       os.Getenv("MQTT_BASE_TOPIC"),
		DeviceName:      os.Getenv("MQTT_DEVICE_NAME"),
		QueueDepth:      func() int { d, _ := strconv.Atoi(os.Getenv("MQTT_QUEUE_DEPTH")); if d <= 0 { d = 16 }; return d }(),
	}
	var err error
	mqttBridge, err = mqtt.New(mqttCfg, srv.RunStreamCheck)
	if err != nil {
		logger.Error().Err(err).Msg("failed to create MQTT bridge")
	}
}

// Add signal handling:
httpServer := &http.Server{Addr: ":8081", Handler: srv.RequestLogging(mux)}

// Start MQTT bridge if configured.
if mqttBridge != nil {
	go func() {
		if err := mqttBridge.Run(context.Background()); err != nil {
			logger.Error().Err(err).Msg("MQTT bridge error")
		}
	}()
}

// Wait for shutdown signal.
sigCh := make(chan os.Signal, 1)
signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
sig := <-sigCh
logger.Info().Str("signal", sig.String()).Msg("shutting down")

// Stop MQTT bridge first (drain, publish offline, disconnect).
if mqttBridge != nil {
	mqttBridge.Stop()
}

// Stop HTTP server with timeout.
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()
httpServer.Shutdown(ctx)
```

Add imports:
```go
import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"h2hsecure.com/face/internal/mqtt"
)
```

**Step 4: Run test to verify it compiles**

Run: `go build ./cmd/face-api/`
Expected: PASS (binary builds)

Run: `go test -count=1 ./internal/server/ ./internal/mqtt/ -v`
Expected: PASS (all tests pass)

**Step 5: Update Helm values**

In `deploy/helm/face-api/values.yaml`, add:

```yaml
env:
  # ... existing env vars ...
  MQTT_BROKER_URL: ""          # Set to "tcp://mosquitto.mqtt.svc.cluster.local:1883" to enable
  MQTT_USERNAME: ""
  MQTT_PASSWORD: ""
  MQTT_CLIENT_ID: "face-api"
  MQTT_BASE_TOPIC: "face/scan"
  MQTT_DEVICE_NAME: "Face API"
  MQTT_QUEUE_DEPTH: "16"
```

In `deploy/helm/face-api/values.pi.yaml`, add:

```yaml
env:
  MQTT_BROKER_URL: "tcp://mosquitto.mqtt.svc.cluster.local:1883"
  MQTT_CLIENT_ID: "face-api"
  MQTT_BASE_TOPIC: "face/scan"
  MQTT_DEVICE_NAME: "Face API"
```

**Step 6: Update AGENTS.md**

Add MQTT section to AGENTS.md documenting:
- MQTT integration overview
- Config env vars
- HA Discovery entities
- Topic layout
- Startup/shutdown behavior

**Step 7: Commit**

```bash
git add internal/server/face.go internal/server/server.go cmd/face-api/main.go \
       deploy/helm/face-api/values.yaml deploy/helm/face-api/values.pi.yaml \
       AGENTS.md
git commit -m "feat: wire MQTT bridge into FaceServer with signal handling and Helm values"
```

---

## Verification Checklist

- [ ] `go build ./cmd/face-api/` — binary builds
- [ ] `go test -count=1 ./internal/server/ -v` — all existing tests pass
- [ ] `go test -count=1 ./internal/mqtt/ -v` — all MQTT tests pass
- [ ] `go vet ./...` — no issues
- [ ] `gofmt -l .` — no files to format
- [ ] `make test` — full test suite passes
- [ ] `make docker-build` — Docker image builds
- [ ] `make test-e2e` — E2E tests pass (with MQTT disabled)
- [ ] Manual: deploy to Pi, verify HA discovery entities appear
- [ ] Manual: trigger HA button, verify stream check runs and result published

