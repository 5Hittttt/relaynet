package media

import (
	"os"
	"testing"
	"time"
)

func TestCaptureQueueKeepsLatestRawPicture(t *testing.T) {
	w := &WgcCapturer{rawFrames: make(chan rawCaptureFrame, 1)}
	done := make(chan struct{})
	go func() {
		for pts := int64(0); pts < 100; pts++ {
			w.queueRaw(rawCaptureFrame{pixels: []byte{byte(pts)}, timestampNS: pts})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("slow encoder blocks screen capture")
	}
	frame := <-w.rawFrames
	if frame.timestampNS != 99 || frame.pixels[0] != 99 || len(w.rawFrames) != 0 {
		t.Fatal("capture queues old pictures instead of retaining the latest raw picture")
	}
}

func TestCaptureEncoderUsesMTA(t *testing.T) {
	if os.Getenv("RELAYNET_REQUIRE_HW_CODEC") != "1" {
		t.Skip("requires asynchronous GPU H264 encoder")
	}
	apartment := make(chan [2]int, 1)
	w := &WgcCapturer{
		quality:     VideoQuality{Width: 320, Height: 240, FPS: 120, Bitrate: 2000},
		rawFrames:   make(chan rawCaptureFrame, 1),
		encoderStop: make(chan struct{}),
		encoderDone: make(chan struct{}),
		callback: func(VideoFrame) {
			kind, qualifier := currentMediaApartment()
			apartment <- [2]int{kind, qualifier}
		},
	}
	go w.encodeLoop()
	defer func() { close(w.encoderStop); <-w.encoderDone }()
	w.queueRaw(rawCaptureFrame{pixels: make([]byte, 320*240*4), width: 320, height: 240})
	select {
	case kind := <-apartment:
		if kind != [2]int{1, 0} { // APTTYPE_MTA, APTTYPEQUALIFIER_NONE (explicit MTA)
			t.Fatalf("encoder worker lacks its own explicit MTA: apartment=%v", kind)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("encoder worker produced no frame")
	}
}

func TestCaptureCadenceUsesPictureTimestamp(t *testing.T) {
	w := &WgcCapturer{quality: VideoQuality{FPS: 120}}
	for frame := int64(0); frame < 20; frame++ {
		pts := frame * int64(time.Second) / 120
		// A late readback can deliver neighbouring pictures close together.
		if !w.acceptCapture(pts) {
			t.Fatalf("valid captured picture dropped because callback arrived quickly: frame=%d", frame)
		}
		if w.acceptCapture(pts) {
			t.Fatal("duplicate picture was accepted")
		}
	}
	w = &WgcCapturer{quality: VideoQuality{FPS: 60}}
	for frame := int64(0); frame < 20; frame++ {
		if accepted := w.acceptCapture(frame * int64(time.Second) / 120); accepted != (frame%2 == 0) {
			t.Fatalf("60 FPS selection was ignored: frame=%d accepted=%v", frame, accepted)
		}
	}
}
