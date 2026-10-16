package main

import (
	"sync/atomic"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/layout"
)

func TestVideoDisplayTracksPhysicalSize(t *testing.T) {
	var bounds atomic.Uint64
	w, h := loadDisplaySize(&bounds)
	if w != 1280 || h != 720 {
		t.Fatalf("missing initial display budget: %dx%d", w, h)
	}
	scale := float32(1.5)
	l := &videoDisplayLayout{base: layout.NewStackLayout(), bounds: &bounds, scale: func() float32 { return scale }}
	l.Layout(nil, fyne.NewSize(640, 360))
	w, h = loadDisplaySize(&bounds)
	if w != 960 || h != 540 {
		t.Fatalf("display budget ignores window size or DPI: %dx%d", w, h)
	}
	scale = 2
	l.Layout(nil, fyne.NewSize(1920, 1080))
	w, h = loadDisplaySize(&bounds)
	if w != 3840 || h != 2160 {
		t.Fatalf("enlarging the window did not restore full detail: %dx%d", w, h)
	}
}
