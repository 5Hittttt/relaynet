package media

import (
	"testing"
)

func TestWGCOutputLifecycle(t *testing.T) {
	parent := &WgcCapturer{running: true}
	stop := parent.AddOutput(VideoQuality{Width: 320, Height: 180, FPS: 30, Bitrate: 500}, func(VideoFrame) {})
	if len(parent.outputs) != 1 {
		t.Fatal("output did not share parent capture")
	}
	var child *WgcCapturer
	for output := range parent.outputs {
		child = output
	}
	stop()
	stop()
	if len(parent.outputs) != 0 {
		t.Fatal("unused output remains attached")
	}
	select {
	case <-child.encoderDone:
	default:
		t.Fatal("output COM worker was not joined")
	}
}
