package main

import (
	"image/png"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/dialog"
	"relaynet/internal/proto"
	"relaynet/internal/relay"
	"relaynet/internal/tunnel"
)

// Opt-in native UI + real UDP probe; no desktop/audio capture is involved.
func TestLiveRoomVoteVetoAndTimeout(t *testing.T) {
	if os.Getenv("RELAYNET_LIVE_GUI_TEST") != "1" {
		t.Skip("requires the Windows test window")
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
	config := tunnel.Config{ServerAddr: addr, Name: "房主", RoomID: "vote-ui", Password: "test", IsHost: true}
	host := tunnel.New(config, nil)
	if err = host.Connect(); err != nil {
		t.Fatal(err)
	}
	defer host.Disconnect()
	config.IsHost, config.Name = false, "发起人"
	member := tunnel.New(config, nil)
	if err = member.Connect(); err != nil {
		t.Fatal(err)
	}
	defer member.Disconnect()
	mu.Lock()
	previous := client
	client = host
	mu.Unlock()
	defer func() { mu.Lock(); client = previous; mu.Unlock() }()
	u := newUI(liveWindow)
	fyne.DoAndWait(func() {
		liveWindow.SetTitle("RelayNet 投票检查（自动关闭）")
		liveWindow.Resize(fyne.NewSize(1280, 800))
		liveWindow.SetContent(u.build())
	})
	wait := func(limit time.Duration, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for time.Now().Before(deadline) {
			if condition() {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatal("native vote probe timed out")
	}
	wait(5*time.Second, func() bool { return member.Status().RoomVotingSupported })
	hostID := host.Status().ClientID
	memberID := member.Status().ClientID
	if err = member.StartRoomVote(hostID); err != nil {
		t.Fatal(err)
	}
	var veto *dialog.ConfirmDialog
	wait(5*time.Second, func() bool {
		fyne.DoAndWait(func() { u.refresh(); veto = u.vetoDialog })
		return veto != nil
	})
	if dir := os.Getenv("RELAYNET_UI_SNAPSHOTS"); dir != "" {
		if err = os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		f, err := os.Create(filepath.Join(dir, "room-vote-veto.png"))
		if err != nil {
			t.Fatal(err)
		}
		err = png.Encode(f, liveWindow.Canvas().Capture())
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("save vote window: %v / %v", err, closeErr)
		}
	}
	fyne.DoAndWait(veto.Confirm)
	wait(5*time.Second, func() bool { return member.Status().RoomVote.Phase == proto.RoomVoteVetoed })
	if !host.Status().IsHost || member.Status().IsHost {
		t.Fatal("explicit veto changed the host")
	}
	t.Log("native veto button cancelled the vote")
	fyne.DoAndWait(u.refresh)
	wait(5*time.Second, func() bool { return !member.Status().RoomControlPending })
	if err = member.StartRoomVote(hostID); err != nil {
		t.Fatal(err)
	}
	veto = nil
	wait(5*time.Second, func() bool {
		fyne.DoAndWait(func() { u.refresh(); veto = u.vetoDialog })
		return veto != nil
	})
	start := time.Now()
	fyne.DoAndWait(veto.Hide)
	var reopen bool
	wait(35*time.Second, func() bool {
		fyne.DoAndWait(func() { u.refresh(); reopen = u.vetoDialog != nil })
		if reopen {
			t.Fatal("dismissed veto window reopened")
		}
		return host.Status().RoomVote.Phase == proto.RoomVoteExecuted && member.Status().IsHost
	})
	if elapsed := time.Since(start); elapsed < 28*time.Second {
		t.Fatalf("dismissal executed before the real 30-second deadline: %v", elapsed)
	}
	st := host.Status()
	if !st.Connected || st.IsHost || len(st.Members) != 2 || st.HostID != memberID || st.GameHostID != hostID {
		t.Fatalf("old host or game source changed incorrectly: %+v", st)
	}
	fyne.DoAndWait(u.refresh)
	t.Log("closing the real popup did not veto; server executed after 30 seconds, old host stayed and game source was preserved")
}
