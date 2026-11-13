package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"h2hsecure.com/face/internal/domain"
)

// The alignment mode is part of an enrollment's meaning: embeddings from
// different crop modes cannot be compared with each other. These tests pin
// that every path which writes a user record keeps the mode consistent with
// the embeddings it stores.

// TestEnrollImageRejectsMixedAlignmentSpace checks that enrolling a photo
// into a user whose record was made in another alignment space is refused.
//
// Mixing the two would put embeddings from both spaces in one record, and the
// matcher would then compare a bbox enrollment against an arcface5 capture —
// the meaningless comparison the alignment bookkeeping exists to prevent.
func TestEnrollImageRejectsMixedAlignmentSpace(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0}
	legacy := &domain.User{
		Name:       "alice",
		Embeddings: []domain.FaceEmbedding{emb},
		Pictures:   []string{"old"},
		// No alignment: the record predates the field, so it is a bbox
		// enrollment.
	}
	users := &burstUserRepo{users: []*domain.User{legacy}}
	audit := &mockAuditRepo{}
	proc := &burstProcessor{crop: &domain.FaceCrop{Data: []byte("crop"), Embedding: emb}}
	s := New(users, audit, proc, 0.45)
	s.alignment = "arcface5"

	err := s.EnrollImage("alice", []byte("image"))
	if err == nil {
		t.Fatal("enrolling into a user from another alignment space was allowed")
	}
	if !isMixedSpaceError(err) {
		t.Fatalf("error = %v, want a mixed-alignment error", err)
	}
}

// TestEnrollImageAllowsSameAlignmentSpace checks that adding a photo to a
// user in the same space still works.
func TestEnrollImageAllowsSameAlignmentSpace(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0}
	existing := &domain.User{
		Name:       "alice",
		Embeddings: []domain.FaceEmbedding{emb},
		Pictures:   []string{"old"},
		Alignment:  "arcface5",
	}
	users := &burstUserRepo{users: []*domain.User{existing}}
	audit := &mockAuditRepo{}
	proc := &burstProcessor{crop: &domain.FaceCrop{Data: []byte("crop"), Embedding: emb}}
	s := New(users, audit, proc, 0.45)
	s.alignment = "arcface5"

	if err := s.EnrollImage("alice", []byte("image")); err != nil {
		t.Fatalf("EnrollImage: %v", err)
	}
}

// TestDeletePictureKeepsAlignment checks that deleting a photo does not lose
// the user's alignment mode.
//
// Losing it would make the record look like a legacy bbox enrollment, and
// under arcface5 the user would stop matching entirely.
func TestDeletePictureKeepsAlignment(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0}
	user := &domain.User{
		Name:       "alice",
		Embeddings: []domain.FaceEmbedding{emb, emb},
		Pictures:   []string{"a", "b"},
		Alignment:  "arcface5",
	}
	users := &burstUserRepo{users: []*domain.User{user}}
	audit := &mockAuditRepo{}
	proc := &burstProcessor{crop: &domain.FaceCrop{Embedding: emb}}
	s := New(users, audit, proc, 0.45)
	s.alignment = "arcface5"

	if err := s.DeletePicture("alice", 0); err != nil {
		t.Fatalf("DeletePicture: %v", err)
	}
	got, err := users.GetByName("alice")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.Alignment != "arcface5" {
		t.Fatalf("Alignment = %q after deleting a picture, want arcface5", got.Alignment)
	}
	if len(got.Embeddings) != 1 || len(got.Pictures) != 1 {
		t.Fatalf("after deleting a picture: %d embeddings, %d pictures, want 1 and 1",
			len(got.Embeddings), len(got.Pictures))
	}
}

// TestPromoteFromAuditStampsAlignment checks that a user created by
// promoting an audit entry records the active alignment mode.
//
// The audit entry's embedding was produced in the active space, so a promoted
// user has to say so, or it is treated as a legacy bbox enrollment and
// skipped.
func TestPromoteFromAuditStampsAlignment(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0}
	users := &burstUserRepo{}
	audit := &mockAuditRepo{entries: []domain.AuditEntry{{
		Time:       time.Now(),
		Endpoint:   "stream-check",
		Name:       "",
		Similarity: 0.3,
		Embedding:  emb,
	}}}
	proc := &burstProcessor{crop: &domain.FaceCrop{Embedding: emb}}
	s := New(users, audit, proc, 0.45)
	s.alignment = "arcface5"

	ts := audit.entries[0].Time.Format(time.RFC3339)
	if err := s.PromoteFromAudit(ts, "alice"); err != nil {
		t.Fatalf("PromoteFromAudit: %v", err)
	}
	got, err := users.GetByName("alice")
	if err != nil {
		t.Fatalf("GetByName: %v", err)
	}
	if got.Alignment != "arcface5" {
		t.Fatalf("Alignment = %q after promotion, want arcface5", got.Alignment)
	}
}

// TestScanBurstReportsFaceWithoutComparableUser checks that a detected face
// is not reported as "no face detected" when there is simply no user
// enrolled in the active alignment space.
//
// The two are different situations: one means nobody was at the camera, the
// other means the users have to be re-enrolled. Conflating them sends the
// operator to debug the camera instead.
func TestScanBurstReportsFaceWithoutComparableUser(t *testing.T) {
	emb := domain.FaceEmbedding{1, 0, 0}
	legacy := &domain.User{
		Name:       "alice",
		Embeddings: []domain.FaceEmbedding{emb},
		Alignment:  "bbox",
	}
	users := &burstUserRepo{users: []*domain.User{legacy}}
	audit := &mockAuditRepo{}
	proc := &burstProcessor{crop: &domain.FaceCrop{Data: []byte("crop"), Embedding: emb}}
	grabber := &burstGrabber{w: 640, h: 480}
	s := New(users, audit, proc, 0.45).WithFrameGrabber(grabber).WithBurst(200*time.Millisecond, 20*time.Millisecond)

	res, err := s.ScanBurst(context.Background())
	if err != nil {
		t.Fatalf("ScanBurst: %v", err)
	}
	if res.Reason == "No face detected" {
		t.Fatalf("Reason = %q, want a reason about the users not being enrolled in the active space", res.Reason)
	}
	if len(audit.entries) != 1 {
		t.Fatalf("expected 1 audit row, got %d", len(audit.entries))
	}
	if audit.entries[0].Status == domain.AuditStatusNoFace {
		t.Error("audit status = no_face, want a face-detected status")
	}
}

// isMixedSpaceError reports whether err says the user was enrolled in
// another alignment space.
func isMixedSpaceError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "alignment")
}
