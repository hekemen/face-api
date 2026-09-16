package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
)

//go:embed templates/*.html
var templateFS embed.FS

// UIPage holds data passed to UI templates.
type UIPage struct {
	Result  string
	Class   string
	Users   []string
	Entries []AuditEntry
	Count   int
}

// RegisterUIHandlers attaches the /ui web interface handlers to the given mux.
// Only called when enableUI is true.
func (s *FaceServer) RegisterUIHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/ui/", s.handleUI)
}

func (s *FaceServer) handleUI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/ui"), "/")
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
	default:
		http.NotFound(w, r)
	}
}

func (s *FaceServer) renderTemplate(w http.ResponseWriter, name string, data any) {
	t, err := template.ParseFS(templateFS, "templates/shared.html", "templates/"+name)
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

	req, err := http.NewRequest(r.Method, endpoint, bytes.NewReader(body))
	if err != nil {
		return "Failed to create request: " + err.Error(), "error"
	}
	req.Header.Set("Content-Type", r.Header.Get("Content-Type"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "API call failed: " + err.Error(), "error"
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	result := string(respBody)

	if resp.StatusCode >= 400 {
		return result, "error"
	}
	return result, ""
}

func (s *FaceServer) handleUIEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.renderTemplate(w, "enroll.html", UIPage{})
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
	defer s.mu.RUnlock()

	users := make([]string, 0, len(s.dbMap))
	for name := range s.dbMap {
		users = append(users, name)
	}

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
