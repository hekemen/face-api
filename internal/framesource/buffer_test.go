package framesource

import (
	"sync"
	"testing"

	"h2hsecure.com/face/internal/imaging"
)

// fillFrame sizes dst to w×h and fills every pixel with v.
func fillFrame(dst *imaging.RGBImage, w, h int, v byte) {
	dst.W, dst.H = w, h
	dst.Pix = make([]byte, w*h*3)
	for i := range dst.Pix {
		dst.Pix[i] = v
	}
}

func TestLatestBeforeFirstFrame(t *testing.T) {
	b := NewBuffer()
	if w, h := b.Dims(); w != 0 || h != 0 {
		t.Fatalf("Dims = %dx%d, want 0x0", w, h)
	}
	w, h, seq, at, ok := b.Latest(make([]byte, 100))
	if ok {
		t.Fatalf("Latest ok = true, want false")
	}
	if w != 0 || h != 0 || seq != 0 || !at.IsZero() {
		t.Fatalf("Latest = (%d,%d,%d,%v), want (0,0,0,zero)", w, h, seq, at)
	}
}

func TestPublishThenLatest(t *testing.T) {
	b := NewBuffer()
	b.Publish(func(dst *imaging.RGBImage) { fillFrame(dst, 4, 3, 7) })

	if w, h := b.Dims(); w != 4 || h != 3 {
		t.Fatalf("Dims = %dx%d, want 4x3", w, h)
	}

	dst := make([]byte, 4*3*3)
	w, h, seq, at, ok := b.Latest(dst)
	if !ok {
		t.Fatal("Latest ok = false, want true")
	}
	if w != 4 || h != 3 || seq != 1 {
		t.Fatalf("Latest = (%d,%d,%d), want (4,3,1)", w, h, seq)
	}
	if at.IsZero() {
		t.Error("at = zero time, want set")
	}
	for i, want := range dst {
		if want != 7 {
			t.Fatalf("dst[%d] = %d, want 7", i, want)
		}
	}
}

func TestLatestCopiesNotAliases(t *testing.T) {
	b := NewBuffer()
	b.Publish(func(dst *imaging.RGBImage) { fillFrame(dst, 4, 3, 7) })

	dst := make([]byte, 4*3*3)
	if _, _, _, _, ok := b.Latest(dst); !ok {
		t.Fatal("Latest ok = false, want true")
	}
	// Mutate the caller's slice; the buffered frame must be untouched.
	for i := range dst {
		dst[i] = 99
	}

	dst2 := make([]byte, 4*3*3)
	w, h, _, _, ok := b.Latest(dst2)
	if !ok || w != 4 || h != 3 {
		t.Fatalf("Latest = (%d,%d,_,_,%v), want (4,3,_,_,true)", w, h, ok)
	}
	for i, want := range dst2 {
		if want != 7 {
			t.Fatalf("dst2[%d] = %d, want 7 (buffer was aliased)", i, want)
		}
	}
}

func TestPublishNewDimensions(t *testing.T) {
	b := NewBuffer()
	b.Publish(func(dst *imaging.RGBImage) { fillFrame(dst, 4, 3, 1) })
	b.Publish(func(dst *imaging.RGBImage) { fillFrame(dst, 320, 240, 2) })

	if w, h := b.Dims(); w != 320 || h != 240 {
		t.Fatalf("Dims = %dx%d, want 320x240", w, h)
	}

	// A 4x3 dst is now too small.
	small := make([]byte, 4*3*3)
	if _, _, _, _, ok := b.Latest(small); ok {
		t.Fatal("Latest ok = true with small dst, want false")
	}

	// A big enough dst returns the new frame.
	big := make([]byte, 320*240*3)
	w, h, seq, _, ok := b.Latest(big)
	if !ok || w != 320 || h != 240 || seq != 2 {
		t.Fatalf("Latest = (%d,%d,%d,_,%v), want (320,240,2,_,true)", w, h, seq, ok)
	}
	for i, want := range big {
		if want != 2 {
			t.Fatalf("big[%d] = %d, want 2", i, want)
		}
	}
}

func TestLatestTooSmallDstReturnsFalse(t *testing.T) {
	b := NewBuffer()
	b.Publish(func(dst *imaging.RGBImage) { fillFrame(dst, 4, 3, 5) })

	// One byte short of 4*3*3.
	small := make([]byte, 4*3*3-1)
	w, h, _, _, ok := b.Latest(small)
	if ok {
		t.Fatal("Latest ok = true with undersized dst, want false")
	}
	if w != 4 || h != 3 {
		t.Fatalf("Latest reported %dx%d even though it returned ok=false", w, h)
	}
}

func TestConcurrentPublishAndLatest(t *testing.T) {
	b := NewBuffer()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for seq := uint64(0); seq < 2000; seq++ {
			v := byte(seq % 251)
			b.Publish(func(dst *imaging.RGBImage) { fillFrame(dst, 640, 480, v) })
		}
	}()

	// 4 concurrent readers.
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dst := make([]byte, 640*480*3)
			for i := 0; i < 500; i++ {
				w, h, _, _, ok := b.Latest(dst)
				if !ok {
					continue // no frame yet
				}
				if w != 640 || h != 480 {
					t.Errorf("Latest dims = %dx%d, want 640x480", w, h)
				}
				// Every pixel of a published frame must carry the same v.
				first := dst[0]
				for j := 1; j < len(dst); j++ {
					if dst[j] != first {
						t.Errorf("frame is not uniform: pixel %d = %d, first = %d", j, dst[j], first)
						return
					}
				}
			}
		}()
	}

	wg.Wait()
}
