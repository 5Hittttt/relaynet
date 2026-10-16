package media

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"runtime"
	"sync"
	"unsafe"

	"github.com/gen2brain/malgo"
)

type AudioDevice struct {
	Name string
	ID   int
}

func ListAudioDevices() []AudioDevice {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return []AudioDevice{{Name: "默认设备", ID: 0}}
	}
	defer func() {
		ctx.Context.Uninit()
		ctx.Free()
	}()

	devices, err := ctx.Context.Devices(malgo.Capture)
	if err != nil {
		return []AudioDevice{{Name: "默认设备", ID: 0}}
	}

	result := make([]AudioDevice, 0, len(devices))
	for i, d := range devices {
		result = append(result, AudioDevice{Name: d.Name(), ID: i})
	}
	return result
}

func ListPlaybackDevices() []AudioDevice {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return []AudioDevice{{Name: "默认设备", ID: 0}}
	}
	defer func() {
		ctx.Context.Uninit()
		ctx.Free()
	}()

	devices, err := ctx.Context.Devices(malgo.Playback)
	if err != nil {
		return []AudioDevice{{Name: "默认设备", ID: 0}}
	}

	result := make([]AudioDevice, 0, len(devices))
	for i, d := range devices {
		result = append(result, AudioDevice{Name: d.Name(), ID: i})
	}
	return result
}

type AudioCapturer struct {
	ctx        *malgo.AllocatedContext
	capture    *malgo.Device
	streaming  bool
	callback   func([]float32)
	sampleRate int
	mu         sync.Mutex
}

func NewAudioCapturer(deviceID int, sampleRate int, callback func([]float32)) (*AudioCapturer, error) {
	return newAudioCapturer(malgo.Capture, deviceID, sampleRate, callback)
}

// NewSystemAudioCapturer captures the default Windows playback endpoint using
// WASAPI loopback. It captures the mixed desktop output, not a microphone.
func NewSystemAudioCapturer(sampleRate int, callback func([]float32)) (*AudioCapturer, error) {
	if runtime.GOOS != "windows" {
		return nil, errors.New("system audio sharing is only supported on Windows")
	}
	return newAudioCapturer(malgo.Loopback, -1, sampleRate, callback)
}

func newAudioCapturer(deviceType malgo.DeviceType, deviceID int, sampleRate int, callback func([]float32)) (*AudioCapturer, error) {
	return newAudioCapturerConfigured(deviceType, deviceID, sampleRate, 2, callback)
}

func newAudioCapturerWithChannels(deviceID, sampleRate, channels int, callback func([]float32)) (*AudioCapturer, error) {
	if runtime.GOOS != "windows" {
		return nil, errors.New("system audio sharing is only supported on Windows")
	}
	return newAudioCapturerConfigured(malgo.Loopback, deviceID, sampleRate, channels, callback)
}

func newAudioCapturerConfigured(deviceType malgo.DeviceType, deviceID int, sampleRate, channels int, callback func([]float32)) (*AudioCapturer, error) {
	ac := &AudioCapturer{
		callback:   callback,
		sampleRate: sampleRate,
	}

	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}
	ac.ctx = ctx

	deviceConfig := malgo.DefaultDeviceConfig(deviceType)
	deviceConfig.Capture.Format = malgo.FormatF32
	deviceConfig.Capture.Channels = uint32(channels)
	deviceConfig.SampleRate = uint32(sampleRate)
	deviceConfig.PeriodSizeInMilliseconds = 10

	if deviceID >= 0 {
		deviceListType := deviceType
		if deviceType == malgo.Loopback {
			deviceListType = malgo.Playback
		}
		devices, err := ctx.Context.Devices(deviceListType)
		if err == nil && deviceID < len(devices) {
			deviceConfig.Capture.DeviceID = devices[deviceID].ID.Pointer()
		}
	}

	capture, err := malgo.InitDevice(ctx.Context, deviceConfig, malgo.DeviceCallbacks{
		Data: func(outputSample, inputSample []byte, framecount uint32) {
			ac.mu.Lock()
			streaming := ac.streaming
			cb := ac.callback
			ac.mu.Unlock()
			if !streaming || cb == nil || len(inputSample) < 4 {
				return
			}
			// malgo FormatF32 is native little-endian float32, interleaved
			// (L,R,L,R,...). Interpret the capture buffer directly as such —
			// zero copy, zero per-frame allocation. The buffer is only valid
			// during this callback, so the consumer must finish synchronously.
			raw := unsafe.Slice((*float32)(unsafe.Pointer(&inputSample[0])), len(inputSample)/4)
			cb(raw)
		},
	})
	if err != nil {
		ctx.Context.Uninit()
		ctx.Free()
		return nil, err
	}

	ac.capture = capture
	return ac, nil
}

func (ac *AudioCapturer) Start() error {
	ac.mu.Lock()
	ac.streaming = true
	ac.mu.Unlock()
	return ac.capture.Start()
}

func (ac *AudioCapturer) Stop() {
	ac.mu.Lock()
	ac.streaming = false
	ac.mu.Unlock()
	ac.capture.Stop()
}

func (ac *AudioCapturer) Close() {
	ac.Stop()
	ac.capture.Uninit()
	ac.ctx.Context.Uninit()
	ac.ctx.Free()
}

type AudioPlayer struct {
	ctx      *malgo.AllocatedContext
	device   *malgo.Device
	buffer   []float32
	readPos  int
	writePos int
	queued   int
	primed   bool
	running  bool
	mu       sync.Mutex
}

const (
	audioPlaybackSampleRate = 48000
	audioPlaybackChannels   = 2
	audioPlaybackBuffer     = audioPlaybackSampleRate * audioPlaybackChannels / 2         // 500 ms
	audioPlaybackPrebuffer  = audioPlaybackSampleRate * audioPlaybackChannels * 60 / 1000 // 60 ms
)

func NewAudioPlayer(deviceID int) (*AudioPlayer, error) {
	ap := &AudioPlayer{
		buffer: make([]float32, audioPlaybackBuffer),
	}

	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}
	ap.ctx = ctx

	deviceConfig := malgo.DefaultDeviceConfig(malgo.Playback)
	deviceConfig.Playback.Format = malgo.FormatF32
	deviceConfig.Playback.Channels = 2
	deviceConfig.SampleRate = audioPlaybackSampleRate

	if deviceID >= 0 {
		devices, err := ctx.Context.Devices(malgo.Playback)
		if err == nil && deviceID < len(devices) {
			deviceConfig.Playback.DeviceID = devices[deviceID].ID.Pointer()
		}
	}

	device, err := malgo.InitDevice(ctx.Context, deviceConfig, malgo.DeviceCallbacks{
		Data: func(outputSample, inputSample []byte, framecount uint32) {
			// Network packet sizes and hardware playback periods rarely line up.
			// Start with silence and fill the device buffer from a continuous ring.
			clear(outputSample)
			ap.mu.Lock()
			defer ap.mu.Unlock()
			if !ap.running || (!ap.primed && ap.queued < audioPlaybackPrebuffer) {
				return
			}
			ap.primed = true
			sampleCount := len(outputSample) / 4
			if sampleCount > ap.queued {
				sampleCount = ap.queued
			}
			for i := 0; i < sampleCount; i++ {
				bits := math.Float32bits(ap.buffer[ap.readPos])
				offs := i * 4
				outputSample[offs] = byte(bits)
				outputSample[offs+1] = byte(bits >> 8)
				outputSample[offs+2] = byte(bits >> 16)
				outputSample[offs+3] = byte(bits >> 24)
				ap.readPos = (ap.readPos + 1) % len(ap.buffer)
			}
			ap.queued -= sampleCount
			if sampleCount < len(outputSample)/4 {
				// Re-prime after an underrun to absorb the next network jitter burst.
				ap.primed = false
			}
		},
	})
	if err != nil {
		ctx.Context.Uninit()
		ctx.Free()
		return nil, err
	}

	ap.device = device
	return ap, nil
}

func (ap *AudioPlayer) Start() error {
	ap.mu.Lock()
	ap.running = true
	ap.mu.Unlock()
	return ap.device.Start()
}

func (ap *AudioPlayer) Stop() {
	ap.mu.Lock()
	ap.running = false
	ap.primed = false
	ap.queued = 0
	ap.readPos = 0
	ap.writePos = 0
	ap.mu.Unlock()
	ap.device.Stop()
}

func (ap *AudioPlayer) Close() {
	ap.Stop()
	ap.device.Uninit()
	ap.ctx.Context.Uninit()
	ap.ctx.Free()
}

func (ap *AudioPlayer) Play(samples []float32) {
	if len(samples) < audioPlaybackChannels {
		return
	}
	if len(samples)%audioPlaybackChannels != 0 {
		samples = samples[:len(samples)-1]
	}
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if !ap.running {
		return
	}
	if len(samples) > len(ap.buffer) {
		samples = samples[len(samples)-len(ap.buffer):]
	}
	if overflow := ap.queued + len(samples) - len(ap.buffer); overflow > 0 {
		if overflow%audioPlaybackChannels != 0 {
			overflow++
		}
		ap.readPos = (ap.readPos + overflow) % len(ap.buffer)
		ap.queued -= overflow
		ap.primed = false
	}
	for _, sample := range samples {
		ap.buffer[ap.writePos] = sample
		ap.writePos = (ap.writePos + 1) % len(ap.buffer)
	}
	ap.queued += len(samples)
}

const adpcmStepTableSize = 89

var adpcmStepTable = []int16{
	7, 8, 9, 10, 11, 12, 13, 14, 16, 17,
	19, 21, 23, 25, 28, 31, 34, 37, 41, 45,
	50, 55, 60, 66, 73, 80, 88, 97, 107, 118,
	130, 143, 157, 173, 190, 209, 230, 253, 279, 307,
	337, 371, 408, 449, 494, 544, 598, 658, 724, 796,
	876, 963, 1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066,
	2272, 2499, 2749, 3024, 3327, 3660, 4026, 4428, 4871, 5358,
	5894, 6484, 7132, 7845, 8630, 9493, 10442, 11487, 12635, 13899,
	15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794, 32767,
}

var adpcmIndexTable = []int8{
	-1, -1, -1, -1, 2, 4, 6, 8,
	-1, -1, -1, -1, 2, 4, 6, 8,
}

func decodeADPCMLegacy(data []byte) []float32 {
	if len(data) < 8 {
		return nil
	}

	buf := bytes.NewReader(data)
	var prev1, prev2 int16
	binary.Read(buf, binary.LittleEndian, &prev1)
	binary.Read(buf, binary.LittleEndian, &prev2)

	sampleCount := (len(data)-8)*2 + 2
	result := make([]float32, 0, sampleCount)
	result = append(result, float32(prev1)/32767)
	result = append(result, float32(prev2)/32767)

	stepIndex := 0

	for {
		b, err := buf.ReadByte()
		if err != nil {
			break
		}

		quantized1 := int8((b >> 4) & 0x0F)
		quantized2 := int8(b & 0x0F)

		step1 := adpcmStepTable[stepIndex]
		sign1 := quantized1 & 8
		magnitude1 := quantized1 & 7
		reconstructed1 := int32(step1)*int32(magnitude1)*2 + int32(step1)
		reconstructed1 /= 4
		if sign1 != 0 {
			reconstructed1 = -reconstructed1
		}
		reconstructed1 += int32(prev1*2 - prev2)
		if reconstructed1 > 32767 {
			reconstructed1 = 32767
		} else if reconstructed1 < -32768 {
			reconstructed1 = -32768
		}
		stepIndex += int(adpcmIndexTable[magnitude1])
		if stepIndex < 0 {
			stepIndex = 0
		}
		if stepIndex >= adpcmStepTableSize {
			stepIndex = adpcmStepTableSize - 1
		}
		prev2 = prev1
		prev1 = int16(reconstructed1)
		result = append(result, float32(prev1)/32767)

		step2 := adpcmStepTable[stepIndex]
		sign2 := quantized2 & 8
		magnitude2 := quantized2 & 7
		reconstructed2 := int32(step2)*int32(magnitude2)*2 + int32(step2)
		reconstructed2 /= 4
		if sign2 != 0 {
			reconstructed2 = -reconstructed2
		}
		reconstructed2 += int32(prev1*2 - prev2)
		if reconstructed2 > 32767 {
			reconstructed2 = 32767
		} else if reconstructed2 < -32768 {
			reconstructed2 = -32768
		}
		stepIndex += int(adpcmIndexTable[magnitude2])
		if stepIndex < 0 {
			stepIndex = 0
		}
		if stepIndex >= adpcmStepTableSize {
			stepIndex = adpcmStepTableSize - 1
		}
		prev2 = prev1
		prev1 = int16(reconstructed2)
		result = append(result, float32(prev1)/32767)
	}

	return result
}
