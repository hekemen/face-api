package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"math"
	"sync"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
	"h2hsecure.com/face/internal/domain"

	_ "golang.org/x/image/webp"
)

const (
	detInputSize = 640 // SCRFD fixed inference size
	recInputSize = 112 // ArcFace inference size
	detThreshold = 0.5 // minimum raw face score to keep a detection

	detInputName  = "input.1"
	recInputName  = "input.1"
	recOutputName = "516"
)

// detLevel describes one SCRFD feature pyramid level.
type detLevel struct {
	stride    int
	scoreName string
	regName   string
}

var detLevels = []detLevel{
	{stride: 8, scoreName: "443", regName: "446"},
	{stride: 16, scoreName: "468", regName: "471"},
	{stride: 32, scoreName: "493", regName: "496"},
}

// FaceProcessorImpl implements domain.FaceProcessor using ONNX Runtime.
type FaceProcessorImpl struct {
	rt    *ort.Runtime
	env   *ort.Env
	detS  *ort.Session
	recS  *ort.Session
	inferMu sync.Mutex
}

// NewFaceProcessor creates a new ONNX-based face processor.
func NewFaceProcessor(rt *ort.Runtime, env *ort.Env, detS, recS *ort.Session) *FaceProcessorImpl {
	return &FaceProcessorImpl{
		rt:    rt,
		env:   env,
		detS:  detS,
		recS:  recS,
	}
}

// DetectAndCrop runs face detection on an image and returns a 112x112 face crop.
func (p *FaceProcessorImpl) DetectAndCrop(data []byte) (*domain.FaceCrop, error) {
	img, err := decodeRGB(data)
	if err != nil {
		return nil, err
	}

	cropImg, bbox, err := p.detectAndCrop112(img)
	if err != nil {
		return nil, err
	}

	// Encode crop to JPEG
	encoded, err := encodeFaceJPEG(cropImg)
	if err != nil {
		return nil, fmt.Errorf("encode crop: %w", err)
	}

	// Extract embedding
	embedding, err := p.extractEmbedding(cropImg)
	if err != nil {
		return nil, fmt.Errorf("extract embedding: %w", err)
	}

	return &domain.FaceCrop{
		Data:      encoded,
		Embedding: embedding,
		BBox:      bbox,
	}, nil
}

// DetectAndCropFromRTSP reads one frame from an RTSP stream, detects a face,
// and returns the crop with embedding.
func (p *FaceProcessorImpl) DetectAndCropFromRTSP(rtspURL string) (*domain.FaceCrop, error) {
	// This is a placeholder — the actual RTSP reading is handled by the
	// HTTP handler / MQTT bridge. The processor only handles image → crop.
	// For now, return an error indicating the caller must provide image data.
	return nil, fmt.Errorf("use DetectAndCrop with image data; RTSP reading is done by the caller")
}

// --- Internal methods ---

func (p *FaceProcessorImpl) detectAndCrop112(img *rgbImage) (*rgbImage, [4]float32, error) {
	h, w := img.h, img.w

	canvas, scale, err := resizePadToSquare(img, detInputSize)
	if err != nil {
		return nil, [4]float32{}, fmt.Errorf("prepare detection input: %w", err)
	}

	blob, err := rgbToNCHW(canvas, detInputSize, 128.0)
	if err != nil {
		return nil, [4]float32{}, fmt.Errorf("build detection blob: %w", err)
	}

	outs, err := p.runDetect(blob)
	if err != nil {
		return nil, [4]float32{}, err
	}

	bestScore := float32(-1)
	bestBox := [4]float32{}

	for _, lvl := range detLevels {
		scores, ok := outs[lvl.scoreName]
		if !ok {
			return nil, [4]float32{}, fmt.Errorf("missing detection output %q", lvl.scoreName)
		}
		regs, ok := outs[lvl.regName]
		if !ok {
			return nil, [4]float32{}, fmt.Errorf("missing detection output %q", lvl.regName)
		}

		hw := detInputSize / lvl.stride
		numAnchors := 2 * hw * hw
		if len(scores) != numAnchors || len(regs) != numAnchors*4 {
			return nil, [4]float32{}, fmt.Errorf(
				"detection output size mismatch for stride %d: scores=%d regs=%d",
				lvl.stride, len(scores), len(regs))
		}

		stride := float32(lvl.stride)
		for i := 0; i < numAnchors; i++ {
			conf := scores[i]
			if conf < detThreshold {
				continue
			}

			cell := i / 2
			row := cell / hw
			col := cell % hw
			cx := float32(col * lvl.stride)
			cy := float32(row * lvl.stride)

			x1 := cx - regs[i*4+0]*stride
			y1 := cy - regs[i*4+1]*stride
			x2 := cx + regs[i*4+2]*stride
			y2 := cy + regs[i*4+3]*stride

			if conf > bestScore {
				bestScore = conf
				bestBox = [4]float32{x1, y1, x2, y2}
			}
		}
	}

	if bestScore < detThreshold {
		return nil, [4]float32{}, fmt.Errorf("no face detected above confidence threshold %v", detThreshold)
	}

	// Map back to original frame
	bestX1 := int(math.Round(float64(bestBox[0]) / scale))
	bestY1 := int(math.Round(float64(bestBox[1]) / scale))
	bestX2 := int(math.Round(float64(bestBox[2]) / scale))
	bestY2 := int(math.Round(float64(bestBox[3]) / scale))

	if bestX1 < 0 {
		bestX1 = 0
	}
	if bestY1 < 0 {
		bestY1 = 0
	}
	if bestX2 > w {
		bestX2 = w
	}
	if bestY2 > h {
		bestY2 = h
	}

	if bestX2 <= bestX1 || bestY2 <= bestY1 {
		return nil, [4]float32{}, fmt.Errorf("invalid bounding box: x1=%d y1=%d x2=%d y2=%d", bestX1, bestY1, bestX2, bestY2)
	}

	cropped, err := cropRegion(img, image.Rect(bestX1, bestY1, bestX2, bestY2))
	if err != nil {
		return nil, [4]float32{}, fmt.Errorf("crop face region: %w", err)
	}

	cropped = bilinearResize(cropped, recInputSize, recInputSize)

	normalizedBbox := [4]float32{
		float32(bestX1) / float32(w),
		float32(bestY1) / float32(h),
		float32(bestX2) / float32(w),
		float32(bestY2) / float32(h),
	}

	return cropped, normalizedBbox, nil
}

func (p *FaceProcessorImpl) extractEmbedding(img *rgbImage) (domain.FaceEmbedding, error) {
	blob, err := rgbToNCHW(img, recInputSize, 127.5)
	if err != nil {
		return nil, fmt.Errorf("build embedding blob: %w", err)
	}
	return p.runRecognize(blob)
}

func (p *FaceProcessorImpl) runDetect(blob []float32) (map[string][]float32, error) {
	p.inferMu.Lock()
	defer p.inferMu.Unlock()

	tensor, err := ort.NewTensorValue[float32](p.rt, blob, []int64{1, 3, detInputSize, detInputSize})
	if err != nil {
		return nil, fmt.Errorf("create detection input tensor: %w", err)
	}
	defer tensor.Close()

	outs, err := p.detS.Run(context.Background(), map[string]*ort.Value{detInputName: tensor})
	if err != nil {
		return nil, fmt.Errorf("run detection session: %w", err)
	}

	res := make(map[string][]float32, len(outs))
	for name, v := range outs {
		data, _, err := ort.GetTensorData[float32](v)
		v.Close()
		if err != nil {
			return nil, fmt.Errorf("read detection output %q: %w", name, err)
		}
		res[name] = data
	}
	return res, nil
}

func (p *FaceProcessorImpl) runRecognize(blob []float32) (domain.FaceEmbedding, error) {
	p.inferMu.Lock()
	defer p.inferMu.Unlock()

	tensor, err := ort.NewTensorValue[float32](p.rt, blob, []int64{1, 3, recInputSize, recInputSize})
	if err != nil {
		return nil, fmt.Errorf("create recognition input tensor: %w", err)
	}
	defer tensor.Close()

	outs, err := p.recS.Run(context.Background(), map[string]*ort.Value{recInputName: tensor})
	if err != nil {
		return nil, fmt.Errorf("run recognition session: %w", err)
	}
	defer func() {
		for _, v := range outs {
			v.Close()
		}
	}()

	v, ok := outs[recOutputName]
	if !ok {
		return nil, fmt.Errorf("recognition session did not return output %q", recOutputName)
	}

	data, _, err := ort.GetTensorData[float32](v)
	if err != nil {
		return nil, fmt.Errorf("read recognition output: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("recognition output is empty")
	}
	return domain.FaceEmbedding(data), nil
}

// ExtractEmbedding extracts a face embedding from a FaceCrop.
// If the crop already contains an embedding, it is returned directly.
// Otherwise the JPEG data is decoded and recognition is run.
func (p *FaceProcessorImpl) ExtractEmbedding(crop *domain.FaceCrop) (domain.FaceEmbedding, error) {
	if crop != nil && len(crop.Embedding) > 0 {
		return crop.Embedding, nil
	}
	if crop == nil {
		return nil, fmt.Errorf("crop is nil")
	}
	img, err := decodeRGB(crop.Data)
	if err != nil {
		return nil, fmt.Errorf("decode crop: %w", err)
	}
	return p.extractEmbedding(img)
}

// --- Image helpers (extracted from internal/server/img.go) ---

// rgbImage is a packed 8-bit RGB (3 bytes per pixel, row-major) image.
type rgbImage struct {
	w, h int
	pix  []uint8
}

func decodeRGB(data []byte) (*rgbImage, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("unable to decode image format: %w", err)
	}
	return imageToRGB(img), nil
}

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

func encodeFaceJPEG(img *rgbImage) ([]byte, error) {
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

func encodeFaceToBase64(img *rgbImage) (string, error) {
	data, err := encodeFaceJPEG(img)
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
