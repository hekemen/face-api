// Package rtspmock provides an in-process M-JPEG RTSP server used by both
// the framesource tests (internal/framesource) and the container RTSP suite
// (test/rtsp). Sharing a single mock keeps the two suites' stream behaviour
// identical.
package rtspmock

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

// mockFrameRate is the frames per second the pump streams.
const mockFrameRate = 10

// Server is an in-process M-JPEG RTSP server.
type Server struct {
	srv    *gortsplib.Server
	stream *gortsplib.ServerStream
	media  *description.Media
	enc    *rtpmjpeg.Encoder

	mu      sync.Mutex
	frame   []byte
	ts      uint32
	stopCh  chan struct{}
	wg      sync.WaitGroup
	rtspAddr string
}

var _ gortsplib.ServerHandlerOnDescribe = (*Server)(nil)
var _ gortsplib.ServerHandlerOnSetup = (*Server)(nil)
var _ gortsplib.ServerHandlerOnPlay = (*Server)(nil)

// New creates a Server listening for RTSP on addr. The RTP/RTCP UDP ports
// are fixed (127.0.0.1:18000 / 127.0.0.1:18001), so only one Server can run
// at a time.
func New(addr string) (*Server, error) {
	return &Server{rtspAddr: addr}, nil
}

// Start binds the RTSP/UDP listeners and begins streaming.
func (s *Server) Start() error {
	s.srv = &gortsplib.Server{
		Handler:        s,
		RTSPAddress:    s.rtspAddr,
		UDPRTPAddress:  "127.0.0.1:18000",
		UDPRTCPAddress: "127.0.0.1:18001",
	}
	if err := s.srv.Start(); err != nil {
		return err
	}

	desc := &description.Session{
		Medias: []*description.Media{{
			Type:    description.MediaTypeVideo,
			Formats: []format.Format{&format.MJPEG{}},
		}},
	}

	s.stream = &gortsplib.ServerStream{Server: s.srv, Desc: desc}
	if err := s.stream.Initialize(); err != nil {
		return err
	}

	s.media = s.stream.Desc.Medias[0]
	var err error
	s.enc, err = s.media.Formats[0].(*format.MJPEG).CreateEncoder()
	if err != nil {
		return err
	}

	s.stopCh = make(chan struct{})
	s.wg.Add(1)
	go s.pump()
	return nil
}

// URL returns the RTSP URL of the stream, e.g. rtsp://127.0.0.1:18554/stream.
func (s *Server) URL() string {
	return "rtsp://" + s.rtspAddr + "/stream"
}

// SetFrame atomically switches the JPEG streamed to clients.
func (s *Server) SetFrame(jpegBytes []byte) {
	s.mu.Lock()
	s.frame = jpegBytes
	s.mu.Unlock()
}

// Stop shuts the server down and waits for the pump to exit.
func (s *Server) Stop() {
	close(s.stopCh)
	s.wg.Wait()
	s.stream.Close()
	s.srv.Close()
}

func (s *Server) OnDescribe(*gortsplib.ServerHandlerOnDescribeCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *Server) OnSetup(*gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	return &base.Response{StatusCode: base.StatusOK}, s.stream, nil
}

func (s *Server) OnPlay(*gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	return &base.Response{StatusCode: base.StatusOK}, nil
}

func (s *Server) pump() {
	defer s.wg.Done()

	ticker := time.NewTicker(time.Second / mockFrameRate)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			frame := s.frame
			ts := s.ts
			s.ts += 9000 // 90000 Hz clock / 10 fps
			s.mu.Unlock()

			if frame == nil {
				continue
			}

			pkts, err := s.enc.Encode(frame)
			if err != nil {
				continue
			}
			for _, p := range pkts {
				p.Timestamp = ts
				_ = s.stream.WritePacketRTP(s.media, p)
			}
		case <-s.stopCh:
			return
		}
	}
}

// SolidJPEG renders a uniformly gray JPEG (no face) at the given dimensions.
func SolidJPEG(w, h int) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.RGBA{R: 128, G: 128, B: 128, A: 255}), image.Point{}, draw.Src)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PadJPEGToSide embeds src on a centered black canvas whose side is a multiple
// of 8 (RTP/M-JPEG constraint) and re-encodes it as JPEG.
func PadJPEGToSide(src []byte, side int) ([]byte, error) {
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
