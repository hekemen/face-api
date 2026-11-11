// Package framesource provides an RTSP frame grabber that captures a single
// high-resolution frame from a camera.
package framesource

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	govidh264 "github.com/liqmix/govid/h264"
	"github.com/pion/rtp"
	"github.com/rs/zerolog"

	"h2hsecure.com/face/internal/imaging"
)

// naluTypeMask masks the NALU type out of a NALU header byte.
const naluTypeMask = 0x1f

// naluTypeIDR is the H.264 NALU type of an IDR (key) frame.
const naluTypeIDR = 5

// ErrNoKeyframe reports that no keyframe arrived before the deadline.
var ErrNoKeyframe = errors.New("no keyframe received")

// Grabber captures a single frame from an RTSP camera.
//
// For H.264 streams it decodes only keyframes: the camera repeats
// SPS+PPS+IDR every couple of seconds and an IDR is self-contained, so it
// decodes to a complete frame without any preceding P-frames. That cuts the
// decode work by more than an order of magnitude, which is what makes a 5 MP
// stream decodable on a small host — and it keeps the decoder ahead of the
// packet rate, so no RTP packets are dropped and the H.264 chain survives.
type Grabber struct {
	url string
	log zerolog.Logger
}

// NewGrabber returns a Grabber for the RTSP URL. The URL is not validated
// until Grab is called.
func NewGrabber(url string, log zerolog.Logger) *Grabber {
	return &Grabber{url: url, log: log}
}

// Grab opens an RTSP session, waits for the next decodable frame and returns
// it as packed RGB pixels (w*h*3 bytes).
//
// The context bounds the whole operation: cancelling it makes Grab return
// without waiting for the camera's next keyframe.
func (g *Grabber) Grab(ctx context.Context) (w, h int, pix []byte, err error) {
	if g.url == "" {
		return 0, 0, nil, errors.New("missing RTSP URL")
	}

	u, err := base.ParseURL(g.url)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("invalid RTSP URL: %w", err)
	}

	proto := gortsplib.ProtocolTCP
	c := &gortsplib.Client{
		Scheme:        u.Scheme,
		Host:          u.Host,
		Protocol:      &proto,
		ReadTimeout:   10 * time.Second,
		WriteTimeout:  10 * time.Second,
		AnyPortEnable: true,
		// The UDP receive buffer must absorb the burst of packets that
		// arrives with a keyframe of a high-resolution stream; the OS
		// default (~200 kB) overflows and the lost packets break the
		// H.264 chain.
		UDPReadBufferSize:        4 * 1024 * 1024,
		DisableRTCPSenderReports: true,
	}
	defer c.Close()

	if err := c.Start(); err != nil {
		return 0, 0, nil, fmt.Errorf("RTSP connect: %w", err)
	}

	desc, _, err := c.Describe(u)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("RTSP describe: %w", err)
	}

	media, forma, err := selectFormat(desc)
	if err != nil {
		return 0, 0, nil, err
	}

	if err := c.SetupAll(u, desc.Medias); err != nil {
		return 0, 0, nil, fmt.Errorf("RTSP setup: %w", err)
	}

	type frameResult struct {
		w, h int
		pix  []byte
		err  error
	}
	frameCh := make(chan frameResult, 1)

	switch f := forma.(type) {
	case *format.H264:
		depacketizer, err := f.CreateDecoder()
		if err != nil {
			return 0, 0, nil, fmt.Errorf("create H.264 depacketizer: %w", err)
		}
		decoder := govidh264.NewDecoder()
		c.OnPacketRTP(media, f, func(pkt *rtp.Packet) {
			nalus, err := depacketizer.Decode(pkt)
			if err != nil {
				return
			}
			// Skip inter frames: only a keyframe decodes standalone, and
			// decoding one is enough to get a full-resolution picture.
			if !isKeyframeAccessUnit(nalus) {
				return
			}
			avc := make([]byte, 0, 1024)
			var lenBuf [4]byte
			for _, nalu := range nalus {
				binary.BigEndian.PutUint32(lenBuf[:], uint32(len(nalu)))
				avc = append(avc, lenBuf[:]...)
				avc = append(avc, nalu...)
			}
			img, err := decoder.DecodePacket(avc)
			if err != nil || img == nil {
				return
			}
			rgb := imaging.ToRGB(img)
			select {
			case frameCh <- frameResult{w: rgb.W, h: rgb.H, pix: rgb.Pix}:
			default:
			}
		})
	case *format.MJPEG:
		decoder, err := f.CreateDecoder()
		if err != nil {
			return 0, 0, nil, fmt.Errorf("create MJPEG decoder: %w", err)
		}
		c.OnPacketRTP(media, f, func(pkt *rtp.Packet) {
			jpegData, err := decoder.Decode(pkt)
			if err != nil || jpegData == nil {
				return
			}
			img, err := imaging.DecodeRGB(jpegData)
			if err != nil {
				return
			}
			select {
			case frameCh <- frameResult{w: img.W, h: img.H, pix: img.Pix}:
			default:
			}
		})
	}

	if _, err := c.Play(nil); err != nil {
		return 0, 0, nil, fmt.Errorf("RTSP play: %w", err)
	}

	select {
	case <-ctx.Done():
		return 0, 0, nil, ctx.Err()
	case res := <-frameCh:
		if res.err != nil {
			return 0, 0, nil, res.err
		}
		return res.w, res.h, res.pix, nil
	}
}

// selectFormat picks the codec to decode: MJPEG when the camera offers it,
// otherwise H.264. H.265 and anything else is rejected with a message that
// names the problem.
func selectFormat(desc *description.Session) (*description.Media, format.Format, error) {
	for _, media := range desc.Medias {
		for _, forma := range media.Formats {
			if _, ok := forma.(*format.MJPEG); ok {
				return media, forma, nil
			}
		}
	}
	for _, media := range desc.Medias {
		for _, forma := range media.Formats {
			if _, ok := forma.(*format.H264); ok {
				return media, forma, nil
			}
		}
	}
	for _, media := range desc.Medias {
		for _, forma := range media.Formats {
			if _, ok := forma.(*format.H265); ok {
				return nil, nil, errors.New("RTSP stream is H.265 (unsupported codec - H.264 and MJPEG only)")
			}
		}
	}
	return nil, nil, errors.New("RTSP stream has no supported codec (H.264 or MJPEG only)")
}

// isKeyframeAccessUnit reports whether an access unit contains an IDR (NALU
// type 5), i.e. whether it can be decoded without any preceding frame.
func isKeyframeAccessUnit(nalus [][]byte) bool {
	for _, nalu := range nalus {
		if len(nalu) > 0 && nalu[0]&naluTypeMask == naluTypeIDR {
			return true
		}
	}
	return false
}
