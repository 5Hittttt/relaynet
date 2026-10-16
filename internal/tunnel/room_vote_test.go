package tunnel

import (
	"errors"
	"net"
	"testing"
	"time"

	"relaynet/internal/crypto"
	"relaynet/internal/proto"
)

func voteTestClient(t *testing.T) (*Client, *net.UDPConn) {
	t.Helper()
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialUDP("udp", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	c := New(Config{}, nil)
	c.conn, c.stop, c.clientID, c.hostID = conn, make(chan struct{}), 2, 1
	c.dataKey = crypto.DeriveDataKey("", "")
	t.Cleanup(func() { c.Disconnect(); server.Close() })
	return c, server
}

func readRoomControl(t *testing.T, server *net.UDPConn) proto.RoomControl {
	t.Helper()
	server.SetReadDeadline(time.Now().Add(time.Second))
	p := make([]byte, 1024)
	n, _, err := server.ReadFromUDP(p)
	if err != nil {
		t.Fatal(err)
	}
	command, err := proto.DecodeRoomControl(p[:n])
	if err != nil {
		t.Fatal(err)
	}
	return command
}

func voteState(sequence uint64) proto.RoomState {
	return proto.RoomState{Sequence: sequence, HostID: 1, GameHostID: 1,
		Members: []proto.Member{{ID: 1, Name: "source", IsHost: true}, {ID: 2, Name: "joiner"}, {ID: 3, Name: "target"}}}
}

func TestRoomControlRetriesUntilAcknowledged(t *testing.T) {
	c, server := voteTestClient(t)
	if err := c.StartRoomVote(3); err == nil {
		t.Fatal("unsupported server accepted vote")
	}
	c.handlePacket(proto.EncodeRoomState(voteState(1)))
	if err := c.StartRoomVote(3); err != nil {
		t.Fatal(err)
	}
	first := readRoomControl(t, server)
	if first.Action != proto.RoomControlStart || first.TargetID != 3 || first.RequestID == 0 {
		t.Fatalf("wrong start: %+v", first)
	}
	if err := c.StartRoomVote(1); err == nil {
		t.Fatal("accepted a second pending command")
	}
	c.sendRoomControl(c.conn)
	if retry := readRoomControl(t, server); retry != first {
		t.Fatalf("retry changed command: %+v", retry)
	}
	state := voteState(2)
	state.AckRequestID, state.Error = first.RequestID, "vote already active"
	c.handlePacket(proto.EncodeRoomState(state))
	st := c.Status()
	if st.RoomControlPending || st.RoomControlAckID != first.RequestID || st.RoomControlError != state.Error {
		t.Fatalf("ack not applied: %+v", st)
	}
	c.sendRoomControl(c.conn)
	if command := readRoomControl(t, server); command.Action != proto.RoomControlSync {
		t.Fatalf("not back to sync: %+v", command)
	}
	if err := c.CastRoomVote(8, false); err != nil {
		t.Fatal(err)
	}
	second := readRoomControl(t, server)
	if second.Action != proto.RoomControlNo || second.VoteID != 8 || second.RequestID <= first.RequestID {
		t.Fatalf("wrong ballot: %+v", second)
	}
	state.Sequence, state.AckRequestID = 3, second.RequestID
	c.handlePacket(proto.EncodeRoomState(state))
	if err := c.VetoRoomVote(8); err != nil {
		t.Fatal(err)
	}
	if command := readRoomControl(t, server); command.Action != proto.RoomControlVeto || command.RequestID <= second.RequestID {
		t.Fatalf("wrong veto: %+v", command)
	}
}

func TestRoomStateAuthoritativeRolesIgnoreLatePackets(t *testing.T) {
	c, _ := voteTestClient(t)
	c.members[1], c.members[2] = Member{ID: 1, IsHost: true, RTT: 15}, Member{ID: 2, RTT: 40}
	state := voteState(5)
	state.HostID = 2
	state.Vote = proto.RoomVote{ID: 9, Phase: proto.RoomVoteVeto, RemainingMS: 5000}
	c.handlePacket(proto.EncodeRoomState(state))
	c.roomStateReceived = time.Now().Add(-2 * time.Second)
	c.handlePacket(proto.EncodeRoomState(voteState(4)))
	c.handlePacket(proto.EncodeMemberUpdate(proto.EventJoin, proto.Member{ID: 1, IsHost: true}))
	c.handlePacket(proto.EncodeMemberUpdate(proto.EventLeave, proto.Member{ID: 2}))
	st := c.Status()
	if !st.RoomVotingSupported || !st.IsHost || st.HostID != 2 || st.GameHostID != 1 || len(st.Members) != 3 {
		t.Fatalf("late packet changed authority: %+v", st)
	}
	if st.RoomVote.ID != 9 || st.RoomVote.RemainingMS > 3000 || st.RoomVote.RemainingMS < 2800 {
		t.Fatalf("countdown not elapsed: %+v", st.RoomVote)
	}
	for _, m := range st.Members {
		if m.IsHost != (m.ID == 2) {
			t.Fatalf("incorrect member host flag: %+v", m)
		}
		if m.ID == 2 && m.RTT != 40 {
			t.Fatalf("snapshot discarded RTT: %+v", m)
		}
	}
	// Equal sequence must also be ignored, including an apparently newer ack.
	state.HostID, state.AckRequestID = 3, 900
	c.handlePacket(proto.EncodeRoomState(state))
	if st := c.Status(); st.HostID != 2 || st.RoomControlAckID != 0 {
		t.Fatalf("equal sequence applied: %+v", st)
	}
	c.roomStateReceived = time.Now().Add(-time.Minute)
	if c.Status().RoomVote.RemainingMS != 0 {
		t.Fatal("countdown underflowed")
	}
}

func TestLegacyMemberPromotionUpdatesRole(t *testing.T) {
	c, _ := voteTestClient(t)
	c.handleMemberUpdate(proto.EncodeMemberUpdate(proto.EventJoin, proto.Member{ID: 1, IsHost: true}))
	c.handleMemberUpdate(proto.EncodeMemberUpdate(proto.EventJoin, proto.Member{ID: 2}))
	c.handleMemberUpdate(proto.EncodeMemberUpdate(proto.EventLeave, proto.Member{ID: 1}))
	c.handleMemberUpdate(proto.EncodeMemberUpdate(proto.EventJoin, proto.Member{ID: 2, IsHost: true}))
	st := c.Status()
	if !st.IsHost || st.HostID != 2 || st.GameHostID != 2 {
		t.Fatalf("legacy promotion did not promote local client: %+v", st)
	}
}

func TestAdministrativeTransferPreservesUDPGameSource(t *testing.T) {
	c, server := voteTestClient(t)
	c.handlePacket(proto.EncodeRoomState(voteState(1)))
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	fs := &forwardState{remotePort: 7777, listener: listener}
	c.fwd[7777] = fs
	c.wg.Add(1)
	go c.forwardReadLoop(fs)
	state := voteState(2)
	state.HostID = 2
	c.handlePacket(proto.EncodeRoomState(state))
	game, err := net.DialUDP("udp", nil, listener.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer game.Close()
	game.Write([]byte("game packet"))
	server.SetReadDeadline(time.Now().Add(time.Second))
	p := make([]byte, 1024)
	n, _, err := server.ReadFromUDP(p)
	if err != nil {
		t.Fatal(err)
	}
	_, dst, _, _, err := proto.DecodeData(p[:n], c.dataKey)
	if err != nil || dst != 1 {
		t.Fatalf("game packet followed admin transfer: dst=%d err=%v", dst, err)
	}
	if c.fwd[7777] != fs {
		t.Fatal("admin transfer replaced forwarding listener")
	}
}

func TestRejectAndReadFailureCompleteCleanup(t *testing.T) {
	for _, reject := range []bool{true, false} {
		t.Run(map[bool]string{true: "reject", false: "read error"}[reject], func(t *testing.T) {
			c, server := voteTestClient(t)
			c.wg.Add(1)
			go c.readLoop()
			if reject {
				server.WriteToUDP(proto.EncodeReject("kicked"), c.conn.LocalAddr().(*net.UDPAddr))
			} else {
				c.conn.Close()
			}
			deadline := time.Now().Add(time.Second)
			for c.Status().Connected && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if c.Status().Connected {
				t.Fatal("reader did not disconnect")
			}
			done := make(chan struct{})
			go func() { c.Disconnect(); close(done) }()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("reader cleanup waited on itself")
			}
			if !c.screenSelectMu.TryLock() {
				t.Fatal("reader cleanup is deadlocked")
			}
			c.screenSelectMu.Unlock()
		})
	}
}

func TestRoomControlLoopDiscoversAndRetries(t *testing.T) {
	c, server := voteTestClient(t)
	c.wg.Add(1)
	go c.roomControlLoop(c.conn, c.stop)
	if command := readRoomControl(t, server); command.Action != proto.RoomControlSync || command.RequestID != 0 {
		t.Fatalf("discovery is not a sync: %+v", command)
	}
	c.handlePacket(proto.EncodeRoomState(voteState(1)))
	if err := c.StartRoomVote(3); err != nil {
		t.Fatal(err)
	}
	command := readRoomControl(t, server)
	server.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	p := make([]byte, 1024)
	n, _, err := server.ReadFromUDP(p)
	if err != nil {
		t.Fatal("periodic retry missing:", err)
	}
	retry, err := proto.DecodeRoomControl(p[:n])
	if err != nil || retry != command {
		t.Fatalf("periodic retry changed command: %+v, %v", retry, err)
	}
}

func TestOriginalGameSourceStillReceivesAfterAdminTransfer(t *testing.T) {
	c, _ := voteTestClient(t)
	c.clientID = 1
	state := voteState(1)
	state.HostID = 2
	c.handlePacket(proto.EncodeRoomState(state))
	game, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer game.Close()
	port := uint16(game.LocalAddr().(*net.UDPAddr).Port)
	p, err := proto.EncodeData(2, 1, c.dataKey, port, []byte("still serving"))
	if err != nil {
		t.Fatal(err)
	}
	c.handleData(p)
	game.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 100)
	if n, _, err := game.ReadFromUDP(buf); err != nil || string(buf[:n]) != "still serving" {
		t.Fatalf("original source stopped serving after transfer: %q, %v", buf[:n], err)
	}
	if c.Status().IsHost {
		t.Fatal("original game source retained administrative role")
	}
}

func voteTCPPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	local, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := listener.AcceptTCP()
	if err != nil {
		local.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close(); remote.Close() })
	return local, remote
}

func TestTCPConnectionRetainsOriginalGamePeer(t *testing.T) {
	c, server := voteTestClient(t)
	c.handlePacket(proto.EncodeRoomState(voteState(1)))
	local, game := voteTCPPair(t)
	ts := &tcpConnState{connID: 51, joinerID: 1, gameConn: local}
	c.tcpConns[51] = ts
	c.wg.Add(1)
	go c.tcpJoinerRelayLoop(ts, 51)
	state := voteState(2)
	state.HostID = 2
	c.handlePacket(proto.EncodeRoomState(state))
	game.Write([]byte("same TCP peer"))
	server.SetReadDeadline(time.Now().Add(time.Second))
	p := make([]byte, 1000)
	n, _, err := server.ReadFromUDP(p)
	if err != nil {
		t.Fatal(err)
	}
	_, dst, id, payload, err := proto.DecodeTCPData(p[:n], c.dataKey)
	if err != nil || dst != 1 || id != 51 || string(payload) != "same TCP peer" {
		t.Fatalf("TCP followed admin transfer: dst=%d id=%d payload=%q err=%v", dst, id, payload, err)
	}
	// A packet from a replacement source must not enter this connection.
	forged, _ := proto.EncodeTCPData(3, 2, 51, c.dataKey, []byte("other source"))
	c.handleTCPData(forged)
	game.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if _, err := game.Read(p); err == nil {
		t.Fatal("TCP connection accepted a different peer's data")
	}
	c.handleTCPClose(proto.EncodeTCPClose(3, 2, 51))
	if c.tcpConns[51] != ts {
		t.Fatal("different source closed TCP connection")
	}
	// When the old source leaves, close its connection instead of migrating it.
	state.Sequence, state.GameHostID = 3, 3
	state.Members = state.Members[1:]
	c.handlePacket(proto.EncodeRoomState(state))
	c.mu.Lock()
	_, exists := c.tcpConns[51]
	c.mu.Unlock()
	if exists || c.Status().GameHostID != 3 {
		t.Fatal("source departure retained old TCP state or ignored new source")
	}
	game.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := game.Read(p); err == nil {
		t.Fatal("old TCP stream remained open after source departure")
	}
}

func TestOriginalGameSourceAcceptsTCPAfterAdminTransfer(t *testing.T) {
	c, _ := voteTestClient(t)
	c.clientID = 1
	state := voteState(1)
	state.HostID = 2
	c.handlePacket(proto.EncodeRoomState(state))
	game, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer game.Close()
	c.handleTCPConnect(proto.EncodeTCPConnect(2, 1, 12, uint16(game.Addr().(*net.TCPAddr).Port)))
	game.SetDeadline(time.Now().Add(time.Second))
	conn, err := game.AcceptTCP()
	if err != nil {
		t.Fatal("game source refused TCP after losing admin role:", err)
	}
	defer conn.Close()
}

func TestStaleSessionCleanupCannotDisconnectNewConnection(t *testing.T) {
	c, server := voteTestClient(t)
	old := c.conn
	defer old.Close()
	current, err := net.DialUDP("udp", nil, server.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	c.conn = current
	c.disconnectSession(old)
	c.sendRoomControl(old)
	if !c.Status().Connected || c.conn != current {
		t.Fatal("old read error closed a replacement connection")
	}
	server.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if _, _, err := server.ReadFromUDP(make([]byte, 100)); err == nil {
		t.Fatal("stale room control loop wrote to replacement connection")
	}
}

func TestGameSourcePromotionReleasesJoinerListeners(t *testing.T) {
	c, _ := voteTestClient(t)
	c.handlePacket(proto.EncodeRoomState(voteState(1)))
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	c.fwd[7777] = &forwardState{remotePort: 7777, listener: udp}
	tcp, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	c.tcpForwards[7777] = &tcpForwardState{remotePort: 7777, listener: tcp}
	state := voteState(2)
	state.HostID, state.GameHostID = 2, 2
	state.Members = state.Members[1:]
	c.handlePacket(proto.EncodeRoomState(state))
	udp.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	if _, _, err := udp.ReadFromUDP(make([]byte, 1)); err == nil {
		t.Fatal("promoted source still has an active forwarding socket")
	}
	if len(c.fwd) != 0 || len(c.tcpForwards) != 0 {
		t.Fatal("promoted source retained joiner listeners on game ports")
	}
	if rebound, err := net.ListenUDP("udp", udp.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal("game cannot bind promoted source UDP port:", err)
	} else {
		rebound.Close()
	}
	if rebound, err := net.ListenTCP("tcp", tcp.Addr().(*net.TCPAddr)); err != nil {
		t.Fatal("game cannot bind promoted source TCP port:", err)
	} else {
		rebound.Close()
	}
}

func TestConnectAcceptsRoomStateBeforeJoined(t *testing.T) {
	server, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	served := make(chan error, 1)
	go func() {
		server.SetReadDeadline(time.Now().Add(time.Second))
		p := make([]byte, 1024)
		_, peer, err := server.ReadFromUDP(p)
		if err == nil {
			state := voteState(9)
			state.HostID = 2
			for _, packet := range [][]byte{
				proto.EncodeRoomState(state), {0xfe}, {proto.TypeRoomState},
				proto.EncodeRoomState(voteState(8)),
				proto.EncodeJoined(2, false, 1, voteState(8).Members),
			} {
				if _, err = server.WriteToUDP(packet, peer); err != nil {
					break
				}
			}
		}
		served <- err
	}()
	c := New(Config{ServerAddr: server.LocalAddr().String(), RoomID: "reordered", Name: "joiner"}, nil)
	defer c.Disconnect()
	if err := c.Connect(); err != nil {
		t.Fatal("reordered room state prevented connection:", err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); !st.Connected || !st.RoomVotingSupported || !st.IsHost || st.HostID != 2 || st.GameHostID != 1 {
		t.Fatalf("latest early room state was not applied after joined: %+v", st)
	}
}

func TestHandshakeUnrelatedPacketsKeepOriginalDeadline(t *testing.T) {
	c, server := voteTestClient(t)
	stop, served := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(served)
		server.SetReadDeadline(time.Now().Add(time.Second))
		_, peer, err := server.ReadFromUDP(make([]byte, 1024))
		if err != nil {
			return
		}
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				server.WriteToUDP([]byte{0xfe}, peer)
			}
		}
	}()
	defer func() { close(stop); <-served }()
	started := time.Now()
	err := c.handshake()
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("handshake did not retain its read deadline: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 4*time.Second || elapsed > 6*time.Second {
		t.Fatalf("unrelated packets changed five second handshake budget: %v", elapsed)
	}
	// The failed handshake must clear its expired deadline for its caller.
	server.WriteToUDP([]byte("deadline cleared"), c.conn.LocalAddr().(*net.UDPAddr))
	p := make([]byte, 100)
	if _, err := c.conn.Read(p); err != nil {
		t.Fatal("failed handshake left an expired read deadline:", err)
	}
}
