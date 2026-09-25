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
	mux.HandleFunc("GET /stats", h.handleListStats)
	mux.HandleFunc("GET /candidates", h.handleListCandidates)
	mux.HandleFunc("POST /candidates/promote", h.handlePromoteCandidate)
	mux.HandleFunc("POST /candidates/bulk-promote", h.handleBulkPromoteCandidates)
	mux.HandleFunc("GET /healthz", h.handleHealthz)
	mux.HandleFunc("GET /readyz", h.handleReadyz)
}

// --- Middleware ---

// statusRecorder captures the response status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// RequestLogging wraps a handler with JSON request logging.
// Skips /healthz and /readyz probes.
func RequestLogging(next http.Handler, logger zerolog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info().Str("method", r.Method).Str("path", r.URL.Path).Int("status", rec.status).Int64("duration_ms", time.Since(start).Milliseconds()).Msg("request")
	})
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
	imageData, err := h.rtspReader.ReadFrame(rtspURL, 3*time.Second)
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

	imageData, err := decodeImageFromRequest(r, "image")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
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

	imageData, err := h.rtspReader.ReadFrame(rtspURL, 3*time.Second)
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

	name := r.URL.Path[len("/users/"):]
	if name == "" {
		writeError(w, http.StatusBadRequest, "Missing user name")
		return
	}

	if err := h.svc.DeleteUser(name); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "deleted",
		"name":   name,
	})
}

func (h *Handlers) handleListAudit(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	entries, err := h.svc.RecentAudit(100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
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

	entries, totalCount, err := h.svc.ListAuditPaginated(domain.ListPaginatedOpts{
		NameFilter:     nameFilter,
		EndpointFilter: endpointFilter,
		MatchedFilter:  matchedFilter,
		Page:           page,
		PerPage:        perPage,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	totalPages := (totalCount + perPage - 1) / perPage
	if totalPages == 0 {
		totalPages = 1
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries":      entries,
		"total_count":  totalCount,
		"page":         page,
		"per_page":     perPage,
		"total_pages":  totalPages,
		"duration_ms":  time.Since(start).Milliseconds(),
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
		"last_matched":      lastMatched,
		"duration_ms":       time.Since(start).Milliseconds(),
	})
}

func (h *Handlers) handleListCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	start := time.Now()

	groups, err := h.svc.ListCandidates()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"groups":      groups,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func (h *Handlers) handlePromoteCandidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "Missing 'name' field")
		return
	}

	candidateID := r.URL.Query().Get("id")
	if candidateID == "" {
		writeError(w, http.StatusBadRequest, "Missing 'id' query parameter")
		return
	}

	start := time.Now()

	if err := h.svc.PromoteCandidate(candidateID, req.Name); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "already exists") {
			writeError(w, http.StatusConflict, msg)
			return
		}
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, msg)
			return
		}
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"status":      "promoted",
		"name":        req.Name,
		"duration_ms": time.Since(start).Milliseconds(),
	})
}

func (h *Handlers) handleBulkPromoteCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}

	var req struct {
		Name         string   `json:"name"`
		CandidateIDs []string `json:"candidate_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "Missing 'name' field")
		return
	}
	if len(req.CandidateIDs) == 0 {
		writeError(w, http.StatusBadRequest, "Missing 'candidate_ids' field")
		return
	}

	start := time.Now()

	if err := h.svc.BulkPromoteCandidates(req.Name, req.CandidateIDs); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"status":      "bulk-promoted",
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
