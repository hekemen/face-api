package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
)

//go:embed templates/*.html templates/pico.min.css
var templateFS embed.FS

// UIPage holds data passed to UI templates.
type UIPage struct {
	Result  string
	Class   string
	Users   []UserInfo
	Entries []AuditEntry
	Count   int
	Stats   *StatsResponse
}

// RegisterUIHandlers attaches the /ui web interface handlers to the given mux.
// Only called when enableUI is true.
func (s *FaceServer) RegisterUIHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/ui/", s.handleUI)
}

func (s *FaceServer) handleUI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/ui"), "/")

	// Serve embedded static assets from the templates dir (e.g. pico.min.css).
	if path == "pico.min.css" {
		s.serveStatic(w, r, "templates/pico.min.css", "text/css; charset=utf-8")
		return
	}

	if path == "" {
		s.renderTemplate(w, "index.html", nil)
		return
	}

	switch path {
	case "enroll":
		s.handleUIEnroll(w, r)
	case "recognize":
		s.handleUIRecognize(w, r)
	case "stream-check":
		s.handleUIStreamCheck(w, r)
	case "users":
		s.handleUIUsers(w, r)
	case "audit":
		s.handleUIAudit(w, r)
	case "stats":
		s.handleUIStats(w, r)
	default:
		http.NotFound(w, r)
	}
}

// serveStatic serves an embedded file from templateFS (set via //go:embed).
func (s *FaceServer) serveStatic(w http.ResponseWriter, r *http.Request, path, contentType string) {
	data, err := templateFS.ReadFile(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Write(data)
}

func (s *FaceServer) renderTemplate(w http.ResponseWriter, name string, data any) {
	t, err := template.ParseFS(templateFS, "templates/"+name, "templates/shared.html")
	if err != nil {
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := t.Execute(w, data); err != nil {
		s.log.Error().Err(err).Str("template", name).Msg("template render error")
	}
}

func (s *FaceServer) proxyAPI(w http.ResponseWriter, r *http.Request, endpoint string) (string, string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "Failed to read request body", "error"
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	rec := httptest.NewRecorder()
	r.URL.Path = endpoint
	r.URL.RawQuery = ""
	r.RequestURI = ""
	s.mux.ServeHTTP(rec, r)

	result := rec.Body.String()
	if rec.Code >= http.StatusBadRequest {
		return result, "error"
	}
	return result, ""
}

func (s *FaceServer) handleUIEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.mu.RLock()
		users := make([]UserInfo, 0, len(s.dbMap))
		names := make([]string, 0, len(s.dbMap))
		for name := range s.dbMap {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			u := s.dbMap[name]
			users = append(users, UserInfo{
				Name:      name,
				Pictures:  u.Pictures,
				UpdatedAt: u.UpdatedAt,
			})
		}
		s.mu.RUnlock()

		s.renderTemplate(w, "enroll.html", UIPage{Users: users})
		return
	}

	// Handle delete form submission.
	if r.FormValue("action") == "delete" {
		name := r.FormValue("user_name")
		if name == "" {
			s.renderTemplate(w, "enroll.html", UIPage{Result: "Missing user name", Class: "error"})
			return
		}
		s.deleteUser(name)
		result := fmt.Sprintf("Deleted user: %s", name)
		s.mu.RLock()
		users := make([]UserInfo, 0, len(s.dbMap))
		var delNames []string
		for n := range s.dbMap {
			delNames = append(delNames, n)
		}
		sort.Strings(delNames)
		for _, n := range delNames {
			u := s.dbMap[n]
			users = append(users, UserInfo{
				Name:      n,
				Pictures:  u.Pictures,
				UpdatedAt: u.UpdatedAt,
			})
		}
		s.mu.RUnlock()
		s.renderTemplate(w, "enroll.html", UIPage{Result: result, Class: "", Users: users})
		return
	}

	result, class := s.proxyAPI(w, r, "/enroll")
	page := UIPage{Result: result, Class: class}
	if class == "" {
		var resp EnrolledResponse
		if err := json.Unmarshal([]byte(result), &resp); err == nil {
			page.Result = fmt.Sprintf("Enrolled: %s (%.0fms)", resp.Name, float64(resp.DurationMs))
		}
	}
	s.mu.RLock()
	users := make([]UserInfo, 0, len(s.dbMap))
	var names []string
	for n := range s.dbMap {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		u := s.dbMap[n]
		users = append(users, UserInfo{
			Name:      n,
			Pictures:  u.Pictures,
			UpdatedAt: u.UpdatedAt,
		})
	}
	s.mu.RUnlock()

	page.Users = users
	s.renderTemplate(w, "enroll.html", page)
}

func (s *FaceServer) handleUIRecognize(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.renderTemplate(w, "recognize.html", UIPage{})
		return
	}

	result, class := s.proxyAPI(w, r, "/recognize")
	page := UIPage{Result: result, Class: class}
	if class == "" {
		var resp RecognitionResult
		if err := json.Unmarshal([]byte(result), &resp); err == nil {
			page.Result = fmt.Sprintf("Name: %s | Similarity: %.3f | Matched: %v (%.0fms)",
				resp.Name, resp.Similarity, resp.Matched, float64(resp.DurationMs))
		}
	}
	s.renderTemplate(w, "recognize.html", page)
}

func (s *FaceServer) handleUIStreamCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.renderTemplate(w, "stream-check.html", UIPage{})
		return
	}

	result, class := s.proxyAPI(w, r, "/stream-check")
	page := UIPage{Result: result, Class: class}
	if class == "" {
		var resp StreamCheckResponse
		if err := json.Unmarshal([]byte(result), &resp); err == nil {
			msg := fmt.Sprintf("Status: %s", resp.Status)
			if resp.Name != "" {
				msg += fmt.Sprintf(" | Name: %s | Similarity: %.3f", resp.Name, resp.Similarity)
			}
			if resp.Reason != "" {
				msg += fmt.Sprintf(" | Reason: %s", resp.Reason)
			}
			msg += fmt.Sprintf(" (%.0fms)", float64(resp.DurationMs))
			page.Result = msg
		}
	}
	s.renderTemplate(w, "stream-check.html", page)
}

func (s *FaceServer) handleUIUsers(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	users := make([]UserInfo, 0, len(s.dbMap))
	names := make([]string, 0, len(s.dbMap))
	for name := range s.dbMap {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		u := s.dbMap[name]
		users = append(users, UserInfo{
			Name:      name,
			Pictures:  u.Pictures,
			UpdatedAt: u.UpdatedAt,
		})
	}
	s.mu.RUnlock()

	s.renderTemplate(w, "users.html", UIPage{Users: users})
}

func (s *FaceServer) handleUIAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.readAudit(100)
	if err != nil {
		http.Error(w, "Failed to read audit log", http.StatusInternalServerError)
		return
	}

	s.renderTemplate(w, "audit.html", UIPage{
		Entries: entries,
		Count:   len(entries),
	})
}

func (s *FaceServer) handleUIStats(w http.ResponseWriter, r *http.Request) {
	// Always fetch stats from the API for the initial page render.
	result, _ := s.proxyAPI(w, r, "/stats")
	var resp StatsResponse
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		resp = StatsResponse{}
	}
	s.renderTemplate(w, "stats.html", UIPage{Stats: &resp})
}
