package main

import (
	"fmt"
	"image/color"
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"

	"relaynet/internal/media"
	"relaynet/internal/relay"
	"relaynet/internal/tunnel"
)

var liveWindow fyne.Window

// Fyne's real OpenGL driver must run on the main OS thread. Normal unit
// tests do not open a window; this desktop probe is explicitly enabled.
func TestMain(m *testing.M) {
	if os.Getenv("RELAYNET_LIVE_GUI_TEST") != "1" {
		os.Exit(m.Run())
	}
	a := app.NewWithID("com.relaynet.performance-probe")
	a.Settings().SetTheme(newFlatTheme())
	liveWindow = a.NewWindow("RelayNet 帧率测试（自动关闭）")
	liveWindow.SetPadded(false)
	liveWindow.Show()
	code := 0
	go func() {
		code = m.Run()
		fyne.Do(a.Quit)
	}()
	a.Run()
	os.Exit(code)
}

func TestLiveScreenGUI(t *testing.T) {
	if os.Getenv("RELAYNET_LIVE_GUI_TEST") != "1" {
		t.Skip("requires Windows desktop capture, audio output and a visible test window")
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
	config := tunnel.Config{ServerAddr: addr, RoomID: "screen-gui-probe", Password: "probe", Name: "source", IsHost: true}
	host := tunnel.New(config, t.Logf)
	if err = host.Connect(); err != nil {
		t.Fatal(err)
	}
	defer host.Disconnect()
	config.IsHost, config.Name = false, "viewer"
	viewer := tunnel.New(config, t.Logf)
	if err = viewer.Connect(); err != nil {
		t.Fatal(err)
	}
	defer viewer.Disconnect()
	hostID := host.Status().ClientID
	viewer.SetPeerVolume(hostID, 0) // Never play captured test audio.
	mu.Lock()
	client = viewer
	mu.Unlock()
	defer func() {
		mu.Lock()
		client = nil
		mu.Unlock()
	}()
	u := newUI(liveWindow)
	var activity *canvas.Rectangle
	fyne.DoAndWait(func() {
		root := u.build()
		u.selectPage(2)
		u.showFPSCheck.SetChecked(true)
		u.playbackResolutionSel.SetSelected("原画")
		u.playbackFPSSel.SetSelected("跟随共享端")
		activity = canvas.NewRectangle(color.NRGBA{R: 80, A: 255})
		activity.SetMinSize(fyne.NewSize(1, 8))
		liveWindow.Resize(fyne.NewSize(960, 600))
		liveWindow.SetContent(container.NewBorder(nil, activity, nil, nil, root))
	})
	go u.refreshLoop()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(time.Second / 120)
		defer ticker.Stop()
		var frame byte
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				frame++
				shade := frame
				fyne.Do(func() {
					activity.FillColor = color.NRGBA{R: shade, G: 255 - shade, A: 255}
					activity.Refresh()
				})
			}
		}
	}()
	qualityName := os.Getenv("RELAYNET_LIVE_GUI_QUALITY")
	if qualityName == "" {
		qualityName = "2160p"
	}
	quality, exists := media.VideoQualities[qualityName]
	if !exists {
		t.Fatalf("unknown quality: %s", qualityName)
	}
	quality.FPS = 120
	// Reproduce the user's paused local preview while the remote view plays.
	preview := make(chan media.VideoFrame, 128)
	previewStop, previewDone := make(chan struct{}), make(chan struct{})
	var missed atomic.Bool
	var sent atomic.Int32
	go u.previewDecodeLoop(preview, previewStop, previewDone, &missed)
	defer func() { close(previewStop); <-previewDone }()
	if err = host.StartVideo(0, quality, media.CaptureMethodAuto, media.ScreenAudioMedium, func(data []byte, typ byte) {
		sent.Add(1)
		select {
		case preview <- media.VideoFrame{Data: data, Type: typ, TimestampNS: media.CaptureTimestamp()}:
		default:
			missed.Store(true)
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer host.StopVideo()
	time.Sleep(2 * time.Second)
	selectSource := func() {
		u.refreshScreenTab()
		for i, id := range u.currentSharers {
			if id == hostID {
				u.screenSharersList.Select(i)
				return
			}
		}
	}
	fyne.DoAndWait(selectSource)
	if u.selectedSharer.Load() != hostID {
		t.Fatal("clicking the actual member list did not select the screen")
	}
	// Exercise the GUI reset path; the connected viewer still buffers media.
	// Selecting the same row after it reappears must restart the A/V clock.
	mu.Lock()
	client = nil
	mu.Unlock()
	fyne.DoAndWait(u.refreshScreenTab)
	mu.Lock()
	client = viewer
	mu.Unlock()
	fyne.DoAndWait(selectSource)
	if u.selectedSharer.Load() != hostID {
		t.Fatal("same-row selection after reset leaves playback at 0 FPS")
	}
	sent.Store(0)
	total := 0
	for sample := 0; sample < 10; sample++ {
		time.Sleep(time.Second)
		fps := 0
		text := ""
		fyne.DoAndWait(func() { text = u.fpsLabel.Text })
		fmt.Sscanf(text, "%d FPS", &fps)
		t.Logf("GUI second %d: %s; source=%d FPS", sample+1, text, sent.Swap(0))
		if sample >= 2 {
			total += fps
		}
	}
	if total/8 < 30 {
		t.Fatalf("real GUI remains slow: average=%d FPS", total/8)
	}
	t.Logf("real GUI playback average=%d FPS (refresh submissions, not physical presentation)", total/8)
	if qualityName == "1080p" && total/8 < 90 {
		t.Fatalf("120 FPS sharing still capped near 60 FPS: average=%d FPS", total/8)
	}
}
