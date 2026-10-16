package tunnel

import (
	"net"
	"relaynet/internal/media"
	"relaynet/internal/proto"
	"testing"
	"time"
)

func TestStaleSourceSnapshotCannotUseNewSubscription(t *testing.T) {
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	conn, err := net.DialUDP("udp", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := New(Config{}, nil)
	c.conn, c.clientID = conn, 3
	c.screenSource, c.screenSubscriptionID = 2, 9
	// A retry captured source 1 before the UI switched to source 2/request 9.
	c.sendScreenSubscription(1, true)
	server.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, _, err := server.ReadFromUDP(make([]byte, 100)); err == nil {
		t.Fatal("stale source snapshot emitted an active request with the new request ID")
	}
	c.sendScreenSubscription(2, true)
	server.SetReadDeadline(time.Now().Add(time.Second))
	packet := make([]byte, 100)
	n, _, err := server.ReadFromUDP(packet)
	if err != nil {
		t.Fatal(err)
	}
	_, dst, ctrl, data, err := proto.DecodeMediaControl(packet[:n])
	request, decodeErr := proto.DecodeScreenSubscription(data)
	if err != nil || decodeErr != nil || dst != 2 || ctrl != proto.MediaCtrlScreenSubscribe || request.RequestID != 9 {
		t.Fatal("current source request is not sent")
	}
}

func TestPlaybackChangeKeepsAudioTimeline(t *testing.T) {
	c := New(Config{}, nil)
	timeline := media.NewScreenTimeline()
	c.screenSource, c.screenTimeline, c.screenSubscriptionID = 7, timeline, 1
	c.screenSubscriptionSources[7] = true
	frame := media.VideoFrame{SourceID: 7, StreamID: 100, HasAudio: true, TimestampNS: 0}
	timeline.ObserveVideo(frame, time.Now())
	timeline.VideoDecoded(100, 0)
	now := time.Now()
	for i := 0; i < 40; i++ {
		timeline.PushAudio(100, int64(i)*int64(media.ScreenAudioPacketDuration), make([]float32, 480), now)
	}
	timeline.FillAudio(make([]float32, 960), now)
	before, ready := timeline.Position(100, now.Add(20*time.Millisecond))
	if !ready {
		t.Fatal("audio did not start")
	}
	c.SetScreenPlayback(1, 30)
	after, ready := timeline.Position(100, now.Add(20*time.Millisecond))
	if c.screenTimeline != timeline || !ready || before != after || c.ScreenSubscriptionID() != 2 {
		t.Fatal("quality change restarted the audio clock")
	}
}

func TestOlderScreenSourceRemainsWatchable(t *testing.T) {
	c := New(Config{}, nil)
	c.videoRenderer = media.NewVideoRenderer(nil)
	c.videoRenderer.Start()
	defer c.videoRenderer.Close()
	c.screenSource, c.screenSubscriptionID = 7, 9
	meta := proto.ScreenPayload{Kind: proto.ScreenVideo, StreamID: 100, Sequence: 1}
	c.renderScreenVideo(7, media.FrameTypeJPEG, proto.EncodeScreenPayload(meta, []byte{1}))
	if _, frames, _ := c.GetVideoFramesSince(0); len(frames) != 1 {
		t.Fatal("selecting an older source rejects its original video")
	}
}

func TestOriginalBroadcastStopsWhenAllViewersSubscribe(t *testing.T) {
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	conn, err := net.DialUDP("udp", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := New(Config{}, nil)
	c.conn, c.clientID, c.dataKey, c.stop = conn, 1, make([]byte, 32), make(chan struct{})
	c.members[1], c.members[2] = Member{ID: 1}, Member{ID: 2}
	q := media.VideoQuality{Width: 1920, Height: 1080, FPS: 60, Bitrate: 6000}
	s := &screenShareSession{client: c, quality: q, streamID: 100, viewers: make(map[uint32]screenViewerRequest)}
	s.request(2, proto.ScreenSubscription{RequestID: 1, Active: true})
	s.send(media.VideoFrame{Data: []byte{9}}, 1, q, true)
	packet := make([]byte, 100)
	server.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := server.ReadFromUDP(packet)
	if err != nil {
		t.Fatal(err)
	}
	_, dst, err := proto.VideoDataHeader(packet[:n])
	if err != nil || dst != 2 {
		t.Fatal("original broadcast still consumes the source bandwidth budget")
	}
	server.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, _, err := server.ReadFromUDP(packet); err == nil {
		t.Fatal("source uploads duplicate original video for a subscribed viewer")
	}
}

func TestPlaybackProfileClampsToSource(t *testing.T) {
	source := media.VideoQuality{Width: 1920, Height: 1080, FPS: 60, Bitrate: 6000}
	low := screenPlaybackQuality(source, proto.ScreenSubscription{Resolution: 1, FPS: 30})
	if low.Width != 1280 || low.Height != 720 || low.FPS != 30 || low.Bitrate >= source.Bitrate {
		t.Fatalf("low profile does not lower bandwidth: %+v", low)
	}
	high := screenPlaybackQuality(source, proto.ScreenSubscription{Resolution: 4, FPS: 120})
	if high != source {
		t.Fatalf("viewer exceeds source quality: %+v", high)
	}
	if q := screenPlaybackQuality(source, proto.ScreenSubscription{}); q != source {
		t.Fatalf("original profile changed: %+v", q)
	}
}

func TestScreenVideoRejectsPreviousSubscription(t *testing.T) {
	c := New(Config{}, nil)
	c.videoRenderer = media.NewVideoRenderer(nil)
	c.videoRenderer.Start()
	defer c.videoRenderer.Close()
	c.screenSource, c.screenSubscriptionID = 7, 9
	c.screenSubscriptionSources[7] = true
	send := func(id uint64) {
		meta := proto.ScreenPayload{Kind: proto.ScreenVideo, StreamID: 100, Sequence: 1, SubscriptionID: id}
		c.renderScreenVideo(7, media.FrameTypeJPEG, proto.EncodeScreenPayload(meta, []byte{1}))
	}
	send(8)
	if _, frames, _ := c.GetVideoFramesSince(0); len(frames) != 0 {
		t.Fatal("old subscription video entered playback queue")
	}
	send(9)
	if _, frames, _ := c.GetVideoFramesSince(0); len(frames) != 1 || frames[0].SubscriptionID != 9 {
		t.Fatal("current subscription video was rejected")
	}
}
