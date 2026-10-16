package media

import (
	"math"
	"testing"
	"time"
)

func TestScreenLateViewerSkipsExpiredPictures(t *testing.T) {
	tl := NewScreenTimeline()
	now := time.Unix(0, 0)
	tl.ObserveVideo(VideoFrame{StreamID: 1, HasAudio: true}, now)
	queueAudio(tl, 1, int64(2*time.Second), 100, now)
	window, ok := any(tl).(interface{ VideoDiscardBefore(uint64, time.Time) int64 })
	if !ok {
		t.Fatal("late viewer cannot discard expired pictures while retaining reference frames")
	}
	if pts := window.VideoDiscardBefore(1, now); pts != int64(2*time.Second) {
		t.Fatalf("wrong initial playback window: %s", time.Duration(pts))
	}
}

func TestH264DiscardKeepsReferenceChain(t *testing.T) {
	enc, err := NewH264Encoder(320, 240, 320, 240, 60, 2000)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := NewH264Decoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	pixels := make([]byte, 320*240*4)
	for frame := 0; frame < 12; frame++ {
		value := byte(40 + frame*10)
		for i := 0; i < len(pixels); i += 4 {
			pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = value, value, value, 255
		}
		pts := int64(frame) * int64(17*time.Millisecond)
		data, _, encodedPTS, err := enc.SendAt(pixels, 320*4, true, pts)
		if err != nil || len(data) == 0 {
			t.Fatalf("encode failed: %v", err)
		}
		if frame < 11 {
			if err = dec.SkipAt(data, encodedPTS); err != nil {
				t.Fatal(err)
			}
			continue
		}
		rgba, w, h, decodedPTS, err := dec.SendAt(data, encodedPTS)
		if err != nil || len(rgba) == 0 || w != 320 || h != 240 || decodedPTS != pts {
			t.Fatalf("reference chain lost after discarding: %v pts=%d", err, decodedPTS)
		}
		if math.Abs(float64(rgba[(w*h/2)*4])-float64(value)) > 6 {
			t.Fatal("discarded reference frames corrupted the next displayed picture")
		}
	}
}
