package media

import (
	"testing"
	"time"
)

func TestScreenOrderRestartsForPlaybackSubscription(t *testing.T) {
	var order ScreenVideoOrder
	order.Push(VideoFrame{StreamID: 100, SubscriptionID: 1, Sequence: 70})
	order.Ready(time.Now())
	order.Push(VideoFrame{StreamID: 100, SubscriptionID: 2, Sequence: 1})
	order.Push(VideoFrame{StreamID: 100, SubscriptionID: 1, Sequence: 71})
	frames := order.Ready(time.Now())
	if len(frames) != 1 || frames[0].SubscriptionID != 2 || frames[0].Sequence != 1 {
		t.Fatal("playback change mixed old reference pictures into the new encoder")
	}
}
