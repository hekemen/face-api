package http

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// authCookieName is the name of the httpOnly cookie set after UI login.
const authCookieName = "face_api_auth"

// authCookieDuration is how long the UI auth cookie is valid.
const authCookieDuration = 24 * time.Hour

// --- authMiddleware ---

// authMiddleware wraps a handler with shared-secret token authentication.
// API routes (non-UI, non-health) check the X-Token header.
// UI routes check the httpOnly auth cookie.
// Health/readiness probes are always allowed.
type authMiddleware struct {
	token    string
	enableUI bool
	handler  http.Handler
}

// NewAuthMiddleware creates middleware that requires X-Token for API routes.
// When token is empty, auth is skipped (development mode).
func NewAuthMiddleware(handler http.Handler, token string, enableUI bool) *authMiddleware {
	return &authMiddleware{
		token:    token,
		enableUI: enableUI,
		handler:  handler,
	}
}

// ServeHTTP enforces authentication.
func (m *authMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Always allow health/readiness probes.
	if path == "/healthz" || path == "/readyz" {
		m.handler.ServeHTTP(w, r)
		return
	}

	// UI routes: verify httpOnly cookie.
	if m.enableUI && strings.HasPrefix(path, "/ui/") {
		if !m.checkCookieAuth(r) {
			if isXHR(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
				return
			}
			http.Redirect(w, r, "/ui/login", http.StatusFound)
			return
		}
		m.handler.ServeHTTP(w, r)
		return
	}

	// UI auth endpoints are always allowed (login form, login POST, logout).
	if m.enableUI && (path == "/ui/login" || path == "/ui/auth" || path == "/ui/logout") {
		m.handler.ServeHTTP(w, r)
		return
	}

	// API routes: check X-Token header.
	if m.token != "" {
		got := r.Header.Get("X-Token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(m.token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
			return
		}
	}

	m.handler.ServeHTTP(w, r)
}

// checkCookieAuth verifies the auth cookie value matches the token.
func (m *authMiddleware) checkCookieAuth(r *http.Request) bool {
	cookie, err := r.Cookie(authCookieName)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(m.token)) == 1
}

// --- maxSizeMiddleware ---

// maxSizeMiddleware wraps a handler with a maximum request body size limit.
type maxSizeMiddleware struct {
	maxBytes int64
	handler  http.Handler
}

// NewMaxSizeMiddleware creates a middleware that limits request body size.
func NewMaxSizeMiddleware(handler http.Handler, maxBytes int64) *maxSizeMiddleware {
	return &maxSizeMiddleware{
		maxBytes: maxBytes,
		handler:  handler,
	}
}

// ServeHTTP enforces the size limit on all requests.
func (m *maxSizeMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, m.maxBytes)
	m.handler.ServeHTTP(w, r)
}

// --- Shared helpers ---

// isXHR returns true if the request looks like an XHR/fetch call.
func isXHR(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json") ||
		strings.EqualFold(r.Header.Get("X-Requested-With"), "XMLHttpRequest")
}

// --- Auth UI handlers ---

// AuthUIHandler handles the UI login/logout flow.
type AuthUIHandler struct {
	token    string
	enableUI bool
}

// NewAuthUIHandler creates the auth UI handler.
func NewAuthUIHandler(token string, enableUI bool) *AuthUIHandler {
	return &AuthUIHandler{token: token, enableUI: enableUI}
}

// RegisterAuthHandlers attaches /ui/login, /ui/auth, /ui/logout to the mux.
func (h *AuthUIHandler) RegisterAuthHandlers(mux *http.ServeMux) {
	if !h.enableUI {
		return
	}
	mux.HandleFunc("GET /ui/login", h.handleLogin)
	mux.HandleFunc("POST /ui/auth", h.handleAuth)
	mux.HandleFunc("POST /ui/logout", h.handleLogout)
}

func (h *AuthUIHandler) handleLogin(w http.ResponseWriter, r *http.Request) {
	renderLoginForm(w)
}

func (h *AuthUIHandler) handleAuth(w http.ResponseWriter, r *http.Request) {
	gotPassword := strings.TrimSpace(r.FormValue("password"))
	if subtle.ConstantTimeCompare([]byte(gotPassword), []byte(h.token)) != 1 {
		renderLoginFormError(w, "Invalid password")
		return
	}

	cookie := &http.Cookie{
		Name:     authCookieName,
		Value:    h.token,
		Path:     "/ui",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(authCookieDuration.Seconds()),
	}
	http.SetCookie(w, cookie)
	http.Redirect(w, r, "/ui/", http.StatusFound)
}

func (h *AuthUIHandler) handleLogout(w http.ResponseWriter, r *http.Request) {
	cookie := &http.Cookie{
		Name:   authCookieName,
		Value:  "",
		Path:   "/ui",
		MaxAge: -1,
	}
	http.SetCookie(w, cookie)
	http.Redirect(w, r, "/ui/login", http.StatusFound)
}

// renderLoginForm renders the login page.
func renderLoginForm(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Face API - Login</title>
<link rel="stylesheet" href="pico.min.css">
<style>
body { display: flex; justify-content: center; align-items: center; min-height: 100vh; margin: 0; background: var(--pico-background-color); }
form { max-width: 320px; width: 100%%; }
</style>
</head>
<body>
<form method="POST" action="auth">
<h2>Face API Login</h2>
<label for="password">Password:</label>
<input type="password" id="password" name="password" required autofocus>
<button type="submit">Login</button>
</form>
</body>
</html>`)
}

// renderLoginFormError renders the login page with an error message.
func renderLoginFormError(w http.ResponseWriter, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Face API - Login</title>
<link rel="stylesheet" href="pico.min.css">
<style>
body { display: flex; justify-content: center; align-items: center; min-height: 100vh; margin: 0; background: var(--pico-background-color); }
form { max-width: 320px; width: 100%%; }
</style>
</head>
<body>
<form method="POST" action="auth">
<h2>Face API Login</h2>
<div class="result error">%s</div>
<label for="password">Password:</label>
<input type="password" id="password" name="password" required autofocus>
<button type="submit">Login</button>
</form>
</body>
</html>`, errMsg)
}
