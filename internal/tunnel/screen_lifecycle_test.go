package tunnel

import (
	"testing"
	"time"

	"relaynet/internal/media"
	"relaynet/internal/proto"
)

func TestScreenAudioSelectionAndStop(t *testing.T) {
	c := New(Config{}, nil)
	tl := media.NewScreenTimeline()
	c.screenSource, c.screenTimeline = 7, tl
	c.screenAudioPlayer = &media.ScreenAudioPlayer{}
	samples := make([]float32, 480)
	for i := range samples {
		samples[i] = 0.5
	}
	payload := media.EncodeScreenAudio(media.ScreenAudioHigh, samples)
	meta := proto.ScreenPayload{Kind: proto.ScreenAudio, HasAudio: true, StreamID: 1}
	for i := 0; i < 30; i++ {
		meta.TimestampNS = int64(i) * int64(media.ScreenAudioPacketDuration)
		c.receiveScreenAudio(8, meta, payload)
	}
	tl.ObserveVideo(media.VideoFrame{StreamID: 1, HasAudio: true}, time.Now())
	tl.VideoDecoded(1, 0)
	out := make([]float32, 960)
	tl.FillAudio(out, time.Now())
	if _, ready := tl.Position(1, time.Now().Add(20*time.Millisecond)); ready {
		t.Fatal("another member's audio entered the selected screen clock")
	}
	for i := 0; i < 30; i++ {
		meta.TimestampNS = int64(i) * int64(media.ScreenAudioPacketDuration)
		c.receiveScreenAudio(7, meta, payload)
	}
	now := time.Now()
	tl.FillAudio(out, now)
	if _, ready := tl.Position(1, now.Add(20*time.Millisecond)); !ready || out[0] < 0.499 {
		t.Fatal("selected screen audio was not played")
	}
	c.receiveScreenAudio(7, meta, []byte{0})
	c.receiveScreenAudio(7, meta, payload)
	tl.FillAudio(out, time.Now())
	for _, sample := range out {
		if sample != 0 {
			t.Fatal("stop marker or late packets left audio playing")
		}
	}
	if _, ready := tl.Position(1, time.Now().Add(time.Second)); ready {
		t.Fatal("late packet revived stopped screen audio")
	}
}
