package mqtt

import (
	"context"
	"sync"
	"testing"
	"time"

	"h2hsecure.com/face/internal/domain"
)

func TestBridgeDisabledWhenNoBroker(t *testing.T) {
	cfg := Config{
		BrokerURL:       "",
		BaseTopic:       "face/scan",
		ClientID:        "test",
		QueueDepth:      1,
		DeviceName:      "Test",
		DiscoveryPrefix: "homeassistant",
	}
	b, err := New(cfg, func(ctx context.Context) (*domain.StreamCheckResult, error) {
		return &domain.StreamCheckResult{Status: "ok"}, nil
	})
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
	b, err := New(cfg, func(ctx context.Context) (*domain.StreamCheckResult, error) {
		return &domain.StreamCheckResult{Status: "ok"}, nil
	})
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
	if b.cfg.QueueDepth != 1 {
		t.Fatalf("expected default QueueDepth 1, got %d", b.cfg.QueueDepth)
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
	b, err := New(cfg, func(ctx context.Context) (*domain.StreamCheckResult, error) {
		callCount++
		return &domain.StreamCheckResult{Status: "ok"}, nil
	})
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

func TestQueueCoalescesAtDepthOne(t *testing.T) {
	// Default config => QueueDepth 1.
	cfg := Config{
		BrokerURL:       "tcp://127.0.0.1:1883",
		BaseTopic:       "test/scan",
		ClientID:        "test-coalesce",
		DeviceName:      "Test",
		DiscoveryPrefix: "homeassistant",
	}
	var mu sync.Mutex
	checkCount := 0
	slow := make(chan struct{})
	b, err := New(cfg, func(ctx context.Context) (*domain.StreamCheckResult, error) {
		mu.Lock()
		checkCount++
		isFirst := checkCount == 1
		mu.Unlock()
		// Only the first check blocks; subsequent ones return immediately.
		if isFirst {
			<-slow
		}
		return &domain.StreamCheckResult{Status: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	b.ctx, b.cancel = context.WithCancel(context.Background())
	b.wg.Add(1)
	go b.worker()

	// Fire 3 triggers in quick succession while the first check is blocked.
	// Small delay between triggers so the worker dequeues trigger 1 before
	// trigger 2 arrives (otherwise all 3 coalesce to 1 in the depth-1 queue).
	b.onMessage(nil, mockMessage{payload: "on"})
	time.Sleep(5 * time.Millisecond)
	b.onMessage(nil, mockMessage{payload: "on"})
	b.onMessage(nil, mockMessage{payload: "on"})
	close(slow) // let the first check complete

	// Wait for up to 2 checks.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := checkCount
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	n := checkCount
	mu.Unlock()
	b.Stop()

	if n != 2 {
		t.Fatalf("expected 2 checks with coalescing depth 1, got %d", n)
	}
}

func TestCheckReceivesContext(t *testing.T) {
	cfg := Config{
		BrokerURL:       "tcp://127.0.0.1:1883",
		BaseTopic:       "test/scan",
		ClientID:        "test-ctx",
		QueueDepth:      1,
		DeviceName:      "Test",
		DiscoveryPrefix: "homeassistant",
	}
	var captured context.Context
	capturedCh := make(chan struct{})
	b, err := New(cfg, func(ctx context.Context) (*domain.StreamCheckResult, error) {
		captured = ctx
		close(capturedCh)
		// Block until the bridge context is cancelled.
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Set the bridge context as Run() would.
	parentCtx, parentCancel := context.WithCancel(context.Background())
	b.ctx, b.cancel = context.WithCancel(parentCtx)

	b.wg.Add(1)
	go b.worker()

	// Enqueue a trigger; the check blocks waiting for ctx cancellation.
	b.queue <- struct{}{}
	select {
	case <-capturedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("check func never received a context")
	}
	if captured == parentCtx {
		t.Fatal("check func received the parent context, not a derived child context")
	}

	// Cancel: the bridge context should be cancelled and the check should return.
	parentCancel()
	b.Stop()
}
