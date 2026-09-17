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
	Matched    bool    `json:"matched,omitempty"`
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

// UserInfo describes one enrolled user in the GET /users response: name plus
// the enrolled face pictures (up to 3) and the last-update timestamp.
type UserInfo struct {
	Name      string    `json:"name"`
	Pictures  []string  `json:"pictures,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UsersListResponse is returned by the GET /users endpoint.
type UsersListResponse struct {
	OperationDuration
	Users []UserInfo `json:"users"`
}

// StatsResponse is returned by the GET /stats endpoint.
type StatsResponse struct {
	OperationDuration   `json:",inline"`
	TotalChecks     int     `json:"total_checks"`
	TotalMatched    int     `json:"total_matched"`
	TotalNoFace     int     `json:"total_no_face"`
	TotalNotMatched int     `json:"total_not_matched"`
	LastMatched     *string `json:"last_matched"`
}
