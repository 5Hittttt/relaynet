package media

import (
	"time"
	"unsafe"

	"github.com/gen2brain/malgo"
)

// The screen player has one device and one timeline for the selected sharer.
// Local preview stays muted, preventing playback of our own loopback capture.
type ScreenAudioPlayer struct {
	ctx    *malgo.AllocatedContext
	device *malgo.Device
}

func NewScreenAudioPlayer(timeline *ScreenTimeline) (*ScreenAudioPlayer, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}
	config := malgo.DefaultDeviceConfig(malgo.Playback)
	config.Playback.Format, config.Playback.Channels, config.SampleRate = malgo.FormatF32, 2, 48000
	config.PeriodSizeInMilliseconds, config.Periods = 10, 2
	device, err := malgo.InitDevice(ctx.Context, config, malgo.DeviceCallbacks{Data: func(out, in []byte, frames uint32) {
		if len(out) == 0 {
			return
		}
		samples := unsafe.Slice((*float32)(unsafe.Pointer(&out[0])), len(out)/4)
		timeline.FillAudio(samples, time.Now())
	}})
	if err != nil {
		ctx.Context.Uninit()
		ctx.Free()
		return nil, err
	}
	if err = device.Start(); err != nil {
		device.Uninit()
		ctx.Context.Uninit()
		ctx.Free()
		return nil, err
	}
	return &ScreenAudioPlayer{ctx, device}, nil
}

func (p *ScreenAudioPlayer) Close() {
	p.device.Stop()
	p.device.Uninit()
	p.ctx.Context.Uninit()
	p.ctx.Free()
}
