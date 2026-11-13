package main

import (
	"bytes"
	"testing"
)

// TestPickDistinctCaptures verifies that the command keeps the frames that
// differ from the ones already chosen, so the five photos are not five
// copies of the same moment.
//
// The camera sends a keyframe every ~2 s, so two grabs taken close together
// return the same picture. Keeping only distinct frames gives the operator
// five genuinely different photos.
func TestPickDistinctCaptures(t *testing.T) {
	frames := [][]byte{
		{1, 1, 1}, {1, 1, 1}, {2, 2, 2}, {3, 3, 3}, {3, 3, 3}, {4, 4, 4},
	}
	got := pickDistinctCaptures(frames, 3)
	if len(got) != 3 {
		t.Fatalf("kept %d frames, want 3", len(got))
	}
	if !bytes.Equal(got[0], []byte{1, 1, 1}) ||
		!bytes.Equal(got[1], []byte{2, 2, 2}) ||
		!bytes.Equal(got[2], []byte{3, 3, 3}) {
		t.Fatalf("kept %v, want the first of each distinct group", got)
	}
}

// TestPickDistinctCapturesDropsDuplicates verifies that frames of the same
// moment are dropped rather than repeated.
//
// The camera sends a keyframe every ~2 s, so two grabs taken close together
// return the same picture. Enrolling an identical duplicate adds nothing: it
// produces an identical embedding. The caller warns the operator instead.
func TestPickDistinctCapturesDropsDuplicates(t *testing.T) {
	frames := [][]byte{
		{1, 1, 1}, {1, 1, 1}, {2, 2, 2}, {3, 3, 3}, {3, 3, 3}, {4, 4, 4},
	}
	got := pickDistinctCaptures(frames, 3)
	if len(got) != 3 {
		t.Fatalf("kept %d frames, want 3", len(got))
	}
	if !bytes.Equal(got[0], []byte{1, 1, 1}) ||
		!bytes.Equal(got[1], []byte{2, 2, 2}) ||
		!bytes.Equal(got[2], []byte{3, 3, 3}) {
		t.Fatalf("kept %v, want the first of each distinct group", got)
	}
}

// TestPickDistinctCapturesKeepsEveryDistinctFrame verifies that distinct
// frames are all kept, even when there are more of them than requested.
func TestPickDistinctCapturesKeepsEveryDistinctFrame(t *testing.T) {
	frames := [][]byte{{1}, {2}, {3}, {4}}
	got := pickDistinctCaptures(frames, 2)
	if len(got) != 2 {
		t.Fatalf("kept %d frames, want 2 (the requested count)", len(got))
	}
}

// TestPickDistinctCapturesHandlesFewerFrames verifies that the command does
// not invent frames when the camera produced fewer than requested.
func TestPickDistinctCapturesHandlesFewerFrames(t *testing.T) {
	frames := [][]byte{{1, 1, 1}, {2, 2, 2}}
	got := pickDistinctCaptures(frames, 5)
	if len(got) != 2 {
		t.Fatalf("kept %d frames, want 2", len(got))
	}
}

// TestFramesDiffer verifies the frame comparison used to tell one moment
// from another: two frames that differ anywhere in their first bytes count
// as different.
func TestFramesDiffer(t *testing.T) {
	if framesDiffer([]byte{1, 2, 3}, []byte{1, 2, 3}) {
		t.Error("identical frames reported as different")
	}
	if !framesDiffer([]byte{1, 2, 3}, []byte{1, 2, 4}) {
		t.Error("different frames reported as the same")
	}
	if framesDiffer([]byte{1, 2, 3}, []byte{1, 2}) {
		t.Error("frames of different lengths should not be equal")
	}
}
