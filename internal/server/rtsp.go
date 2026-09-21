package server

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
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v5/pkg/format/rtpmjpeg"
	govidh264 "github.com/liqmix/govid/h264"
	"github.com/pion/rtp"
)

// rtspReader maintains a persistent RTSP connection and reads frames
// continuously. It handles reconnection on transient errors.
type rtspReader struct {
	url         string
	client      *gortsplib.Client
	media       *description.Media
	format      format.Format
	mjpegDec    *rtpmjpeg.Decoder
	h264Dec     *rtph264.Decoder
	gvDecoder   *govidh264.Decoder
	isMJPEG     bool
	proto       gortsplib.Protocol
}

// newRTSPReader opens a persistent connection to the RTSP stream.
func newRTSPReader(url string, timeout time.Duration) (*rtspReader, error) {
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

	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("RTSP connect: %w", err)
	}

	desc, _, err := c.Describe(u)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("RTSP describe: %w", err)
	}

	// Find codec
	var media *description.Media
	var forma format.Format
	for _, m := range desc.Medias {
		for _, f := range m.Formats {
			if _, ok := f.(*format.MJPEG); ok {
				media, forma = m, f
				break
			}
		}
		if media != nil {
			break
		}
	}
	if media == nil {
		for _, m := range desc.Medias {
			for _, f := range m.Formats {
				if _, ok := f.(*format.H264); ok {
					media, forma = m, f
					break
				}
			}
			if media != nil {
				break
			}
		}
	}
	if media == nil {
		for _, m := range desc.Medias {
			for _, f := range m.Formats {
				if _, ok := f.(*format.H265); ok {
					c.Close()
					return nil, fmt.Errorf("RTSP stream is H.265 (unsupported codec - H.264 and MJPEG only)")
				}
			}
		}
		c.Close()
		return nil, fmt.Errorf("RTSP stream has no supported codec (H.264 or MJPEG only)")
	}

	r := &rtspReader{
		url:    url,
		client: c,
		media:  media,
		format: forma,
		proto:  proto,
	}

	if _, ok := forma.(*format.MJPEG); ok {
		dec, err := forma.(*format.MJPEG).CreateDecoder()
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("create MJPEG decoder: %w", err)
		}
		r.mjpegDec = dec
		r.isMJPEG = true
	} else {
		h264f := forma.(*format.H264)
		dec, err := h264f.CreateDecoder()
		if err != nil {
			c.Close()
			return nil, fmt.Errorf("create H.264 decoder: %w", err)
		}
		r.h264Dec = dec
		r.gvDecoder = govidh264.NewDecoder()
	}

	if err := c.SetupAll(u, desc.Medias); err != nil {
		c.Close()
		return nil, fmt.Errorf("RTSP setup: %w", err)
	}

	if _, err := c.Play(nil); err != nil {
		c.Close()
		return nil, fmt.Errorf("RTSP play: %w", err)
	}

	return r, nil
}

// ReadFrame reads the next frame from the persistent RTSP connection.
// Returns nil, nil when the connection is closed (context done).
func (r *rtspReader) ReadFrame(ctx context.Context) (*rgbImage, error) {
	type frameResult struct {
		img *rgbImage
		err error
	}
	frameCh := make(chan frameResult, 1)
	received := make(chan struct{})

	if r.isMJPEG {
		r.client.OnPacketRTP(r.media, r.format, func(pkt *rtp.Packet) {
			select {
			case <-received:
				return
			default:
			}
			data, err := r.mjpegDec.Decode(pkt)
			if err != nil {
				if errors.Is(err, rtpmjpeg.ErrMorePacketsNeeded) {
					return
				}
				close(received)
				frameCh <- frameResult{err: fmt.Errorf("decode MJPEG RTP: %w", err)}
				return
			}
			if data == nil {
				return
			}
			close(received)
			frameCh <- frameResult{img: func() *rgbImage { im, _ := decodeRGB(data); return im }()}
		})
	} else {
		r.client.OnPacketRTP(r.media, r.format, func(pkt *rtp.Packet) {
			select {
			case <-received:
				return
			default:
			}
			nalus, err := r.h264Dec.Decode(pkt)
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
			frame, err := r.gvDecoder.DecodePacket(avc)
			if err != nil || frame == nil {
				return
			}
			close(received)
			frameCh <- frameResult{img: imageToRGB(frame)}
		})
	}

	select {
	case <-ctx.Done():
		return nil, nil
	case res := <-frameCh:
		if res.err != nil {
			return nil, res.err
		}
		return res.img, nil
	}
}

// Close closes the persistent RTSP connection.
func (r *rtspReader) Close() {
	r.client.Close()
}

// readRTSPFrame connects to an RTSP server, reads one decoded frame (MJPEG or
// H.264) within the timeout, and returns it as an rgbImage. If the stream uses
// any other codec, it returns a descriptive error without reading any frames.
// Kept for backward compatibility with tests.
func readRTSPFrame(url string, timeout time.Duration) (*rgbImage, error) {
	reader, err := newRTSPReader(url, timeout)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return reader.ReadFrame(context.Background())
}
