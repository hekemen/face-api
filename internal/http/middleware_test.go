package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "test-token-123"

// buildAuthStack mirrors the production wiring in di.NewFaceAPI: auth UI
// routes + a protected UI page + a protected API route, all behind the auth
// middleware.
func buildAuthStack(basePath string) http.Handler {
	mux := http.NewServeMux()
	NewAuthUIHandler(testToken, true, basePath).RegisterAuthHandlers(mux)
	mux.HandleFunc("GET /ui/enroll", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /users", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return NewAuthMiddleware(mux, testToken, true, basePath)
}

func findCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestAuth_Login_WithBasePath verifies that with a configured base path, the
// login redirect and cookie path include it. This is the fix for the broken
// login flow behind Traefik stripPrefix: the ingress strips /face before the
// app sees it, so the base path must come from configuration, not the request
// path.
func TestAuth_Login_WithBasePath(t *testing.T) {
	handler := buildAuthStack("/face")

	req := httptest.NewRequest(http.MethodPost, "/ui/auth", strings.NewReader("password="+testToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/face/ui/" {
		t.Errorf("Location = %q, want %q", got, "/face/ui/")
	}
	c := findCookie(w, authCookieName)
	if c == nil {
		t.Fatal("expected auth cookie to be set")
	}
	if c.Path != "/face/ui" {
		t.Errorf("cookie Path = %q, want %q", c.Path, "/face/ui")
	}
	if !c.HttpOnly {
		t.Error("cookie should be HttpOnly")
	}
}

// TestAuth_Login_RootBehavior verifies that an empty base path keeps the
// existing root behavior (local dev, port-forward).
func TestAuth_Login_RootBehavior(t *testing.T) {
	handler := buildAuthStack("")

	req := httptest.NewRequest(http.MethodPost, "/ui/auth", strings.NewReader("password="+testToken))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/ui/" {
		t.Errorf("Location = %q, want %q", got, "/ui/")
	}
	c := findCookie(w, authCookieName)
	if c == nil {
		t.Fatal("expected auth cookie to be set")
	}
	if c.Path != "/ui" {
		t.Errorf("cookie Path = %q, want %q", c.Path, "/ui")
	}
}

// TestAuth_Login_WrongPassword renders the login form with an error and sets
// no cookie (regression guard).
func TestAuth_Login_WrongPassword(t *testing.T) {
	handler := buildAuthStack("/face")

	req := httptest.NewRequest(http.MethodPost, "/ui/auth", strings.NewReader("password=wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (login form with error), got %d", w.Code)
	}
	if c := findCookie(w, authCookieName); c != nil {
		t.Error("expected no cookie to be set for a wrong password")
	}
}

// TestAuth_UnauthenticatedUIPage_RedirectsToLogin verifies that an
// unauthenticated UI page request is redirected to the base-path-aware login
// URL.
func TestAuth_UnauthenticatedUIPage_RedirectsToLogin(t *testing.T) {
	handler := buildAuthStack("/face")

	req := httptest.NewRequest(http.MethodGet, "/ui/enroll", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/face/ui/login" {
		t.Errorf("Location = %q, want %q", got, "/face/ui/login")
	}
}

// TestAuth_UnauthenticatedUIPage_RootBehavior is the empty-base-path variant
// of TestAuth_UnauthenticatedUIPage_RedirectsToLogin.
func TestAuth_UnauthenticatedUIPage_RootBehavior(t *testing.T) {
	handler := buildAuthStack("")

	req := httptest.NewRequest(http.MethodGet, "/ui/enroll", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/ui/login" {
		t.Errorf("Location = %q, want %q", got, "/ui/login")
	}
}

// TestAuth_AuthenticatedUIPage_Passes verifies that a request carrying a
// valid auth cookie reaches the UI handler.
func TestAuth_AuthenticatedUIPage_Passes(t *testing.T) {
	handler := buildAuthStack("/face")

	req := httptest.NewRequest(http.MethodGet, "/ui/enroll", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: testToken})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid cookie, got %d", w.Code)
	}
}

// TestAuth_Logout_RewritesLocationAndClearsCookie verifies that logout
// redirects to the base-path-aware login URL and clears the cookie at the
// base-path-aware path.
func TestAuth_Logout_RewritesLocationAndClearsCookie(t *testing.T) {
	handler := buildAuthStack("/face")

	req := httptest.NewRequest(http.MethodPost, "/ui/logout", nil)
	req.AddCookie(&http.Cookie{Name: authCookieName, Value: testToken})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/face/ui/login" {
		t.Errorf("Location = %q, want %q", got, "/face/ui/login")
	}
	c := findCookie(w, authCookieName)
	if c == nil {
		t.Fatal("expected cookie-clearing response")
	}
	if c.Value != "" {
		t.Errorf("cleared cookie Value = %q, want empty", c.Value)
	}
	if c.Path != "/face/ui" {
		t.Errorf("cleared cookie Path = %q, want %q", c.Path, "/face/ui")
	}
}

// TestAuth_API_RequiresToken verifies that API token auth is unaffected by the
// base path (regression guard).
func TestAuth_API_RequiresToken(t *testing.T) {
	handler := buildAuthStack("/face")

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/users", nil)
	req.Header.Set("X-Token", testToken)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 with token, got %d", w.Code)
	}
}
