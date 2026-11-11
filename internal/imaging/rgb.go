// Package imaging holds the pixel-level building blocks of the face pipeline:
// a packed 8-bit RGB image, conversions from the standard library image types,
// and JPEG encode/decode. It depends on the standard library only (plus the
// image format registrations) and imports nothing from this module's internal
// packages, so it can be used by any layer.
package imaging

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/webp"
)

// RGBImage is a packed 8-bit RGB image: three bytes per pixel, row-major, with
// no row padding. Alpha is dropped during conversion.
type RGBImage struct {
	W, H int
	Pix  []byte
}

// NewRGB returns a black W×H RGB image.
func NewRGB(w, h int) *RGBImage {
	if w <= 0 || h <= 0 {
		return &RGBImage{W: w, H: h}
	}
	return &RGBImage{W: w, H: h, Pix: make([]byte, w*h*3)}
}

// At returns the red, green and blue components of pixel (x, y). The caller
// must ensure 0 <= x < W and 0 <= y < H.
func (img *RGBImage) At(x, y int) (r, g, b uint8) {
	i := (y*img.W + x) * 3
	return img.Pix[i], img.Pix[i+1], img.Pix[i+2]
}

// ConvertInto writes src into dst, which must already be sized to src's bounds.
// It reports false without touching dst when dst is nil, when its dimensions do
// not match src.Bounds(), or when its Pix buffer is too small.
func ConvertInto(dst *RGBImage, src image.Image) bool {
	if dst == nil || src == nil {
		return false
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || dst.W != w || dst.H != h {
		return false
	}
	if len(dst.Pix) < w*h*3 {
		return false
	}

	switch s := src.(type) {
	case *image.YCbCr:
		i := 0
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				ci := s.COffset(x, y)
				r, g, bl := color.YCbCrToRGB(s.Y[s.YOffset(x, y)], s.Cb[ci], s.Cr[ci])
				dst.Pix[i] = r
				dst.Pix[i+1] = g
				dst.Pix[i+2] = bl
				i += 3
			}
		}
	case *image.RGBA:
		// RGBA is already alpha-premultiplied, so its bytes are exactly what
		// the generic At().RGBA()>>8 conversion produces.
		i := 0
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				j := s.PixOffset(x, y)
				dst.Pix[i] = s.Pix[j]
				dst.Pix[i+1] = s.Pix[j+1]
				dst.Pix[i+2] = s.Pix[j+2]
				i += 3
			}
		}
	case *image.NRGBA:
		i := 0
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				j := s.PixOffset(x, y)
				a := s.Pix[j+3]
				dst.Pix[i] = premultiply(s.Pix[j], a)
				dst.Pix[i+1] = premultiply(s.Pix[j+1], a)
				dst.Pix[i+2] = premultiply(s.Pix[j+2], a)
				i += 3
			}
		}
	case *image.Gray:
		i := 0
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				v := s.Pix[s.PixOffset(x, y)]
				dst.Pix[i] = v
				dst.Pix[i+1] = v
				dst.Pix[i+2] = v
				i += 3
			}
		}
	default:
		i := 0
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				r, g, bl, _ := src.At(x, y).RGBA()
				dst.Pix[i] = uint8(r >> 8)
				dst.Pix[i+1] = uint8(g >> 8)
				dst.Pix[i+2] = uint8(bl >> 8)
				i += 3
			}
		}
	}
	return true
}

// premultiply mirrors color.NRGBA.RGBA() followed by a >>8, so the NRGBA fast
// path stays byte-identical to the generic conversion for translucent pixels.
func premultiply(v, a uint8) uint8 {
	c := uint32(v)
	c |= c << 8
	c *= uint32(a)
	c /= 0xff
	return uint8(c >> 8)
}

// ToRGB converts src into a new RGBImage sized to src.Bounds().
func ToRGB(src image.Image) *RGBImage {
	b := src.Bounds()
	out := NewRGB(b.Dx(), b.Dy())
	ConvertInto(out, src)
	return out
}

// DecodeRGB decodes an encoded image (JPEG, PNG, WebP) into an RGBImage.
func DecodeRGB(data []byte) (*RGBImage, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("unable to decode image format: %w", err)
	}
	return ToRGB(img), nil
}

// EncodeJPEG encodes img as a JPEG with the encoder's default options (quality
// 75), matching the historical crop encoding.
func EncodeJPEG(img *RGBImage) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, rgbAdapter{img: img}, nil); err != nil {
		return nil, fmt.Errorf("encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}

// rgbAdapter exposes an RGBImage to image/jpeg without materialising an RGBA
// copy. Its At returns opaque, premultiplied RGBA, which is what the previous
// per-pixel RGBA round-trip produced.
type rgbAdapter struct {
	img *RGBImage
}

func (a rgbAdapter) ColorModel() color.Model { return color.RGBAModel }

func (a rgbAdapter) Bounds() image.Rectangle { return image.Rect(0, 0, a.img.W, a.img.H) }

func (a rgbAdapter) At(x, y int) color.Color {
	r, g, b := a.img.At(x, y)
	return color.RGBA{R: r, G: g, B: b, A: 255}
}
