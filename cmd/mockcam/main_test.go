package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"h2hsecure.com/face/internal/framesource"
	"h2hsecure.com/face/internal/rtsp/rtspmock"
)

// startMockCam starts a mock camera on a free port and returns its stream
// URL.
func startMockCam(t *testing.T, photo string) string {
	t.Helper()
	raw, err := os.ReadFile(photo)
	if err != nil {
		t.Skipf("read photo: %v", err)
	}
	// Pick an even port: the RTP port is derived as port+1000 and must be
	// even.
	for port := 18700; port < 18800; port += 2 {
		srv, err := rtspmock.New(fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue // the port is in use
		}
		if err := srv.Start(); err != nil {
			continue
		}
		srv.SetFrame(raw)
		t.Cleanup(srv.Stop)
		return srv.URL()
	}
	t.Skipf("no free port for the mock camera")
	return ""
}

// TestMockCamServesAFrame checks that the mock camera serves a photo that the
// grabber can read back.
//
// It exists so the re-enroll path can be exercised end to end without a
// person at the door: a camera serving a real photograph is the input the
// command is written against.
func TestMockCamServesAFrame(t *testing.T) {
	photo := "../../test_hopkins_1.jpg"
	if _, err := os.Stat(photo); err != nil {
		t.Skipf("missing fixture: %v", err)
	}

	srv := startMockCam(t, photo)

	g := framesource.NewGrabber(srv, zerolog.Nop())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	w, h, pix, err := g.Grab(ctx)
	if err != nil {
		t.Fatalf("Grab: %v", err)
	}
	if w <= 0 || h <= 0 {
		t.Fatalf("bad dimensions %dx%d", w, h)
	}
	if len(pix) != w*h*3 {
		t.Fatalf("pixel count %d, want %d", len(pix), w*h*3)
	}
}
