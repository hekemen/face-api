// Command mockcam serves a mock RTSP camera that plays a still photograph.
//
// It exists so the re-enroll path can be exercised end to end without a
// person at the door: a camera serving a real photograph is the input the
// command is written against.
//
// Usage:
//
//	mockcam <listen-addr> <photo.jpg>
package main

import (
	"fmt"
	"os"
	"time"

	"h2hsecure.com/face/internal/rtsp/rtspmock"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: mockcam <listen-addr> <photo.jpg>")
		os.Exit(2)
	}
	addr, photo := os.Args[1], os.Args[2]

	raw, err := os.ReadFile(photo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read photo:", err)
		os.Exit(1)
	}
	srv, err := rtspmock.New(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start mock camera:", err)
		os.Exit(1)
	}
	defer srv.Stop()
	srv.SetFrame(raw)
	fmt.Printf("mock camera serving %s at rtsp://%s/stream\n", photo, addr)
	for {
		time.Sleep(time.Second)
	}
}
