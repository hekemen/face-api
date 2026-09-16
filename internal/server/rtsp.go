package server

import (
	"errors"
	"fmt"
	"time"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/bluenviron/gortsplib/v5/pkg/base"
	"github.com/bluenviron/gortsplib/v5/pkg/description"
	"github.com/bluenviron/gortsplib/v5/pkg/format"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmjpeg"
	"github.com/pion/rtp"
)

// readRTSPFrame connects to an RTSP server, reads one MJPEG frame within the
// timeout, and returns it as an rgbImage. If the stream uses H.264 (or any
// non-MJPEG codec), it returns a descriptive error without reading any frames.
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

	// Find the first MJPEG media/format; reject H.264 or anything else.
	var mjpegMedia *description.Media
	var mjpegFormat *format.MJPEG
	for _, media := range desc.Medias {
		for _, forma := range media.Formats {
			if f, ok := forma.(*format.MJPEG); ok {
				mjpegMedia = media
				mjpegFormat = f
				break
			}
		}
		if mjpegMedia != nil {
			break
		}
	}
	if mjpegMedia == nil {
		for _, media := range desc.Medias {
			for _, forma := range media.Formats {
				if _, ok := forma.(*format.H264); ok {
					return nil, fmt.Errorf("RTSP stream is H.264 (unsupported codec - MJPEG only)")
				}
			}
		}
		return nil, fmt.Errorf("RTSP stream does not advertise MJPEG")
	}

	decoder, err := mjpegFormat.CreateDecoder()
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
	c.OnPacketRTP(mjpegMedia, mjpegFormat, func(pkt *rtp.Packet) {
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
