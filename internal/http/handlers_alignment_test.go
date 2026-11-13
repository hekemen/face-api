package http

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"h2hsecure.com/face/internal/domain"
)

// TestListUsersReportsAlignment verifies that GET /users reports the
// alignment mode each user was enrolled with.
//
// The operator needs this to know who still has to be re-enrolled after a
// change of alignment mode: a user enrolled in another space is skipped by
// the matcher, so it would silently stop matching.
func TestListUsersReportsAlignment(t *testing.T) {
	svc := &fakeFaceService{
		users: []*domain.User{
			{Name: "alice", Pictures: []string{"a"}, Alignment: "arcface5"},
			{Name: "bob", Pictures: []string{"b"}, Alignment: "bbox"},
			{Name: "carol", Pictures: []string{"c"}}, // legacy: unknown mode
		},
	}
	mux := http.NewServeMux()
	NewHandlers(svc, "", nil, false, "", "", 0, zerolog.Nop()).RegisterHandlers(mux)

	req := httptest.NewRequest(http.MethodGet, "/users", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Users []struct {
			Name      string `json:"name"`
			Alignment string `json:"alignment"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Users) != 3 {
		t.Fatalf("got %d users, want 3", len(resp.Users))
	}
	want := map[string]string{"alice": "arcface5", "bob": "bbox", "carol": ""}
	for _, u := range resp.Users {
		if u.Alignment != want[u.Name] {
			t.Errorf("user %s alignment = %q, want %q", u.Name, u.Alignment, want[u.Name])
		}
	}
}
