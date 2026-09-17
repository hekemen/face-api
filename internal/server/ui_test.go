package server

import (
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

	for _, page := range []string{"/ui/", "/ui/enroll", "/ui/recognize", "/ui/stream-check", "/ui/stats", "/ui/audit"} {
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

	for _, page := range []string{"/ui/", "/ui/enroll", "/ui/recognize", "/ui/stream-check", "/ui/stats", "/ui/audit"} {
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

	// Stream-check has no image input → must not include camera wiring.
	req := httptest.NewRequest(http.MethodGet, "/ui/stream-check", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "getUserMedia") {
		t.Fatal("/ui/stream-check should not include camera script")
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

	// /stream-check with no rtsp_url and no RTSP_URL config → 400 from the API,
	// which must arrive as the UI's "error" result (proving in-process dispatch).
	req := httptest.NewRequest(http.MethodPost, "/ui/stream-check", nil)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "error") || !strings.Contains(body, "status") {
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
