package media

import (
	"sort"
	"time"
)

// Completed UDP frames can arrive out of order when their fragment counts
// differ. Wait briefly for a gap before declaring a broken reference chain.
type ScreenVideoOrder struct {
	stream       uint64
	subscription uint64
	next         uint32
	pending      map[uint32]VideoFrame
	gapAt        time.Time
}

func (q *ScreenVideoOrder) Push(frame VideoFrame) {
	if frame.StreamID < q.stream || (frame.StreamID == q.stream && frame.SubscriptionID < q.subscription) {
		return
	}
	if frame.StreamID != q.stream || frame.SubscriptionID != q.subscription {
		q.stream, q.next, q.gapAt = frame.StreamID, 0, time.Time{}
		q.subscription = frame.SubscriptionID
		q.pending = nil
	}
	if q.next != 0 && frame.Sequence < q.next {
		return
	}
	if q.pending == nil {
		q.pending = make(map[uint32]VideoFrame)
	}
	q.pending[frame.Sequence] = frame
}

func (q *ScreenVideoOrder) Ready(now time.Time) []VideoFrame {
	var frames []VideoFrame
	for len(q.pending) > 0 {
		keys := make([]uint32, 0, len(q.pending))
		for key := range q.pending {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		if q.next == 0 {
			q.next = keys[0]
		}
		frame, ok := q.pending[q.next]
		if !ok {
			if q.gapAt.IsZero() {
				q.gapAt = now
			}
			if now.Sub(q.gapAt) < 30*time.Millisecond && len(q.pending) < 120 {
				break
			}
			q.next = keys[0]
			frame = q.pending[q.next]
			frame.Discontinuity = true
		}
		delete(q.pending, q.next)
		q.next++
		q.gapAt = time.Time{}
		frames = append(frames, frame)
	}
	return frames
}
