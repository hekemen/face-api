package http

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"h2hsecure.com/face/internal/domain"
)

// fakeFaceService implements domain.FaceService with call counters for the
// audit methods, used to verify UI handlers don't block on expensive scans.
type fakeFaceService struct {
	recentAuditCalls   int
	listUnmatchedCalls int
	users              []*domain.User
	deletePictureCalls []deletePictureCall
}

type deletePictureCall struct {
	name  string
	index int
}

func (f *fakeFaceService) EnrollImage(name string, imageData []byte) error { return nil }
func (f *fakeFaceService) RecognizeImage(imageData []byte) (*domain.RecognitionResult, error) {
	return nil, nil
}
func (f *fakeFaceService) CheckStream(rtspURL string) (*domain.StreamCheckResult, error) {
	return nil, nil
}
func (f *fakeFaceService) CheckStreamImage(imageData []byte) (*domain.StreamCheckResult, error) {
	return nil, nil
}
func (f *fakeFaceService) ListUsers() ([]*domain.User, error) { return f.users, nil }
func (f *fakeFaceService) DeleteUser(name string) error       { return nil }
func (f *fakeFaceService) DeletePicture(name string, index int) error {
	f.deletePictureCalls = append(f.deletePictureCalls, deletePictureCall{name: name, index: index})
	return nil
}
func (f *fakeFaceService) PromoteFromAudit(auditTime, name string) error {
	return nil
}
func (f *fakeFaceService) RecentAudit(n int) ([]domain.AuditEntry, error) {
	f.recentAuditCalls++
	return nil, nil
}
func (f *fakeFaceService) ListAuditPaginated(opts domain.ListPaginatedOpts) ([]domain.AuditEntry, int, []string, error) {
	return nil, 0, nil, nil
}
func (f *fakeFaceService) ListUnmatched(n int) ([]domain.AuditEntry, error) {
	f.listUnmatchedCalls++
	return nil, nil
}
func (f *fakeFaceService) ComputeStats() (domain.Stats, error) { return domain.Stats{}, nil }
func (f *fakeFaceService) GetAuditFace(timestamp string) (string, error) {
	return "", nil
}
func (f *fakeFaceService) ListRecognizeEntries(opts domain.ListRecognizeEntriesOpts) ([]domain.AuditEntry, int, error) {
	return nil, 0, nil
}
func (f *fakeFaceService) ComputeRecognizeStats() (domain.RecognizeSummaryResponse, error) {
	return domain.RecognizeSummaryResponse{}, nil
}

// TestHandleUIAuditRendersWithoutBlockingServiceCalls verifies the audit page
// renders without calling RecentAudit or ListUnmatched: the page renders
// client-side via fetch('../api/audit'), so server-side scans only delay the
// first byte (and, with a full-bucket ListUnmatched scan, the whole document).
func TestHandleUIAuditRendersWithoutBlockingServiceCalls(t *testing.T) {
	svc := &fakeFaceService{}
	h := NewUIHandler(svc, true, "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/ui/audit", nil)
	h.handleUIAudit(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String()[:min(len(w.Body.String()), 200)])
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="auditFilters"`) {
		t.Errorf("expected audit page markup, got: %s", body[:min(len(body), 200)])
	}
	if svc.recentAuditCalls != 0 {
		t.Errorf("handleUIAudit must not call RecentAudit (page renders client-side), got %d calls", svc.recentAuditCalls)
	}
	if svc.listUnmatchedCalls != 0 {
		t.Errorf("handleUIAudit must not call ListUnmatched (page renders client-side), got %d calls", svc.listUnmatchedCalls)
	}
}

// TestAuditPageFaceModalImageNoSrcResetLoop verifies the audit page's
// face-modal image does not use an onerror handler that resets src to the
// empty string. An <img src=""> resolves to the document's own URL; an onerror
// that sets src='' again re-triggers the image load on every error, an infinite
// loop that keeps the document's load event from ever firing. The symptom was
// the audit page sticking in the browser "loading" state (stop button visible)
// even though every API call and the client-side render succeeded.
func TestAuditPageFaceModalImageNoSrcResetLoop(t *testing.T) {
	svc := &fakeFaceService{}
	h := NewUIHandler(svc, true, "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/ui/audit", nil)
	h.handleUIAudit(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `onerror="this.src=''`) {
		t.Errorf("audit page must not reset img src to empty in onerror: <img src=\"\"> resolves to the document URL and re-setting src='' on error loops forever, blocking the load event")
	}
}

// TestAuditPageNoFaceFrameModal verifies the audit page renders the no-face
// "View frame" button and that the face modal uses a distinct title for
// no-face entries (the stored camera frame is not a face crop).
func TestAuditPageNoFaceFrameModal(t *testing.T) {
	svc := &fakeFaceService{}
	h := NewUIHandler(svc, true, "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/ui/audit", nil)
	h.handleUIAudit(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "no-face-link") {
		t.Errorf("audit page must render the no-face 'View frame' button (no-face-link)")
	}
	if !strings.Contains(body, "Original frame") {
		t.Errorf("audit page face modal must use an 'Original frame' title for no-face entries")
	}
}

// TestEnrollPageFormHiddenByDefault verifies the enroll page ships with the
// "add new user" form collapsed behind a toggle button (hidden by default on
// every load; no state persistence).
func TestEnrollPageFormHiddenByDefault(t *testing.T) {
	svc := &fakeFaceService{}
	h := NewUIHandler(svc, true, "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/ui/enroll", nil)
	h.handleUIEnroll(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="enrollFormWrap" hidden`) {
		t.Error("enroll form must be hidden by default (enrollFormWrap with hidden attribute)")
	}
	if !strings.Contains(body, `id="toggleEnrollForm"`) {
		t.Error("expected the '+ Add new user' toggle button (toggleEnrollForm)")
	}
}

// TestEnrollPageRendersPictureDeleteForms verifies each enrolled picture gets
// a delete form posting to the relative enroll/delete-picture endpoint with
// the user name and picture index.
func TestEnrollPageRendersPictureDeleteForms(t *testing.T) {
	svc := &fakeFaceService{
		users: []*domain.User{
			{Name: "alice", Pictures: []string{"p0", "p1"}, UpdatedAt: time.Now()},
			{Name: "bob", Pictures: nil, UpdatedAt: time.Now()},
		},
	}
	h := NewUIHandler(svc, true, "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/ui/enroll", nil)
	h.handleUIEnroll(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if got := strings.Count(body, `action="enroll/delete-picture"`); got != 2 {
		t.Errorf("expected 2 picture delete forms (alice has 2 pictures), got %d", got)
	}
	if !strings.Contains(body, `name="user_name" value="alice"`) {
		t.Error("picture delete form must carry the user name")
	}
	if !strings.Contains(body, `name="picture_index" value="0"`) || !strings.Contains(body, `name="picture_index" value="1"`) {
		t.Error("picture delete forms must carry picture_index 0 and 1")
	}
}

// TestEnrollPageDeletePicture verifies a delete_picture form POST calls
// DeletePicture with the parsed index and re-renders the page with a success
// result and the refreshed user list.
func TestEnrollPageDeletePicture(t *testing.T) {
	svc := &fakeFaceService{
		users: []*domain.User{
			{Name: "alice", Pictures: []string{"p0", "p1"}, UpdatedAt: time.Now()},
		},
	}
	h := NewUIHandler(svc, true, "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/ui/enroll/delete-picture",
		strings.NewReader("action=delete_picture&user_name=alice&picture_index=1"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.handleUIEnroll(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if len(svc.deletePictureCalls) != 1 || svc.deletePictureCalls[0].name != "alice" || svc.deletePictureCalls[0].index != 1 {
		t.Fatalf("DeletePicture calls = %+v, want [{alice 1}]", svc.deletePictureCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Deleted picture for user: alice") {
		t.Error("expected success result in re-rendered page")
	}
}

// TestRenderTemplateSetsContentLength verifies renderTemplate sets
// Content-Length so reverse proxies (Traefik) can complete the response
// without buffering the whole document as chunked.
func TestRenderTemplateSetsContentLength(t *testing.T) {
	svc := &fakeFaceService{}
	h := NewUIHandler(svc, true, "")

	w := httptest.NewRecorder()
	h.renderTemplate(w, "audit.html", uiPageData{GitVersion: h.gitVersion})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	cl := w.Header().Get("Content-Length")
	if cl == "" {
		t.Fatalf("expected Content-Length header, got none")
	}
	if cl != strconv.Itoa(w.Body.Len()) {
		t.Errorf("Content-Length %s does not match body length %d", cl, w.Body.Len())
	}
}

// TestServeStaticSetsContentLength verifies serveStatic sets Content-Length
// for the same reason as renderTemplate.
func TestServeStaticSetsContentLength(t *testing.T) {
	svc := &fakeFaceService{}
	h := NewUIHandler(svc, true, "")

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/ui/pico.min.css", nil)
	h.serveStatic(w, r, "templates/pico.min.css", "text/css; charset=utf-8")

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	cl := w.Header().Get("Content-Length")
	if cl == "" {
		t.Fatalf("expected Content-Length header, got none")
	}
	if cl != strconv.Itoa(w.Body.Len()) {
		t.Errorf("Content-Length %s does not match body length %d", cl, w.Body.Len())
	}
}