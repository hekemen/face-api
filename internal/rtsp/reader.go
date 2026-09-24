package rtsp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmjpeg"
	govidh264 "github.com/liqmix/govid/h264"
	"github.com/pion/rtp"
)

// Reader connects to an RTSP server, reads one decoded frame (MJPEG or H.264)
// within the timeout, and returns it as JPEG bytes. If the stream uses any
// other codec, it returns a descriptive error without reading any frames.
type Reader struct{}

// New creates a new Reader.
func New() *Reader {
	return &Reader{}
}

// ReadFrame connects to an RTSP server and returns the first decoded frame as
// JPEG bytes. The timeout applies to the full connection + frame capture.
func (r *Reader) ReadFrame(url string, timeout time.Duration) ([]byte, error) {
	u, err := base.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("invalid RTSP URL: %w", err)
	}

	proto := gortsplib.ProtocolTCP
	c := &gortsplib.Client{
		Scheme:                   u.Scheme,
		Host:                     u.Host,
		Protocol:                 &proto,
		ReadTimeout:              timeout,
		WriteTimeout:             timeout,
		AnyPortEnable:            true,
		DisableRTCPSenderReports: true,
	}
	defer c.Close()

	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("RTSP connect: %w", err)
	}

	desc, _, err := c.Describe(u)
	if err != nil {
		return nil, fmt.Errorf("RTSP describe: %w", err)
	}

	// Prefer MJPEG when available; fall back to H.264. Reject anything else.
	for _, media := range desc.Medias {
		for _, forma := range media.Formats {
			if f, ok := forma.(*format.MJPEG); ok {
				return r.readMJPEG(c, u, desc, media, f, timeout)
			}
		}
	}
	for _, media := range desc.Medias {
		for _, forma := range media.Formats {
			if f, ok := forma.(*format.H264); ok {
				return r.readH264(c, u, desc, media, f, timeout)
			}
		}
	}
	for _, media := range desc.Medias {
		for _, forma := range media.Formats {
			if _, ok := forma.(*format.H265); ok {
				return nil, fmt.Errorf("RTSP stream is H.265 (unsupported codec - H.264 and MJPEG only)")
			}
		}
	}
	return nil, fmt.Errorf("RTSP stream has no supported codec (H.264 or MJPEG only)")
}

func (r *Reader) readMJPEG(c *gortsplib.Client, u *base.URL, desc *description.Session,
	media *description.Media, f *format.MJPEG, timeout time.Duration) ([]byte, error) {
	decoder, err := f.CreateDecoder()
	if err != nil {
		return nil, fmt.Errorf("create MJPEG decoder: %w", err)
	}

	if err := c.SetupAll(u, desc.Medias); err != nil {
		return nil, fmt.Errorf("RTSP setup: %w", err)
	}

	type frameResult struct {
		data []byte
		err  error
	}
	frameCh := make(chan frameResult, 1)
	received := make(chan struct{})

	c.OnPacketRTP(media, f, func(pkt *rtp.Packet) {
		select {
		case <-received:
			return
		default:
		}
		jpegData, err := decoder.Decode(pkt)
		if err != nil {
			if errors.Is(err, rtpmjpeg.ErrMorePacketsNeeded) {
				return
			}
			close(received)
			frameCh <- frameResult{err: fmt.Errorf("decode MJPEG RTP: %w", err)}
			return
		}
		if jpegData == nil {
			return
		}
		close(received)
		frameCh <- frameResult{data: jpegData}
	})

	if _, err := c.Play(nil); err != nil {
		return nil, fmt.Errorf("RTSP play: %w", err)
	}

	select {
	case <-time.After(timeout):
		return nil, fmt.Errorf("RTSP timeout: no frame received within %v", timeout)
	case res := <-frameCh:
		if res.err != nil {
			return nil, res.err
		}
		return res.data, nil
	}
}

func (r *Reader) readH264(c *gortsplib.Client, u *base.URL, desc *description.Session,
	media *description.Media, f *format.H264, timeout time.Duration) ([]byte, error) {
	depacketizer, err := f.CreateDecoder()
	if err != nil {
		return nil, fmt.Errorf("create H.264 depacketizer: %w", err)
	}

	if err := c.SetupAll(u, desc.Medias); err != nil {
		return nil, fmt.Errorf("RTSP setup: %w", err)
	}

	gv := govidh264.NewDecoder()

	type frameResult struct {
		jpeg []byte
		err  error
	}
	frameCh := make(chan frameResult, 1)
	received := make(chan struct{})

	c.OnPacketRTP(media, f, func(pkt *rtp.Packet) {
		select {
		case <-received:
			return
		default:
		}
		nalus, err := depacketizer.Decode(pkt)
		if err != nil {
			if errors.Is(err, rtph264.ErrMorePacketsNeeded) {
				return
			}
			return
		}

		avc := make([]byte, 0, 1024)
		var lenBuf [4]byte
		for _, nalu := range nalus {
			binary.BigEndian.PutUint32(lenBuf[:], uint32(len(nalu)))
			avc = append(avc, lenBuf[:]...)
			avc = append(avc, nalu...)
		}

		frame, err := gv.DecodePacket(avc)
		if err != nil {
			return
		}
		if frame == nil {
			return
		}
		close(received)

		jpegData, err := encodeToJPEG(frame)
		if err != nil {
			frameCh <- frameResult{err: fmt.Errorf("encode H.264 frame: %w", err)}
			return
		}
		frameCh <- frameResult{jpeg: jpegData}
	})

	if _, err := c.Play(nil); err != nil {
		return nil, fmt.Errorf("RTSP play: %w", err)
	}

	select {
	case <-time.After(timeout):
		return nil, fmt.Errorf("RTSP timeout: no frame received within %v", timeout)
	case res := <-frameCh:
		if res.err != nil {
			return nil, res.err
		}
		return res.jpeg, nil
	}
}

// encodeToJPEG converts an image.Image to JPEG bytes.
func encodeToJPEG(img image.Image) ([]byte, error) {
	b := img.Bounds()
	dst := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			dst.Set(x, y, color.NRGBA{
				R: uint8(r >> 8),
				G: uint8(g >> 8),
				B: uint8(bl >> 8),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, nil); err != nil {
		return nil, fmt.Errorf("encode frame to JPEG: %w", err)
	}
	return buf.Bytes(), nil
}
