package main

import (
	"fmt"
	"image"
	"image/color"
	"math/rand"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"relaynet/internal/media"
	"relaynet/internal/proto"
	"relaynet/internal/relay"
	"relaynet/internal/tunnel"
)

// Count actual video datagrams (including IPv4+UDP overhead) arriving at the
// viewer, so discarded original frames cannot masquerade as reduced bandwidth.
func countingScreenProxy(t *testing.T, server string) (string, *atomic.Int64) {
	t.Helper()
	socket, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	relayAddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := net.DialUDP("udp", nil, relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	var peer *net.UDPAddr
	var peerMu sync.Mutex
	var count atomic.Int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		data := make([]byte, 65535)
		for {
			n, addr, err := socket.ReadFromUDP(data)
			if err != nil {
				return
			}
			peerMu.Lock()
			peer = addr
			peerMu.Unlock()
			upstream.Write(data[:n])
		}
	}()
	go func() {
		defer wg.Done()
		data := make([]byte, 65535)
		for {
			n, err := upstream.Read(data)
			if err != nil {
				return
			}
			if n > 0 && (data[0] == proto.TypeVideoData || data[0] == proto.TypeVideoFragment) {
				count.Add(int64(n + 28))
			}
			peerMu.Lock()
			target := peer
			peerMu.Unlock()
			if target != nil {
				socket.WriteToUDP(data[:n], target)
			}
		}
	}()
	t.Cleanup(func() { socket.Close(); upstream.Close(); wg.Wait() })
	return socket.LocalAddr().String(), &count
}

func TestLiveScreenPlaybackProfiles(t *testing.T) {
	if os.Getenv("RELAYNET_LIVE_GUI_TEST") != "1" {
		t.Skip("requires a visible Windows capture window")
	}
	listen, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := listen.LocalAddr().String()
	listen.Close()
	server, err := relay.New(addr)
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve()
	cfg := tunnel.Config{ServerAddr: addr, RoomID: "playback-profiles", Password: "test", Name: "source", IsHost: true}
	host := tunnel.New(cfg, t.Logf)
	if err = host.Connect(); err != nil {
		t.Fatal(err)
	}
	defer host.Disconnect()
	lowAddr, lowBytes := countingScreenProxy(t, addr)
	highAddr, highBytes := countingScreenProxy(t, addr)
	cfg.IsHost, cfg.Name, cfg.ServerAddr = false, "low", lowAddr
	low := tunnel.New(cfg, t.Logf)
	if err = low.Connect(); err != nil {
		t.Fatal(err)
	}
	defer low.Disconnect()
	cfg.Name, cfg.ServerAddr = "high", highAddr
	high := tunnel.New(cfg, t.Logf)
	if err = high.Connect(); err != nil {
		t.Fatal(err)
	}
	defer high.Disconnect()
	source := host.Status().ClientID
	low.SetPeerVolume(source, 0)
	high.SetPeerVolume(source, 0)
	mu.Lock()
	client = low
	mu.Unlock()
	defer func() { mu.Lock(); client = nil; mu.Unlock() }()
	u := newUI(liveWindow)
	noise := make([]*image.RGBA, 16)
	rng := rand.New(rand.NewSource(123))
	for i := range noise {
		noise[i] = image.NewRGBA(image.Rect(0, 0, 512, 288))
		rng.Read(noise[i].Pix)
		for x := 3; x < len(noise[i].Pix); x += 4 {
			noise[i].Pix[x] = 255
		}
	}
	var activity *canvas.Image
	fyne.DoAndWait(func() {
		root := u.build()
		u.selectPage(2)
		u.showFPSCheck.SetChecked(true)
		u.playbackResolutionSel.SetSelected("720p")
		u.playbackFPSSel.SetSelected("30 FPS")
		activity = canvas.NewImageFromImage(noise[0])
		activity.SetMinSize(fyne.NewSize(512, 288))
		activity.FillMode = canvas.ImageFillContain
		liveWindow.Resize(fyne.NewSize(960, 900))
		liveWindow.SetContent(container.NewBorder(nil, activity, nil, nil, root))
	})
	stop := make(chan struct{})
	var stopActivity sync.Once
	defer stopActivity.Do(func() { close(stop) })
	go func() {
		tick := time.NewTicker(time.Second / 60)
		defer tick.Stop()
		frame := 0
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				shown := noise[frame%len(noise)]
				frame++
				fyne.Do(func() { activity.Image = shown; activity.Refresh() })
			}
		}
	}()
	go u.refreshLoop()
	q := media.VideoQualities["1080p"]
	q.FPS, q.Bitrate = 60, 12000
	if err = host.StartVideo(0, q, media.CaptureMethodAuto, media.ScreenAudioMedium, nil); err != nil {
		t.Fatal(err)
	}
	defer host.StopVideo()
	high.SetScreenPlayback(2, 60)
	high.SelectScreenSource(source)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fyne.DoAndWait(func() {
			u.refreshScreenTab()
			for i, id := range u.currentSharers {
				if id == source {
					u.screenSharersList.Select(i)
					break
				}
			}
		})
		if u.selectedSharer.Load() == source {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if u.selectedSharer.Load() != source {
		t.Fatal("sharing announcements do not discover the source")
	}
	time.Sleep(3 * time.Second) // Both encoders reach their first decodable keyframe.
	lowBytes.Store(0)
	highBytes.Store(0)
	var lowSeq, highSeq uint64
	var lowFrames, highFrames int
	var lowSize, highSize image.Point
	inspect := func(c *tunnel.Client, seq *uint64, total *int, size *image.Point) {
		next, frames, _ := c.GetVideoFramesSince(*seq)
		*seq = next
		for _, frame := range frames {
			if frame.SourceID != source || frame.SubscriptionID != c.ScreenSubscriptionID() {
				continue
			}
			*total++
			if size.X != 0 || frame.Type != media.FrameTypeH264IDR {
				continue
			}
			decoder, err := media.NewH264Decoder()
			if err != nil {
				t.Fatal(err)
			}
			_, w, h, _, err := decoder.SendAt(frame.Data, frame.TimestampNS)
			decoder.Close()
			if err != nil {
				t.Fatal(err)
			}
			if w > 0 {
				*size = image.Pt(w, h)
			}
		}
	}
	// Skip startup history, then compare the same six seconds at both sockets.
	lowSeq, _, _ = low.GetVideoFramesSince(0)
	highSeq, _, _ = high.GetVideoFramesSince(0)
	start := time.Now()
	for time.Since(start) < 6*time.Second {
		inspect(low, &lowSeq, &lowFrames, &lowSize)
		inspect(high, &highSeq, &highFrames, &highSize)
		time.Sleep(10 * time.Millisecond)
	}
	if lowSize != image.Pt(1280, 720) || highSize != image.Pt(1920, 1080) {
		t.Fatalf("wrong encoded sizes: low=%v high=%v", lowSize, highSize)
	}
	lowMbps, highMbps := float64(lowBytes.Load())*8/time.Since(start).Seconds()/1e6, float64(highBytes.Load())*8/time.Since(start).Seconds()/1e6
	t.Logf("720p30: %d frames, %.2f Mbps; 1080p60: %d frames, %.2f Mbps (actual UDP video including overhead)", lowFrames, lowMbps, highFrames, highMbps)
	if lowFrames < 100 || highFrames <= lowFrames || lowFrames > 195 || lowMbps >= highMbps*.85 {
		t.Fatal("independent playback options did not reduce actual bandwidth/cadence")
	}
	text := ""
	fyne.DoAndWait(func() { text = u.fpsLabel.Text })
	fps := 0
	fmt.Sscanf(text, "%d FPS", &fps)
	if fps < 15 || fps > 32 {
		t.Fatalf("30 FPS viewer did not play smoothly: %s", text)
	}
	// Change only the watching profile; StreamID/audio clock must survive.
	before := low.GetLatestFrame().StreamID
	oldRequest := low.ScreenSubscriptionID()
	fyne.DoAndWait(func() { u.playbackResolutionSel.SetSelected("1080p"); u.playbackFPSSel.SetSelected("60 FPS") })
	if low.ScreenSubscriptionID() <= oldRequest {
		t.Fatal("live controls did not issue a new subscription")
	}
	time.Sleep(4 * time.Second)
	frame := low.GetLatestFrame()
	fyne.DoAndWait(func() { text = u.fpsLabel.Text })
	fmt.Sscanf(text, "%d FPS", &fps)
	if frame.StreamID != before || frame.SubscriptionID != low.ScreenSubscriptionID() || fps < 35 {
		t.Fatalf("switch failed or reset audio stream: %s stream=%d/%d request=%d", text, before, frame.StreamID, frame.SubscriptionID)
	}
	t.Logf("switch to 1080p60 restored GUI: %s; shared stream unchanged", text)
	stopActivity.Do(func() { close(stop) })
	fyne.DoAndWait(func() { liveWindow.SetContent(canvas.NewRectangle(color.NRGBA{R: 40, G: 70, B: 90, A: 255})) })
	time.Sleep(time.Second)
	fyne.DoAndWait(func() { u.playbackResolutionSel.SetSelected("720p"); u.playbackFPSSel.SetSelected("30 FPS") })
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frame = low.GetLatestFrame()
		if frame.SubscriptionID == low.ScreenSubscriptionID() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if frame.SubscriptionID != low.ScreenSubscriptionID() {
		t.Fatal("switching profile on a static desktop never receives a picture")
	}
	t.Log("static desktop profile switch received current subscription")
}
