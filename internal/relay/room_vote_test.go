package relay

import (
	"fmt"
	"net"
	"testing"
	"time"

	"relaynet/internal/proto"
)

func voteRoom(t *testing.T, count int) (*Server, *Room, time.Time) {
	t.Helper()
	s, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.conn.Close() })
	now := time.Unix(1000000, 0)
	r := &Room{Name: "vote", HostID: 1, GameHostID: 1, Members: make(map[uint32]*Member), NextID: uint32(count + 1)}
	s.rooms[r.Name] = r
	for i := 1; i <= count; i++ {
		m := &Member{ID: uint32(i), Name: fmt.Sprint(i), IsHost: i == 1, LastSeen: now, Room: r,
			Addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 12000 + i}}
		r.Members[m.ID], s.byAddr[m.Addr.String()] = m, m
	}
	return s, r, now
}

func voteCommand(s *Server, m *Member, action byte, request uint64, target uint32, vote uint64, now time.Time) {
	s.applyRoomControlLocked(m, proto.RoomControl{Action: action, RequestID: request, TargetID: target, VoteID: vote}, now)
}

func TestRoomVoteRequiresStrictMajority(t *testing.T) {
	for _, tc := range []struct{ electors, required int }{{2, 2}, {3, 2}, {4, 3}} {
		t.Run(fmt.Sprint(tc.electors), func(t *testing.T) {
			s, r, now := voteRoom(t, tc.electors+1)
			voteCommand(s, r.Members[2], proto.RoomControlStart, 1, 1, 0, now)
			v := s.roomStateLocked(r.Members[2], now).Vote
			if v.Eligible != uint16(tc.electors) || v.Yes != 1 || v.Phase != proto.RoomVoteVoting || v.Ballot != 1 || v.CanVote {
				t.Fatalf("bad initial vote: %+v", v)
			}
			for yes := 2; yes <= tc.required; yes++ {
				voteCommand(s, r.Members[uint32(yes+1)], proto.RoomControlYes, 1, 0, v.ID, now)
				got := s.roomStateLocked(r.Members[2], now).Vote
				want := proto.RoomVoteVoting
				if yes == tc.required {
					want = proto.RoomVoteVeto
				}
				if got.Phase != want || got.Yes != uint16(yes) {
					t.Fatalf("%d yes: %+v", yes, got)
				}
			}
		})
	}
}

func TestRoomVoteTwoMembersTransfersAdminAndPreservesGameSource(t *testing.T) {
	s, r, now := voteRoom(t, 2)
	old := r.Members[1]
	voteCommand(s, r.Members[2], proto.RoomControlStart, 1, 1, 0, now)
	state := s.roomStateLocked(r.Members[2], now)
	if state.Vote.Kind != proto.RoomVoteTransfer || state.Vote.Phase != proto.RoomVoteVeto || state.Vote.VetoHostID != 1 {
		t.Fatalf("not awaiting old host veto: %+v", state.Vote)
	}
	s.advanceRoomVoteLocked(r, now.Add(30*time.Second))
	state = s.roomStateLocked(r.Members[2], now.Add(30*time.Second))
	if state.HostID != 2 || state.GameHostID != 1 || state.Vote.Phase != proto.RoomVoteExecuted || len(r.Members) != 2 || r.Members[1] != old || old.IsHost || !r.Members[2].IsHost {
		t.Fatalf("transfer changed membership or game source: %+v", state)
	}
}

func TestRoomVoteKickRemovesTargetAndTransfersAdmin(t *testing.T) {
	s, r, now := voteRoom(t, 4)
	target := r.Members[3]
	voteCommand(s, r.Members[2], proto.RoomControlStart, 1, 3, 0, now)
	id := s.roomStateLocked(r.Members[2], now).Vote.ID
	voteCommand(s, r.Members[1], proto.RoomControlYes, 1, 0, id, now)
	s.advanceRoomVoteLocked(r, now.Add(30*time.Second))
	state := s.roomStateLocked(r.Members[2], now.Add(30*time.Second))
	if state.Vote.Kind != proto.RoomVoteKick || state.Vote.Phase != proto.RoomVoteExecuted || state.HostID != 2 || state.GameHostID != 1 || len(r.Members) != 3 || r.Members[3] != nil || s.byAddr[target.Addr.String()] != nil {
		t.Fatalf("kick/transfer failed: %+v", state)
	}
}

func TestRoomVoteRejectsUnauthorizedAndDuplicateCommands(t *testing.T) {
	s, r, now := voteRoom(t, 4)
	m := r.Members[2]
	voteCommand(s, m, proto.RoomControlStart, 1, 1, 0, now)
	id := s.roomStateLocked(m, now).Vote.ID
	voteCommand(s, m, proto.RoomControlStart, 1, 3, 0, now)
	if s.roomStateLocked(m, now).Vote.ID != id {
		t.Fatal("duplicate start replaced vote")
	}
	voteCommand(s, r.Members[1], proto.RoomControlYes, 1, 0, id, now)
	if st := s.roomStateLocked(r.Members[1], now); st.Error == "" || st.Vote.Yes != 1 || st.Vote.CanVote {
		t.Fatalf("target voted: %+v", st)
	}
	voteCommand(s, r.Members[3], proto.RoomControlNo, 2, 0, id, now)
	voteCommand(s, r.Members[3], proto.RoomControlYes, 1, 0, id, now)
	voteCommand(s, r.Members[3], proto.RoomControlYes, 3, 0, id, now)
	if st := s.roomStateLocked(r.Members[3], now); st.Vote.No != 1 || st.Vote.Yes != 1 || st.Vote.Ballot != 2 || st.AckRequestID != 3 || st.Error == "" {
		t.Fatalf("ballot changed: %+v", st)
	}
	voteCommand(s, r.Members[4], proto.RoomControlYes, 1, 0, id+1, now)
	if st := s.roomStateLocked(r.Members[4], now); st.Error == "" || st.Vote.Yes != 1 {
		t.Fatalf("stale vote accepted: %+v", st)
	}
	voteCommand(s, r.Members[4], proto.RoomControlYes, 2, 0, id, now)
	voteCommand(s, r.Members[3], proto.RoomControlVeto, 4, 0, id, now)
	if st := s.roomStateLocked(r.Members[3], now); st.Error == "" || st.Vote.Phase != proto.RoomVoteVeto {
		t.Fatalf("nonhost veto accepted: %+v", st)
	}
	voteCommand(s, r.Members[1], proto.RoomControlVeto, 2, 0, id, now)
	if st := s.roomStateLocked(r.Members[1], now); st.Error != "" || st.Vote.Phase != proto.RoomVoteVetoed || r.HostID != 1 {
		t.Fatalf("host veto failed: %+v", st)
	}
}

func TestRoomVoteDeadlinePrecedesLateBallotOrVeto(t *testing.T) {
	for _, veto := range []bool{false, true} {
		t.Run(fmt.Sprint(veto), func(t *testing.T) {
			s, r, now := voteRoom(t, 3)
			voteCommand(s, r.Members[2], proto.RoomControlStart, 1, 1, 0, now)
			id := s.roomStateLocked(r.Members[2], now).Vote.ID
			action, member, deadline, want := proto.RoomControlYes, r.Members[3], 60*time.Second, proto.RoomVoteExpired
			if veto {
				voteCommand(s, r.Members[3], proto.RoomControlYes, 1, 0, id, now)
				action, member, deadline, want = proto.RoomControlVeto, r.Members[1], 30*time.Second, proto.RoomVoteExecuted
			}
			voteCommand(s, member, action, 2, 0, id, now.Add(deadline))
			if st := s.roomStateLocked(member, now.Add(deadline)); st.Vote.Phase != want || st.Error == "" {
				t.Fatalf("late command accepted: %+v", st)
			}
			s.advanceRoomVoteLocked(r, now.Add(deadline+30*time.Second))
			if st := s.roomStateLocked(r.Members[2], now.Add(deadline+30*time.Second)); st.Vote.Phase != proto.RoomVoteNone {
				t.Fatalf("terminal not cleared: %+v", st.Vote)
			}
		})
	}
}

func TestRoomVoteFixedElectorateAndDepartureCancellation(t *testing.T) {
	for _, tc := range []struct {
		depart uint32
		veto   bool
	}{{1, false}, {2, false}, {3, false}, {4, false}, {1, true}, {2, true}} {
		t.Run(fmt.Sprint(tc.depart, tc.veto), func(t *testing.T) {
			depart := tc.depart
			s, r, now := voteRoom(t, 4)
			voteCommand(s, r.Members[2], proto.RoomControlStart, 1, 1, 0, now)
			id := s.roomStateLocked(r.Members[2], now).Vote.ID
			voteCommand(s, r.Members[3], proto.RoomControlNo, 1, 0, id, now)
			if tc.veto {
				voteCommand(s, r.Members[4], proto.RoomControlYes, 1, 0, id, now)
			}
			s.removeMemberLocked(r.Members[depart])
			observer := r.Members[4]
			if observer == nil {
				observer = r.Members[3]
			}
			v := s.roomStateLocked(observer, now).Vote
			want := proto.RoomVoteVoting
			if depart == 1 || depart == 2 {
				want = proto.RoomVoteCancelled
			}
			yes := uint16(1)
			if tc.veto {
				yes = 2
			}
			if v.Phase != want || v.Eligible != 3 || v.Yes != yes || v.No != 1 {
				t.Fatalf("departure changed fixed tally: %+v", v)
			}
			newMember := &Member{ID: 5, Room: r}
			r.Members[5] = newMember
			if st := s.roomStateLocked(newMember, now); st.Vote.CanVote {
				t.Fatal("late joiner can vote")
			}
		})
	}
}

func TestRoomVotePreservesDirectKickAuthorityAfterTransfer(t *testing.T) {
	s, r, now := voteRoom(t, 3)
	old, newHost := r.Members[1], r.Members[2]
	voteCommand(s, newHost, proto.RoomControlStart, 1, 1, 0, now)
	id := s.roomStateLocked(newHost, now).Vote.ID
	voteCommand(s, r.Members[3], proto.RoomControlYes, 1, 0, id, now)
	s.advanceRoomVoteLocked(r, now.Add(30*time.Second))
	s.handle(proto.EncodeKick(3), old.Addr)
	if r.Members[3] == nil {
		t.Fatal("demoted host retained direct kick authority")
	}
	s.handle(proto.EncodeKick(3), newHost.Addr)
	if r.Members[3] != nil || r.HostID != 2 || r.GameHostID != 1 {
		t.Fatal("new host cannot directly kick")
	}
}

func TestRoomControlTransportAcknowledgesAndPersonalizes(t *testing.T) {
	s, r, _ := voteRoom(t, 3)
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	m := r.Members[2]
	delete(s.byAddr, m.Addr.String())
	m.Addr = peer.LocalAddr().(*net.UDPAddr)
	s.byAddr[m.Addr.String()] = m
	s.handle(proto.EncodeRoomControl(proto.RoomControl{Action: proto.RoomControlStart, RequestID: 17, TargetID: 1}), m.Addr)
	readState := func() proto.RoomState {
		t.Helper()
		peer.SetReadDeadline(time.Now().Add(time.Second))
		b := make([]byte, 65535)
		n, _, err := peer.ReadFromUDP(b)
		if err != nil {
			t.Fatal(err)
		}
		state, err := proto.DecodeRoomState(b[:n])
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	first := readState()
	if first.AckRequestID != 17 || first.Vote.Ballot != 1 || first.Vote.CanVote || first.GameHostID != 1 || first.Vote.RemainingMS > 60000 || first.Vote.RemainingMS < 59000 {
		t.Fatalf("bad personalized response: %+v", first)
	}
	s.handle(proto.EncodeRoomControl(proto.RoomControl{Action: proto.RoomControlSync}), m.Addr)
	second := readState()
	if second.Sequence <= first.Sequence || second.AckRequestID != 17 || second.Vote.ID != first.Vote.ID {
		t.Fatalf("sync lost ack or ordering: %+v", second)
	}
}

func TestRoomHelloInitializesGameHostAndPublishesState(t *testing.T) {
	s, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.conn.Close()
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	packet, err := proto.EncodeHello(proto.Hello{Room: "fresh", Name: "host", IsHost: true, AuthToken: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	s.handle(packet, peer.LocalAddr().(*net.UDPAddr))
	peer.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 65535)
	n, _, err := peer.ReadFromUDP(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := proto.DecodeJoined(b[:n]); err != nil {
		t.Fatal(err)
	}
	n, _, err = peer.ReadFromUDP(b)
	if err != nil {
		t.Fatal(err)
	}
	state, err := proto.DecodeRoomState(b[:n])
	if err != nil {
		t.Fatal(err)
	}
	if state.HostID != 1 || state.GameHostID != 1 || len(state.Members) != 1 || !state.Members[0].IsHost {
		t.Fatalf("bad fresh room: %+v", state)
	}
}

func TestRoomReconnectCannotCancelVoteAfterExecutionDeadline(t *testing.T) {
	s, r, _ := voteRoom(t, 2)
	old := r.Members[1]
	now := time.Now().Add(-31 * time.Second)
	voteCommand(s, r.Members[2], proto.RoomControlStart, 1, 1, 0, now)
	packet, err := proto.EncodeHello(proto.Hello{Room: r.Name, Name: "rejoined", AuthToken: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	// The fixture uses no token; give the room the same token as Hello.
	r.AuthToken = make([]byte, 32)
	s.handle(packet, old.Addr)
	if st := s.roomStateLocked(r.Members[2], time.Now()); st.Vote.Phase != proto.RoomVoteExecuted || st.HostID != 2 {
		t.Fatalf("reconnect cancelled expired veto: %+v", st)
	}
}

func TestRoomSweepCancelsVoteWhenInitiatorAlreadyTimedOut(t *testing.T) {
	s, r, _ := voteRoom(t, 2)
	now := time.Now()
	voteCommand(s, r.Members[2], proto.RoomControlStart, 1, 1, 0, now.Add(-31*time.Second))
	r.Members[1].LastSeen = now
	r.Members[2].LastSeen = now.Add(-s.timeout - time.Second)
	s.sweep()
	state := s.roomStateLocked(r.Members[1], time.Now())
	if state.Vote.Phase != proto.RoomVoteCancelled || r.Members[2] != nil || state.HostID != 1 || state.GameHostID != 1 || r.Members[1] == nil {
		t.Fatalf("offline initiator executed vote: %+v", state)
	}
}

func TestRoomVoteNewRoundReplacesTerminalResult(t *testing.T) {
	s, r, now := voteRoom(t, 2)
	voteCommand(s, r.Members[2], proto.RoomControlStart, 1, 1, 0, now)
	first := s.roomStateLocked(r.Members[2], now).Vote.ID
	voteCommand(s, r.Members[1], proto.RoomControlVeto, 1, 0, first, now)
	voteCommand(s, r.Members[2], proto.RoomControlStart, 2, 1, 0, now)
	state := s.roomStateLocked(r.Members[2], now)
	if state.Error != "" || state.Vote.ID <= first || state.Vote.Phase != proto.RoomVoteVeto {
		t.Fatalf("terminal result blocks new round: %+v", state)
	}
}

func TestRoomVoteLockedVetoHostDepartureDoesNotCancel(t *testing.T) {
	s, r, now := voteRoom(t, 5)
	voteCommand(s, r.Members[2], proto.RoomControlStart, 1, 4, 0, now)
	id := s.roomStateLocked(r.Members[2], now).Vote.ID
	voteCommand(s, r.Members[3], proto.RoomControlYes, 1, 0, id, now)
	voteCommand(s, r.Members[5], proto.RoomControlYes, 1, 0, id, now)
	s.removeMemberLocked(r.Members[1])
	if r.HostID != 2 || r.GameHostID != 2 {
		t.Fatalf("automatic succession failed: %+v", r)
	}
	voteCommand(s, r.Members[2], proto.RoomControlVeto, 2, 0, id, now)
	if st := s.roomStateLocked(r.Members[2], now); st.Error == "" || st.Vote.Phase != proto.RoomVoteVeto || st.Vote.VetoHostID != 1 {
		t.Fatalf("successor vetoed: %+v", st)
	}
	s.advanceRoomVoteLocked(r, now.Add(30*time.Second))
	if r.Members[4] != nil || r.HostID != 2 {
		t.Fatal("host departure stopped execution")
	}
}

func TestRoomVoteGameSourceDeparturePreservesDifferentAdmin(t *testing.T) {
	s, r, now := voteRoom(t, 3)
	voteCommand(s, r.Members[3], proto.RoomControlStart, 1, 1, 0, now)
	id := s.roomStateLocked(r.Members[3], now).Vote.ID
	voteCommand(s, r.Members[2], proto.RoomControlYes, 1, 0, id, now)
	s.advanceRoomVoteLocked(r, now.Add(30*time.Second))
	s.removeMemberLocked(r.Members[1])
	if r.HostID != 3 || r.GameHostID != 2 || !r.Members[3].IsHost || r.Members[2].IsHost {
		t.Fatalf("source departure changed admin: %+v", r)
	}
}

func TestRoomControlUnknownAddressReceivesReject(t *testing.T) {
	s, _, _ := voteRoom(t, 2)
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	s.handle(proto.EncodeRoomControl(proto.RoomControl{Action: proto.RoomControlSync}), peer.LocalAddr().(*net.UDPAddr))
	peer.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 65535)
	n, _, err := peer.ReadFromUDP(b)
	if err != nil {
		t.Fatal("lost kick rejection cannot recover:", err)
	}
	if _, err := proto.DecodeReject(b[:n]); err != nil {
		t.Fatal("unknown source did not receive rejection:", err)
	}
}

func TestRoomVoteGlobalIDsAndSnapshotSequence(t *testing.T) {
	s, r, now := voteRoom(t, 2)
	voteCommand(s, r.Members[2], proto.RoomControlStart, 1, 1, 0, now)
	first := s.roomStateLocked(r.Members[2], now)
	second := s.roomStateLocked(r.Members[1], now)
	if second.Sequence <= first.Sequence || second.AckRequestID != 0 || second.Vote.Ballot != 0 || first.AckRequestID != 1 {
		t.Fatal("states not personalized and increasing")
	}
	voteCommand(s, r.Members[1], proto.RoomControlVeto, 1, 0, first.Vote.ID, now)
	r2 := &Room{Name: "other", HostID: 8, GameHostID: 8, Members: map[uint32]*Member{}}
	m8, m9 := &Member{ID: 8, IsHost: true, Room: r2}, &Member{ID: 9, Room: r2}
	r2.Members[8], r2.Members[9] = m8, m9
	voteCommand(s, m9, proto.RoomControlStart, 1, 8, 0, now)
	if got := s.roomStateLocked(m9, now).Vote.ID; got <= first.Vote.ID {
		t.Fatal("vote IDs reused across rooms")
	}
	voteCommand(s, m8, proto.RoomControlVeto, 1, 0, first.Vote.ID, now)
	if st := s.roomStateLocked(m8, now); st.Error == "" || st.Vote.Phase != proto.RoomVoteVeto {
		t.Fatal("cross-room command accepted")
	}
}
