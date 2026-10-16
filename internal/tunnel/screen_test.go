package tunnel

import (
	"bytes"
	"net"
	"testing"
	"time"

	"relaynet/internal/crypto"
	"relaynet/internal/media"
	"relaynet/internal/proto"
	"relaynet/internal/relay"
)

func TestScreenMediaThroughRelay(t *testing.T) {
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
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
	config := Config{ServerAddr: addr, Name: "screen-source", RoomID: "av-sync-test", Password: "test-password", IsHost: true}
	host := New(config, func(string, ...interface{}) {})
	if err = host.Connect(); err != nil {
		t.Fatal(err)
	}
	defer host.Disconnect()
	config.IsHost, config.Name = false, "screen-viewer"
	viewer := New(config, func(string, ...interface{}) {})
	if err = viewer.Connect(); err != nil {
		t.Fatal(err)
	}
	// Exercise the device callback's real timeline without emitting test audio.
	tl := media.NewScreenTimeline()
	viewer.mu.Lock()
	viewer.screenSource, viewer.screenTimeline = host.clientID, tl
	viewer.screenAudioPlayer = &media.ScreenAudioPlayer{}
	viewer.mu.Unlock()
	defer func() { viewer.mu.Lock(); viewer.screenAudioPlayer = nil; viewer.mu.Unlock(); viewer.Disconnect() }()
	const stream = 100
	video := bytes.Repeat([]byte{0, 0, 0, 1, 0x65, 2, 3, 4}, 600)
	meta := proto.ScreenPayload{Kind: proto.ScreenVideo, HasAudio: true, StreamID: stream, Sequence: 1, TimestampNS: 0}
	blob, err := crypto.Seal(host.dataKey, proto.EncodeScreenPayload(meta, video))
	if err != nil {
		t.Fatal(err)
	}
	fragments := proto.EncodeVideoFragments(host.clientID, proto.BroadcastID, 1, media.FrameTypeH264IDR, blob)
	for i := len(fragments) - 1; i >= 0; i-- {
		if _, err = host.conn.Write(fragments[i]); err != nil {
			t.Fatal(err)
		}
	}
	for packet := 0; packet < 30; packet++ {
		samples := make([]float32, 480)
		for i := range samples {
			samples[i] = 0.25
		}
		meta := proto.ScreenPayload{Kind: proto.ScreenAudio, HasAudio: true, StreamID: stream, Sequence: uint32(packet + 1), TimestampNS: int64(packet) * int64(media.ScreenAudioPacketDuration)}
		payload := proto.EncodeScreenPayload(meta, media.EncodeScreenAudio(media.ScreenAudioHigh, samples))
		data, err := proto.EncodeSystemAudioData(host.clientID, proto.BroadcastID, host.dataKey, payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = host.conn.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	var frame media.VideoFrame
	for time.Now().Before(deadline) {
		_, frames, _ := viewer.GetVideoFramesSince(0)
		if len(frames) > 0 {
			frame = frames[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if frame.StreamID != stream || frame.Sequence != 1 || !frame.HasAudio || frame.TimestampNS != 0 || !bytes.Equal(frame.Data, video) {
		t.Fatal("relay lost synchronized video metadata or reordered fragments")
	}
	viewer.ScreenVideoDecoded(frame)
	out := make([]float32, 960)
	for time.Now().Before(deadline) {
		now := time.Now()
		tl.FillAudio(out, now)
		pts, ready := tl.Position(stream, now.Add(20*time.Millisecond))
		if ready {
			if pts != 0 || out[0] < 0.249 || out[0] > 0.251 {
				t.Fatalf("relayed audio did not start on video's timeline: pts=%d sample=%f", pts, out[0])
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("relayed audio/video never became ready together")
}
