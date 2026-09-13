package server

// Response types for the face recognition API.

type RecognitionResult struct {
	Name       string  `json:"name"`
	Similarity float32 `json:"similarity"`
	Matched    bool    `json:"matched"`
}

type StreamCheckResponse struct {
	Status     string  `json:"status"`
	Name       string  `json:"name,omitempty"`
	Similarity float32 `json:"similarity,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}
