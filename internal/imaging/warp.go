package imaging

import "math"

// This file aligns a detected face to the ArcFace template: a 4-DOF similarity
// fit (Umeyama) over five keypoints followed by an inverse-mapped, bilinearly
// sampled warp into a preallocated destination. Like the rest of the package it
// is allocation-free, so a per-frame loop can reuse its buffers.
//
// Coordinates are pixel-index coordinates throughout, which is the convention
// ArcFaceRefPoints has always been quoted in (the same one the detector reports
// keypoints in and cv2.warpAffine samples with): the centre of the pixel with
// index x sits at coordinate x, so a destination pixel (x, y) is inverse-mapped
// from exactly (x, y). Every sample is clamped to the source's edges.

// ArcFaceRefPoints is the canonical 5-point ArcFace template for a 112×112
// crop, in pixel-index coordinates: left eye, right eye, nose, left mouth
// corner, right mouth corner.
var ArcFaceRefPoints = [5][2]float32{{38.2946, 51.6963}, {73.5318, 51.5014}, {56.0252, 71.7366}, {41.5493, 92.3655}, {70.7299, 92.2041}}

const (
	// similarityRankEps rejects point sets whose covariance is rank-deficient
	// (collinear points): the smaller singular value is then zero up to
	// round-off, and the rotation is not determined by the data.
	similarityRankEps = 1e-9
	// similarityMinDet is the smallest determinant of the linear part that
	// Warp5Into will invert; below it the inverse mapping is ill-conditioned.
	similarityMinDet = 1e-6
)

// SimilarityFromPoints fits the 4-DOF similarity transform that maps src onto
// dst in the least-squares sense:
//
//	dst ≈ [[a, b], [c, d]]·src + [tx, ty]   =>   m = {a, b, tx, c, d, ty}
//
// It is Umeyama's closed form specialised to 2-D: with the centred points s',
// d' and the cross-covariance H = Σ d'·s'ᵀ = [[h00,h01],[h10,h11]], the
// singular values follow from
//
//	p = (h00+h11)/2, q = (h10-h01)/2, r = (h00-h11)/2, s = (h01+h10)/2
//	σmax = hypot(p,q) + hypot(r,s),  σmin = hypot(p,q) - hypot(r,s)
//
// and the optimal rotation is R(atan2(q, p)). Umeyama's det(U·Vᵀ) correction is
// implicit: when σmin is negative it flips both the singular value's sign and
// the correction, and the rotation comes out as the same R(atan2(q, p)).
//
// ok is false when the fit is unusable: a non-finite sample, no spread in src
// (var_s == 0), a rank-deficient covariance (|σmin| ≤ similarityRankEps·σmax,
// i.e. collinear points), or a singular/non-finite linear part
// (|det| < similarityMinDet), which the inverse map could not undo.
func SimilarityFromPoints(src, dst [5][2]float64) (m [6]float64, ok bool) {
	var meanS, meanD [2]float64
	for _, p := range src {
		if !finite(p[0]) || !finite(p[1]) {
			return m, false
		}
		meanS[0] += p[0]
		meanS[1] += p[1]
	}
	for _, p := range dst {
		if !finite(p[0]) || !finite(p[1]) {
			return m, false
		}
		meanD[0] += p[0]
		meanD[1] += p[1]
	}
	meanS[0] /= 5
	meanS[1] /= 5
	meanD[0] /= 5
	meanD[1] /= 5

	var varS, h00, h01, h10, h11 float64
	for i := range src {
		sx := src[i][0] - meanS[0]
		sy := src[i][1] - meanS[1]
		dx := dst[i][0] - meanD[0]
		dy := dst[i][1] - meanD[1]

		varS += sx*sx + sy*sy
		h00 += dx * sx
		h01 += dx * sy
		h10 += dy * sx
		h11 += dy * sy
	}
	if !finite(varS) || varS <= 0 {
		return m, false
	}

	p := (h00 + h11) / 2
	q := (h10 - h01) / 2
	r := (h00 - h11) / 2
	s := (h01 + h10) / 2
	big := math.Hypot(p, q)
	small := math.Hypot(r, s)
	sigmaMax := big + small
	sigmaMin := big - small
	if !finite(sigmaMax) || sigmaMax <= 0 || math.Abs(sigmaMin) <= similarityRankEps*sigmaMax {
		return m, false
	}

	angle := math.Atan2(q, p)
	// Umeyama's scale is trace(Σ·diag(1, sign(det(U·Vᵀ))))/var_s, which is
	// σmax+σmin (= 2·big) whichever sign the second singular value takes.
	scale := (sigmaMax + sigmaMin) / varS
	cos, sin := math.Cos(angle), math.Sin(angle)

	m[0] = scale * cos
	m[1] = -scale * sin
	m[3] = scale * sin
	m[4] = scale * cos
	m[2] = meanD[0] - (m[0]*meanS[0] + m[1]*meanS[1])
	m[5] = meanD[1] - (m[3]*meanS[0] + m[4]*meanS[1])

	det := m[0]*m[4] - m[1]*m[3]
	if !finite(det) || math.Abs(det) < similarityMinDet {
		return m, false
	}
	for _, v := range m {
		if !finite(v) {
			return m, false
		}
	}
	return m, true
}

// Warp5Into aligns src to the ArcFace template and writes the 112×112 (or any
// other preallocated size) result into dst: it fits the similarity transform
// that maps the detected keypoints kps onto ArcFaceRefPoints and fills dst by
// inverse-mapping every destination pixel into src and sampling it bilinearly,
// clamping to the source edges. dst must not alias src.
//
// It reports false and leaves dst untouched when the fit is unusable (see
// SimilarityFromPoints) or either image is nil, empty, or undersized, so a
// caller can fall back to a bounding-box crop.
func Warp5Into(dst, src *RGBImage, kps [5][2]float32) bool {
	if !usable(dst) || !usable(src) {
		return false
	}

	var srcPts [5][2]float64
	for i, p := range kps {
		srcPts[i][0] = float64(p[0])
		srcPts[i][1] = float64(p[1])
	}
	var dstPts [5][2]float64
	for i, p := range ArcFaceRefPoints {
		dstPts[i][0] = float64(p[0])
		dstPts[i][1] = float64(p[1])
	}

	m, ok := SimilarityFromPoints(srcPts, dstPts)
	if !ok {
		return false
	}

	// Inverse of the forward linear part, then of its translation.
	det := m[0]*m[4] - m[1]*m[3]
	if !finite(det) || math.Abs(det) < similarityMinDet {
		return false
	}
	ia := m[4] / det
	ib := -m[1] / det
	ic := -m[3] / det
	id := m[0] / det
	itx := -(ia*m[2] + ib*m[5])
	ity := -(ic*m[2] + id*m[5])
	if !finite(ia) || !finite(ib) || !finite(ic) || !finite(id) || !finite(itx) || !finite(ity) {
		return false
	}

	maxX := float64(src.W - 1)
	maxY := float64(src.H - 1)
	stride := src.W * 3

	for y := 0; y < dst.H; y++ {
		fy := float64(y)
		outRow := y * dst.W * 3
		for x := 0; x < dst.W; x++ {
			out := outRow + x*3
			fx := float64(x)

			u := ia*fx + ib*fy + itx
			v := ic*fx + id*fy + ity
			if !finite(u) || !finite(v) {
				// Unreachable for a validated transform, but never let a NaN
				// reach dst.Pix.
				dst.Pix[out], dst.Pix[out+1], dst.Pix[out+2] = 0, 0, 0
				continue
			}
			u = math.Min(math.Max(u, 0), maxX)
			v = math.Min(math.Max(v, 0), maxY)

			x0 := int(u) // u >= 0, so truncation is floor
			y0 := int(v)
			x1, y1 := x0+1, y0+1
			if x1 > src.W-1 {
				x1 = src.W - 1
			}
			if y1 > src.H-1 {
				y1 = src.H - 1
			}
			ax := u - float64(x0)
			ay := v - float64(y0)

			row0 := y0 * stride
			row1 := y1 * stride
			col0 := x0 * 3
			col1 := x1 * 3
			for c := 0; c < 3; c++ {
				v00 := float64(src.Pix[row0+col0+c])
				v01 := float64(src.Pix[row0+col1+c])
				v10 := float64(src.Pix[row1+col0+c])
				v11 := float64(src.Pix[row1+col1+c])

				top := v00 + (v01-v00)*ax
				bot := v10 + (v11-v10)*ax
				// Round rather than truncate: an identity warp must reproduce
				// src bit for bit, and truncation would drop one LSB whenever
				// the sampled position lands a sub-ULP below the pixel centre.
				dst.Pix[out+c] = uint8(math.Round(top + (bot-top)*ay))
			}
		}
	}
	return true
}

// finite reports whether v is neither NaN nor an infinity.
func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
