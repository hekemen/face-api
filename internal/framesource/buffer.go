// Package framesource provides a long-lived RTSP session that continuously
// decodes the camera sub-stream into a double-buffered latest-frame store.
package framesource

import (
	"sync"
	"time"

	"h2hsecure.com/face/internal/imaging"
)

// Buffer is a double-buffered store of the latest decoded camera frame.
//
// The writer fills the spare buffer outside the lock (Publish's fill callback
// runs lock-free and is responsible for sizing dst to the frame), then swaps
// cur/back under Lock() and bumps seq. Readers take RLock and copy cur.Pix
// into their own slice, so they never observe a torn frame and the lock is
// never held across a decode or a memcpy larger than one frame (~0.1 ms).
type Buffer struct {
	mu   sync.RWMutex
	cur  *imaging.RGBImage // latest fully published frame
	back *imaging.RGBImage // spare buffer, filled by the next Publish
	w, h int               // dimensions of cur
	seq  uint64            // increments on every successful publish
	at   time.Time         // wall-clock time of the latest publish
}

// NewBuffer returns an empty Buffer. Latest reports ok=false until the first
// Publish.
func NewBuffer() *Buffer {
	return &Buffer{}
}

// Publish fills the spare buffer with a new frame and publishes it.
//
// fill runs without the lock and writes into dst (the spare buffer), which it
// must size to the frame (dst.W/dst.H/dst.Pix) before writing pixels. The swap
// of cur/back happens under Lock(), so readers never observe a torn frame.
//
// The first call allocates cur and back; subsequent calls reuse them,
// reallocating dst.Pix only when the frame size changes.
func (b *Buffer) Publish(fill func(dst *imaging.RGBImage)) {
	b.mu.Lock()
	if b.cur == nil {
		b.cur = &imaging.RGBImage{}
		b.back = &imaging.RGBImage{}
	}
	dst := b.back
	b.mu.Unlock()

	// fill runs lock-free and writes the new frame into the spare buffer.
	fill(dst)

	b.mu.Lock()
	b.cur, b.back = b.back, b.cur
	b.w, b.h = dst.W, dst.H
	b.seq++
	b.at = time.Now()
	b.mu.Unlock()
}

// Dims returns the dimensions of the latest published frame, or 0,0 before the
// first Publish.
func (b *Buffer) Dims() (w, h int) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.w, b.h
}

// LastAt returns the publish time of the newest frame, or the zero time when
// no frame has been published yet.
func (b *Buffer) LastAt() time.Time {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.at
}

// Latest copies the latest frame into dst and reports its dimensions, sequence
// number and capture time. It returns ok=false when no frame has been published
// yet (seq == 0), or when len(dst) < w*h*3 (the caller must supply a big enough
// slice).
//
// dst is overwritten, not aliased: mutating it afterwards cannot affect the
// buffered frame or a later Latest call.
func (b *Buffer) Latest(dst []byte) (w, h int, seq uint64, at time.Time, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.seq == 0 {
		return 0, 0, 0, at, false
	}
	if len(dst) < b.w*b.h*3 {
		return b.w, b.h, 0, time.Time{}, false
	}
	copy(dst, b.cur.Pix)
	return b.w, b.h, b.seq, b.at, true
}
