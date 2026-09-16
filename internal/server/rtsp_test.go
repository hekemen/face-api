package server

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"testing"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmjpeg"
)

const mockRTSPAddr = "127.0.0.1:18555"

// mockRTSPTest is an in-process M-JPEG RTSP server with an optional H.264
// variant, used to test the pure-Go readRTSPFrame client.
type mockRTSPTest struct {
	srv    *gortsplib.Server
	stream *gortsplib.ServerStream
	media  *description.Media
	enc    *rtpmjpeg.Encoder

	frame []byte
	ts    uint32
	stop  chan struct{}
	done  chan struct{}
}

var _ gortsplib.ServerHandlerOnDescribe = (*mockRTSPTest)(nil)
var _ gortsplib.ServerHandlerOnSetup = (*mockRTSPTest)(nil)
var _ gortsplib.ServerHandlerOnPlay = (*mockRTSPTest)(nil)

func (m *mockRTSPTest) OnDescribe(*gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, m.stream, nil
}

func (m *mockRTSPTest) OnSetup(*gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, m.stream, nil
}

func (m *mockRTSPTest) OnPlay(*gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func newMockRTSP(mjpeg bool) (*mockRTSPTest, error) {
	m := &mockRTSPTest{stop: make(chan struct{}), done: make(chan struct{})}
	m.srv = &gortsplib.Server{Handler: m, RTSPAddress: mockRTSPAddr}
	if err := m.srv.Start(); err != nil {
		return nil, err
	}

	var desc *description.Session
	if mjpeg {
		desc = &description.Session{
			Medias: []*description.Media{{
				Type:    description.MediaTypeVideo,
				Formats: []format.Format{&format.MJPEG{}},
			}},
		}
	} else {
		desc = &description.Session{
			Medias: []*description.Media{{
				Type: description.MediaTypeVideo,
				Formats: []format.Format{&format.H264{
					PacketizationMode: 1,
				}},
			}},
		}
	}

	m.stream = &gortsplib.ServerStream{Server: m.srv, Desc: desc}
	if err := m.stream.Initialize(); err != nil {
		return nil, err
	}

	if mjpeg {
		m.media = m.stream.Desc.Medias[0]
		var err error
		m.enc, err = m.media.Formats[0].(*format.MJPEG).CreateEncoder()
		if err != nil {
			return nil, err
		}
	}

	go m.pump()
	return m, nil
}

func (m *mockRTSPTest) pump() {
	defer close(m.done)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			if m.enc == nil || m.frame == nil {
				continue
			}
			ts := uint32(90000 * time.Since(start) / time.Second)
			pkts, err := m.enc.Encode(m.frame)
			if err != nil {
				continue
			}
			for _, p := range pkts {
				p.Timestamp = ts
				_ = m.stream.WritePacketRTP(m.media, p)
			}
		}
	}
}

func (m *mockRTSPTest) stopSrv() {
	close(m.stop)
	<-m.done
	m.stream.Close()
	m.srv.Close()
}

func mockJPEGFrame(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{R: 200, G: 100, B: 50, A: 255}), image.Point{}, draw.Src)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode mock frame: %v", err)
	}
	return buf.Bytes()
}

func TestReadRTSPFrameMJPEG(t *testing.T) {
	m, err := newMockRTSP(true)
	if err != nil {
		t.Fatalf("start mock RTSP: %v", err)
	}
	defer m.stopSrv()
	m.frame = mockJPEGFrame(t)

	img, err := readRTSPFrame("rtsp://"+mockRTSPAddr+"/face", 2*time.Second)
	if err != nil {
		t.Fatalf("readRTSPFrame: %v", err)
	}
	if img == nil || img.w != 32 || img.h != 32 {
		t.Fatalf("readRTSPFrame returned image %v, want 32x32", img)
	}
	if img.w != 32 {
		t.Fatalf("frame width = %d, want 32", img.w)
	}
}

func TestReadRTSPFrameH264Rejected(t *testing.T) {
	m, err := newMockRTSP(false)
	if err != nil {
		t.Fatalf("start mock RTSP (H264): %v", err)
	}
	defer m.stopSrv()

	_, err = readRTSPFrame("rtsp://"+mockRTSPAddr+"/face", 2*time.Second)
	if err == nil {
		t.Fatal("readRTSPFrame accepted an H.264 stream, want not-ok error")
	}
}

func TestReadRTSPFrameUnreachable(t *testing.T) {
	_, err := readRTSPFrame("rtsp://127.0.0.1:9/nope", 1*time.Second)
	if err == nil {
		t.Fatal("readRTSPFrame returned nil error for unreachable server")
	}
}
