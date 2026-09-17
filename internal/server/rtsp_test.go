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
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmjpeg"
)

const mockRTSPAddr = "127.0.0.1:18555"

// mockRTSPTest is an in-process M-JPEG / H.264 RTSP server used to test the
// pure-Go readRTSPFrame client.
type mockRTSPTest struct {
	srv    *gortsplib.Server
	stream *gortsplib.ServerStream
	media  *description.Media

	mjpegEnc    *rtpmjpeg.Encoder
	mjpegFrame  []byte
	h264Enc     *rtph264.Encoder
	h264NALUs   [][]byte
	description *description.Session

	ts   uint32
	stop chan struct{}
	done chan struct{}
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
					PayloadTyp:        96,
					PacketizationMode: 1,
				}},
			}},
		}
	}

	m.stream = &gortsplib.ServerStream{Server: m.srv, Desc: desc}
	if err := m.stream.Initialize(); err != nil {
		return nil, err
	}

	m.media = m.stream.Desc.Medias[0]
	if mjpeg {
		var err error
		m.mjpegEnc, err = m.media.Formats[0].(*format.MJPEG).CreateEncoder()
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		m.h264Enc, err = m.media.Formats[0].(*format.H264).CreateEncoder()
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
			ts := uint32(90000 * time.Since(start) / time.Second)
			if m.mjpegEnc != nil && m.mjpegFrame != nil {
				pkts, err := m.mjpegEnc.Encode(m.mjpegFrame)
				if err == nil {
					for _, p := range pkts {
						p.Timestamp = ts
						_ = m.stream.WritePacketRTP(m.media, p)
					}
				}
			}
			if m.h264Enc != nil && m.h264NALUs != nil {
				pkts, err := m.h264Enc.Encode(m.h264NALUs)
				if err == nil {
					for _, p := range pkts {
						p.Timestamp = ts
						_ = m.stream.WritePacketRTP(m.media, p)
					}
				}
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
	// 256x256 to force multiple RTP packets (> MTU); matches the e2e suite.
	img := image.NewRGBA(image.Rect(0, 0, 256, 256))
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
	m.mjpegFrame = mockJPEGFrame(t)

	img, err := readRTSPFrame("rtsp://"+mockRTSPAddr+"/face", 2*time.Second)
	if err != nil {
		t.Fatalf("readRTSPFrame: %v", err)
	}
	if img == nil || img.w != 256 || img.h != 256 {
		t.Fatalf("readRTSPFrame returned image %v, want 256x256", img)
	}
}

func TestReadRTSPFrameH264(t *testing.T) {
	m, err := newMockRTSP(false)
	if err != nil {
		t.Fatalf("start mock RTSP (H264): %v", err)
	}
	defer m.stopSrv()

	// Split the length-prefixed AVC fixture into raw NALUs (including the
	// 1-byte NAL header). gortsplib's encoder/decoder expects the header;
	// govid's ParseNALUnits strips it (RBSP only), so we split manually.
	nalus := splitAVC(h264TestAU, 4)
	m.h264NALUs = nalus

	img, err := readRTSPFrame("rtsp://"+mockRTSPAddr+"/face", 5*time.Second)
	if err != nil {
		t.Fatalf("readRTSPFrame (H.264): %v", err)
	}
	if img == nil || img.w != 160 || img.h != 120 {
		t.Fatalf("readRTSPFrame returned image %v, want 160x120", img)
	}
}

// splitAVC splits length-prefixed AVC data into raw NALUs (each including
// the 1-byte NAL header byte) suitable for gortsplib's rtph264 encoder.
func splitAVC(data []byte, lengthSize int) [][]byte {
	var nalus [][]byte
	off := 0
	for off+lengthSize <= len(data) {
		var n uint32
		switch lengthSize {
		case 4:
			n = uint32(data[off])<<24 | uint32(data[off+1])<<16 | uint32(data[off+2])<<8 | uint32(data[off+3])
		}
		off += lengthSize
		if off+int(n) > len(data) || n == 0 {
			break
		}
		nalus = append(nalus, data[off:off+int(n)])
		off += int(n)
	}
	return nalus
}
