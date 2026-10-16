package media

import (
	"os"
	"testing"
	"time"
)

func TestRequiredHardwareH264(t *testing.T) {
	if os.Getenv("RELAYNET_REQUIRE_HW_CODEC") != "1" {
		t.Skip("requires a GPU H264 encoder")
	}
	e, err := NewH264Encoder(1920, 1080, 1920, 1080, 120, 20000)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	backend, ok := any(e).(interface{ IsHardware() bool })
	if !ok || !backend.IsHardware() {
		t.Fatal("GPU encoder was silently replaced by CPU encoding")
	}
	d, err := NewH264Decoder()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pixels := make([]byte, 1920*1080*4)
	for i := 0; i < len(pixels); i += 4 {
		pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = 80, 80, 80, 255
	}
	decoded := 0
	var encodeTime, decodeTime time.Duration
	start := time.Now()
	for frame := 0; frame < 40; frame++ {
		pts := int64(frame) * int64(time.Second) / 120
		beforeEncode := time.Now()
		data, _, encodedPTS, err := e.SendAt(pixels, 1920*4, true, pts)
		encodeTime += time.Since(beforeEncode)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			continue
		}
		beforeDecode := time.Now()
		rgba, w, h, decodedPTS, err := d.SendAt(data, encodedPTS)
		decodeTime += time.Since(beforeDecode)
		if err != nil {
			t.Fatal(err)
		}
		if len(rgba) > 0 {
			if w != 1920 || h != 1080 || decodedPTS != encodedPTS {
				t.Fatalf("GPU output frame=%d dimensions=%dx%d encodedPTS=%d decodedPTS=%d", frame, w, h, encodedPTS, decodedPTS)
			}
			decoded++
		}
	}
	t.Logf("GPU codec: decoded=%d/40 elapsed=%s encode=%s decode=%s", decoded, time.Since(start), encodeTime/40, decodeTime/40)
	if decoded < 38 {
		t.Fatalf("GPU codec failed to produce timely output: %d/40", decoded)
	}
	if encodeTime > 40*time.Second/120 {
		t.Fatalf("GPU encode exceeds 120 FPS frame budget: %s/frame", encodeTime/40)
	}
}
