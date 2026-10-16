package relay

import (
	"encoding/binary"
	"net"
	"relaynet/internal/proto"
	"testing"
	"time"
)

func TestScreenSubscriptionFiltersBroadcast(t *testing.T) {
	s, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.conn.Close()
	viewer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer viewer.Close()
	room := &Room{Members: make(map[uint32]*Member)}
	source := &Member{ID: 1, Addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12345}, Room: room}
	watched := &Member{ID: 2, Addr: viewer.LocalAddr().(*net.UDPAddr), Room: room}
	room.Members[1], room.Members[2] = source, watched
	s.byAddr[source.Addr.String()], s.byAddr[watched.Addr.String()] = source, watched
	request := make([]byte, 12)
	request[0], request[1], request[2], request[3] = 1, 1, 1, 30
	binary.BigEndian.PutUint64(request[4:12], 1)
	s.handleMediaControl(proto.EncodeMediaControl(2, 1, 7, request), watched.Addr)
	if !watched.ScreenNegotiated || watched.ScreenSource != 1 {
		t.Fatal("subscription not recorded")
	}
	stale := append([]byte(nil), request...)
	stale[1] = 0
	binary.BigEndian.PutUint64(stale[4:12], 2)
	s.handleMediaControl(proto.EncodeMediaControl(2, 1, 7, stale), watched.Addr)
	s.handleMediaControl(proto.EncodeMediaControl(2, 1, 7, request), watched.Addr)
	if watched.ScreenSource != 0 || watched.ScreenRequestID != 2 {
		t.Fatal("late old subscription revived stopped playback")
	}
	binary.BigEndian.PutUint64(request[4:12], 3)
	s.handleMediaControl(proto.EncodeMediaControl(2, 1, 7, request), watched.Addr)
	packet := []byte{proto.TypeVideoData, 0, 0, 0, 1, 255, 255, 255, 255, 0, 9}
	s.handleVideoData(packet, source.Addr)
	viewer.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, _, err = viewer.ReadFromUDP(make([]byte, 100)); err == nil {
		t.Fatal("low-quality viewer still receives original broadcast video")
	}
	binary.BigEndian.PutUint32(packet[5:9], 2)
	s.handleVideoData(packet, source.Addr)
	viewer.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err = viewer.ReadFromUDP(make([]byte, 100)); err != nil {
		t.Fatal("selected video is not forwarded", err)
	}
	request[1] = 2 // Legacy source: resume original broadcast delivery.
	binary.BigEndian.PutUint64(request[4:12], 4)
	s.handleMediaControl(proto.EncodeMediaControl(2, 1, 7, request), watched.Addr)
	binary.BigEndian.PutUint32(packet[5:9], proto.BroadcastID)
	s.handleVideoData(packet, source.Addr)
	viewer.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, _, err = viewer.ReadFromUDP(make([]byte, 100)); err != nil {
		t.Fatal("legacy source fallback does not restore broadcast", err)
	}
	request[1] = 1
	binary.BigEndian.PutUint64(request[4:12], 5)
	s.handleMediaControl(proto.EncodeMediaControl(2, 1, 7, request), watched.Addr)
	request[1] = 2
	binary.BigEndian.PutUint64(request[4:12], 6)
	s.handleMediaControl(proto.EncodeMediaControl(2, proto.BroadcastID, 7, request), watched.Addr)
	if watched.ScreenNegotiated || watched.ScreenSource != 0 {
		t.Fatal("idle viewer cannot discover older sources through original broadcasts")
	}
}
