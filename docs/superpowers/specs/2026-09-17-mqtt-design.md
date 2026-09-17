# MQTT Integration & Home Assistant Discovery

**Status**: approved
**Date**: 2026-09-17
**Approach**: A — dedicated `internal/mqtt` bridge, shared check function

---

## 1. Overview

Add MQTT support to the face-api server so that Home Assistant automations can trigger a stream check (RTSP camera scan) and receive structured results. The server publishes Home Assistant MQTT Discovery configs for three entities: a sensor (last detected name), a binary sensor (matched/not matched), and a button (manual trigger).

## 2. Architecture

### 2.1 Components

**`internal/mqtt/bridge.go`** — new package. `Bridge` type:

- `New(cfg Config, check CheckFunc) (*Bridge, error)` — creates the bridge; `CheckFunc` is the shared stream-check function (injected, so testable with a mock).
- `Run(ctx context.Context) error` — connects, publishes discovery + availability, subscribes to trigger, starts worker goroutine, blocks until ctx cancelled.
- `Stop()` — graceful: drain queue, publish availability offline, unsubscribe, disconnect.

**`internal/server/server.go`** — refactored:

- `handleStreamCheck` becomes a thin wrapper that calls `s.RunStreamCheck(rtspURL)` (extracted from the current handler body).
- `RunStreamCheck(rtspURL) (StreamCheckResponse, error)` — the shared core: reads RTSP frame, detects, recognizes, matches, stores audit, returns result.

**`cmd/face-api/main.go`** — adds signal handling (SIGINT/SIGTERM), starts MQTT bridge before HTTP server, waits for shutdown signal, stops bridge then HTTP server.

### 2.2 Data Flow

```
HA automation ──publish──> <base>/trigger ──MQTT──> Bridge
                                                              │
                                                         subscribe
                                                              │
                                                         enqueue
                                                              │
                                                         worker
                                                              │
                                                         RunStreamCheck(rtspURL)
                                                              │
                                                         RTSP frame → detect → recognize → match
                                                              │
                                                         publish──> <base>/result (retained, JSON)
                                                         publish──> <base>/matched (retained, "on"/"off")
                                                         publish──> <base>/availability (retained, "online"/"offline")
```

### 2.3 Config (env vars)

| Env | Default | Description |
|-----|---------|-------------|
| `MQTT_BROKER_URL` | (empty) | `tcp://host:port`. Empty = MQTT disabled. |
| `MQTT_USERNAME` | (empty) | Optional auth. |
| `MQTT_PASSWORD` | (empty) | Optional auth. |
| `MQTT_CLIENT_ID` | `face-api` | Client ID. |
| `MQTT_BASE_TOPIC` | `face/scan` | Base topic; derived: `trigger`, `result`, `availability`, `matched`. |
| `MQTT_DEVICE_NAME` | `Face API` | HA device name. |
| `MQTT_QUEUE_DEPTH` | `16` | Trigger queue depth. |

### 2.4 Startup / Shutdown

- If `MQTT_BROKER_URL` is empty → MQTT disabled, app runs as-is (HTTP only).
- If set but broker unreachable → log error, retry with exponential backoff (up to 30s), app continues serving HTTP.
- On SIGINT/SIGTERM: stop MQTT bridge (drain queue, publish offline, disconnect), then stop HTTP server.

### 2.5 Error Handling

- MQTT connection failure: log + retry (paho auto-reconnect handles this).
- Stream check failure: result published with `status: "not ok"`, `reason` set.
- Queue full (bounded): drop oldest pending trigger, log warning.
- Audit write failure: log error, don't block result publishing.

## 3. Home Assistant Discovery

Three entities under `homeassistant/<component>/face_api_<name>/config`:

| Entity | Component | State Topic | Command Topic | Attributes |
|--------|-----------|-------------|---------------|------------|
| `sensor.face_api_last_result` | `mqtt` | `<base>/result` | — | `similarity`, `matched`, `reason`, `duration_ms`, `last_scan` |
| `binary_sensor.face_api_matched` | `mqtt` | `<base>/matched` | — | `last_scan` |
| `button.face_api_check` | `button` | — | `<base>/trigger` | — |

Device block: `name: "Face API"`, `identifiers: ["face_api"]`, `model: "face-api"`, `sw_version: <git sha>`.

Availability: `<base>/availability` (retained). LWT (Last Will & Testament) set to `offline` on connect; `online` published on connect.

### 3.1 Discovery Configs

**Sensor** (`homeassistant/sensor/face_api_last_result/config`):

```json
{
  "name": "Last Result",
  "unique_id": "face_api_last_result",
  "state_topic": "face/scan/result",
  "value_template": "{{ value_json.name if value_json.name else 'unknown' }}",
  "json_attributes_topic": "face/scan/result",
  "device": {
    "name": "Face API",
    "identifiers": ["face_api"],
    "model": "face-api",
    "sw_version": "dev"
  },
  "availability_topic": "face/scan/availability",
  "payload_available": "online",
  "payload_not_available": "offline",
  "availability_mode": "all"
}
```

**Binary sensor** (`homeassistant/binary_sensor/face_api_matched/config`):

```json
{
  "name": "Matched",
  "unique_id": "face_api_matched",
  "state_topic": "face/scan/matched",
  "value_template": "{{ value_json.matched }}",
  "device": {
    "name": "Face API",
    "identifiers": ["face_api"],
    "model": "face-api",
    "sw_version": "dev"
  },
  "availability_topic": "face/scan/availability",
  "payload_available": "online",
  "payload_not_available": "offline",
  "availability_mode": "all"
}
```

**Button** (`homeassistant/button/face_api_check/config`):

```json
{
  "name": "Check",
  "unique_id": "face_api_check",
  "command_topic": "face/scan/trigger",
  "payload_press": "on",
  "device": {
    "name": "Face API",
    "identifiers": ["face_api"],
    "model": "face-api",
    "sw_version": "dev"
  },
  "availability_topic": "face/scan/availability",
  "payload_available": "online",
  "payload_not_available": "offline",
  "availability_mode": "all"
}
```

### 3.2 Result Payload

Published to `<base>/result` (retained, QoS 1):

```json
{
  "status": "ok",
  "name": "John",
  "similarity": 0.87,
  "matched": true,
  "reason": "",
  "duration_ms": 234,
  "timestamp": "2026-09-17T12:00:00Z"
}
```

Published to `<base>/matched` (retained, QoS 1):

```json
{
  "matched": true,
  "last_scan": "2026-09-17T12:00:00Z"
}
```

## 4. MQTT Bridge Internals

### 4.1 Connection

`paho.mqtt.golang` `NewClient` with `ClientOptions`. Auto-reconnect enabled (default). LWT set on `<base>/availability` with QoS 1, retained, payload `"offline"`. On connect success: publish `"online"`, subscribe to `<base>/trigger` QoS 1.

### 4.2 Trigger Subscription

On message received: if payload is empty, skip (avoid reacting to our own retained messages if trigger topic is retained — trigger is NOT retained). Enqueue into bounded channel (depth `MQTT_QUEUE_DEPTH`, default 16). If channel full: drop oldest (drain one item, log warning).

### 4.3 Worker Goroutine

Loop: dequeue trigger → call `checkFunc(ctx)` → publish result JSON to `<base>/result` (retained, QoS 1) → publish matched state to `<base>/matched` (retained, QoS 1). If checkFunc returns error: publish `status: "not ok"` with error reason.

### 4.4 Shutdown

`Stop()` calls `ctx.cancel()` → worker exits → publishes availability `"offline"` → disconnects. HTTP server waits for bridge to finish.

## 5. Files Changed

| File | Change |
|------|--------|
| `internal/mqtt/bridge.go` | **New** — Bridge type, Config, CheckFunc, discovery publishing, trigger subscription, worker, shutdown |
| `internal/mqtt/bridge_test.go` | **New** — unit tests with mock broker |
| `internal/mqtt/bridge_integration_test.go` | **New** — integration with testcontainer broker |
| `internal/server/server.go` | Extract `RunStreamCheck(rtspURL) (StreamCheckResponse, error)` from `handleStreamCheck`; handler becomes thin wrapper |
| `internal/server/face.go` | Add `mqttBridge *mqtt.Bridge` field to `FaceServer` (nil when disabled) |
| `cmd/face-api/main.go` | Add signal handling (SIGINT/SIGTERM), start MQTT bridge, graceful shutdown |
| `go.mod` / `go.sum` | Add `github.com/eclipse/paho.mqtt.golang` |
| `Dockerfile` | No change (paho is pure Go, no CGO) |
| `deploy/helm/face-api/values.yaml` | Add `MQTT_BROKER_URL` etc. to `extraEnv` defaults |
| `deploy/helm/face-api/values.pi.yaml` | Add MQTT env vars for Pi deployment |
| `AGENTS.md` | Document MQTT integration, config, discovery |

## 6. Dependencies

- `github.com/eclipse/paho.mqtt.golang` — lightweight, pure Go, MQTT 3.1.1, built-in auto-reconnect, no CGO.
- `github.com/mochimq/mochi-mqtt` (dev/test only) — in-memory Go MQTT broker for unit tests. Or use `eclipse-mosquitto` testcontainer.

## 7. Testing

### Unit tests (`internal/mqtt/bridge_test.go`)

- `TestBridgePublishesDiscovery` — create bridge with mock checkFunc, start it, verify discovery configs are published (using a mock MQTT broker or `mochi-mqtt` testcontainer).
- `TestBridgeTriggerToResult` — publish to trigger topic, verify result published with correct JSON.
- `TestBridgeQueueDropOldest` — flood queue past capacity, verify oldest dropped.
- `TestBridgeAvailability` — verify online/offline LWT behavior.
- `TestBridgeDisabledWhenNoBroker` — empty broker URL → bridge is a no-op (or not started).

### Integration test (`internal/mqtt/bridge_integration_test.go`)

- Uses `testcontainers-go` with `eclipse-mosquitto` container.
- Full flow: connect → discover → trigger → result → availability.

### Refactored `handleStreamCheck`

Existing tests in `test/rtsp/rtsp_e2e_test.go` already cover stream-check via HTTP; after extracting `RunStreamCheck`, the HTTP handler tests still pass (same behavior). New unit test for `RunStreamCheck` directly (no HTTP).

## 8. Rejected Approaches

- **Approach B** — inline MQTT in `internal/server`: mixes transport concerns into the core server, harder to test without a broker.
- **Approach C** — no in-app MQTT, HA calls HTTP endpoint: doesn't satisfy the requirement for native MQTT integration and discovery.
