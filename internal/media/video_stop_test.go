package media

import (
	"testing"
	"time"
)

func TestVideoStopJoinsCaptureBeforeDestroyingEncoder(t *testing.T) {
	vc := NewVideoCapturer(0, VideoQuality{}, CaptureMethodDXGI, nil)
	vc.running = true
	vc.h264 = &H264Encoder{} // Ownership marker; no native allocation needed.
	vc.wg.Add(1)
	done := make(chan struct{})
	go func() { vc.Stop(); close(done) }()
	select {
	case <-done:
		vc.wg.Done()
		t.Fatal("Stop returned while capture still owns the encoder")
	case <-time.After(30 * time.Millisecond):
	}
	vc.wg.Done()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not join finished capture")
	}
}
