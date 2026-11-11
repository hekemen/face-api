package service

import (
	"context"
	"fmt"
	"image"
	"math"
	"sync"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
	"h2hsecure.com/face/internal/domain"
	"h2hsecure.com/face/internal/imaging"
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
	rt      *ort.Runtime
	env     *ort.Env
	detS    *ort.Session
	recS    *ort.Session
	inferMu sync.Mutex
}

// NewFaceProcessor creates a new ONNX-based face processor.
func NewFaceProcessor(rt *ort.Runtime, env *ort.Env, detS, recS *ort.Session) *FaceProcessorImpl {
	return &FaceProcessorImpl{
		rt:   rt,
		env:  env,
		detS: detS,
		recS: recS,
	}
}

// DetectAndCrop runs face detection on an image and returns a 112x112 face crop.
func (p *FaceProcessorImpl) DetectAndCrop(data []byte) (*domain.FaceCrop, error) {
	img, err := imaging.DecodeRGB(data)
	if err != nil {
		return nil, err
	}

	cropImg, bbox, err := p.detectAndCrop112(img)
	if err != nil {
		return nil, err
	}

	// Encode crop to JPEG
	encoded, err := imaging.EncodeJPEG(cropImg)
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

func (p *FaceProcessorImpl) detectAndCrop112(img *imaging.RGBImage) (*imaging.RGBImage, [4]float32, error) {
	h, w := img.H, img.W

	canvas := imaging.NewRGB(detInputSize, detInputSize)
	scale := imaging.PadSquareInto(canvas, img)
	if scale == 0 {
		return nil, [4]float32{}, fmt.Errorf("prepare detection input: invalid scaled dimensions for %dx%d image", w, h)
	}

	blob := make([]float32, 3*detInputSize*detInputSize)
	if err := imaging.NCHWInto(blob, canvas, 128.0); err != nil {
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

	cropped := imaging.NewRGB(recInputSize, recInputSize)
	if err := imaging.CropInto(cropped, img, image.Rect(bestX1, bestY1, bestX2, bestY2)); err != nil {
		return nil, [4]float32{}, fmt.Errorf("crop face region: %w", err)
	}

	normalizedBbox := [4]float32{
		float32(bestX1) / float32(w),
		float32(bestY1) / float32(h),
		float32(bestX2) / float32(w),
		float32(bestY2) / float32(h),
	}

	return cropped, normalizedBbox, nil
}

func (p *FaceProcessorImpl) extractEmbedding(img *imaging.RGBImage) (domain.FaceEmbedding, error) {
	if img.W != recInputSize || img.H != recInputSize {
		return nil, fmt.Errorf("build embedding blob: expected %dx%dx3 image, got %dx%d",
			recInputSize, recInputSize, img.W, img.H)
	}

	blob := make([]float32, 3*recInputSize*recInputSize)
	if err := imaging.NCHWInto(blob, img, 127.5); err != nil {
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
	img, err := imaging.DecodeRGB(crop.Data)
	if err != nil {
		return nil, fmt.Errorf("decode crop: %w", err)
	}
	return p.extractEmbedding(img)
}

// round rounds to the nearest integer, halves away from zero.
func round(v float64) int {
	if v < 0 {
		return int(v - 0.5)
	}
	return int(v + 0.5)
}
