package media

import (
	"os"
	"testing"
	"time"
)

func TestHardwarePipelinePreservesFrames(t *testing.T) {
	if os.Getenv("RELAYNET_REQUIRE_HW_CODEC") != "1" {
		t.Skip("requires asynchronous GPU H264 encoder")
	}
	enc, err := NewH264Encoder(320, 240, 640, 480, 120, 4000)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	pipe, ok := any(enc).(interface {
		SubmitAt([]byte, int, bool, int64) (bool, error)
		Poll() ([]byte, bool, int64, error)
	})
	if !ok {
		t.Fatal("hardware encoding still blocks capture while waiting for output")
	}
	dec, err := NewH264Decoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	pixels := make([]byte, 320*240*4)
	submitted, received := 0, 0
	deadline := time.Now().Add(4 * time.Second)
	for received < 40 && time.Now().Before(deadline) {
		if submitted < 40 {
			value := byte(50 + submitted*3)
			for i := 0; i < len(pixels); i += 4 {
				pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = value, value, value, 255
			}
			accepted, err := pipe.SubmitAt(pixels, 320*4, true, int64(submitted)*int64(17*time.Millisecond))
			if err != nil {
				t.Fatal(err)
			}
			if accepted {
				submitted++
			}
		}
		for {
			data, key, pts, err := pipe.Poll()
			if err != nil {
				t.Fatal(err)
			}
			if len(data) == 0 {
				break
			}
			if pts != int64(received)*int64(17*time.Millisecond) || (received == 0 && !key) {
				t.Fatalf("queued samples merged or reordered: frame=%d pts=%d", received, pts)
			}
			rgba, w, h, outputPTS, err := dec.SendSizedAt(data, pts, 320, 240)
			if err != nil || len(rgba) == 0 || w != 320 || h != 240 || outputPTS != pts {
				t.Fatalf("queued stream broke references: %v", err)
			}
			value := 50 + received*3
			got := int(rgba[(w*h/2)*4])
			if got < value-6 || got > value+6 {
				t.Fatalf("sample and timestamp disagree: frame=%d value=%d want=%d", received, got, value)
			}
			received++
		}
		time.Sleep(time.Millisecond)
	}
	if received != 40 {
		t.Fatalf("pipeline did not drain its last frames: submitted=%d received=%d", submitted, received)
	}
}

func TestHardwarePipeline4KPreparationBudget(t *testing.T) {
	if os.Getenv("RELAYNET_REQUIRE_HW_CODEC") != "1" {
		t.Skip("requires asynchronous GPU H264 encoder")
	}
	enc, err := NewH264Encoder(1920, 1080, 3840, 2160, 120, 20000)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	if !enc.IsHardware() {
		t.Fatal("requires hardware encoding")
	}
	pixels := make([]byte, 1920*1080*4)
	for i := 0; i < len(pixels); i += 4 {
		pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = 70, 90, 180, 255
	}
	var preparation time.Duration
	submitted, received := 0, 0
	deadline := time.Now().Add(4 * time.Second)
	for received < 60 && time.Now().Before(deadline) {
		if submitted < 60 {
			before := time.Now()
			accepted, err := enc.SubmitAt(pixels, 1920*4, false, int64(submitted)*int64(time.Second)/120)
			if err != nil {
				t.Fatal(err)
			}
			if accepted {
				if submitted >= 10 {
					preparation += time.Since(before)
				}
				submitted++
			}
		}
		for {
			data, _, pts, err := enc.Poll()
			if err != nil {
				t.Fatal(err)
			}
			if len(data) == 0 {
				break
			}
			if pts != (int64(received)*int64(time.Second)/120)/100*100 {
				t.Fatalf("4K output timestamp reordered: frame=%d pts=%d", received, pts)
			}
			received++
		}
		time.Sleep(time.Millisecond)
	}
	if received != 60 {
		t.Fatalf("4K pipeline stalled: received=%d", received)
	}
	t.Logf("4K preparation + submission average=%s", preparation/50)
	if preparation/50 > 5*time.Millisecond {
		t.Fatalf("4K preparation leaves too little 120 FPS headroom: %s", preparation/50)
	}
}
