package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"h2hsecure.com/face/internal/framesource"
)

// re enrollFlowTest exercises the whole re-enroll flow against a fake API
// and a mock camera, without a person at the door.
//
// It covers the two properties the flow has to have: the user ends up with
// the new photos, and a rejected photo leaves the existing record alone.

// fakeUserStore is a minimal stand-in for the face API's user store.
type fakeUserStore struct {
	photos map[string][][]byte
	// reject makes the API reject any photo whose first byte is 0, standing
	// in for a frame with no detectable face.
	reject bool
	// cap is the number of photos a user may hold.
	cap int
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{photos: map[string][][]byte{}, cap: 5}
}

func (f *fakeUserStore) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/enroll":
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			name := r.FormValue("name")
			file, _, err := r.FormFile("image")
			if err != nil {
				http.Error(w, "no image", http.StatusBadRequest)
				return
			}
			defer file.Close()
			photo, _ := io.ReadAll(file)
			if f.reject && len(photo) > 0 && photo[0] == 0 {
				http.Error(w, "face detection: no face detected", http.StatusBadRequest)
				return
			}
			if len(f.photos[name]) >= f.cap {
				http.Error(w, "maximum pictures per user", http.StatusBadRequest)
				return
			}
			f.photos[name] = append(f.photos[name], photo)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"status": "enrolled", "name": name})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/users/"):
			name := strings.TrimPrefix(r.URL.Path, "/users/")
			delete(f.photos, name)
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]any{"status": "deleted", "name": name})

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestReenrollFlowReplacesPhotos checks the whole flow: capture from the
// mock camera, then replace the user's photos.
func TestReenrollFlowReplacesPhotos(t *testing.T) {
	photo := "../../test_hopkins_1.jpg"
	if _, err := os.Stat(photo); err != nil {
		t.Skipf("missing fixture: %v", err)
	}
	stream := startMockCam(t, photo)

	store := newFakeUserStore()
	store.photos["ali"] = [][]byte{{9, 9}, {9, 9}}
	srv := store.serve(t)

	// Capture from the mock camera, then replace the user's photos.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	grabber := framesource.NewGrabber(stream, zerolog.Nop())
	_, _, pix, err := grabber.Grab(ctx)
	if err != nil {
		t.Fatalf("Grab: %v", err)
	}
	frames := [][]byte{pix}

	if err := replaceEnrollment(ctx, srv.URL, "token", "ali", frames); err != nil {
		t.Fatalf("replaceEnrollment: %v", err)
	}

	got := store.photos["ali"]
	if len(got) != len(frames) {
		t.Fatalf("user holds %d photos, want %d", len(got), len(frames))
	}
	for i, want := range frames {
		if !bytes.Equal(got[i], want) {
			t.Errorf("photo %d differs from the captured frame", i)
		}
	}
}

// TestReenrollFlowKeepsPhotosOnRejection checks that a photo the API
// rejects leaves the existing photos alone.
func TestReenrollFlowKeepsPhotosOnRejection(t *testing.T) {
	store := newFakeUserStore()
	store.reject = true
	store.photos["ali"] = [][]byte{{9, 9}}
	srv := store.serve(t)

	frames := [][]byte{{1, 1}, {0, 0}} // the second has no face in it
	if err := replaceEnrollment(context.Background(), srv.URL, "token", "ali", frames); err == nil {
		t.Fatal("expected an error when a photo is rejected")
	}
	got := store.photos["ali"]
	if len(got) != 1 || !bytes.Equal(got[0], []byte{9, 9}) {
		t.Fatalf("user's photos = %v, want the original one", got)
	}
}
