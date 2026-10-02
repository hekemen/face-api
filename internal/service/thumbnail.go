package service

import (
	"encoding/base64"
	"fmt"
)

// thumbnailFromBase64 decodes a base64-encoded image, resizes it so its
// longest dimension is <= maxDim (aspect ratio preserved), and re-encodes
// it as a base64 JPEG. If the image is already small enough it is simply
// re-encoded. Returns an error if the input is not valid base64 or not a
// decodable image.
func thumbnailFromBase64(b64 string, maxDim int) (string, error) {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}
	img, err := decodeRGB(data)
	if err != nil {
		return "", fmt.Errorf("decode image: %w", err)
	}
	longest := img.w
	if img.h > longest {
		longest = img.h
	}
	if longest <= maxDim {
		// Already small enough; just re-encode to normalized JPEG.
		return encodeFaceToBase64(img)
	}
	scale := float64(maxDim) / float64(longest)
	nw := round(float64(img.w) * scale)
	nh := round(float64(img.h) * scale)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	resized := bilinearResize(img, nw, nh)
	return encodeFaceToBase64(resized)
}
