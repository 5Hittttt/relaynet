package media

import (
	"sort"
	"sync"
	"time"
)

const (
	screenPrebuffer     = 120 * time.Millisecond
	screenMaxBuffer     = 2 * time.Second
	screenAudioTimeout  = 500 * time.Millisecond
	screenOutputLatency = 20 * time.Millisecond
)

// ScreenTimeline owns the receiver's shared A/V clock. Video follows samples
// submitted to the audio device; underruns and resynchronization affect both.
type ScreenTimeline struct {
	mu           sync.Mutex
	streamID     uint64
	hasAudio     bool
	queue        []ScreenAudioChunk
	playing      bool
	cursor       int64
	basePTS      int64
	baseLocal    time.Time
	firstAudible time.Time
	lastAudio    time.Time
	endPTS       int64
	ended        bool
	videoReady   bool
	videoPTS     int64
}

func NewScreenTimeline() *ScreenTimeline { return &ScreenTimeline{} }

func (t *ScreenTimeline) reset(id uint64, audio bool, pts int64, now time.Time) {
	t.streamID, t.hasAudio = id, audio
	t.queue, t.playing = nil, !audio
	t.cursor, t.basePTS, t.endPTS = pts, pts, pts
	t.baseLocal = now.Add(screenPrebuffer)
	t.firstAudible, t.lastAudio = time.Time{}, time.Time{}
	t.ended = false
	t.videoReady, t.videoPTS = false, 0
}

func (t *ScreenTimeline) ObserveVideo(frame VideoFrame, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if frame.StreamID < t.streamID || (frame.StreamID == t.streamID && t.ended) {
		return
	}
	if frame.StreamID != t.streamID || frame.HasAudio != t.hasAudio {
		t.reset(frame.StreamID, frame.HasAudio, frame.TimestampNS, now)
	}
}

func (t *ScreenTimeline) PushAudio(id uint64, pts int64, samples []float32, now time.Time) {
	if id == 0 || pts < 0 || len(samples) != 480 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if id < t.streamID || (id == t.streamID && t.ended) {
		return
	}
	if id != t.streamID || !t.hasAudio {
		t.reset(id, true, pts, now)
	}
	if t.playing && pts+int64(ScreenAudioPacketDuration) <= t.cursor {
		return
	}
	i := sort.Search(len(t.queue), func(i int) bool { return t.queue[i].TimestampNS >= pts })
	if i < len(t.queue) && t.queue[i].TimestampNS == pts {
		return
	}
	t.queue = append(t.queue, ScreenAudioChunk{})
	copy(t.queue[i+1:], t.queue[i:])
	t.queue[i] = ScreenAudioChunk{pts, samples}
	t.lastAudio = now
	// Bound latency and memory under playback stalls or source-clock drift.
	latest := t.queue[len(t.queue)-1].TimestampNS + int64(ScreenAudioPacketDuration)
	oldest := t.queue[0].TimestampNS
	if t.playing {
		oldest = t.cursor
	}
	if latest-oldest > int64(screenMaxBuffer) {
		cut := latest - int64(screenMaxBuffer)
		i = sort.Search(len(t.queue), func(i int) bool { return t.queue[i].TimestampNS >= cut })
		t.queue = t.queue[i:]
		t.playing = false
	}
}

// FillAudio is called only by the audio device. Missing packets produce
// silence at their timestamps instead of shifting all later audio earlier.
func (t *ScreenTimeline) FillAudio(out []float32, now time.Time) {
	clear(out)
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.hasAudio {
		return
	}
	if t.playing && now.Sub(t.lastAudio) > screenAudioTimeout {
		t.playing = false
	}
	if t.playing && t.cursor-t.videoPTS > int64(60*time.Millisecond) {
		t.playing = false
	}
	if !t.playing {
		if !t.videoReady || len(t.queue) == 0 {
			return
		}
		start := max(t.cursor, t.queue[0].TimestampNS)
		if t.videoPTS < start || t.queue[len(t.queue)-1].TimestampNS+int64(ScreenAudioPacketDuration)-start < int64(screenPrebuffer) {
			return
		}
		t.cursor = start
		t.firstAudible = now.Add(screenOutputLatency)
		t.playing = true
	}
	start := t.cursor
	frames := len(out) / 2
	for frame := 0; frame < frames; frame++ {
		pts := start + int64(frame)*int64(time.Second)/48000
		for len(t.queue) > 0 && t.queue[0].TimestampNS+int64(ScreenAudioPacketDuration) <= pts {
			t.queue = t.queue[1:]
		}
		if len(t.queue) == 0 || pts < t.queue[0].TimestampNS {
			continue
		}
		index := int(((pts-t.queue[0].TimestampNS)*48000+int64(time.Second)/2)/int64(time.Second)) * 2
		if index+1 < len(t.queue[0].Samples) {
			copy(out[frame*2:frame*2+2], t.queue[0].Samples[index:index+2])
		}
	}
	t.cursor = start + int64(frames)*int64(time.Second)/48000
	t.basePTS, t.baseLocal, t.endPTS = start, now.Add(screenOutputLatency), t.cursor
}

func (t *ScreenTimeline) Position(id uint64, now time.Time) (int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == 0 || id != t.streamID || !t.playing {
		return 0, false
	}
	if t.hasAudio {
		if now.Before(t.firstAudible) || now.Sub(t.lastAudio) > screenAudioTimeout {
			return 0, false
		}
		return min(t.basePTS+now.Sub(t.baseLocal).Nanoseconds(), t.endPTS), true
	}
	if now.Before(t.baseLocal) {
		return 0, false
	}
	return t.basePTS + now.Sub(t.baseLocal).Nanoseconds(), true
}

func (t *ScreenTimeline) Stop(id uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == t.streamID {
		t.ended, t.playing, t.queue = true, false, nil
	}
}

// The GUI reports decoded timestamps, not just network arrivals. Audio waits
// for actual decodable video and re-primes with it after a prolonged stall.
func (t *ScreenTimeline) VideoDecoded(id uint64, pts int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id == t.streamID && !t.ended {
		if !t.videoReady || (!t.playing && pts-t.cursor > int64(screenPrebuffer)) {
			t.cursor = max(t.cursor, pts)
		}
		t.videoReady = true
		t.videoPTS = max(t.videoPTS, pts)
	}
}

// Decode slightly ahead of playback rather than retaining hundreds of large
// RGBA pictures while audio primes or stalls. Compressed frames stay queued.
func (t *ScreenTimeline) DecodeLimit(id uint64, now time.Time) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id != t.streamID {
		return 0
	}
	if !t.hasAudio {
		return t.basePTS + max(now.Sub(t.baseLocal).Nanoseconds(), 0) + int64(30*time.Millisecond)
	}
	start := t.cursor
	if len(t.queue) > 0 {
		start = max(start, t.queue[0].TimestampNS)
	}
	if t.playing {
		start = min(t.basePTS+now.Sub(t.baseLocal).Nanoseconds(), t.endPTS)
	} else if len(t.queue) > 0 {
		// At startup or after a stall, catch up toward current audio. Limiting
		// decode to the oldest buffered packet traps a late viewer at the
		// buffer's eviction boundary and repeatedly stops both streams.
		end := t.queue[len(t.queue)-1].TimestampNS + int64(ScreenAudioPacketDuration)
		return max(start, end-int64(screenPrebuffer))
	}
	if !t.videoReady {
		return start + int64(100*time.Millisecond)
	}
	return start + int64(30*time.Millisecond)
}

// VideoDiscardBefore excludes pictures older than the audio still available
// for playback, or far behind the running clock. H.264 references must still
// be decoded, but their full display images are no longer useful.
func (t *ScreenTimeline) VideoDiscardBefore(id uint64, now time.Time) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if id != t.streamID || t.ended {
		return 0
	}
	if !t.hasAudio {
		return max(t.basePTS+now.Sub(t.baseLocal).Nanoseconds()-int64(100*time.Millisecond), 0)
	}
	if t.playing {
		return max(min(t.basePTS+now.Sub(t.baseLocal).Nanoseconds(), t.endPTS)-int64(100*time.Millisecond), 0)
	}
	start := t.cursor
	if len(t.queue) > 0 {
		start = max(start, t.queue[0].TimestampNS)
	}
	return start
}
