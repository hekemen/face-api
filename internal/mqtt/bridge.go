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
type CheckFunc func(rtspURL string) (server.StreamCheckResponse, error)

// EnrollFunc enrolls a face image for a user. The image is a base64-encoded JPEG.
type EnrollFunc func(name string, imageBase64 string) (server.EnrolledResponse, error)

// DeleteFunc removes a user by name.
type DeleteFunc func(name string) (map[string]string, error)

// ListUsersFunc returns the list of enrolled users.
type ListUsersFunc func() (server.UsersListResponse, error)

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
	enroll EnrollFunc
	delete DeleteFunc
	list   ListUsersFunc
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
		// Subscribe to trigger and command topics.
		triggerTopic := cfg.BaseTopic + "/trigger"
		c.Subscribe(triggerTopic, 1, b.onMessage)
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

// SetEnrollFunc attaches an enroll function to the bridge.
func (b *Bridge) SetEnrollFunc(f EnrollFunc) {
	b.enroll = f
}

// SetDeleteFunc attaches a delete function to the bridge.
func (b *Bridge) SetDeleteFunc(f DeleteFunc) {
	b.delete = f
}

// SetListUsersFunc attaches a list-users function to the bridge.
func (b *Bridge) SetListUsersFunc(f ListUsersFunc) {
	b.list = f
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

// onMessage handles incoming trigger messages (stream-check).
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

// onCommand handles incoming command messages (enroll, delete, list).
func (b *Bridge) onCommand(_ mqtt.Client, msg mqtt.Message) {
	payload := string(msg.Payload())
	if payload == "" {
		return
	}

	var cmd struct {
		Action string          `json:"action"`
		Name   string          `json:"name"`
		Image  string          `json:"image"`
	}
	if err := json.Unmarshal([]byte(payload), &cmd); err != nil {
		b.log.Warn().Err(err).Msg("invalid command payload")
		return
	}

	switch cmd.Action {
	case "enroll":
		if b.enroll == nil {
			b.client.Publish(b.responseTopic("enroll"), 1, true, mustJSON(map[string]any{"error": "enroll not configured"}))
			return
		}
		if cmd.Name == "" || cmd.Image == "" {
			b.client.Publish(b.responseTopic("enroll"), 1, true, mustJSON(map[string]any{"error": "missing name or image"}))
			return
		}
		resp, err := b.enroll(cmd.Name, cmd.Image)
		if err != nil {
			b.client.Publish(b.responseTopic("enroll"), 1, true, mustJSON(map[string]any{"error": err.Error()}))
			return
		}
		b.client.Publish(b.responseTopic("enroll"), 1, true, mustJSON(resp))

	case "delete":
		if b.delete == nil {
			b.client.Publish(b.responseTopic("delete"), 1, true, mustJSON(map[string]any{"error": "delete not configured"}))
			return
		}
		if cmd.Name == "" {
			b.client.Publish(b.responseTopic("delete"), 1, true, mustJSON(map[string]any{"error": "missing name"}))
			return
		}
		resp, err := b.delete(cmd.Name)
		if err != nil {
			b.client.Publish(b.responseTopic("delete"), 1, true, mustJSON(map[string]any{"error": err.Error()}))
			return
		}
		b.client.Publish(b.responseTopic("delete"), 1, true, mustJSON(resp))

	case "list":
		if b.list == nil {
			b.client.Publish(b.responseTopic("list"), 1, true, mustJSON(map[string]any{"error": "list not configured"}))
			return
		}
		resp, err := b.list()
		if err != nil {
			b.client.Publish(b.responseTopic("list"), 1, true, mustJSON(map[string]any{"error": err.Error()}))
			return
		}
		b.client.Publish(b.responseTopic("list"), 1, true, mustJSON(resp))

	default:
		b.log.Warn().Str("action", cmd.Action).Msg("unknown command")
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
				result = server.StreamCheckResponse{
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
				"matched":    result.Matched,
				"name":       result.Name,
				"similarity": result.Similarity,
				"last_scan":  time.Now().UTC().Format(time.RFC3339),
				"face_image": result.FaceImage,
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

	// Button: enroll (triggers a snapshot capture in HA, sends image via MQTT).
	enrollConfig := map[string]any{
		"name":                  "Enroll",
		"unique_id":             "face_api_enroll",
		"command_topic":         b.cfg.BaseTopic + "/cmd",
		"payload_press":         `{"action":"enroll"}`,
		"device":                device,
		"availability_topic":    availTopic,
		"payload_available":     "online",
		"payload_not_available": "offline",
		"availability_mode":     availMode,
	}
	c.Publish(b.discoveryTopic("button", "face_api_enroll"), 1, true, mustJSON(enrollConfig))

	// Button: delete user.
	deleteConfig := map[string]any{
		"name":                  "Delete User",
		"unique_id":             "face_api_delete",
		"command_topic":         b.cfg.BaseTopic + "/cmd",
		"payload_press":         `{"action":"delete"}`,
		"device":                device,
		"availability_topic":    availTopic,
		"payload_available":     "online",
		"payload_not_available": "offline",
		"availability_mode":     availMode,
	}
	c.Publish(b.discoveryTopic("button", "face_api_delete"), 1, true, mustJSON(deleteConfig))

	// Select: user list (populated dynamically via list command).
	selectConfig := map[string]any{
		"name":                   "User",
		"unique_id":              "face_api_user",
		"command_topic":          b.cfg.BaseTopic + "/cmd",
		"value_template":         "{{ value_json.name }}",
		"options_topic":          b.cfg.BaseTopic + "/res/list",
		"options_value_template": "{{ value_json.users | map(attribute='name') | list }}",
		"device":                 device,
		"availability_topic":     availTopic,
		"payload_available":      "online",
		"payload_not_available":  "offline",
		"availability_mode":      availMode,
	}
	c.Publish(b.discoveryTopic("select", "face_api_user"), 1, true, mustJSON(selectConfig))
}

func (b *Bridge) discoveryTopic(component, objectID string) string {
	return fmt.Sprintf("%s/%s/%s/config", b.cfg.DiscoveryPrefix, component, objectID)
}

func (b *Bridge) responseTopic(action string) string {
	return b.cfg.BaseTopic + "/res/" + action
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
