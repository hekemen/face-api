package main

import (
	"fmt"
	"os"
	"testing"

	"h2hsecure.com/face/internal/rtsp/rtspmock"
)

// startMockCam starts a mock camera on a free port and returns its stream
// URL, so the flow can be exercised without a person at the door.
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
