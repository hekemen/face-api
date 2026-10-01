package http

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"h2hsecure.com/face/internal/domain"
)

// RTSPReader abstracts RTSP frame capture for the HTTP handlers.
type RTSPReader interface {
	// ReadFrame returns the first decoded frame (as JPEG bytes) from the stream.
	ReadFrame(rtspURL string, timeout time.Duration) ([]byte, error)
}

// Handlers holds the HTTP handlers for the face API.
type Handlers struct {
	svc        domain.FaceService
	rtspURL    string
	rtspReader RTSPReader
	enableUI   bool
	logger     zerolog.Logger
}

// NewHandlers creates a new Handlers instance.
func NewHandlers(svc domain.FaceService, rtspURL string, rtspReader RTSPReader, enableUI bool, logger zerolog.Logger) *Handlers {
	return &Handlers{
		svc:        svc,
		rtspURL:    rtspURL,
		rtspReader: rtspReader,
		enableUI:   enableUI,
		logger:     logger,
	}
}

// RegisterHandlers attaches all handlers to the given mux.
func (h *Handlers) RegisterHandlers(mux *http.ServeMux) {
	mux.HandleFunc("POST /enroll", h.handleEnroll)
	mux.HandleFunc("POST /enroll-from-stream", h.handleEnrollFromStream)
	mux.HandleFunc("POST /recognize", h.handleRecognize)
	mux.HandleFunc("POST /stream-check", h.handleStreamCheck)
	mux.HandleFunc("GET /users", h.handleListUsers)
	mux.HandleFunc("DELETE /users/", h.handleDeleteUser)
	mux.HandleFunc("GET /audit", h.handleListAudit)
	mux.HandleFunc("GET /api/audit", h.handleListAuditPaginated)
	mux.HandleFunc("GET /api/audit/face", h.handleGetAuditFace)
	mux.HandleFunc("GET /api/audit-unmatched", h.handleAuditUnmatched)
	mux.HandleFunc("GET /api/audit/recognize-summary", h.handleRecognizeSummary)
	mux.HandleFunc("GET /api/audit/recognize-list", h.handleRecognizeList)
	mux.HandleFunc("GET /stats", h.handleListStats)
	mux.HandleFunc("POST /audit/promote", h.handlePromoteFromAudit)
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /readyz", h.handleReadyz)
}

// --- Helper functions ---

// decodeImageFromRequest reads a multipart form field as a file and returns bytes.
func decodeImageFromRequest(r *http.Request, fieldName string) ([]byte, error) {
	file, _, err := r.FormFile(fieldName)
	if err != nil {
		return nil, fmt.Errorf("invalid image field: %v", err)
	}
	defer file.Close()

	buf, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read image file: %v", err)
	}
	return buf, nil
}

// writeJSON encodes a response as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// --- Handlers ---

func (h *Handlers) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	start := time.Now()

	name := r.FormValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Missing 'name' field")
		return
	}

	imageData, err := decodeImageFromRequest(r, "image")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := h.svc.EnrollImage(name, imageData); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "maximum") {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		// Decode/format errors are bad requests.
		if strings.Contains(msg, "decode") || strings.Contains(msg, "format") {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		writeError(w, http.StatusInternalServerError, "Face processing or storage failed: "+msg)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"status":      "enrolled",
		"name":        name,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func (h *Handlers) handleEnrollFromStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	start := time.Now()

	name := r.FormValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Missing 'name' field")
		return
	}

	rtspURL := h.rtspURL
	if rtspURL == "" {
		writeError(w, http.StatusBadRequest, "Missing 'rtsp_url' field and no RTSP_URL configured")
		return
	}

	// Read RTSP frame
	imageData, err := h.rtspReader.ReadFrame(rtspURL, 10*time.Second)
	if err != nil {
		reason := "Failed to connect to RTSP stream"
		if strings.Contains(err.Error(), "No face detected") {
			writeError(w, http.StatusBadRequest, reason)
			return
		}
		writeError(w, http.StatusInternalServerError, "RTSP error: "+reason)
		return
	}

	// Process through service
	if err := h.svc.EnrollImage(name, imageData); err != nil {
		writeError(w, http.StatusInternalServerError, "Enrollment failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"status":      "enrolled",
		"name":        name,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func (h *Handlers) handleRecognize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	start := time.Now()

	var imageData []byte

	// If rtsp_url is provided, capture a frame from the stream.
	rtspURL := r.FormValue("rtsp_url")
	if rtspURL != "" {
		if h.rtspReader == nil {
			writeError(w, http.StatusBadRequest, "RTSP reader not configured")
			return
		}
		var err error
		imageData, err = h.rtspReader.ReadFrame(rtspURL, 10*time.Second)
		if err != nil {
			msg := err.Error()
			if strings.Contains(strings.ToLower(msg), "no face detected") {
				writeError(w, http.StatusBadRequest, msg)
				return
			}
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	} else {
		var err error
		imageData, err = decodeImageFromRequest(r, "image")
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	result, err := h.svc.RecognizeImage(imageData)
	if err != nil {
		msg := err.Error()
		// Decode/format errors are bad requests, not internal errors.
		if strings.Contains(msg, "decode") || strings.Contains(msg, "format") {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		writeError(w, http.StatusInternalServerError, msg)
		return
	}

	// Add duration_ms to response
	result.DurationMs = time.Since(start).Milliseconds()

	writeJSON(w, http.StatusOK, result)
}

func (h *Handlers) handleStreamCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	start := time.Now()

	rtspURL := r.FormValue("rtsp_url")
	if rtspURL == "" {
		rtspURL = h.rtspURL
	}
	if rtspURL == "" {
		writeError(w, http.StatusBadRequest, "Missing 'rtsp_url' field and no RTSP_URL configured")
		return
	}

	imageData, err := h.rtspReader.ReadFrame(rtspURL, 10*time.Second)
	if err != nil {
		reason := "Failed to connect to RTSP stream"
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":      "not ok",
			"reason":      reason,
			"duration_ms": time.Since(start).Milliseconds(),
		})
		return
	}

	result, err := h.svc.CheckStreamImage(imageData)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (h *Handlers) handleListUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	start := time.Now()

	users, err := h.svc.ListUsers()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	userInfos := make([]map[string]interface{}, 0, len(users))
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, u.Name)
	}
	sort.Strings(names)

	for _, name := range names {
		for _, u := range users {
			if u.Name == name {
				userInfos = append(userInfos, map[string]interface{}{
					"name":       u.Name,
					"pictures":   u.Pictures,
					"updated_at": u.UpdatedAt.Format(time.RFC3339),
				})
				break
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"users":       userInfos,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func (h *Handlers) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	start := time.Now()
	name := r.URL.Path[len("/users/"):]
	if name == "" {
		writeError(w, http.StatusBadRequest, "Missing user name")
		return
	}

	if err := h.svc.DeleteUser(name); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":      "deleted",
		"name":        name,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func (h *Handlers) handleListAudit(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	entries, err := h.svc.RecentAudit(100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		entries = []domain.AuditEntry{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count":       len(entries),
		"entries":     entries,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func (h *Handlers) handleListAuditPaginated(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	nameFilter := r.URL.Query().Get("name")
	endpointFilter := r.URL.Query().Get("endpoint")
	matchedFilter := r.URL.Query().Get("matched")
	statusFilter := r.URL.Query().Get("status")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))

	if page <= 0 {
		page = 1
	}
	if perPage <= 0 {
		perPage = 20
	}
	if perPage > 100 {
		perPage = 100
	}

	entries, totalCount, users, err := h.svc.ListAuditPaginated(domain.ListPaginatedOpts{
		NameFilter:     nameFilter,
		EndpointFilter: endpointFilter,
		MatchedFilter:  matchedFilter,
		StatusFilter:   statusFilter,
		Page:           page,
		PerPage:        perPage,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		entries = []domain.AuditEntry{}
	}
	if users == nil {
		users = []string{}
	}

	// Merge enrolled user names so the audit page shows all enrolled users,
	// not just names that appear in audit entries.
	allUsers, userErr := h.svc.ListUsers()
	if userErr == nil && allUsers != nil {
		seen := make(map[string]bool)
		for _, u := range allUsers {
			if u != nil && u.Name != "" {
				seen[u.Name] = true
			}
		}
		for _, n := range users {
			seen[n] = true
		}
		merged := make([]string, 0, len(seen))
		for n := range seen {
			merged = append(merged, n)
		}
		sort.Strings(merged)
		users = merged
	}

	totalPages := (totalCount + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries":     entries,
		"total_count": totalCount,
		"page":        page,
		"per_page":    perPage,
		"total_pages": totalPages,
		"users":       users,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

// handleGetAuditFace returns the face image (base64 JPEG) for a given audit entry.
func (h *Handlers) handleGetAuditFace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	timestamp := r.URL.Query().Get("id")
	if timestamp == "" {
		writeError(w, http.StatusBadRequest, "Missing 'id' query parameter")
		return
	}

	faceImage, err := h.svc.GetAuditFace(timestamp)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"face_image": faceImage,
	})
}

func (h *Handlers) handleAuditUnmatched(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	entries, err := h.svc.ListUnmatched(100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		entries = []domain.AuditEntry{}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries,
		"count":   len(entries),
	})
}

func (h *Handlers) handleListStats(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	stats, err := h.svc.ComputeStats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var lastMatched *string
	if stats.LastMatched != nil {
		t := stats.LastMatched.Format(time.RFC3339)
		lastMatched = &t
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"total_checks":      stats.TotalChecks,
		"total_matched":     stats.TotalMatched,
		"total_no_face":     stats.TotalNoFace,
		"total_not_matched": stats.TotalNotMatched,
		"total_collected":   0,
		"last_matched":      lastMatched,
		"duration_ms":       time.Since(start).Milliseconds(),
	})
}

func (h *Handlers) handlePromoteFromAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.logger.Warn().Err(err).Msg("promote: failed to decode request body")
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "Missing 'name' field")
		return
	}

	auditTime := r.URL.Query().Get("id")
	if auditTime == "" {
		writeError(w, http.StatusBadRequest, "Missing 'id' query parameter (audit entry timestamp)")
		return
	}

	h.logger.Info().Str("name", req.Name).Str("audit_time", auditTime).Msg("promote: starting")

	start := time.Now()

	if err := h.svc.PromoteFromAudit(auditTime, req.Name); err != nil {
		h.logger.Error().Err(err).Str("name", req.Name).Str("audit_time", auditTime).Msg("promote: failed")
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, msg)
			return
		}
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	h.logger.Info().Str("name", req.Name).Str("audit_time", auditTime).Dur("duration_ms", time.Since(start)).Msg("promote: success")

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"status":      "promoted",
		"name":        req.Name,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func (h *Handlers) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handlers) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// For now, always ready. A real implementation would check bbolt health.
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleRecognizeSummary returns aggregate stats for recognize-only audit entries.
func (h *Handlers) handleRecognizeSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	start := time.Now()

	summary, err := h.svc.ComputeRecognizeStats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	summary.DurationMs = time.Since(start).Milliseconds()

	writeJSON(w, http.StatusOK, summary)
}

// handleRecognizeList returns a paginated list of recognize-only audit entries.
func (h *Handlers) handleRecognizeList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	start := time.Now()

	userFilter := r.URL.Query().Get("user")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))

	if page <= 0 {
		page = 1
	}
	if perPage <= 0 {
		perPage = 20
	}
	if perPage > 100 {
		perPage = 100
	}

	entries, totalCount, err := h.svc.ListRecognizeEntries(domain.ListRecognizeEntriesOpts{
		UserFilter: userFilter,
		Page:       page,
		PerPage:    perPage,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entries == nil {
		entries = []domain.AuditEntry{}
	}

	// Strip endpoint field for recognize-only responses.
	recognizeEntries := make([]domain.RecognizeAuditEntry, len(entries))
	for i, e := range entries {
		recognizeEntries[i] = domain.RecognizeAuditEntry{AuditEntry: e}
	}

	totalPages := (totalCount + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}

	writeJSON(w, http.StatusOK, domain.RecognizeListResponse{
		OperationDuration: domain.OperationDuration{DurationMs: time.Since(start).Milliseconds()},
		Entries:           recognizeEntries,
		TotalCount:        totalCount,
		Page:              page,
		PerPage:           perPage,
		TotalPages:        totalPages,
	})
}

// apiMux is the main API mux, set after handler registration for UI in-process proxying.
var apiMux *http.ServeMux

// SetAPIMux sets the API mux for UI in-process API proxying.
func SetAPIMux(mux *http.ServeMux) {
	apiMux = mux
}
