package framesource

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"h2hsecure.com/face/internal/rtsp/rtspmock"
)

// threadSafeBuf is a bytes.Buffer safe for concurrent Write and String calls,
// so a zerolog logger can write to it from one goroutine while the test reads
// it from another.
type threadSafeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *threadSafeBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *threadSafeBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// grayImage returns a solid-color image.Image of the given size.
func grayImage(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(c), image.Point{}, draw.Src)
	return img
}

// newSource is the test seam: it builds a Source whose stream is the given
// fake instead of the real RTSP client.
func newSource(url string, log zerolog.Logger, stream streamFunc) *Source {
	s := New(url, log)
	s.stream = stream
	return s
}

func TestSourcePublishesMockFrames(t *testing.T) {
	srv, err := rtspmock.New("127.0.0.1:18556")
	if err != nil {
		t.Skipf("rtspmock port busy: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("rtspmock Start: %v", err)
	}
	defer srv.Stop()
	frame, err := rtspmock.SolidJPEG(320, 240)
	if err != nil {
		t.Fatalf("SolidJPEG: %v", err)
	}
	srv.SetFrame(frame)

	s := New(srv.URL(), zerolog.Nop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// Wait for at least 3 frames to be published.
	dst := make([]byte, 320*240*3)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, seq, _, ok := s.Latest(dst); ok && seq >= 3 {
			if w, h := s.Dims(); w == 320 && h == 240 {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for frames from RTSP mock")
}

func TestSourceHandlesResolutionChange(t *testing.T) {
	switchCh := make(chan struct{})
	img640 := grayImage(640, 480, color.RGBA{R: 100, G: 150, B: 200, A: 255})
	img320 := grayImage(320, 240, color.RGBA{R: 200, G: 100, B: 100, A: 255})

	stream := func(ctx context.Context, url string, onFrame func(image.Image)) error {
		switched := false
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-switchCh:
				switched = true
			default:
			}
			frame := img640
			if switched {
				frame = img320
			}
			onFrame(frame)
			time.Sleep(5 * time.Millisecond)
		}
	}

	s := newSource("rtsp://fake/stream", zerolog.Nop(), stream)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// First resolution: 640x480.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w, h := s.Dims(); w == 640 && h == 480 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if w, h := s.Dims(); w != 640 || h != 480 {
		t.Fatalf("expected initial dims 640x480, got %dx%d", w, h)
	}

	// Switch resolution.
	close(switchCh)

	// Verify the published content matches the new size.
	deadline = time.Now().Add(5 * time.Second)
	dst := make([]byte, 320*240*3)
	for time.Now().Before(deadline) {
		if w, h := s.Dims(); w == 320 && h == 240 {
			if _, _, _, _, ok := s.Latest(dst); ok {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for resolution change to 320x240")
}

func TestSourceReconnects(t *testing.T) {
	var logBuf threadSafeBuf
	log := zerolog.New(&logBuf).With().Timestamp().Logger()

	attempts := 0
	stream := func(ctx context.Context, url string, onFrame func(image.Image)) error {
		attempts++
		if attempts <= 2 {
			return errors.New("connection refused")
		}
		// Block until cancelled.
		<-ctx.Done()
		return ctx.Err()
	}

	s := newSource("rtsp://fake/stream", log, stream)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait for two warn log lines (two refused connections).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(logBuf.String(), "connection refused") >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := strings.Count(logBuf.String(), "connection refused"); got < 2 {
		t.Fatalf("expected >= 2 'connection refused' log lines, got %d", got)
	}

	// Cancel: Run must return promptly.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancel")
	}
}

func TestSourceBacksOffOnUnsupportedCodec(t *testing.T) {
	var logBuf threadSafeBuf
	log := zerolog.New(&logBuf).With().Timestamp().Logger()

	var mu sync.Mutex
	var attempts []time.Time
	stream := func(ctx context.Context, url string, onFrame func(image.Image)) error {
		mu.Lock()
		attempts = append(attempts, time.Now())
		n := len(attempts)
		mu.Unlock()
		if n > 3 {
			// Stop after a few attempts so the test can cancel.
			<-ctx.Done()
			return ctx.Err()
		}
		return errors.New("RTSP stream is H.265 (unsupported codec)")
	}

	s := newSource("rtsp://fake/stream", log, stream)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Wait for the first attempt, then check that the second attempt is not
	// immediate (backoff >= 1s) and that only one unsupported-codec line was
	// logged.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(attempts)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	if len(attempts) < 2 {
		mu.Unlock()
		t.Fatalf("expected >= 2 stream attempts, got %d", len(attempts))
	}
	interval := attempts[1].Sub(attempts[0])
	mu.Unlock()
	if interval < minBackoff {
		t.Fatalf("backoff between attempts = %v, want >= %v", interval, minBackoff)
	}

	// Exactly one unsupported-codec log line (suppressed after the first).
	unsupportedCount := strings.Count(logBuf.String(), "unsupported codec")
	if unsupportedCount != 1 {
		t.Fatalf("expected exactly 1 unsupported-codec log line, got %d", unsupportedCount)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancel")
	}
}

func TestSourceRunReturnsOnCancel(t *testing.T) {
	stream := func(ctx context.Context, url string, onFrame func(image.Image)) error {
		<-ctx.Done()
		return ctx.Err()
	}

	s := newSource("rtsp://fake/stream", zerolog.Nop(), stream)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancel")
	}
}
