package server

import (
	"encoding/binary"
	"errors"
	"fmt"
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

// readRTSPFrame connects to an RTSP server, reads one decoded frame (MJPEG or
// H.264) within the timeout, and returns it as an rgbImage. If the stream uses
// any other codec, it returns a descriptive error without reading any frames.
func readRTSPFrame(url string, timeout time.Duration) (*rgbImage, error) {
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

	// Prefer MJPEG when available; fall back to H.264 (the two codecs the
	// pure-Go pipeline can decode). Reject anything else.
	for _, media := range desc.Medias {
		for _, forma := range media.Formats {
			if f, ok := forma.(*format.MJPEG); ok {
				return readRTSPFrameMJPEG(c, u, desc, media, f, timeout)
			}
		}
	}
	for _, media := range desc.Medias {
		for _, forma := range media.Formats {
			if f, ok := forma.(*format.H264); ok {
				return readRTSPFrameH264(c, u, desc, media, f, timeout)
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

func readRTSPFrameMJPEG(c *gortsplib.Client, u *base.URL, desc *description.Session,
	media *description.Media, f *format.MJPEG, timeout time.Duration) (*rgbImage, error) {
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

	// MJPEG frames are fragmented across multiple RTP packets. Feed every
	// packet to the decoder until it assembles one complete frame.
	c.OnPacketRTP(media, f, func(pkt *rtp.Packet) {
		select {
		case <-received:
			return
		default:
		}
		jpegData, err := decoder.Decode(pkt)
		if err != nil {
			if errors.Is(err, rtpmjpeg.ErrMorePacketsNeeded) {
				return // intermediate fragment, keep accumulating
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
		return decodeRGB(res.data)
	}
}

func readRTSPFrameH264(c *gortsplib.Client, u *base.URL, desc *description.Session,
	media *description.Media, f *format.H264, timeout time.Duration) (*rgbImage, error) {
	depacketizer, err := f.CreateDecoder()
	if err != nil {
		return nil, fmt.Errorf("create H.264 depacketizer: %w", err)
	}

	if err := c.SetupAll(u, desc.Medias); err != nil {
		return nil, fmt.Errorf("RTSP setup: %w", err)
	}

	// Pure-Go software decoder (github.com/liqmix/govid/h264). It expects
	// length-prefixed AVC access units (4-byte big-endian NALU sizes), while
	// gortsplib's rtph264.Decoder yields one NALU slice per element.
	gv := govidh264.NewDecoder()

	type frameResult struct {
		img *rgbImage
		err error
	}
	frameCh := make(chan frameResult, 1)
	received := make(chan struct{})

	// H.264 access units (SPS/PPS/slices) are reassembled by the depacketizer;
	// govid decodes the whole AU at once. The decoder is stateful across calls
	// (keeps SPS/PPS and reference frames), so transient errors like a missing
	// SPS on a mid-GOP join are expected and skipped — keep reading until a
	// full frame is produced or the timeout fires.
	c.OnPacketRTP(media, f, func(pkt *rtp.Packet) {
		select {
		case <-received:
			return
		default:
		}
		nalus, err := depacketizer.Decode(pkt)
		if err != nil {
			if errors.Is(err, rtph264.ErrMorePacketsNeeded) {
				return // intermediate fragment, keep accumulating
			}
			return // ignore non-starting packets / transient depacketizer errors
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
			return // e.g. slice before SPS/PPS on a mid-GOP join; keep reading
		}
		if frame == nil {
			return // parameter sets only, no picture yet
		}
		close(received)
		frameCh <- frameResult{img: imageToRGB(frame)}
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
		return res.img, nil
	}
}
