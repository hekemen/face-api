package rtspmock

import (
	"bytes"
	"image"
	"image/jpeg"
	"testing"
	"time"

	"github.com/pion/rtp"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
)

func TestServerServesFrames(t *testing.T) {
	srv, err := New("127.0.0.1:18554")
	if err != nil {
		t.Skipf("port busy: %v", err)
	}
	defer srv.Stop()

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Put one gray frame on the stream.
	srv.SetFrame(mustSolidJPEG(t, 320, 240))

	u := base.URL{Scheme: "rtsp", Host: "127.0.0.1:18554", Path: "/stream"}
	c := gortsplib.Client{Scheme: u.Scheme, Host: u.Host}

	if err := c.Start(); err != nil {
		t.Fatalf("client Start: %v", err)
	}
	defer c.Close()

	desc, _, err := c.Describe(&u)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}

	var forma *format.MJPEG
	medi := desc.FindFormat(&forma)
	if medi == nil {
		t.Fatal("M-JPEG media not found")
	}

	rtpDec, err := forma.CreateDecoder()
	if err != nil {
		t.Fatalf("CreateDecoder: %v", err)
	}

	if _, err := c.Setup(desc.BaseURL, medi, 0, 0); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	decoded := make(chan image.Image, 1)
	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		enc, err := rtpDec.Decode(pkt)
		if err != nil {
			return // ignore non-complete packets
		}
		img, err := jpeg.Decode(bytes.NewReader(enc))
		if err != nil {
			return
		}
		select {
		case decoded <- img:
		default:
		}
	})

	if _, err := c.Play(nil); err != nil {
		t.Fatalf("Play: %v", err)
	}

	select {
	case img := <-decoded:
		if b := img.Bounds(); b.Dx() != 320 || b.Dy() != 240 {
			t.Fatalf("decoded frame %dx%d, want 320x240", b.Dx(), b.Dy())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no M-JPEG frame decoded within 5 seconds")
	}
}

func mustSolidJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	b, err := SolidJPEG(w, h)
	if err != nil {
		t.Fatalf("SolidJPEG: %v", err)
	}
	return b
}
