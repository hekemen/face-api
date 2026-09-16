package rtsp

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmjpeg"
)

const (
	mockRTSPAddr  = "127.0.0.1:18554"
	mockUHDRTP    = "127.0.0.1:18000"
	mockUDHRTCP   = "127.0.0.1:18001"
	mockFrameRate = 10
)

// mockRTSP is an in-process M-JPEG RTSP server used to
// feed frames to the face-api /stream-check endpoint.
type mockRTSP struct {
	srv    *gortsplib.Server
	stream *gortsplib.ServerStream
	media  *description.Media
	enc    *rtpmjpeg.Encoder

	mu    sync.Mutex
	frame []byte
	ts    uint32

	stopCh chan struct{}
	wg     sync.WaitGroup
}

var _ gortsplib.ServerHandlerOnDescribe = (*mockRTSP)(nil)
var _ gortsplib.ServerHandlerOnSetup = (*mockRTSP)(nil)
var _ gortsplib.ServerHandlerOnPlay = (*mockRTSP)(nil)

func (m *mockRTSP) OnDescribe(
	*gortsplib.ServerHandlerOnDescribeCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, m.stream, nil
}

func (m *mockRTSP) OnSetup(
	*gortsplib.ServerHandlerOnSetupCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, m.stream, nil
}

func (m *mockRTSP) OnPlay(*gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// start binds the RTSP/UDP listeners and begins streaming.
func (m *mockRTSP) start() error {
	m.srv = &gortsplib.Server{
		Handler:        m,
		RTSPAddress:    mockRTSPAddr,
		UDPRTPAddress:  mockUHDRTP,
		UDPRTCPAddress: mockUDHRTCP,
	}

	// must be started before a ServerStream is created
	if err := m.srv.Start(); err != nil {
		return err
	}

	desc := &description.Session{
		Medias: []*description.Media{{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{&format.MJPEG{}},
		}},
	}

	m.stream = &gortsplib.ServerStream{Server: m.srv, Desc: desc}
	if err := m.stream.Initialize(); err != nil {
		return err
	}

	m.media = m.stream.Desc.Medias[0]
	var err error
	m.enc, err = m.media.Formats[0].(*format.MJPEG).CreateEncoder()
	if err != nil {
		return err
	}

	m.stopCh = make(chan struct{})
	m.wg.Add(1)
	go m.pump()
	return nil
}

// setFrame atomically switches the JPEG streamed to clients.
func (m *mockRTSP) setFrame(jpegBytes []byte) {
	m.mu.Lock()
	m.frame = jpegBytes
	m.mu.Unlock()
}

func (m *mockRTSP) stop() {
	close(m.stopCh)
	m.wg.Wait()
	m.stream.Close()
	m.srv.Close()
}

func (m *mockRTSP) pump() {
	defer m.wg.Done()

	ticker := time.NewTicker(time.Second / mockFrameRate)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			frame := m.frame
			ts := m.ts
			m.ts += 9000 // 90000 Hz clock / 10 fps
			m.mu.Unlock()

			if frame == nil {
				continue
			}

			pkts, err := m.enc.Encode(frame)
			if err != nil {
				continue
			}
			for _, p := range pkts {
				p.Timestamp = ts
				_ = m.stream.WritePacketRTP(m.media, p)
			}
		case <-m.stopCh:
			return
		}
	}
}

// padJPEGToSide embeds a JPEG on a centered black canvas whose side
// is a multiple of 8 (RTP/M-JPEG constraint) and re-encodes it as JPEG.
func padJPEGToSide(src []byte, side int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, err
	}
	b := img.Bounds()

	canvas := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)

	offX := (side - b.Dx()) / 2
	offY := (side - b.Dy()) / 2
	draw.Draw(canvas, image.Rect(offX, offY, offX+b.Dx(), offY+b.Dy()), img, b.Min, draw.Src)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// solidJPEG renders a uniformly gray JPEG (no face) at RTP-valid dimensions.
func solidJPEG(w, h int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{R: 128, G: 128, B: 128, A: 255}), image.Point{}, draw.Src)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
