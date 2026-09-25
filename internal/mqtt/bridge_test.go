package mqtt

import (
	"testing"
	"time"

	"h2hsecure.com/face/internal/domain"
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
	b, err := New(cfg, func(rtspURL string) (*domain.StreamCheckResult, error) {
		return &domain.StreamCheckResult{Status: "ok"}, nil
	}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b != nil {
		t.Fatal("expected nil bridge when broker URL is empty")
	}
}

func TestBridgeDefaults(t *testing.T) {
	cfg := Config{
		BrokerURL: "tcp://127.0.0.1:1883",
	}
	b, err := New(cfg, func(rtspURL string) (*domain.StreamCheckResult, error) {
		return &domain.StreamCheckResult{Status: "ok"}, nil
	}, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b == nil {
		t.Fatal("expected non-nil bridge")
	}
	if b.cfg.ClientID != "face-api" {
		t.Fatalf("expected default ClientID 'face-api', got %q", b.cfg.ClientID)
	}
	if b.cfg.BaseTopic != "face/scan" {
		t.Fatalf("expected default BaseTopic 'face/scan', got %q", b.cfg.BaseTopic)
	}
	if b.cfg.DeviceName != "Face API" {
		t.Fatalf("expected default DeviceName 'Face API', got %q", b.cfg.DeviceName)
	}
	if b.cfg.DiscoveryPrefix != "homeassistant" {
		t.Fatalf("expected default DiscoveryPrefix 'homeassistant', got %q", b.cfg.DiscoveryPrefix)
	}
	if b.cfg.QueueDepth != 16 {
		t.Fatalf("expected default QueueDepth 16, got %d", b.cfg.QueueDepth)
	}
}

func TestBridgeQueueDrop(t *testing.T) {
	cfg := Config{
		BrokerURL:       "tcp://127.0.0.1:1883",
		BaseTopic:       "test/scan",
		ClientID:        "test-queue",
		QueueDepth:      2,
		DeviceName:      "Test",
		DiscoveryPrefix: "homeassistant",
	}
	callCount := 0
	b, err := New(cfg, func(rtspURL string) (*domain.StreamCheckResult, error) {
		callCount++
		return &domain.StreamCheckResult{Status: "ok"}, nil
	}, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Fill the queue to capacity.
	for i := 0; i < 2; i++ {
		b.onMessage(nil, mockMessage{payload: "on"})
	}
	// This should be dropped (queue full).
	b.onMessage(nil, mockMessage{payload: "on"})

	// Verify queue is full.
	if len(b.queue) != 2 {
		t.Fatalf("expected queue length 2, got %d", len(b.queue))
	}

	// Drain the queue via worker.
	b.wg.Add(1)
	go b.worker()

	// Wait for both items to be processed.
	time.Sleep(1 * time.Second)

	// Stop the worker.
	b.Stop()

	if callCount != 2 {
		t.Fatalf("expected 2 calls (1 dropped), got %d", callCount)
	}
}

// mockMessage implements mqtt.Message for testing onMessage without a real client.
type mockMessage struct {
	payload string
}

func (m mockMessage) Topic() string                 { return "" }
func (m mockMessage) Duplicate() bool               { return false }
func (m mockMessage) Qos() byte                     { return 0 }
func (m mockMessage) Retained() bool                { return false }
func (m mockMessage) MessageID() uint16             { return 0 }
func (m mockMessage) Ack()                          {}
func (m mockMessage) Payload() []byte               { return []byte(m.payload) }
func (m mockMessage) Client() interface{}           { return nil }
func (m mockMessage) Acknowledged() <-chan struct{} { return nil }
