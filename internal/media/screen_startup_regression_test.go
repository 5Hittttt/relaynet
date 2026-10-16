package media

import (
	"testing"
	"time"
)

func TestScreenStartupDecodesTowardBufferedAudioEnd(t *testing.T) {
	now := time.Unix(0, 0)
	tl := NewScreenTimeline()
	tl.ObserveVideo(VideoFrame{StreamID: 1, HasAudio: true}, now)
	// A late viewer has old video reference frames but only current audio.
	queueAudio(tl, 1, int64(2*time.Second), 400, now)
	tl.VideoDecoded(1, 0)
	limit := tl.DecodeLimit(1, now)
	// The audio ends at 4 seconds; retain 120 ms to start both streams.
	if limit < int64(3880*time.Millisecond) || limit > int64(4*time.Second) {
		t.Fatalf("startup decoder is stuck at old audio: limit=%s", time.Duration(limit))
	}
	tl.VideoDecoded(1, int64(3880*time.Millisecond))
	out := make([]float32, 960)
	tl.FillAudio(out, now)
	position, ready := tl.Position(1, now.Add(20*time.Millisecond))
	if !ready || position != int64(3880*time.Millisecond) {
		t.Fatalf("late viewer did not start near live audio: position=%s ready=%v", time.Duration(position), ready)
	}
}
