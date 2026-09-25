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

	"h2hsecure.com/face/internal/domain"
)

// CheckFunc is the stream-check function that the Bridge calls when
// a trigger message arrives. It returns the result of the check.
type CheckFunc func(rtspURL string) (*domain.StreamCheckResult, error)

// CollectFunc is the collect start/stop function that the Bridge calls when
// a collect command arrives.
type CollectFunc func(action string) error

// CollectStateFunc returns true if the collector is currently running.
type CollectStateFunc func() bool

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
	cfg        Config
	check      CheckFunc
	collect    CollectFunc
	collectSt  CollectStateFunc
	client     mqtt.Client
	log        zerolog.Logger
	queue      chan struct{}
	done       chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New creates a new Bridge. Returns nil if cfg.BrokerURL is empty (MQTT disabled).
func New(cfg Config, check CheckFunc, collect CollectFunc, collectSt CollectStateFunc) (*Bridge, error) {
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
		cfg:       cfg,
		check:     check,
		collect:   collect,
		collectSt: collectSt,
		queue:     make(chan struct{}, cfg.QueueDepth),
		done:      make(chan struct{}),
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
		// Subscribe to command topic for collect start/stop.
		cmdTopic := cfg.BaseTopic + "/cmd"
		c.Subscribe(cmdTopic, 1, b.onCommand)
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

// onCommand handles collect start/stop commands from Home Assistant.
func (b *Bridge) onCommand(_ mqtt.Client, msg mqtt.Message) {
	payload := string(msg.Payload())
	if payload == "" {
		return
	}

	if b.collect == nil {
		b.log.Warn().Msg("MQTT collect function not set")
		return
	}

	b.log.Info().Str("action", payload).Msg("MQTT collect command received")
	if err := b.collect(payload); err != nil {
		b.log.Error().Err(err).Str("action", payload).Msg("MQTT collect command failed")
		return
	}

	// Publish current state to HA switch.
	if b.collectSt != nil {
		stateTopic := b.cfg.BaseTopic + "/collect/state"
		if b.collectSt() {
			b.client.Publish(stateTopic, 1, true, "on")
		} else {
			b.client.Publish(stateTopic, 1, true, "off")
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
			result, err := b.check("")
			if err != nil {
				result = &domain.StreamCheckResult{
					Status: "not ok",
					Reason: err.Error(),
				}
			}

			// Publish result JSON (includes name, matched, similarity, face_image).
			resultJSON, _ := json.Marshal(result)
			resultTopic := b.cfg.BaseTopic + "/result"
			b.client.Publish(resultTopic, 1, true, resultJSON)

			// Publish matched state with face image thumbnail.
			matchedPayload := map[string]any{
				"matched":     result.Matched,
				"name":        result.Name,
				"similarity":  result.Similarity,
				"last_scan":   time.Now().UTC().Format(time.RFC3339),
				"face_image":  result.FaceImage,
			}
			matchedJSON, _ := json.Marshal(matchedPayload)
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

	// Unpublish any stale entities from previous versions that no longer exist.
	// This cleans up old button/sensor configs that HA still shows.
	staleIDs := []string{
		"face_api_enroll",
		"face_api_delete",
		"face_api_start",
		"face_api_stop",
		"face_api_trigger",
		"face_api_scan",
		"face_api_stream",
	}
	for _, id := range staleIDs {
		c.Publish(b.discoveryTopic("button", id), 1, true, []byte{})
		c.Publish(b.discoveryTopic("sensor", id), 1, true, []byte{})
		c.Publish(b.discoveryTopic("binary_sensor", id), 1, true, []byte{})
	}

	// Sensor: last result.
	sensorConfig := map[string]any{
		"name":                  "Last Result",
		"unique_id":             "face_api_last_result",
		"state_topic":           b.cfg.BaseTopic + "/matched",
		"value_template":        "{{ 'matched ' + value_json.name if value_json.matched else 'not matched' }}",
		"json_attributes_topic": b.cfg.BaseTopic + "/matched",
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
		"payload_on":            true,
		"payload_off":           false,
		"device":                device,
		"availability_topic":    availTopic,
		"payload_available":     "online",
		"payload_not_available": "offline",
		"availability_mode":     availMode,
	}
	c.Publish(b.discoveryTopic("binary_sensor", "face_api_matched"), 1, true, mustJSON(binaryConfig))

	// Button: check.
	buttonConfig := map[string]any{
		"name":                  "Check",
		"unique_id":             "face_api_check",
		"command_topic":         b.cfg.BaseTopic + "/trigger",
		"payload_press":         "on",
		"device":                device,
		"availability_topic":    availTopic,
		"payload_available":     "online",
		"payload_not_available": "offline",
		"availability_mode":     availMode,
	}
	c.Publish(b.discoveryTopic("button", "face_api_check"), 1, true, mustJSON(buttonConfig))

	// Unpublish old collect button config (button type) to force HA to
	// recreate it as a switch with on/off state.
	c.Publish(b.discoveryTopic("button", "face_api_collect"), 1, true, []byte{})
	time.Sleep(100 * time.Millisecond)

	// Switch: collect (toggle start/stop with state feedback).
	switchConfig := map[string]any{
		"name":                  "Collect",
		"unique_id":             "face_api_collect",
		"command_topic":         b.cfg.BaseTopic + "/cmd",
		"payload_on":            "start",
		"payload_off":           "stop",
		"state_topic":           b.cfg.BaseTopic + "/collect/state",
		"payload_available":     "online",
		"payload_not_available": "offline",
		"device":                device,
		"availability_topic":    availTopic,
		"availability_mode":     availMode,
		"optimistic":            false,
	}
	c.Publish(b.discoveryTopic("switch", "face_api_collect"), 1, true, mustJSON(switchConfig))
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
