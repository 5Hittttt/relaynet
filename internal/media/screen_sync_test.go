package media

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func TestScreenAudioFormatsAndDuration(t *testing.T) {
	for _, q := range []ScreenAudioQuality{ScreenAudioLow, ScreenAudioMedium, ScreenAudioHigh} {
		rate, channels := q.Format()
		input := make([]float32, rate*channels/200)
		for i := range input {
			input[i] = 0.5
			if channels == 2 && i%2 != 0 {
				input[i] = -0.5
			}
		}
		encoded := EncodeScreenAudio(q, input)
		if len(encoded)+26+28+9 > 1300 {
			t.Fatalf("quality %d exceeds UDP packet budget", q)
		}
		output, err := DecodeScreenAudio(encoded)
		if err != nil || len(output) != 480 {
			t.Fatalf("quality %d changes sample duration: %v", q, err)
		}
		for i, value := range output {
			want := float32(0.5)
			if channels == 2 && i%2 != 0 {
				want = -0.5
			}
			if math.Abs(float64(value-want)) > 0.0001 {
				t.Fatalf("quality %d mixes channels at %d", q, i)
			}
		}
		if _, err := DecodeScreenAudio(encoded[:len(encoded)-1]); err == nil {
			t.Fatal("truncated audio accepted")
		}
	}
}

func TestScreenPacketizerUsesSampleClock(t *testing.T) {
	p := NewScreenAudioPacketizer(ScreenAudioHigh)
	for call := 0; call < 20; call++ {
		jitter := int64(0)
		if call%2 != 0 {
			jitter = int64(4 * time.Millisecond)
		}
		chunks := p.Push(make([]float32, 960), int64(call)*int64(10*time.Millisecond)+jitter)
		if len(chunks) != 2 {
			t.Fatal("callback was not split into 5 ms packets")
		}
		for i, chunk := range chunks {
			want := int64(call*2+i) * int64(ScreenAudioPacketDuration)
			if chunk.TimestampNS != want {
				t.Fatalf("capture callback jitter leaked into timestamps: got %d want %d", chunk.TimestampNS, want)
			}
		}
	}
	chunks := p.Push(make([]float32, 480), int64(time.Second))
	if chunks[0].TimestampNS != int64(time.Second) {
		t.Fatal("capture gap did not reset device sample clock")
	}
}

func queueAudio(timeline *ScreenTimeline, id uint64, first int64, count int, now time.Time) {
	for i := count - 1; i >= 0; i-- {
		samples := make([]float32, 480)
		for j := range samples {
			samples[j] = float32(i + 1)
		}
		timeline.PushAudio(id, first+int64(i)*int64(ScreenAudioPacketDuration), samples, now)
	}
}

func TestScreenClockWaitsForBothStreams(t *testing.T) {
	now := time.Unix(0, 0)
	tl := NewScreenTimeline()
	frame := VideoFrame{StreamID: 1, TimestampNS: int64(time.Second), HasAudio: true}
	tl.ObserveVideo(frame, now)
	queueAudio(tl, 1, frame.TimestampNS, 30, now)
	out := make([]float32, 960)
	tl.FillAudio(out, now)
	if _, ready := tl.Position(1, now.Add(time.Second)); ready {
		t.Fatal("audio clock started before a decoded video frame")
	}
	tl.VideoDecoded(1, frame.TimestampNS)
	tl.FillAudio(out, now)
	if _, ready := tl.Position(1, now.Add(19*time.Millisecond)); ready {
		t.Fatal("video displayed before first audible sample")
	}
	pts, ready := tl.Position(1, now.Add(20*time.Millisecond))
	if !ready || pts != frame.TimestampNS {
		t.Fatalf("wrong audio master position: %d %v", pts, ready)
	}
	if out[0] != 1 || out[480] != 2 {
		t.Fatal("reordered audio packets were played out of sequence")
	}
}

func TestScreenClockMissingAudioDoesNotShiftVideo(t *testing.T) {
	now := time.Unix(0, 0)
	tl := NewScreenTimeline()
	tl.ObserveVideo(VideoFrame{StreamID: 1, HasAudio: true}, now)
	queueAudio(tl, 1, 0, 40, now)
	tl.mu.Lock()
	for i, chunk := range tl.queue {
		if chunk.TimestampNS == int64(50*time.Millisecond) {
			tl.queue = append(tl.queue[:i], tl.queue[i+1:]...)
			break
		}
	}
	tl.mu.Unlock()
	tl.VideoDecoded(1, 0)
	out := make([]float32, 960)
	for block := 0; block < 10; block++ {
		at := now.Add(time.Duration(block) * 10 * time.Millisecond)
		tl.VideoDecoded(1, int64(block)*int64(10*time.Millisecond))
		tl.FillAudio(out, at)
		pts, ready := tl.Position(1, at.Add(20*time.Millisecond))
		if !ready || pts != int64(block)*int64(10*time.Millisecond) {
			t.Fatalf("audio loss shifted video clock: block=%d pts=%d", block, pts)
		}
		if block == 5 && (out[0] != 0 || out[480] != 12) {
			t.Fatal("lost interval was not replaced with exactly 5 ms silence")
		}
	}
}

func TestScreenClockVideoStallStopAndRestart(t *testing.T) {
	now := time.Unix(0, 0)
	tl := NewScreenTimeline()
	tl.ObserveVideo(VideoFrame{StreamID: 1, HasAudio: true}, now)
	queueAudio(tl, 1, 0, 60, now)
	tl.VideoDecoded(1, 0)
	out := make([]float32, 960)
	for i := 0; i < 9; i++ {
		tl.FillAudio(out, now.Add(time.Duration(i)*10*time.Millisecond))
	}
	if _, ready := tl.Position(1, now.Add(110*time.Millisecond)); ready {
		t.Fatal("audio continued running away from stalled video")
	}
	tl.VideoDecoded(1, int64(120*time.Millisecond))
	tl.FillAudio(out, now.Add(120*time.Millisecond))
	if pts, ready := tl.Position(1, now.Add(140*time.Millisecond)); !ready || pts < int64(70*time.Millisecond) {
		t.Fatal("streams did not resume together")
	}
	tl.Stop(1)
	queueAudio(tl, 1, int64(time.Second), 30, now.Add(time.Second))
	if _, ready := tl.Position(1, now.Add(time.Second)); ready {
		t.Fatal("late packets revived a stopped session")
	}
	tl.ObserveVideo(VideoFrame{StreamID: 2, HasAudio: true}, now.Add(time.Second))
	queueAudio(tl, 2, 0, 30, now.Add(time.Second))
	tl.VideoDecoded(2, 0)
	tl.FillAudio(out, now.Add(time.Second))
	if pts, ready := tl.Position(2, now.Add(time.Second+20*time.Millisecond)); !ready || pts != 0 {
		t.Fatal("restart retained previous session clock")
	}
}

func TestScreenVideoOrderAndLoss(t *testing.T) {
	now := time.Unix(0, 0)
	var q ScreenVideoOrder
	q.Push(VideoFrame{StreamID: 1, Sequence: 1})
	if len(q.Ready(now)) != 1 {
		t.Fatal("initial frame missing")
	}
	q.Push(VideoFrame{StreamID: 1, Sequence: 3})
	if len(q.Ready(now)) != 0 {
		t.Fatal("out of order P-frame was released early")
	}
	q.Push(VideoFrame{StreamID: 1, Sequence: 2})
	frames := q.Ready(now.Add(10 * time.Millisecond))
	if len(frames) != 2 || frames[0].Sequence != 2 || frames[1].Sequence != 3 {
		t.Fatal("frames not reordered")
	}
	q.Push(VideoFrame{StreamID: 1, Sequence: 5})
	q.Ready(now)
	frames = q.Ready(now.Add(31 * time.Millisecond))
	if len(frames) != 1 || !frames[0].Discontinuity {
		t.Fatal("lost H264 reference frame not reported")
	}
	q.Push(VideoFrame{StreamID: 2, Sequence: 1})
	q.Push(VideoFrame{StreamID: 1, Sequence: 6})
	if frames = q.Ready(now); len(frames) != 1 || frames[0].StreamID != 2 {
		t.Fatal("old session leaked after restart")
	}
}

func TestH264KeepsCaptureTimestamps(t *testing.T) {
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
	known := map[int64]byte{}
	decoded, encoded, firstDecoded := 0, 0, -1
	var lastPTS int64 = -1
	for i := 0; i < 40; i++ {
		time.Sleep(time.Second / 60)
		pts := int64(1000000000 + i*17000000 + (i/7)*9000000)
		value := byte(40 + (i%6)*25)
		known[pts] = value
		for p := 0; p < len(pixels); p += 4 {
			pixels[p], pixels[p+1], pixels[p+2], pixels[p+3] = value, value, value, 255
		}
		data, _, encodedPTS, err := enc.SendAt(pixels, 320*4, true, pts)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			continue
		}
		encoded++
		rgba, w, h, outputPTS, err := dec.SendAt(data, encodedPTS)
		if err != nil {
			t.Fatal(err)
		}
		if len(rgba) == 0 {
			continue
		}
		if w != 320 || h != 240 || outputPTS <= lastPTS {
			t.Fatal("invalid decoded timing or dimensions")
		}
		want, ok := known[outputPTS]
		if !ok || math.Abs(float64(rgba[(w*h/2)*4])-float64(want)) > 6 {
			t.Fatalf("decoded picture attached to wrong capture timestamp: pts=%d got=%d want=%d", outputPTS, rgba[(w*h/2)*4], want)
		}
		if firstDecoded < 0 {
			firstDecoded = i
		}
		lastPTS = outputPTS
		decoded++
	}
	t.Logf("input=40 encoded=%d decoded=%d firstDecoded=%d lastPTS=%d", encoded, decoded, firstDecoded, lastPTS)
	if decoded < 38 || firstDecoded > 2 {
		t.Fatalf("low-latency codec buffered too many frames: decoded=%d first=%d", decoded, firstDecoded)
	}
}

func TestScreenClockHighFrameRates(t *testing.T) {
	for _, fps := range []int{30, 60, 120} {
		t.Run(fmt.Sprint(fps), func(t *testing.T) {
			tl := NewScreenTimeline()
			now := time.Unix(0, 0)
			tl.ObserveVideo(VideoFrame{StreamID: 1, HasAudio: true}, now)
			queueAudio(tl, 1, 0, 40, now)
			tl.VideoDecoded(1, 0)
			out := make([]float32, 960)
			interval := int64(time.Second) / int64(fps)
			for block := 0; block < 1000; block++ {
				pts := int64(block) * int64(10*time.Millisecond)
				at := now.Add(time.Duration(pts))
				// Keep a bounded jitter buffer, with packets intentionally reordered.
				queueAudio(tl, 1, pts+int64(200*time.Millisecond), 2, at)
				decodedPTS := (pts + int64(30*time.Millisecond)) / interval * interval
				tl.VideoDecoded(1, decodedPTS)
				tl.FillAudio(out, at)
				position, ready := tl.Position(1, at.Add(screenOutputLatency))
				if !ready || position != pts {
					t.Fatalf("clock drift at %d fps after %d blocks: got=%d want=%d ready=%v", fps, block, position, pts, ready)
				}
			}
		})
	}
}
