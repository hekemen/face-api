package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeAPI mimics the face API's user store: a POST /enroll appends a photo
// (up to the cap), a DELETE /users/{name} removes the whole record, and a
// photo without a detectable face is rejected.
type fakeAPI struct {
	t         *testing.T
	ops       []string
	photos    map[string][][]byte
	maxPhotos int
	// rejectPhotos makes the API reject a photo whose first byte is 0,
	// standing in for a frame with no face in it.
	rejectPhotos bool
}

func newFakeAPI(t *testing.T) *fakeAPI {
	return &fakeAPI{
		t:            t,
		photos:       map[string][][]byte{},
		maxPhotos:    5,
		rejectPhotos: true,
	}
}

func (f *fakeAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/enroll":
			name, photo, err := readEnroll(r)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			f.ops = append(f.ops, "enroll:"+name)
			if f.rejectPhotos && len(photo) > 0 && photo[0] == 0 {
				http.Error(w, "face detection: no face detected", http.StatusBadRequest)
				return
			}
			if len(f.photos[name]) >= f.maxPhotos {
				http.Error(w, "maximum 5 pictures per user", http.StatusBadRequest)
				return
			}
			f.photos[name] = append(f.photos[name], photo)
			writeJSON(w, http.StatusCreated, map[string]any{"status": "enrolled", "name": name})

		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/users/"):
			name := strings.TrimPrefix(r.URL.Path, "/users/")
			f.ops = append(f.ops, "delete:"+name)
			delete(f.photos, name)
			writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "name": name})

		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func readEnroll(r *http.Request) (string, []byte, error) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		return "", nil, err
	}
	name := r.FormValue("name")
	file, _, err := r.FormFile("image")
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	photo, err := io.ReadAll(file)
	if err != nil {
		return "", nil, err
	}
	return name, photo, nil
}

// TestReplaceEnrollmentEndsWithThePhotos verifies that after a re-enrollment
// the user holds exactly the new photos.
//
// It catches the ordering bug this command once had: enrolling a photo before
// deleting the old record meant the delete removed the photo that had just
// been enrolled, leaving the user with nothing.
func TestReplaceEnrollmentEndsWithThePhotos(t *testing.T) {
	api := newFakeAPI(t)
	api.photos["ali"] = [][]byte{{9, 9}, {9, 9}, {9, 9}}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	frames := [][]byte{{1, 1}, {2, 2}, {3, 3}}
	err := replaceEnrollment(context.Background(), srv.URL, "token", "ali", frames)
	if err != nil {
		t.Fatalf("replaceEnrollment: %v", err)
	}

	got := api.photos["ali"]
	if len(got) != len(frames) {
		t.Fatalf("user holds %d photos, want %d (ops: %v)", len(got), len(frames), api.ops)
	}
	for i, want := range frames {
		if !bytes.Equal(got[i], want) {
			t.Errorf("photo %d = %v, want %v", i, got[i], want)
		}
	}
}

// TestReplaceEnrollmentKeepsOldRecordOnFailure verifies that a photo the API
// rejects leaves the existing enrollment untouched.
//
// The old photos are in the previous alignment space and cannot be matched,
// but they are still the user's data: a failed re-enrollment must not
// destroy them.
func TestReplaceEnrollmentKeepsOldRecordOnFailure(t *testing.T) {
	api := newFakeAPI(t)
	api.photos["ali"] = [][]byte{{9, 9}, {9, 9}}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	// A photo starting with 0 stands in for a frame with no face in it.
	frames := [][]byte{{1, 1}, {0, 0}}
	err := replaceEnrollment(context.Background(), srv.URL, "token", "ali", frames)
	if err == nil {
		t.Fatal("expected an error when a photo is rejected")
	}

	got := api.photos["ali"]
	if len(got) != 2 || !bytes.Equal(got[0], []byte{9, 9}) {
		t.Fatalf("user's photos = %v, want the two original ones (ops: %v)", got, api.ops)
	}
}

// TestReplaceEnrollmentDeletesBeforeEnrolling verifies the operation order:
// the old record has to be removed before the new photos are added, because
// enrolling appends and the user holds at most a fixed number of photos.
func TestReplaceEnrollmentDeletesBeforeEnrolling(t *testing.T) {
	api := newFakeAPI(t)
	api.photos["ali"] = [][]byte{{9, 9}, {9, 9}, {9, 9}, {9, 9}, {9, 9}}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	frames := [][]byte{{1, 1}, {2, 2}}
	err := replaceEnrollment(context.Background(), srv.URL, "token", "ali", frames)
	if err != nil {
		t.Fatalf("replaceEnrollment: %v", err)
	}

	if len(api.photos["ali"]) != 2 {
		t.Fatalf("user holds %d photos, want 2 (ops: %v)", len(api.photos["ali"]), api.ops)
	}
	deleteIdx, firstEnroll := -1, -1
	for i, op := range api.ops {
		if op == "delete:ali" && deleteIdx < 0 {
			deleteIdx = i
		}
		if op == "enroll:ali" && firstEnroll < 0 {
			firstEnroll = i
		}
	}
	if deleteIdx < 0 {
		t.Fatalf("no delete in %v", api.ops)
	}
	if firstEnroll < 0 {
		t.Fatalf("no enroll in %v", api.ops)
	}
	if deleteIdx > firstEnroll {
		t.Fatalf("enroll happened before the delete (ops: %v)", api.ops)
	}
}

// TestReplaceEnrollmentVerifiesBeforeDeleting verifies that every photo is
// checked against the API before the old record is removed.
//
// Enrolling appends, so the old record has to be deleted before the new
// photos can be added — but deleting first would leave the user with nothing
// if a photo then turned out to be unusable. Verifying the photos first
// avoids that.
func TestReplaceEnrollmentVerifiesBeforeDeleting(t *testing.T) {
	api := newFakeAPI(t)
	api.photos["ali"] = [][]byte{{9, 9}}
	srv := httptest.NewServer(api.handler())
	defer srv.Close()

	frames := [][]byte{{1, 1}, {0, 0}} // the second is unusable
	err := replaceEnrollment(context.Background(), srv.URL, "token", "ali", frames)
	if err == nil {
		t.Fatal("expected an error when a photo is rejected")
	}
	for _, op := range api.ops {
		if op == "delete:ali" {
			t.Fatalf("the old record was deleted even though a photo was rejected (ops: %v)", api.ops)
		}
	}
	if len(api.photos["ali"]) != 1 {
		t.Fatalf("user's photos = %v, want the original one", api.photos["ali"])
	}
}

var _ = fmt.Sprintf
