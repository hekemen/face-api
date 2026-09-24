package domain

import "time"

// Repository ports — interfaces the service depends on for data access.

// UserRepository manages enrolled users.
type UserRepository interface {
	// GetByName returns the user with the given name, or an error if not found.
	GetByName(name string) (*User, error)
	// Exists returns true if a user with the given name already exists.
	Exists(name string) (bool, error)
	// Save stores or updates a user.
	Save(u *User) error
	// Delete removes a user by name.
	Delete(name string) error
	// ListAll returns all enrolled users.
	ListAll() ([]*User, error)
}

// CandidateRepository manages collected candidate faces.
type CandidateRepository interface {
	// Save stores a candidate face.
	Save(c *Candidate) error
	// GetByID returns a candidate by ID, or an error if not found.
	GetByID(id string) (*Candidate, error)
	// Delete removes a candidate by ID.
	Delete(id string) error
	// ListAll returns all candidates.
	ListAll() ([]*Candidate, error)
	// DeleteByID removes candidates by their IDs.
	DeleteByID(ids []string) error
	// GroupBySimilarity groups candidates by pairwise cosine similarity >= threshold.
	GroupBySimilarity(threshold float32) ([]*CandidateGroup, error)
}

// AuditRepository manages audit trail entries.
type AuditRepository interface {
	// Append writes one or more audit entries.
	Append(entries []AuditEntry) error
	// Recent returns the newest n entries (newest first).
	Recent(n int) ([]AuditEntry, error)
	// ListPaginated returns a page of entries with filtering support.
	ListPaginated(opts ListPaginatedOpts) ([]AuditEntry, int, error)
	// CountAll returns the total number of audit entries.
	CountAll() (int, error)
	// ComputeStats returns aggregate statistics from all audit entries.
	ComputeStats() (Stats, error)
}

// ListPaginatedOpts holds filtering and pagination parameters.
type ListPaginatedOpts struct {
	NameFilter   string // substring match on name (case-insensitive)
	EndpointFilter string // exact match on endpoint
	MatchedFilter  string // "yes"=matched only, "no"=not matched only, ""=all
	Page         int
	PerPage      int
}

// Stats holds aggregate statistics from the audit log.
type Stats struct {
	TotalChecks     int
	TotalMatched    int
	TotalNoFace     int
	TotalNotMatched int
	LastMatched     *time.Time
}

// FaceProcessor encapsulates face detection and embedding extraction.
// This port allows the service to be tested without real ONNX sessions.
type FaceProcessor interface {
	// DetectAndCrop runs face detection on an image and returns a 112×112
	// face crop suitable for recognition. The second return value is the
	// bounding box normalized to [0,1] in (x1, y1, x2, y2) order.
	DetectAndCrop(data []byte) (*FaceCrop, error)
	// ExtractEmbedding takes a 112×112 face crop and returns a 512-dim
	// embedding vector.
	ExtractEmbedding(crop *FaceCrop) (FaceEmbedding, error)
}

// RTSPReader reads one frame from an RTSP stream as JPEG bytes.
type RTSPReader interface {
	ReadFrame(url string, timeout time.Duration) ([]byte, error)
}

// FaceCrop holds the result of face detection/cropping.
type FaceCrop struct {
	Data      []byte // raw JPEG bytes of the 112×112 crop
	Embedding FaceEmbedding
	BBox      [4]float32 // normalized bounding box
}

// FaceService is the main business logic interface.
// All HTTP handlers and MQTT triggers call into this service.
type FaceService interface {
	// EnrollImage enrolls a face from an image upload.
	EnrollImage(name string, imageData []byte) error
	// RecognizeImage recognizes a face from an image upload.
	RecognizeImage(imageData []byte) (*RecognitionResult, error)
	// CheckStream reads a single RTSP frame, detects a face, and matches it.
	// The caller (HTTP handler / MQTT bridge) is responsible for reading the
	// frame from RTSP; use CheckStreamImage to pass pre-read image data.
	CheckStream(rtspURL string) (*StreamCheckResult, error)
	// CheckStreamImage runs face detection and recognition on image data
	// captured from an RTSP stream. Handles detection, recognition, and
	// auto-collection of unmatched faces.
	CheckStreamImage(imageData []byte) (*StreamCheckResult, error)
	// CollectStreamCandidate stores an unmatched face from a stream as a candidate.
	CollectStreamCandidate(c *Candidate) error
	// ListUsers returns all enrolled users.
	ListUsers() ([]*User, error)
	// DeleteUser removes a user by name.
	DeleteUser(name string) error
	// ListCandidates returns all candidates grouped by similarity.
	ListCandidates() ([]*CandidateGroup, error)
	// PromoteCandidate promotes a candidate to an enrolled user.
	PromoteCandidate(candidateID, name string) error
	// BulkPromoteCandidates promotes multiple candidates to a single user.
	BulkPromoteCandidates(name string, candidateIDs []string) error
	// RecentAudit returns the newest audit entries.
	RecentAudit(n int) ([]AuditEntry, error)
	// ListAuditPaginated returns a page of audit entries with filtering.
	ListAuditPaginated(opts ListPaginatedOpts) ([]AuditEntry, int, error)
	// ComputeStats returns aggregate statistics from the audit log.
	ComputeStats() (Stats, error)
}

// --- Response types ---

// OperationDuration carries the elapsed wall-clock time of an API operation.
type OperationDuration struct {
	DurationMs int64 `json:"duration_ms"`
}

// RecognitionResult is returned by the recognize endpoint.
type RecognitionResult struct {
	OperationDuration
	Name       string  `json:"name"`
	Similarity float32 `json:"similarity"`
	Matched    bool    `json:"matched"`
}

// StreamCheckResult is returned by the stream-check endpoint.
type StreamCheckResult struct {
	OperationDuration
	Status     string  `json:"status"`
	Name       string  `json:"name,omitempty"`
	Similarity float32 `json:"similarity,omitempty"`
	Reason     string  `json:"reason,omitempty"`
	Matched    bool    `json:"matched,omitempty"`
	FaceImage  string  `json:"face_image,omitempty"`
}

// EnrollmentResult is returned by the enroll endpoint.
type EnrollmentResult struct {
	OperationDuration
	Status string `json:"status"`
	Name   string `json:"name"`
}

// UsersListResponse is returned by the GET /users endpoint.
type UsersListResponse struct {
	OperationDuration
	Users []UserInfo `json:"users"`
}

// UserInfo describes one enrolled user in the GET /users response.
type UserInfo struct {
	Name      string    `json:"name"`
	Pictures  []string  `json:"pictures,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AuditListResponse is returned by the GET /audit endpoint.
type AuditListResponse struct {
	OperationDuration
	Count   int          `json:"count"`
	Entries []AuditEntry `json:"entries"`
}

// CandidatesListResponse is returned by GET /candidates.
type CandidatesListResponse struct {
	OperationDuration
	Groups []*CandidateGroup `json:"groups"`
}

// AuditPaginatedResponse is returned by the GET /api/audit endpoint.
type AuditPaginatedResponse struct {
	OperationDuration `json:",inline"`
	Entries           []AuditEntry `json:"entries"`
	TotalCount        int          `json:"total_count"`
	Page              int          `json:"page"`
	PerPage           int          `json:"per_page"`
	TotalPages        int          `json:"total_pages"`
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
