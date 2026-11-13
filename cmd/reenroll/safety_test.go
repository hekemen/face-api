package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The verify step enrolls the photos into a scratch user. That user must not
// survive the command: it is a fully enrolled, matchable identity, and a
// leftover one would be reported by the matcher and would break the next run
// by filling the scratch user's picture cap.

// TestVerifyPhotosRemovesTheScratchUser checks that the scratch user is gone
// after a successful verification.
func TestVerifyPhotosRemovesTheScratchUser(t *testing.T) {
	api := newFakeAPI(t)
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	frames := [][]byte{{1, 1}, {2, 2}}
	if err := verifyPhotos(context.Background(), srv.URL, "token", "ali", frames); err != nil {
		t.Fatalf("verifyPhotos: %v", err)
	}
	if len(api.photos["ali-reenroll-verify"]) != 0 {
		t.Fatalf("the scratch user still holds %d photos (ops: %v)",
			len(api.photos["ali-reenroll-verify"]), api.ops)
	}
}

// TestVerifyPhotosRemovesTheScratchUserOnFailure checks that the scratch
// user is removed even when a photo is rejected.
func TestVerifyPhotosRemovesTheScratchUserOnFailure(t *testing.T) {
	api := newFakeAPI(t)
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	frames := [][]byte{{1, 1}, {0, 0}} // the second is rejected
	if err := verifyPhotos(context.Background(), srv.URL, "token", "ali", frames); err == nil {
		t.Fatal("expected an error when a photo is rejected")
	}
	deleted := false
	for _, op := range api.ops {
		if op == "delete:ali-reenroll-verify" {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("the scratch user was not removed after a failure (ops: %v)", api.ops)
	}
}

// TestAPICallsHonourTheContext checks that the API calls stop when the
// context is cancelled.
//
// Without this a half-open connection to the API leaves the command hanging
// forever, stuck in the delete-then-enroll window where interrupting it
// leaves the user with no photos.
func TestAPICallsHonourTheContext(t *testing.T) {
	// A server that never answers, so the request can only end by the
	// context being cancelled.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := enrollOne(ctx, srv.URL, "token", "ali", []byte{1, 1})
	if err == nil {
		t.Fatal("expected the request to fail when the context expired")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the request took %v, so the context was not honoured", elapsed)
	}
}
