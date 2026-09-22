package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestUIRenderNonEmpty(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/test.db", 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}, enableUI: true}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	for _, page := range []string{"/ui/", "/ui/enroll", "/ui/recognize", "/ui/stats", "/ui/audit"} {
		req := httptest.NewRequest(http.MethodGet, page, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", page, rec.Code)
		}
		if len(rec.Body.Bytes()) < 1000 {
			t.Fatalf("%s: body too small (%d bytes) — template likely not rendering full page", page, rec.Body.Len())
		}
		if !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
			t.Fatalf("%s: missing DOCTYPE", page)
		}
	}
}

// TestUIRendersPicoCSS checks that every page pulls PicoCSS from the app itself
// (relative link) so the UI works at root and behind the stripPrefix ingress.
func TestUIRendersPicoCSS(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/test.db", 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}, enableUI: true}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	for _, page := range []string{"/ui/", "/ui/enroll", "/ui/recognize", "/ui/stats", "/ui/audit"} {
		req := httptest.NewRequest(http.MethodGet, page, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		body := rec.Body.String()
		if !strings.Contains(body, `rel="stylesheet"`) || !strings.Contains(body, "pico") {
			t.Fatalf("%s: missing PicoCSS stylesheet link", page)
		}
		// The link must be relative (no leading /ui) so it survives stripPrefix.
		if strings.Contains(body, `href="/ui/pico`) {
			t.Fatalf("%s: PicoCSS link must be relative, got absolute /ui prefixed href", page)
		}
	}

	// The stylesheet must be served by the app itself.
	req := httptest.NewRequest(http.MethodGet, "/ui/pico.min.css", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/pico.min.css: status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Fatalf("GET /ui/pico.min.css: Content-Type %q, want text/css", ct)
	}
	if len(rec.Body.Bytes()) < 10000 {
		t.Fatalf("GET /ui/pico.min.css: body too small (%d bytes) — vendored file missing?", rec.Body.Len())
	}
}

// TestUIRendersCameraSupport checks that the enroll + recognize pages (the ones
// with an image input) wire up the webcam capture script.
func TestUIRendersCameraSupport(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/test.db", 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}, enableUI: true}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	cameraPages := []string{"enroll", "recognize"}
	for _, page := range cameraPages {
		req := httptest.NewRequest(http.MethodGet, "/ui/"+page, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		body := rec.Body.String()
		for _, marker := range []string{"getUserMedia", "data-camera", "capture"} {
			if !strings.Contains(body, marker) {
				t.Fatalf("/ui/%s: missing camera marker %q", page, marker)
			}
		}
	}

	// Recognize has image input → must include camera wiring.
	req := httptest.NewRequest(http.MethodGet, "/ui/recognize", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, marker := range []string{"getUserMedia", "data-camera", "capture"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("/ui/recognize: missing camera marker %q", marker)
		}
	}
}

func TestUIProxyAPIInProcess(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/test.db", 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}, enableUI: true}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	// /recognize with no image → 400 from the API,
	// which must arrive as the UI's "error" result (proving in-process dispatch).
	req := httptest.NewRequest(http.MethodPost, "/ui/recognize", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	t.Logf("Body length: %d", len(body))
	t.Logf("Body contains 'error': %v", strings.Contains(body, "error"))
	if !strings.Contains(body, "error") {
		t.Fatalf("expected error result embedded in page, got: %.200s", body)
	}
}

func TestUIProxyReturnsUnderlyingBody(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/test.db", 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}, enableUI: true}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)
	mux.HandleFunc("/fake-endpoint", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ping":"pong"}`)
	})

	req := httptest.NewRequest(http.MethodPost, "/ui/whatever", nil)
	result, class := s.proxyAPI(httptest.NewRecorder(), req, "/fake-endpoint")
	if class != "" {
		t.Fatalf("expected success class, got %q", class)
	}
	if result != `{"ping":"pong"}` {
		t.Fatalf("expected fake endpoint body, got %q", result)
	}
}

// TestAPIAuditPaginated tests the /api/audit endpoint with pagination and filtering.
func TestAPIAuditPaginated(t *testing.T) {
	db, err := bolt.Open(t.TempDir()+"/test.db", 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Seed audit entries.
	err = db.Update(func(tx *bolt.Tx) error {
		b, _ := tx.CreateBucketIfNotExists([]byte("Audit"))
		if b == nil {
			return nil
		}
		entries := []struct {
			name     string
			endpoint string
			matched  bool
		}{
			{"alice", "enroll", true},
			{"bob", "recognize", true},
			{"charlie", "recognize", false},
			{"alice", "recognize", true},
			{"dave", "enroll", false},
		}
		for _, e := range entries {
			seq, _ := b.NextSequence()
			val := map[string]interface{}{
				"time":       "2026-01-01T00:00:00Z",
				"endpoint":   e.endpoint,
				"name":       e.name,
				"matched":    e.matched,
				"similarity": 0.9,
			}
			data, _ := json.Marshal(val)
			b.Put([]byte(fmt.Sprintf("%d", seq)), data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	s := &FaceServer{boltDB: db, dbMap: map[string]*storedUser{}, enableUI: true}
	mux := http.NewServeMux()
	s.RegisterHandlers(mux)

	// Default: 20 per page, should return all 5 entries.
	req := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("default: status %d", rec.Code)
	}
	var resp AuditPaginatedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("default: unmarshal error: %v", err)
	}
	if resp.TotalCount != 5 {
		t.Fatalf("expected total_count=5, got %d", resp.TotalCount)
	}
	if len(resp.Entries) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(resp.Entries))
	}
	if resp.Page != 1 || resp.PerPage != 20 || resp.TotalPages != 1 {
		t.Fatalf("unexpected pagination: page=%d per_page=%d total_pages=%d", resp.Page, resp.PerPage, resp.TotalPages)
	}

	// Page 1, per_page=2: should return 2 entries.
	req = httptest.NewRequest(http.MethodGet, "/api/audit?page=1&per_page=2", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page1: status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("page1: unmarshal error: %v", err)
	}
	if len(resp.Entries) != 2 {
		t.Fatalf("page1: expected 2 entries, got %d", len(resp.Entries))
	}
	if resp.TotalCount != 5 || resp.TotalPages != 3 {
		t.Fatalf("page1: total_count=%d total_pages=%d", resp.TotalCount, resp.TotalPages)
	}

	// Page 3, per_page=2: should return last 1 entry.
	req = httptest.NewRequest(http.MethodGet, "/api/audit?page=3&per_page=2", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page3: status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("page3: unmarshal error: %v", err)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("page3: expected 1 entry, got %d", len(resp.Entries))
	}

	// Filter by name "alice": should return 2 entries.
	req = httptest.NewRequest(http.MethodGet, "/api/audit?name=alice", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("name filter: status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("name filter: unmarshal error: %v", err)
	}
	if resp.TotalCount != 2 {
		t.Fatalf("name filter: expected total_count=2, got %d", resp.TotalCount)
	}

	// Filter by endpoint "enroll": should return 2 entries.
	req = httptest.NewRequest(http.MethodGet, "/api/audit?endpoint=enroll", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("endpoint filter: status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("endpoint filter: unmarshal error: %v", err)
	}
	if resp.TotalCount != 2 {
		t.Fatalf("endpoint filter: expected total_count=2, got %d", resp.TotalCount)
	}

	// Filter by matched "yes": should return 3 entries.
	req = httptest.NewRequest(http.MethodGet, "/api/audit?matched=yes", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("matched filter: status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("matched filter: unmarshal error: %v", err)
	}
	if resp.TotalCount != 3 {
		t.Fatalf("matched filter: expected total_count=3, got %d", resp.TotalCount)
	}

	// Filter by matched "no": should return 2 entries.
	req = httptest.NewRequest(http.MethodGet, "/api/audit?matched=no", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("matched=no filter: status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("matched=no filter: unmarshal error: %v", err)
	}
	if resp.TotalCount != 2 {
		t.Fatalf("matched=no filter: expected total_count=2, got %d", resp.TotalCount)
	}

	// Combined filter: name=alice + matched=yes → 2 entries (both alice entries are matched).
	req = httptest.NewRequest(http.MethodGet, "/api/audit?name=alice&matched=yes", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("combined filter: status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("combined filter: unmarshal error: %v", err)
	}
	if resp.TotalCount != 2 {
		t.Fatalf("combined filter: expected total_count=2, got %d", resp.TotalCount)
	}

	// No results: empty name filter.
	req = httptest.NewRequest(http.MethodGet, "/api/audit?name=nonexistent", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no results: status %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("no results: unmarshal error: %v", err)
	}
	if resp.TotalCount != 0 || len(resp.Entries) != 0 {
		t.Fatalf("no results: expected 0 entries, got %d", len(resp.Entries))
	}
}
