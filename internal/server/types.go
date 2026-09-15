package server

import "time"

// Response types for the face recognition API.

// OperationDuration carries the elapsed wall-clock time of an API operation.
type OperationDuration struct {
	DurationMs int64 `json:"duration_ms"`
}

type RecognitionResult struct {
	OperationDuration
	Name       string  `json:"name"`
	Similarity float32 `json:"similarity"`
	Matched    bool    `json:"matched"`
}

type StreamCheckResponse struct {
	OperationDuration
	Status     string  `json:"status"`
	Name       string  `json:"name,omitempty"`
	Similarity float32 `json:"similarity,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

// EnrolledResponse is returned by the enroll endpoint on success.
type EnrolledResponse struct {
	OperationDuration
	Status string `json:"status"`
	Name   string `json:"name"`
}

// AuditEntry records a single face scan attempt for the audit log.
type AuditEntry struct {
	Time       time.Time `json:"time"`
	Endpoint   string    `json:"endpoint"` // enroll | recognize | stream-check
	Name       string    `json:"name"`
	Similarity float32   `json:"similarity"`
	Matched    bool      `json:"matched"`
	DurationMs int64     `json:"duration_ms"`
	FaceImage  string    `json:"face_image,omitempty"`
}

// AuditListResponse is returned by the GET /audit endpoint.
type AuditListResponse struct {
	OperationDuration
	Count   int          `json:"count"`
	Entries []AuditEntry `json:"entries"`
}
