package service

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"sync"

	"golang.org/x/image/draw"
	"h2hsecure.com/face/internal/domain"
)

// thumbnailSizeThreshold is the base64 length above which an audit
// FaceImage is considered large. A 112x112 face crop encodes to roughly
// 2.2 KB of base64, well under this; full-resolution scans are not.
const thumbnailSizeThreshold = 8192

// thumbMaxDim is the maximum dimension of a stored or generated thumbnail.
const thumbMaxDim = 96

// thumbnailFromBase64 decodes a base64-encoded image, resizes it so its
// longest dimension is <= maxDim (aspect ratio preserved), and re-encodes
// it as a base64 JPEG. It is a pure primitive with no skip rules: callers
// decide which images to thumbnailize.
//
// The source is sampled directly while scaling down, so the cost is
// proportional to the destination size rather than the source's full
// resolution. Returns an error if the input is not valid base64 or not a
// decodable image.
func thumbnailFromBase64(b64 string, maxDim int) (string, error) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	longest := w
	if h > longest {
		longest = h
	}
	out := src
	if longest > maxDim {
		scale := float64(maxDim) / float64(longest)
		nw, nh := round(float64(w)*scale), round(float64(h)*scale)
		if nw < 1 {
			nw = 1
		}
		if nh < 1 {
			nh = 1
		}
		dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
		draw.BiLinear.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
		out = dst
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, nil); err != nil {
		return "", fmt.Errorf("encode jpeg: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// thumbnailForEntry returns a stored-face thumbnail for a base64 audit
// image that is large enough to slow down list responses, or "" for
// small/empty images and on any decode failure. Used at write time so the
// conversion happens once per scan instead of on every list read.
func thumbnailForEntry(b64 string) string {
	if len(b64) <= thumbnailSizeThreshold {
		return ""
	}
	thumb, err := thumbnailFromBase64(b64, thumbMaxDim)
	if err != nil {
		return ""
	}
	return thumb
}

// applyThumbnails replaces the face image of each list entry with a small
// thumbnail. Entries with a stored FaceThumb are swapped in without any
// decoding; entries with a large FaceImage but no stored thumb (legacy data
// that missed the startup backfill) are thumbnailized on the fly by a
// bounded two-worker pool. Small images and images that fail to decode are
// left untouched.
func applyThumbnails(entries []domain.AuditEntry, maxDim int) {
	var jobs []int
	for i := range entries {
		e := &entries[i]
		if e.FaceImage == "" {
			continue
		}
		if e.FaceThumb != "" {
			e.FaceImage = e.FaceThumb
			e.FaceThumb = ""
			continue
		}
		if len(e.FaceImage) <= thumbnailSizeThreshold {
			continue
		}
		jobs = append(jobs, i)
	}
	if len(jobs) == 0 {
		return
	}
	ch := make(chan int, len(jobs))
	for _, i := range jobs {
		ch <- i
	}
	close(ch)
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ch {
				e := &entries[i]
				if thumb, err := thumbnailFromBase64(e.FaceImage, maxDim); err == nil {
					e.FaceImage = thumb
				}
			}
		}()
	}
	wg.Wait()
}
