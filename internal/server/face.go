package server

import (
	"context"
	"fmt"
	"image"
	"math"
	"sync"

	ort "github.com/shota3506/onnxruntime-purego/onnxruntime"
	bolt "go.etcd.io/bbolt"
	"gocv.io/x/gocv"
)

const (
	detInputSize = 640 // SCRFD fixed inference size
	recInputSize = 112 // ArcFace inference size
	detThreshold = 0.5 // minimum raw face score to keep a detection

	detInputName  = "input.1"
	recInputName  = "input.1"
	recOutputName = "516"
)

// detLevel describes one SCRFD feature pyramid level: its stride and the
// ONNX output names for the classification scores and bounding-box offsets.
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

// FaceServer holds the face recognition state: in-memory cache, persistent
// bbolt store, and the two ONNX Runtime inference sessions.
type FaceServer struct {
	mu     sync.RWMutex
	dbMap  map[string][]float32 // In-memory cache for high-speed lookups
	boltDB *bolt.DB             // Persistent bbolt datastore

	ortRT   *ort.Runtime // ONNX Runtime handle (shared)
	ortEnv  *ort.Env     // ONNX Runtime environment
	detSess *ort.Session // SCRFD face detection session
	recSess *ort.Session // ArcFace recognition session

	inferMu sync.Mutex // serializes ONNX Runtime inference

	threshold float32 // Match confidence threshold (~0.45)
}

// resizePadToSquare resizes a BGR image so its longest side fits size,
// preserving aspect ratio, and places it centered on a size x size black
// canvas. It returns the canvas Mat and the applied scale factor so model
// coordinates can be mapped back to the original image.
func resizePadToSquare(mat gocv.Mat, size int) (gocv.Mat, float64, error) {
	h, w := mat.Rows(), mat.Cols()

	longest := h
	if w > longest {
		longest = w
	}
	scale := float64(size) / float64(longest)

	nw := int(math.Round(float64(w) * scale))
	nh := int(math.Round(float64(h) * scale))
	if nw <= 0 || nh <= 0 {
		return gocv.Mat{}, 0, fmt.Errorf("invalid scaled dimensions %dx%d", nw, nh)
	}

	resized := gocv.NewMat()
	gocv.Resize(mat, &resized, image.Pt(nw, nh), 0, 0, gocv.InterpolationLinear)

	if nw == size && nh == size {
		return resized, scale, nil
	}

	canvas := gocv.Zeros(size, size, gocv.MatTypeCV8UC3)
	roi := canvas.Region(image.Rect(0, 0, nw, nh))
	resized.CopyTo(&roi)
	roi.Close()
	resized.Close()
	return canvas, scale, nil
}

// matToNCHW converts a continuous size x size 3-channel BGR Mat into an NCHW
// float32 blob with RGB channel order, normalized as (pixel - 127.5) / div.
func matToNCHW(mat gocv.Mat, size int, div float32) ([]float32, error) {
	if mat.Rows() != size || mat.Cols() != size || mat.Channels() != 3 {
		return nil, fmt.Errorf("expected %dx%dx3 image, got %dx%dx%d",
			size, size, mat.Rows(), mat.Cols(), mat.Channels())
	}
	if !mat.IsContinuous() {
		return nil, fmt.Errorf("image mat is not continuous")
	}

	pix, err := mat.DataPtrUint8()
	if err != nil {
		return nil, fmt.Errorf("read mat data: %w", err)
	}
	if len(pix) != size*size*3 {
		return nil, fmt.Errorf("unexpected mat byte length %d (want %d)", len(pix), size*size*3)
	}

	data := make([]float32, 3*size*size)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			base := (y*size + x) * 3
			b := float32(pix[base+0])
			g := float32(pix[base+1])
			r := float32(pix[base+2])
			idx := y*size + x
			data[0*size*size+idx] = (r - 127.5) / div
			data[1*size*size+idx] = (g - 127.5) / div
			data[2*size*size+idx] = (b - 127.5) / div
		}
	}
	return data, nil
}

// runDetect feeds a 640x640 NCHW blob through the SCRFD session and returns
// every output tensor, copied into Go slices keyed by ONNX output name.
func (s *FaceServer) runDetect(blob []float32) (map[string][]float32, error) {
	s.inferMu.Lock()
	defer s.inferMu.Unlock()

	tensor, err := ort.NewTensorValue[float32](s.ortRT, blob, []int64{1, 3, detInputSize, detInputSize})
	if err != nil {
		return nil, fmt.Errorf("create detection input tensor: %w", err)
	}
	defer tensor.Close()

	outs, err := s.detSess.Run(context.Background(), map[string]*ort.Value{detInputName: tensor})
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

// runRecognize feeds a 112x112 NCHW blob through the ArcFace session and
// returns the 512-dimensional embedding vector.
func (s *FaceServer) runRecognize(blob []float32) ([]float32, error) {
	s.inferMu.Lock()
	defer s.inferMu.Unlock()

	tensor, err := ort.NewTensorValue[float32](s.ortRT, blob, []int64{1, 3, recInputSize, recInputSize})
	if err != nil {
		return nil, fmt.Errorf("create recognition input tensor: %w", err)
	}
	defer tensor.Close()

	outs, err := s.recSess.Run(context.Background(), map[string]*ort.Value{recInputName: tensor})
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
	return data, nil
}

// detectAndCrop112 runs SCRFD detection on the input frame, decodes bounding
// boxes with the insightface distance decode, clamps the best box to frame
// bounds, crops the ROI, and bilinearly resizes it to 112x112.
func (s *FaceServer) detectAndCrop112(mat gocv.Mat) (gocv.Mat, [4]float32, error) {
	h := mat.Rows()
	w := mat.Cols()

	canvas, scale, err := resizePadToSquare(mat, detInputSize)
	if err != nil {
		return gocv.Mat{}, [4]float32{}, fmt.Errorf("prepare detection input: %w", err)
	}
	defer canvas.Close()

	blob, err := matToNCHW(canvas, detInputSize, 128.0)
	if err != nil {
		return gocv.Mat{}, [4]float32{}, fmt.Errorf("build detection blob: %w", err)
	}

	outs, err := s.runDetect(blob)
	if err != nil {
		return gocv.Mat{}, [4]float32{}, err
	}

	bestScore := float32(-1)
	bestBox := [4]float32{}

	for _, lvl := range detLevels {
		scores, ok := outs[lvl.scoreName]
		if !ok {
			return gocv.Mat{}, [4]float32{}, fmt.Errorf("missing detection output %q", lvl.scoreName)
		}
		regs, ok := outs[lvl.regName]
		if !ok {
			return gocv.Mat{}, [4]float32{}, fmt.Errorf("missing detection output %q", lvl.regName)
		}

		hw := detInputSize / lvl.stride
		numAnchors := 2 * hw * hw
		if len(scores) != numAnchors || len(regs) != numAnchors*4 {
			return gocv.Mat{}, [4]float32{}, fmt.Errorf(
				"detection output size mismatch for stride %d: scores=%d regs=%d (want %d/%d)",
				lvl.stride, len(scores), len(regs), numAnchors, numAnchors*4)
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
		return gocv.Mat{}, [4]float32{}, fmt.Errorf("no face detected above confidence threshold %v", detThreshold)
	}

	// Map detection box from the 640 canvas back to original frame coordinates.
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
		return gocv.Mat{}, [4]float32{}, fmt.Errorf("invalid bounding box: x1=%d y1=%d x2=%d y2=%d (w=%d h=%d)", bestX1, bestY1, bestX2, bestY2, w, h)
	}

	roi := mat.Region(image.Rect(bestX1, bestY1, bestX2, bestY2))

	cropped := gocv.NewMat()
	gocv.Resize(roi, &cropped, image.Point{recInputSize, recInputSize}, 0, 0, gocv.InterpolationLinear)
	roi.Close()

	normalizedBbox := [4]float32{
		float32(bestX1) / float32(w),
		float32(bestY1) / float32(h),
		float32(bestX2) / float32(w),
		float32(bestY2) / float32(h),
	}

	return cropped, normalizedBbox, nil
}

// extractEmbedding feeds a 112x112 face image through the ArcFace model and
// returns a 512-dimensional embedding vector.
func (s *FaceServer) extractEmbedding(mat gocv.Mat) ([]float32, error) {
	blob, err := matToNCHW(mat, recInputSize, 127.5)
	if err != nil {
		return nil, fmt.Errorf("build embedding blob: %w", err)
	}
	return s.runRecognize(blob)
}

// cosineSimilarity computes the cosine similarity between two float32 vectors.
func cosineSimilarity(a, b []float32) float32 {
	var dot, normA, normB float32
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (float32(math.Sqrt(float64(normA))) * float32(math.Sqrt(float64(normB))))
}
