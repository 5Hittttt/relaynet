package tunnel

import (
	"net"
	"os"
	"testing"
	"time"

	"relaynet/internal/media"
	"relaynet/internal/relay"
)

func TestLiveScreenLateViewer(t *testing.T) {
	if os.Getenv("RELAYNET_LIVE_SCREEN_TEST") != "1" {
		t.Skip("requires Windows desktop capture and playback devices")
	}
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.LocalAddr().String()
	listener.Close()
	server, err := relay.New(addr)
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve()
	config := Config{ServerAddr: addr, RoomID: "screen-startup-probe", Password: "probe", Name: "source", IsHost: true}
	host := New(config, t.Logf)
	if err = host.Connect(); err != nil {
		t.Fatal(err)
	}
	defer host.Disconnect()
	config.IsHost, config.Name = false, "viewer"
	viewer := New(config, t.Logf)
	if err = viewer.Connect(); err != nil {
		t.Fatal(err)
	}
	defer viewer.Disconnect()
	viewer.SetPeerVolume(host.clientID, 0) // Probe never emits captured desktop audio.
	quality := media.VideoQualities["2160p"]
	quality.FPS = 120
	if err = host.StartVideo(0, quality, media.CaptureMethod(0), media.ScreenAudioMedium, nil); err != nil {
		t.Fatal(err)
	}
	defer host.StopVideo()
	time.Sleep(2 * time.Second)
	viewer.SelectScreenSource(host.clientID)
	var decoder *media.H264Decoder
	defer func() {
		if decoder != nil {
			decoder.Close()
		}
	}()
	var order media.ScreenVideoOrder
	var encoded []media.VideoFrame
	var lastSeq uint64
	var lastFrame uint32
	var latest media.VideoFrame
	var scheduled []media.VideoFrame
	var lastDisplay time.Time
	received, decoded, displayed, gaps := 0, 0, 0, 0
	deadline, nextLog := time.Now().Add(10*time.Second), time.Now()
	for time.Now().Before(deadline) {
		seq, frames, missed := viewer.GetVideoFramesSince(lastSeq)
		lastSeq = seq
		if missed && decoder != nil {
			decoder.Close()
			decoder = nil
			lastFrame = 0
		}
		for _, frame := range frames {
			order.Push(frame)
			received++
		}
		encoded = append(encoded, order.Ready(time.Now())...)
		for len(encoded) > 0 {
			frame := encoded[0]
			viewer.ScreenPosition(frame)
			gap := frame.Discontinuity || (lastFrame != 0 && frame.Sequence != lastFrame+1)
			if decoder != nil && !gap && frame.TimestampNS > viewer.ScreenDecodeLimit(frame) {
				break
			}
			encoded = encoded[1:]
			if gap && decoder != nil {
				decoder.Close()
				decoder = nil
				gaps++
			}
			lastFrame = frame.Sequence
			if frame.Type == media.FrameTypeH264P && decoder == nil {
				continue
			}
			if decoder == nil {
				decoder, err = media.NewH264Decoder()
				if err != nil {
					t.Fatal(err)
				}
			}
			if frame.TimestampNS < viewer.ScreenDiscardBefore(frame) {
				if err := decoder.SkipAt(frame.Data, frame.TimestampNS); err != nil {
					decoder.Close()
					decoder = nil
				}
				continue
			}
			// Match the displayed pixel budget of a 1280x720 viewing area;
			// the transmitted stream remains 3840x2160.
			pixels, w, h, pts, decErr := decoder.SendSizedAt(frame.Data, frame.TimestampNS, 1280, 720)
			if decErr != nil {
				decoder.Close()
				decoder = nil
				t.Logf("decode error: %v", decErr)
				continue
			}
			if len(pixels) == 0 {
				continue
			}
			if w != 1280 || h != 720 {
				t.Fatalf("display dimensions wrong: %dx%d", w, h)
			}
			decoded++
			frame.TimestampNS = pts
			viewer.ScreenVideoDecoded(frame)
			latest = frame
			scheduled = append(scheduled, frame)
			if _, ready := viewer.ScreenPosition(frame); ready {
				break
			}
		}
		position, ready := viewer.ScreenPosition(latest)
		if ready {
			due := -1
			for i, frame := range scheduled {
				if frame.TimestampNS <= position {
					due = i
				}
			}
			if due >= 0 {
				if position-scheduled[due].TimestampNS <= int64(100*time.Millisecond) {
					displayed++
					lastDisplay = time.Now()
				}
				scheduled = scheduled[due+1:]
			}
		}
		if time.Now().After(nextLog) {
			t.Logf("received=%d decoded=%d displayed=%d gaps=%d queued=%d latest=%s clock=%s ready=%v limit=%s", received, decoded, displayed, gaps, len(encoded), time.Duration(latest.TimestampNS), time.Duration(position), ready, time.Duration(viewer.ScreenDecodeLimit(latest)))
			nextLog = time.Now().Add(time.Second)
		}
		time.Sleep(8 * time.Millisecond)
	}
	t.Logf("10-second probe totals: received=%d decoded=%d displayed=%d", received, decoded, displayed)
	if received == 0 || decoded == 0 || displayed < 50 || displayed*3 < decoded || time.Since(lastDisplay) > time.Second {
		t.Fatalf("screen failed to start: received=%d decoded=%d displayed=%d", received, decoded, displayed)
	}
}
