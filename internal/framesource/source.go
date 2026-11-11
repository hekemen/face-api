// Package framesource provides a long-lived RTSP sub-stream reader that
// continuously decodes the camera into a double-buffered latest-frame store.
package framesource

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"strings"
	"time"

	"github.com/pion/rtp"
	"github.com/rs/zerolog"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	govidh264 "github.com/liqmix/govid/h264"

	"h2hsecure.com/face/internal/imaging"
)

// backoff bounds, in seconds.
const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
)

// unsupportedCodec is returned by the real stream when the camera offers only
// an unsupported codec (e.g. H.265), so the source backs off at the cap and
// logs once per attempt instead of tight-reconnecting.
var unsupportedCodec = errors.New("unsupported codec")

// Source exposes the newest decoded frame of a long-lived RTSP sub-stream.
//
// Run connects once, decodes every frame and publishes it to the buffer;
// on disconnect it reconnects with exponential backoff (1s … 30s). Run
// returns when ctx is cancelled.
type Source struct {
	url string
	log zerolog.Logger
	buf *Buffer

	// stream is injected for tests; production uses streamRTSP.
	stream streamFunc
}

// streamFunc connects to url, decodes frames and hands each fully decoded
// image.Image to onFrame. It returns when the stream ends; ctx cancellation
// must surface as ctx.Err().
type streamFunc func(ctx context.Context, url string, onFrame func(image.Image)) error

// New creates a Source for url. The buffer starts empty; Run must be called
// to begin streaming.
func New(url string, log zerolog.Logger) *Source {
	return &Source{
		url:    url,
		log:    log,
		buf:    NewBuffer(),
		stream: streamRTSP,
	}
}

// Dims returns the dimensions of the latest published frame, or 0,0 before the
// first frame.
func (s *Source) Dims() (w, h int) {
	return s.buf.Dims()
}

// Latest copies the newest frame into dst (see Buffer.Latest).
func (s *Source) Latest(dst []byte) (w, h int, seq uint64, at time.Time, ok bool) {
	return s.buf.Latest(dst)
}

// Run connects to url and streams frames into the buffer until ctx is
// cancelled. On disconnect it reconnects with exponential backoff.
func (s *Source) Run(ctx context.Context) error {
	backoff := minBackoff
	unsupportedLogged := false
	for {
		if err := s.stream(ctx, s.url, s.publish); err != nil {
			if ctx.Err() != nil {
				return nil // cancelled: exit, do not reconnect
			}
			if strings.Contains(err.Error(), unsupportedCodec.Error()) {
				// H.265 or another unsupported codec: log once per attempt and
				// back off at the minimum interval instead of tight-reconnecting.
				if !unsupportedLogged {
					s.log.Warn().Msg("rtsp stream has unsupported codec")
					unsupportedLogged = true
				}
				backoff = minBackoff
			} else {
				s.log.Warn().Err(err).Msg("rtsp stream ended, reconnecting")
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		// stream ended cleanly (e.g. server closed the connection): reset the
		// backoff and reconnect.
		backoff = minBackoff
	}
}

// publish converts img to RGB and publishes it to the buffer. It is called
// from the stream goroutine, so it must not block for long.
func (s *Source) publish(img image.Image) {
	b := img.Bounds()
	s.buf.Publish(func(dst *imaging.RGBImage) {
		dst.W, dst.H = b.Dx(), b.Dy()
		dst.Pix = make([]byte, dst.W*dst.H*3)
		if !imaging.ConvertInto(dst, img) {
			s.log.Error().Msg("framesource: convert into failed")
		}
	})
}

// streamRTSP is the production implementation of streamFunc: one long-lived
// gortsplib client that decodes MJPEG or H.264 and hands every fully decoded
// frame to onFrame.
func streamRTSP(ctx context.Context, url string, onFrame func(image.Image)) error {
	u, err := base.ParseURL(url)
	if err != nil {
		return fmt.Errorf("invalid RTSP URL: %w", err)
	}

	proto := gortsplib.ProtocolTCP
	c := &gortsplib.Client{
		Scheme:        u.Scheme,
		Host:          u.Host,
		Protocol:      &proto,
		ReadTimeout:   10 * time.Second,
		WriteTimeout:  10 * time.Second,
		AnyPortEnable: true,
		// A long-lived stream decodes continuously, so the UDP receive buffer
		// must absorb a decode hiccup. The OS default (~200 kB) overflows on a
		// high-bitrate stream and the lost packets break the H.264 chain
		// ("buffer length exceeds 64"), which kills the session.
		UDPReadBufferSize:        4 * 1024 * 1024,
		DisableRTCPSenderReports: true,
	}
	defer c.Close()

	if err := c.Start(); err != nil {
		return fmt.Errorf("RTSP connect: %w", err)
	}

	desc, _, err := c.Describe(u)
	if err != nil {
		return fmt.Errorf("RTSP describe: %w", err)
	}

	// Select a codec: prefer MJPEG, then H.264; reject H.265 and anything else.
	var media *description.Media
	var forma format.Format
	isH264 := false

	for _, m := range desc.Medias {
		for _, f := range m.Formats {
			if _, ok := f.(*format.MJPEG); ok {
				media, forma = m, f
				goto selected
			}
		}
	}
	for _, m := range desc.Medias {
		for _, f := range m.Formats {
			if f, ok := f.(*format.H264); ok {
				media, forma = m, f
				isH264 = true
				goto selected
			}
		}
	}
	for _, m := range desc.Medias {
		for _, f := range m.Formats {
			if _, ok := f.(*format.H265); ok {
				return fmt.Errorf("%w: H.265 (H.264 and MJPEG only)", unsupportedCodec)
			}
		}
	}
	return fmt.Errorf("RTSP stream has no supported codec (H.264 or MJPEG only)")

selected:
	if err := c.SetupAll(u, desc.Medias); err != nil {
		return fmt.Errorf("RTSP setup: %w", err)
	}

	if isH264 {
		depacketizer, err := forma.(*format.H264).CreateDecoder()
		if err != nil {
			return fmt.Errorf("create H.264 depacketizer: %w", err)
		}
		gv := govidh264.NewDecoder()

		c.OnPacketRTP(media, forma.(*format.H264), func(pkt *rtp.Packet) {
			nalus, err := depacketizer.Decode(pkt)
			if err != nil {
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
			if err != nil || frame == nil {
				return
			}
			onFrame(frame)
		})
	} else {
		decoder, err := forma.(*format.MJPEG).CreateDecoder()
		if err != nil {
			return fmt.Errorf("create MJPEG decoder: %w", err)
		}

		c.OnPacketRTP(media, forma.(*format.MJPEG), func(pkt *rtp.Packet) {
			jpegData, err := decoder.Decode(pkt)
			if err != nil {
				return
			}
			if jpegData == nil {
				return
			}
			img, err := jpeg.Decode(bytes.NewReader(jpegData))
			if err != nil {
				return
			}
			onFrame(img)
		})
	}

	if _, err := c.Play(nil); err != nil {
		return fmt.Errorf("RTSP play: %w", err)
	}

	// Block until the stream ends or ctx is cancelled.
	errCh := make(chan error, 1)
	go func() { errCh <- c.Wait() }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}
