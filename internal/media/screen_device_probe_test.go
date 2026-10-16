package media

import (
	"os"
	"testing"
	"time"
)

func TestLiveScreenPlaybackStartup(t *testing.T) {
	if os.Getenv("RELAYNET_LIVE_SCREEN_TEST") != "1" {
		t.Skip("requires the Windows playback endpoint")
	}
	tl := NewScreenTimeline()
	start := time.Now()
	tl.ObserveVideo(VideoFrame{StreamID: 1, HasAudio: true}, start)
	tl.VideoDecoded(1, 0)
	for i := 0; i < 40; i++ {
		tl.PushAudio(1, int64(i)*int64(5*time.Millisecond), make([]float32, 480), start)
	}
	p, err := NewScreenAudioPlayer(tl)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	t.Logf("playback device started after %s", time.Since(start))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		position, ready := tl.Position(1, time.Now())
		if ready && position > 0 {
			t.Logf("playback callback advances the A/V clock: %s", time.Duration(position))
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("playback device started but no audible clock position became ready")
}
