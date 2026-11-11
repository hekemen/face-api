package framesource

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"h2hsecure.com/face/internal/rtsp/rtspmock"
)

// TestGrabberReturnsFrameFromMJPEGStream checks that a Grabber returns a
// decoded frame from a real RTSP server.
func TestGrabberReturnsFrameFromMJPEGStream(t *testing.T) {
	srv, err := rtspmock.New("127.0.0.1:18562")
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

	g := NewGrabber(srv.URL(), zerolog.Nop())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	w, h, pix, err := g.Grab(ctx)
	if err != nil {
		t.Fatalf("Grab: %v", err)
	}
	if w != 320 || h != 240 {
		t.Fatalf("Grab dimensions = %dx%d, want 320x240", w, h)
	}
	if len(pix) != w*h*3 {
		t.Fatalf("Grab returned %d pixels, want %d", len(pix), w*h*3)
	}
}

// TestGrabberRejectsEmptyURL checks that an empty URL is rejected before any
// network work happens.
func TestGrabberRejectsEmptyURL(t *testing.T) {
	g := NewGrabber("", zerolog.Nop())

	if _, _, _, err := g.Grab(context.Background()); err == nil {
		t.Fatal("Grab with an empty URL returned no error")
	}
}

// TestGrabberHonoursContextCancellation checks that Grab gives up when the
// context is cancelled, so a burst window can bound the wait for a keyframe.
func TestGrabberHonoursContextCancellation(t *testing.T) {
	srv, err := rtspmock.New("127.0.0.1:18564")
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

	g := NewGrabber(srv.URL(), zerolog.Nop())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, _, err := g.Grab(ctx); err == nil {
		t.Fatal("Grab with a cancelled context returned no error")
	}
}

// TestIsKeyframeAccessUnit checks the H.264 access-unit classifier that
// decides whether a frame is worth decoding.
//
// The camera sends an IDR (NALU type 5) every ~2 s, and an IDR is
// self-contained: it decodes to a full frame without any preceding P-frames.
// Decoding only IDRs cuts the decode work by ~21x, which is what makes a
// 5 MP stream decodable on a small host.
func TestIsKeyframeAccessUnit(t *testing.T) {
	tests := []struct {
		name  string
		nalus [][]byte
		want  bool
	}{
		{
			name:  "IDR present",
			nalus: [][]byte{{0x65, 0x00}, {0x41, 0x00}},
			want:  true,
		},
		{
			name:  "IDR with SPS and PPS",
			nalus: [][]byte{{0x67, 0x00}, {0x68, 0x00}, {0x65, 0x00}},
			want:  true,
		},
		{
			name:  "P-frames only",
			nalus: [][]byte{{0x41, 0x00}, {0x41, 0x00}},
			want:  false,
		},
		{
			name:  "empty",
			nalus: nil,
			want:  false,
		},
		{
			name:  "empty NALU",
			nalus: [][]byte{{}},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isKeyframeAccessUnit(tt.nalus); got != tt.want {
				t.Fatalf("isKeyframeAccessUnit() = %v, want %v", got, tt.want)
			}
		})
	}
}
