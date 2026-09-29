package http

import (
	"bytes"
	"encoding/json"
	"embed"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"time"

	"h2hsecure.com/face/internal/domain"
)

//go:embed templates/*.html templates/pico.min.css
var embeddedTemplates embed.FS

// UIHandler handles the web interface at /ui/.
type UIHandler struct {
	svc        domain.FaceService
	enableUI   bool
	gitVersion string
	githubURL  string
}

// NewUIHandler creates a UI handler. Set enableUI to false to disable.
func NewUIHandler(svc domain.FaceService, enableUI bool) *UIHandler {
	return &UIHandler{
		svc:        svc,
		enableUI:   enableUI,
		gitVersion: "v0.0.8",
		githubURL:  "https://github.com/hekemen/face-api",
	}
}

// RegisterUIHandlers attaches the /ui web interface handlers to the given mux.
func (h *UIHandler) RegisterUIHandlers(mux *http.ServeMux) {
	if !h.enableUI {
		return
	}
	mux.HandleFunc("/ui/", h.handleUI)
}

func (h *UIHandler) handleUI(w http.ResponseWriter, r *http.Request) {
	path := stripUIPrefix(r.URL.Path)

	// Serve embedded static assets (e.g. pico.min.css).
	if path == "pico.min.css" {
		h.serveStatic(w, r, "templates/pico.min.css", "text/css; charset=utf-8")
		return
	}

	if path == "" {
		// Dashboard page.
		h.renderIndex(w, r)
		return
	}

	switch path {
	case "enroll", "enroll/delete":
		h.handleUIEnroll(w, r)
	case "recognize":
		h.handleUIRecognize(w, r)
	case "audit":
		h.handleUIAudit(w, r)
	case "stats":
		h.handleUIStats(w, r)
	default:
		http.NotFound(w, r)
	}
}

// --- Template helpers ---

var templateCache = make(map[string]*template.Template)

func (h *UIHandler) renderTemplate(w http.ResponseWriter, name string, data any) {
	key := name
	var tmpl *template.Template
	var err error

	if t, ok := templateCache[key]; ok {
		tmpl = t
	} else {
		tmpl, err = template.ParseFS(embeddedTemplates, "templates/"+name, "templates/shared.html")
		if err != nil {
			http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		templateCache[key] = tmpl
	}

	var buf bytes.Buffer
	var execErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				execErr = fmt.Errorf("template execution panic: %v", r)
			}
		}()
		execErr = tmpl.Execute(&buf, data)
	}()
	if execErr != nil {
		log.Printf("ui: template %s error: %v", name, execErr)
		http.Error(w, "Template execution error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

// serveStatic serves an embedded file.
func (h *UIHandler) serveStatic(w http.ResponseWriter, r *http.Request, path, contentType string) {
	data, err := embeddedTemplates.ReadFile("templates/pico.min.css")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Write(data)
}

// proxyAPI proxies a request in-process to the API mux.
func (h *UIHandler) proxyAPI(w http.ResponseWriter, r *http.Request, endpoint string) (string, string) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))

	rec := httptest.NewRecorder()
	r.URL.Path = endpoint
	r.URL.RawQuery = ""
	r.RequestURI = ""
	if apiMux != nil {
		apiMux.ServeHTTP(rec, r)
	}
	result := rec.Body.String()
	if rec.Code >= http.StatusBadRequest {
		return result, "error"
	}
	return result, ""
}

// --- Dashboard ---

type uiPageData struct {
	Result             string
	Class              string
	Users              []domain.UserInfo
	Entries            []domain.AuditEntry
	Count              int
	Stats              *domain.StatsResponse
	DashUsers          []domain.UserInfo
	DashStats          *domain.StatsResponse
	DashAudit          []domain.AuditEntry
	UnmatchedEntries   []domain.AuditEntry
	TotalCollected     int
	GitVersion         string
	GitHubURL          string
}

func (h *UIHandler) renderIndex(w http.ResponseWriter, r *http.Request) {
	page := uiPageData{
		GitVersion:  h.gitVersion,
		GitHubURL:   h.githubURL,
	}

	// Fetch users for dashboard.
	users, err := h.svc.ListUsers()
	if err == nil {
		names := make([]string, 0, len(users))
		for _, u := range users {
			names = append(names, u.Name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, u := range users {
				if u.Name == name {
					page.DashUsers = append(page.DashUsers, domain.UserInfo{
						Name:      u.Name,
						Pictures:  u.Pictures,
						UpdatedAt: u.UpdatedAt,
					})
					break
				}
			}
		}
	}

	// Fetch stats for dashboard.
	stats, err := h.svc.ComputeStats()
	if err == nil {
		var lastMatchedStr *string
		if stats.LastMatched != nil {
			t := stats.LastMatched.Format(time.RFC3339)
			lastMatchedStr = &t
		}
		page.DashStats = &domain.StatsResponse{
			TotalChecks:     stats.TotalChecks,
			TotalMatched:    stats.TotalMatched,
			TotalNoFace:     stats.TotalNoFace,
			TotalNotMatched: stats.TotalNotMatched,
			TotalCollected:  0,
			LastMatched:     lastMatchedStr,
		}
		entries, _ := h.svc.RecentAudit(10)
		page.DashAudit = entries
	}

	h.renderTemplate(w, "index.html", page)
}

// --- Enroll page ---

func (h *UIHandler) handleUIEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		users, err := h.svc.ListUsers()
		if err != nil {
			h.renderTemplate(w, "enroll.html", uiPageData{Result: "Failed to list users", Class: "error"})
			return
		}
		userInfos := userSliceToInfos(users)
		h.renderTemplate(w, "enroll.html", uiPageData{Users: userInfos})
		return
	}

	// Handle delete form submission.
	if r.FormValue("action") == "delete" {
		name := r.FormValue("user_name")
		if name == "" {
			h.renderTemplate(w, "enroll.html", uiPageData{Result: "Missing user name", Class: "error"})
			return
		}
		if err := h.svc.DeleteUser(name); err != nil {
			h.renderTemplate(w, "enroll.html", uiPageData{Result: "Delete failed: "+err.Error(), Class: "error"})
			return
		}
		result := "Deleted user: " + name
		users, _ := h.svc.ListUsers()
		h.renderTemplate(w, "enroll.html", uiPageData{Result: result, Class: "", Users: userSliceToInfos(users)})
		return
	}

	// Proxy to enroll API.
	result, class := h.proxyAPI(w, r, "/enroll")
	page := uiPageData{Result: result, Class: class}
	if class == "" {
		var resp domain.EnrollmentResult
		if err := json.Unmarshal([]byte(result), &resp); err == nil {
			page.Result = "Enrolled: " + resp.Name
		}
	}
	users, _ := h.svc.ListUsers()
	page.Users = userSliceToInfos(users)
	h.renderTemplate(w, "enroll.html", page)
}

// --- Recognize page ---

func (h *UIHandler) handleUIRecognize(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.renderTemplate(w, "recognize.html", uiPageData{})
		return
	}

	result, class := h.proxyAPI(w, r, "/recognize")
	page := uiPageData{Result: result, Class: class}
	if class == "" {
		var resp domain.RecognitionResult
		if err := json.Unmarshal([]byte(result), &resp); err == nil {
			page.Result = "Name: " + resp.Name +
				" | Similarity: " + strconv.FormatFloat(float64(resp.Similarity), 'f', 3, 32) +
				" | Matched: " + strconv.FormatBool(resp.Matched)
		}
	}
	h.renderTemplate(w, "recognize.html", page)
}

// --- Audit page ---

func (h *UIHandler) handleUIAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := h.svc.RecentAudit(100)
	if err != nil {
		http.Error(w, "Failed to read audit log", http.StatusInternalServerError)
		return
	}

	unmatched, _ := h.svc.ListUnmatched(100)

	h.renderTemplate(w, "audit.html", uiPageData{
		Entries:          entries,
		Count:            len(entries),
		UnmatchedEntries: unmatched,
		TotalCollected:   0,
	})
}

// --- Stats page ---

func (h *UIHandler) handleUIStats(w http.ResponseWriter, r *http.Request) {
	stats, err := h.svc.ComputeStats()
	if err != nil {
		http.Error(w, "Failed to compute stats", http.StatusInternalServerError)
		return
	}

	var lastMatchedStr *string
	if stats.LastMatched != nil {
		t := stats.LastMatched.Format(time.RFC3339)
		lastMatchedStr = &t
	}

	sr := &domain.StatsResponse{
		TotalChecks:     stats.TotalChecks,
		TotalMatched:    stats.TotalMatched,
		TotalNoFace:     stats.TotalNoFace,
		TotalNotMatched: stats.TotalNotMatched,
		TotalCollected:  stats.TotalNotMatched,
		LastMatched:     lastMatchedStr,
	}
	h.renderTemplate(w, "stats.html", uiPageData{Stats: sr})
}

// --- Utility ---

func stripUIPrefix(path string) string {
	s := path
	if len(s) >= 4 && s[:4] == "/ui/" {
		return s[4:]
	}
	if len(s) >= 3 && s[:3] == "/ui" {
		return ""
	}
	return s
}

func userSliceToInfos(users []*domain.User) []domain.UserInfo {
	if users == nil {
		return nil
	}
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, u.Name)
	}
	sort.Strings(names)
	result := make([]domain.UserInfo, 0, len(names))
	for _, name := range names {
		for _, u := range users {
			if u.Name == name {
				result = append(result, domain.UserInfo{
					Name:      u.Name,
					Pictures:  u.Pictures,
					UpdatedAt: u.UpdatedAt,
				})
				break
			}
		}
	}
	return result
}
