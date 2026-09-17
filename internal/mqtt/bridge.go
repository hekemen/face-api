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
	done   chan struct{}
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
		done:  make(chan struct{}),
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
	// Signal worker to stop.
	select {
	case <-b.done:
		// Already closed.
	default:
		close(b.done)
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

	// Try to enqueue. If queue is full, drop the oldest item and enqueue the new one.
	select {
	case b.queue <- struct{}{}:
		// Enqueued.
	default:
		// Queue full — drop oldest to make room, then enqueue.
		select {
		case <-b.queue:
			b.log.Warn().Msg("trigger queue full, dropped oldest")
			// Now try to enqueue again.
			select {
			case b.queue <- struct{}{}:
				// Enqueued after drop.
			default:
				// Still full (race condition); drop this message too.
			}
		default:
			// Queue became non-full between checks; drop this message.
		}
	}
}

// worker dequeues triggers and runs the check function.
func (b *Bridge) worker() {
	defer b.wg.Done()
	for {
		select {
		case <-b.done:
			return
		case <-b.queue:
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
	c.Publish(b.discoveryTopic("sensor", "face_api_last_result"), 1, true, mustJSON(sensorConfig))

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
	c.Publish(b.discoveryTopic("binary_sensor", "face_api_matched"), 1, true, mustJSON(binaryConfig))

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
	c.Publish(b.discoveryTopic("button", "face_api_check"), 1, true, mustJSON(buttonConfig))
}

func (b *Bridge) discoveryTopic(component, objectID string) string {
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
