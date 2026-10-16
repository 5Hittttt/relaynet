package media

import (
	"testing"
	"time"
)

func TestH264DisplaySizeAndPausedReferences(t *testing.T) {
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
	sized, ok := any(dec).(interface {
		SendSizedAt([]byte, int64, int, int) ([]byte, int, int, int64, error)
	})
	if !ok {
		t.Fatal("decoder still converts full resolution for a smaller display")
	}
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
		if frame > 0 && frame < 11 {
			if err = dec.SkipAt(data, encodedPTS); err != nil {
				t.Fatal(err)
			}
			continue
		}
		maxW, maxH, wantW, wantH := 160, 90, 120, 90
		if frame == 11 {
			maxW, maxH, wantW, wantH = 640, 480, 320, 240
		}
		rgba, w, h, outputPTS, err := sized.SendSizedAt(data, encodedPTS, maxW, maxH)
		if err != nil || w != wantW || h != wantH || len(rgba) != w*h*4 || outputPTS != pts {
			t.Fatalf("display size or reference chain wrong: size=%dx%d pts=%d err=%v", w, h, outputPTS, err)
		}
		got := int(rgba[(w*h/2)*4])
		if got < int(value)-6 || got > int(value)+6 {
			t.Fatalf("paused reference frames corrupted resumed image: got %d want %d", got, value)
		}
	}
}

func TestH264ScaledDisplayColorsAndAperture(t *testing.T) {
	enc, err := NewH264Encoder(320, 180, 640, 360, 60, 2000)
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	dec, err := NewH264Decoder()
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	colors := [4][3]byte{{180, 70, 30}, {40, 90, 200}, {70, 180, 40}, {180, 60, 170}}
	pixels := make([]byte, 320*180*4)
	for y := 0; y < 180; y++ {
		for x := 0; x < 320; x++ {
			c := colors[(y/90)*2+x/160]
			i := (y*320 + x) * 4
			pixels[i], pixels[i+1], pixels[i+2], pixels[i+3] = c[0], c[1], c[2], 255
		}
	}
	data, _, pts, err := enc.SendAt(pixels, 320*4, true, 17000000)
	if err != nil {
		t.Fatal(err)
	}
	rgba, w, h, outputPTS, err := dec.SendSizedAt(data, pts, 320, 180)
	if err != nil || w != 320 || h != 180 || outputPTS != pts {
		t.Fatalf("scaled aperture incorrect: %dx%d pts=%d err=%v", w, h, outputPTS, err)
	}
	for quadrant, want := range colors {
		x, y := 80+(quadrant%2)*160, 45+(quadrant/2)*90
		for channel, expected := range want {
			got := int(rgba[(y*w+x)*4+channel])
			if got < int(expected)-8 || got > int(expected)+8 {
				t.Fatalf("quadrant %d channel %d got=%d want=%d", quadrant, channel, got, expected)
			}
		}
	}
}
