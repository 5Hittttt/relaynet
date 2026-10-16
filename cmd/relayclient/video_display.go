package main

import (
	"math"
	"sync/atomic"

	"fyne.io/fyne/v2"
)

// Layout runs on the UI thread. Publish physical pixel bounds so decoding
// goroutines do not access Fyne objects or upload unused full-size textures.
type videoDisplayLayout struct {
	base   fyne.Layout
	bounds *atomic.Uint64
	scale  func() float32
}

func (l *videoDisplayLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	return l.base.MinSize(objects)
}

func (l *videoDisplayLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	l.base.Layout(objects, size)
	scale := l.scale()
	w := uint32(math.Ceil(float64(size.Width * scale)))
	h := uint32(math.Ceil(float64(size.Height * scale)))
	if w > 0 && h > 0 {
		l.bounds.Store(uint64(w)<<32 | uint64(h))
	}
}

func loadDisplaySize(bounds *atomic.Uint64) (int, int) {
	size := bounds.Load()
	if size == 0 {
		return 1280, 720
	}
	return int(size >> 32), int(uint32(size))
}
