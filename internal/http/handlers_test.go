package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"h2hsecure.com/face/internal/domain"
)

// newSettingsTestMux builds a mux with the full handler set, backed by the
// given fake service (with a known threshold).
func newSettingsTestMux(svc *fakeFaceService) *http.ServeMux {
	h := NewHandlers(svc, "", nil, true, "", "", 10<<20, zerolog.Nop())
	mux := http.NewServeMux()
	h.RegisterHandlers(mux)
	return mux
}

// TestHandleSettingsGet verifies GET /settings returns the current threshold.
func TestHandleSettingsGet(t *testing.T) {
	svc := &fakeFaceService{threshold: 0.45}
	mux := newSettingsTestMux(svc)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/settings", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp domain.SettingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v (body: %s)", err, w.Body.String())
	}
	if resp.Threshold != 0.45 {
		t.Errorf("expected threshold 0.45, got %v", resp.Threshold)
	}
}

// TestHandleSettingsPostValid verifies POST /settings with a valid threshold
// updates it and reports the new value.
func TestHandleSettingsPostValid(t *testing.T) {
	svc := &fakeFaceService{threshold: 0.45}
	mux := newSettingsTestMux(svc)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader("threshold=0.5"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if svc.threshold != 0.5 {
		t.Errorf("expected service threshold 0.5, got %v", svc.threshold)
	}
	var resp domain.SettingsUpdateResult
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v (body: %s)", err, w.Body.String())
	}
	if resp.Status != "updated" {
		t.Errorf("expected status updated, got %q", resp.Status)
	}
	if resp.Threshold != 0.5 {
		t.Errorf("expected threshold 0.5 in response, got %v", resp.Threshold)
	}
}

// TestHandleSettingsPostInvalid verifies out-of-range values are rejected
// with 400 and leave the threshold unchanged.
func TestHandleSettingsPostInvalid(t *testing.T) {
	svc := &fakeFaceService{threshold: 0.45}
	mux := newSettingsTestMux(svc)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader("threshold=1.5"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body: %s)", w.Code, w.Body.String())
	}
	if svc.threshold != 0.45 {
		t.Errorf("threshold must stay 0.45 after invalid POST, got %v", svc.threshold)
	}
}

// TestHandleSettingsPostMissing verifies a missing threshold field is
// rejected with 400.
func TestHandleSettingsPostMissing(t *testing.T) {
	svc := &fakeFaceService{threshold: 0.45}
	mux := newSettingsTestMux(svc)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(""))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body: %s)", w.Code, w.Body.String())
	}
	if svc.threshold != 0.45 {
		t.Errorf("threshold must stay 0.45 after missing-field POST, got %v", svc.threshold)
	}
}