package service

import (
	"context"
	"fmt"
	"image"
	"sync"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"

	"h2hsecure.com/face/internal/config"
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
	kpsName   string // keypoints output (449/474/499), wired above
}

var detLevels = []detLevel{
	{stride: 8, scoreName: "443", regName: "446", kpsName: "449"},
	{stride: 16, scoreName: "468", regName: "471", kpsName: "474"},
	{stride: 32, scoreName: "493", regName: "496", kpsName: "499"},
}

// FaceProcessorImpl implements domain.FaceProcessor using ONNX Runtime.
//
// All scratch buffers and both ONNX input tensors are allocated once at
// construction and reused for every inference. Because ONNX Runtime sessions
// are not thread-safe, pipelineMu is held for the whole detect+align+embed
// pipeline; callers must not hold it across a burst of inferences (the burst
// releases it between samples, so HTTP is never blocked for the whole window).
type FaceProcessorImpl struct {
	rt         *ort.Runtime
	env        *ort.Env
	detS       *ort.Session
	recS       *ort.Session
	align      AlignmentMode // crop strategy: bbox (default) or arcface5
	pipelineMu sync.Mutex    // guards scratch + both sessions

	canvas  *imaging.RGBImage // 640×640×3 detection canvas (top-left pad target)
	detBlob []float32         // 3×640×640 detection input blob
	detTens *ort.Value        // wraps detBlob; created once, reused every detection Run
	crop    *imaging.RGBImage // 112×112×3 recognition crop target
	recBlob []float32         // 3×112×112 recognition input blob
	recTens *ort.Value        // wraps recBlob; created once, reused every recognition Run
}

// AlignmentMode selects the face-crop alignment strategy. Bbox (the default)
// reproduces today's embeddings exactly; arcface5 is opt-in and requires
// re-enrollment because it produces a different embedding space.
type AlignmentMode string

const (
	AlignmentBBox     AlignmentMode = "bbox"     // raw bounding-box crop + resize (default)
	AlignmentArcFace5 AlignmentMode = "arcface5" // 5-point similarity warp to the ArcFace template
)

// ParseAlignment parses the FACE_ALIGNMENT env value, delegating to
// config.Alignment (the single source of truth for that validation).
func ParseAlignment(raw string) (AlignmentMode, error) {
	s, err := config.Alignment(func(string) string { return raw })
	if err != nil {
		return "", err
	}
	return AlignmentMode(s), nil
}

// NewFaceProcessor creates a new ONNX-based face processor, eagerly
// allocating one canvas, one blob and one input tensor per session. The
// tensors wrap the Go slices zero-copy and are reused on every Run. The align
// argument selects the crop strategy; it is stored and used by alignCrop.
func NewFaceProcessor(rt *ort.Runtime, env *ort.Env, detS, recS *ort.Session, align AlignmentMode) (*FaceProcessorImpl, error) {
	p := &FaceProcessorImpl{
		rt:      rt,
		env:     env,
		detS:    detS,
		recS:    recS,
		align:   align,
		canvas:  imaging.NewRGB(detInputSize, detInputSize),
		crop:    imaging.NewRGB(recInputSize, recInputSize),
		detBlob: make([]float32, 3*detInputSize*detInputSize),
		recBlob: make([]float32, 3*recInputSize*recInputSize),
	}

	var err error
	p.detTens, err = ort.NewTensorValue[float32](rt, p.detBlob, []int64{1, 3, detInputSize, detInputSize})
	if err != nil {
		return nil, fmt.Errorf("create detection input tensor: %w", err)
	}
	p.recTens, err = ort.NewTensorValue[float32](rt, p.recBlob, []int64{1, 3, recInputSize, recInputSize})
	if err != nil {
		p.detTens.Close()
		return nil, fmt.Errorf("create recognition input tensor: %w", err)
	}
	return p, nil
}

// Close releases the preallocated ONNX input tensors. It is safe to call
// more than once.
func (p *FaceProcessorImpl) Close() {
	p.detTens.Close()
	p.recTens.Close()
}

// DetectAndCrop runs face detection on an image and returns a 112x112 face
// crop. The second return value is the bounding box normalized to [0,1] in
// (x1, y1, x2, y2) order.
func (p *FaceProcessorImpl) DetectAndCrop(data []byte) (*domain.FaceCrop, error) {
	img, err := imaging.DecodeRGB(data)
	if err != nil {
		return nil, err
	}
	return p.DetectAndCropFrame(img.W, img.H, img.Pix)
}

// DetectAndCropFromRTSP reads one frame from an RTSP stream, detects a face,
// and returns the crop with embedding.
//
// This is a placeholder — the actual RTSP reading is handled by the HTTP
// handler / MQTT bridge. The processor only handles image → crop.
func (p *FaceProcessorImpl) DetectAndCropFromRTSP(rtspURL string) (*domain.FaceCrop, error) {
	return nil, fmt.Errorf("use DetectAndCrop with image data; RTSP reading is done by the caller")
}

// DetectAndCropFrame runs detection + alignment + recognition on a decoded
// RGB frame (len(pix) >= w*h*3), reusing the preallocated canvas, blobs and
// input tensors. pipelineMu is held for the whole pipeline.
func (p *FaceProcessorImpl) DetectAndCropFrame(w, h int, pix []byte) (*domain.FaceCrop, error) {
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("invalid frame dimensions %dx%d", w, h)
	}

	p.pipelineMu.Lock()
	defer p.pipelineMu.Unlock()

	img := &imaging.RGBImage{W: w, H: h, Pix: pix}

	hit, err := p.detectAndCrop112(img)
	if err != nil {
		return nil, err
	}

	// Align + crop the winning box into the preallocated 112×112 recognition
	// target. p.crop is reused across frames; alignCrop overwrites every pixel
	// of it, so no stale data survives between calls. Bbox mode (default) is
	// byte-identical to the pre-Task-8 pipeline; arcface5 is opt-in and kept
	// out of the stored pictures path.
	p.alignCrop(p.crop, img, hit)

	// Encode crop to JPEG.
	encoded, err := imaging.EncodeJPEG(p.crop)
	if err != nil {
		return nil, fmt.Errorf("encode crop: %w", err)
	}

	// Extract embedding from the preallocated recognition blob.
	if err := imaging.NCHWInto(p.recBlob, p.crop, 127.5); err != nil {
		return nil, fmt.Errorf("build embedding blob: %w", err)
	}
	embedding, err := p.runRecognize()
	if err != nil {
		return nil, fmt.Errorf("extract embedding: %w", err)
	}

	normalizedBbox := [4]float32{
		hit.Box[0] / float32(w),
		hit.Box[1] / float32(h),
		hit.Box[2] / float32(w),
		hit.Box[3] / float32(h),
	}
	return &domain.FaceCrop{
		Data:      encoded,
		Embedding: embedding,
		BBox:      normalizedBbox,
	}, nil
}

// --- Internal methods ---

// detectAndCrop112 pads img into the preallocated 640×640 canvas, runs the
// SCRFD detector over the preallocated detection blob, decodes the best
// detection via decodeBestFace and returns the hit. Caller must hold
// pipelineMu.
func (p *FaceProcessorImpl) detectAndCrop112(img *imaging.RGBImage) (*faceHit, error) {
	w, h := img.W, img.H

	scale := imaging.PadSquareInto(p.canvas, img)
	if scale == 0 {
		return nil, fmt.Errorf("prepare detection input: invalid scaled dimensions for %dx%d image", w, h)
	}

	if err := imaging.NCHWInto(p.detBlob, p.canvas, 128.0); err != nil {
		return nil, fmt.Errorf("build detection blob: %w", err)
	}

	outs, err := p.runDetect()
	if err != nil {
		return nil, err
	}

	// Convert the named output map into the structured detOutputs that
	// decodeBestFace consumes, preserving the stride→level order.
	var levels detOutputs
	for i, lvl := range detLevels {
		levels[i] = detLevelOutputs{
			Scores: outs[lvl.scoreName],
			Boxes:  outs[lvl.regName],
			KPs:    outs[lvl.kpsName], // 449/474/499
		}
	}

	hit, err := decodeBestFace(levels, scale, w, h)
	if err != nil {
		return nil, err
	}
	return hit, nil
}

// alignCrop writes the aligned face into dst (preallocated 112×112) and
// reports whether the arcface5 warp was used. Bbox mode (default) and any
// degenerate-warp case fall through to the bbox crop, so default embeddings
// stay byte-identical to today's pipeline. Caller must hold pipelineMu.
func (p *FaceProcessorImpl) alignCrop(dst, img *imaging.RGBImage, hit *faceHit) (usedWarp bool) {
	if p.align == AlignmentArcFace5 && hit.HasKPs {
		if imaging.Warp5Into(dst, img, hit.KPs) {
			return true
		}
		// Degenerate transform (collinear / non-finite landmarks) — fall back
		// to the bbox crop for this frame.
	}

	// hit.Box holds integral pixel coords (decodeBestFace rounds them), so the
	// conversion is exact. CropInto resamples the box region straight into the
	// 112×112 dst — byte-identical to the pre-Task-8 pipeline.
	box := image.Rect(
		int(hit.Box[0]),
		int(hit.Box[1]),
		int(hit.Box[2]),
		int(hit.Box[3]),
	)
	if err := imaging.CropInto(dst, img, box); err != nil {
		// CropInto errors only on an empty clip; decodeBestFace already
		// rejected zero-area boxes, so this is defensive.
		return false
	}
	return false
}

// runDetect runs SCRFD over the preallocated detection input tensor.
// Caller must hold pipelineMu.
func (p *FaceProcessorImpl) runDetect() (map[string][]float32, error) {
	outs, err := p.detS.Run(context.Background(), map[string]*ort.Value{detInputName: p.detTens})
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

// runRecognize runs ArcFace over the preallocated recognition input tensor.
// Caller must hold pipelineMu.
func (p *FaceProcessorImpl) runRecognize() (domain.FaceEmbedding, error) {
	outs, err := p.recS.Run(context.Background(), map[string]*ort.Value{recInputName: p.recTens})
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
	p.pipelineMu.Lock()
	defer p.pipelineMu.Unlock()
	if img.W != recInputSize || img.H != recInputSize {
		return nil, fmt.Errorf("build embedding blob: expected %dx%dx3 image, got %dx%d",
			recInputSize, recInputSize, img.W, img.H)
	}
	if err := imaging.NCHWInto(p.recBlob, img, 127.5); err != nil {
		return nil, fmt.Errorf("build embedding blob: %w", err)
	}
	return p.runRecognize()
}

// round rounds to the nearest integer, halves away from zero.
func round(v float64) int {
	if v < 0 {
		return int(v - 0.5)
	}
	return int(v + 0.5)
}
