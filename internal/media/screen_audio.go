package media

import (
	"encoding/binary"
	"errors"
	"time"
)

// All capture timestamps use this monotonic clock, including WGC, DXGI and
// WASAPI audio. The session start is subtracted before transmission.
var captureOrigin = time.Now()

func CaptureTimestamp() int64 { return time.Since(captureOrigin).Nanoseconds() }

type ScreenAudioQuality byte

const (
	ScreenAudioOff ScreenAudioQuality = iota
	ScreenAudioLow
	ScreenAudioMedium
	ScreenAudioHigh
	ScreenAudioPacketDuration = 5 * time.Millisecond
)

func (q ScreenAudioQuality) Format() (rate, channels int) {
	switch q {
	case ScreenAudioLow:
		return 16000, 1
	case ScreenAudioMedium:
		return 32000, 2
	case ScreenAudioHigh:
		return 48000, 2
	default:
		return 0, 0
	}
}

func NewScreenAudioCapturer(q ScreenAudioQuality, cb func([]float32)) (*AudioCapturer, error) {
	rate, channels := q.Format()
	if rate == 0 {
		return nil, errors.New("invalid screen audio quality")
	}
	return newAudioCapturerWithChannels(-1, rate, channels, cb)
}

// PCM16 packets remain below the internet MTU even at 48 kHz stereo. Each
// packet is independent; losing one cannot corrupt subsequent audio.
func EncodeScreenAudio(q ScreenAudioQuality, samples []float32) []byte {
	rate, channels := q.Format()
	if rate == 0 || len(samples) != rate*channels/200 {
		return nil
	}
	b := make([]byte, 1+len(samples)*2)
	b[0] = byte(q)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(b[1+i*2:], uint16(floatToPCM16(sample)))
	}
	return b
}

func DecodeScreenAudio(b []byte) ([]float32, error) {
	if len(b) == 0 {
		return nil, errors.New("empty screen audio")
	}
	rate, channels := ScreenAudioQuality(b[0]).Format()
	frames := rate / 200
	if rate == 0 || len(b) != 1+frames*channels*2 {
		return nil, errors.New("invalid screen audio format")
	}
	read := func(frame, channel int) float32 {
		if channels == 1 {
			channel = 0
		}
		return float32(int16(binary.LittleEndian.Uint16(b[1+(frame*channels+channel)*2:]))) / 32768
	}
	// Playback always uses 48 kHz stereo. Linear interpolation preserves the
	// sample duration across all three formats and avoids device reinitialization.
	out := make([]float32, 48000*2/200)
	for frame := 0; frame < len(out)/2; frame++ {
		pos := float64(frame) * float64(rate) / 48000
		left := int(pos)
		right := min(left+1, frames-1)
		frac := float32(pos - float64(left))
		for ch := 0; ch < 2; ch++ {
			a := read(left, ch)
			out[frame*2+ch] = a + (read(right, ch)-a)*frac
		}
	}
	return out, nil
}

type ScreenAudioChunk struct {
	TimestampNS int64
	Samples     []float32
}

type ScreenAudioPacketizer struct {
	rate, channels int
	pending        []float32
	pendingPTS     int64
	nextPTS        int64
	started        bool
}

func NewScreenAudioPacketizer(q ScreenAudioQuality) *ScreenAudioPacketizer {
	rate, channels := q.Format()
	return &ScreenAudioPacketizer{rate: rate, channels: channels}
}

// firstPTS is the capture time of the first sample, rather than encode/send
// completion time. Preserve the device sample clock across callback jitter.
func (p *ScreenAudioPacketizer) Push(samples []float32, firstPTS int64) []ScreenAudioChunk {
	if p.rate == 0 {
		return nil
	}
	if !p.started || firstPTS-p.nextPTS > int64(25*time.Millisecond) || p.nextPTS-firstPTS > int64(25*time.Millisecond) {
		p.pending = nil
		p.nextPTS = max(firstPTS, 0)
		p.started = true
	}
	if len(p.pending) == 0 {
		p.pendingPTS = p.nextPTS
	}
	p.pending = append(p.pending, samples...)
	p.nextPTS += int64(len(samples)/p.channels) * int64(time.Second) / int64(p.rate)
	size := p.rate * p.channels / 200
	var chunks []ScreenAudioChunk
	for len(p.pending) >= size {
		chunks = append(chunks, ScreenAudioChunk{p.pendingPTS, append([]float32(nil), p.pending[:size]...)})
		p.pending = p.pending[size:]
		p.pendingPTS += int64(ScreenAudioPacketDuration)
	}
	return chunks
}
