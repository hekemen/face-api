package server

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // register PNG decode for image.Decode

	_ "golang.org/x/image/webp" // register WebP decode for image.Decode
)

// rgbImage is a packed 8-bit RGB (3 bytes per pixel, row-major) image that
// replaces gocv.Mat for the pure-Go image pipeline.
type rgbImage struct {
	w, h int
	pix  []uint8
}

// decodeRGB decodes a JPEG/PNG/WebP byte buffer into an rgbImage.
func decodeRGB(data []byte) (*rgbImage, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("unable to decode image format: %w", err)
	}
	return imageToRGB(img), nil
}

// imageToRGB converts any image.Image into a packed RGB rgbImage.
func imageToRGB(src image.Image) *rgbImage {
	b := src.Bounds()
	out := &rgbImage{w: b.Dx(), h: b.Dy(), pix: make([]uint8, b.Dx()*b.Dy()*3)}
	i := 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := src.At(x, y).RGBA()
			out.pix[i] = uint8(r >> 8)
			out.pix[i+1] = uint8(g >> 8)
			out.pix[i+2] = uint8(bl >> 8)
			i += 3
		}
	}
	return out
}

// resizePadToSquare resizes an image so its longest side fits size,
// preserving aspect ratio, and places it at the top-left of a size x size
// black canvas. It returns the canvas and the applied scale factor so model
// coordinates can be mapped back to the original image.
func resizePadToSquare(img *rgbImage, size int) (*rgbImage, float64, error) {
	longest := img.h
	if img.w > longest {
		longest = img.w
	}
	scale := float64(size) / float64(longest)

	nw := int(round(float64(img.w) * scale))
	nh := int(round(float64(img.h) * scale))
	if nw <= 0 || nh <= 0 {
		return nil, 0, fmt.Errorf("invalid scaled dimensions %dx%d", nw, nh)
	}

	resized := bilinearResize(img, nw, nh)

	if nw == size && nh == size {
		return resized, scale, nil
	}

	canvas := &rgbImage{w: size, h: size, pix: make([]uint8, size*size*3)}
	for y := 0; y < nh; y++ {
		off := y * size * 3
		copy(canvas.pix[off:off+nw*3], resized.pix[y*nw*3:(y+1)*nw*3])
	}
	return canvas, scale, nil
}

// bilinearResize resizes img to w x h using bilinear interpolation.
func bilinearResize(src *rgbImage, w, h int) *rgbImage {
	if w <= 0 || h <= 0 {
		return &rgbImage{w: w, h: h}
	}
	scaleX := float64(src.w) / float64(w)
	scaleY := float64(src.h) / float64(h)

	out := &rgbImage{w: w, h: h, pix: make([]uint8, w*h*3)}
	for y := 0; y < h; y++ {
		srcY := float64(y) * scaleY
		y0 := int(srcY)
		if y0 > src.h-1 {
			y0 = src.h - 1
		}
		y1 := y0 + 1
		if y1 > src.h-1 {
			y1 = src.h - 1
		}
		fy := srcY - float64(y0)

		for x := 0; x < w; x++ {
			srcX := float64(x) * scaleX
			x0 := int(srcX)
			if x0 > src.w-1 {
				x0 = src.w - 1
			}
			x1 := x0 + 1
			if x1 > src.w-1 {
				x1 = src.w - 1
			}
			fx := srcX - float64(x0)

			for c := 0; c < 3; c++ {
				v00 := float64(src.pix[(y0*src.w+x0)*3+c])
				v01 := float64(src.pix[(y0*src.w+x1)*3+c])
				v10 := float64(src.pix[(y1*src.w+x0)*3+c])
				v11 := float64(src.pix[(y1*src.w+x1)*3+c])

				top := v00 + (v01-v00)*fx
				bot := v10 + (v11-v10)*fx
				out.pix[(y*w+x)*3+c] = uint8(top + (bot-top)*fy)
			}
		}
	}
	return out
}

// cropRegion returns a new rgbImage for the given rectangle within img.
func cropRegion(img *rgbImage, r image.Rectangle) (*rgbImage, error) {
	r = r.Intersect(image.Rect(0, 0, img.w, img.h))
	if r.Dx() <= 0 || r.Dy() <= 0 {
		return nil, fmt.Errorf("invalid crop region %v", r)
	}
	out := &rgbImage{w: r.Dx(), h: r.Dy(), pix: make([]uint8, r.Dx()*r.Dy()*3)}
	i := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		start := (y*img.w + r.Min.X) * 3
		copy(out.pix[i:i+r.Dx()*3], img.pix[start:start+r.Dx()*3])
		i += r.Dx() * 3
	}
	return out, nil
}

// rgbToNCHW converts a size x size 3-channel RGB image into an NCHW float32
// blob, normalized as (pixel - 127.5) / div.
func rgbToNCHW(img *rgbImage, size int, div float32) ([]float32, error) {
	if img.w != size || img.h != size {
		return nil, fmt.Errorf("expected %dx%dx3 image, got %dx%d", size, size, img.w, img.h)
	}

	data := make([]float32, 3*size*size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			base := (y*size + x) * 3
			r := float32(img.pix[base+0])
			g := float32(img.pix[base+1])
			b := float32(img.pix[base+2])
			idx := y*size + x
			data[0*size*size+idx] = (r - 127.5) / div
			data[1*size*size+idx] = (g - 127.5) / div
			data[2*size*size+idx] = (b - 127.5) / div
		}
	}
	return data, nil
}

// encodeJPEG encodes an rgbImage to a JPEG byte slice.
func encodeJPEG(img *rgbImage) ([]byte, error) {
	dst := image.NewRGBA(image.Rect(0, 0, img.w, img.h))
	for y := 0; y < img.h; y++ {
		for x := 0; x < img.w; x++ {
			i := (y*img.w + x) * 3
			dst.Set(x, y, color.NRGBA{R: img.pix[i], G: img.pix[i+1], B: img.pix[i+2], A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, nil); err != nil {
		return nil, fmt.Errorf("encode face image: %w", err)
	}
	return buf.Bytes(), nil
}

// encodeFaceToBase64 encodes an rgbImage to a base64-encoded JPEG string.
func encodeFaceToBase64(img *rgbImage) (string, error) {
	data, err := encodeJPEG(img)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func round(v float64) int {
	if v < 0 {
		return int(v - 0.5)
	}
	return int(v + 0.5)
}
