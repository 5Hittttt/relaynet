package relay

import (
	"fmt"
	"testing"
	"time"

	"relaynet/internal/proto"
	"relaynet/internal/tunnel"
)

func roomVoteUDPClients(t *testing.T, count int) (*Server, []*tunnel.Client) {
	t.Helper()
	s, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		s.Serve()
	}()
	t.Cleanup(func() {
		s.conn.Close()
		select {
		case <-served:
		case <-time.After(2 * time.Second):
			t.Error("relay read loop did not stop after closing its listener")
		}
	})
	clients := make([]*tunnel.Client, 0, count)
	for i := 0; i < count; i++ {
		c := tunnel.New(tunnel.Config{
			ServerAddr: s.conn.LocalAddr().String(),
			Name:       fmt.Sprintf("member-%d", i+1),
			Hostname:   "loopback",
			RoomID:     "room-vote-integration",
			Password:   "vote-password",
			Protocol:   "udp",
			IsHost:     i == 0,
		}, nil)
		t.Cleanup(func() { roomVoteDisconnect(t, c) })
		if err := c.Connect(); err != nil {
			t.Fatalf("connect member %d: %v", i+1, err)
		}
		clients = append(clients, c)
	}
	ids := roomVoteUDPIDs(clients)
	roomVoteUDPViews(t, clients, ids[0], ids[0], ids, 0, proto.RoomVoteNone)
	return s, clients
}

func roomVoteDisconnect(t *testing.T, c *tunnel.Client) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Disconnect()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("client disconnect did not finish; the read loop may be waiting on itself")
	}
}

func roomVoteUDPIDs(clients []*tunnel.Client) []uint32 {
	ids := make([]uint32, 0, len(clients))
	for _, c := range clients {
		ids = append(ids, c.Status().ClientID)
	}
	return ids
}

func waitRoomVoteUDPStatus(t *testing.T, c *tunnel.Client, description string, matches func(tunnel.Status) bool) tunnel.Status {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := c.Status()
		if matches(status) {
			return status
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s: %+v", description, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func roomVoteUDPViews(t *testing.T, clients []*tunnel.Client, hostID, gameHostID uint32, memberIDs []uint32, voteID uint64, phase byte) {
	t.Helper()
	for _, c := range clients {
		waitRoomVoteUDPStatus(t, c, fmt.Sprintf("member %d room roles and vote phase %d", c.Status().ClientID, phase), func(st tunnel.Status) bool {
			if !st.Connected || !st.RoomVotingSupported || st.HostID != hostID || st.GameHostID != gameHostID || st.IsHost != (st.ClientID == hostID) || len(st.Members) != len(memberIDs) {
				return false
			}
			if st.RoomVote.ID != voteID || st.RoomVote.Phase != phase || st.RoomControlPending || st.RoomControlError != "" {
				return false
			}
			for _, id := range memberIDs {
				found := false
				for _, m := range st.Members {
					if m.ID == id && m.IsHost == (id == hostID) {
						found = true
						break
					}
				}
				if !found {
					return false
				}
			}
			return true
		})
	}
}

func startRoomVoteUDP(t *testing.T, c *tunnel.Client, targetID uint32, phase byte) uint64 {
	t.Helper()
	ack := c.Status().RoomControlAckID
	if err := c.StartRoomVote(targetID); err != nil {
		t.Fatal(err)
	}
	st := waitRoomVoteUDPStatus(t, c, "acknowledged vote start", func(st tunnel.Status) bool {
		return st.RoomVote.ID != 0 && st.RoomVote.TargetID == targetID && st.RoomVote.Phase == phase && st.RoomControlAckID > ack && !st.RoomControlPending && st.RoomControlError == ""
	})
	return st.RoomVote.ID
}

// Move only the server's private clock boundary; vote commands and resulting
// snapshots still travel through the real UDP listener and tunnel clients.
func expireRoomVoteUDP(t *testing.T, s *Server, roomName string, voteID uint64, phase byte) {
	t.Helper()
	s.mu.Lock()
	r := s.rooms[roomName]
	if r == nil || r.Vote == nil || r.Vote.ID != voteID || r.Vote.Phase != phase {
		s.mu.Unlock()
		t.Fatalf("vote %d is not in expected phase %d before expiration", voteID, phase)
	}
	r.Vote.deadline = time.Now().Add(-time.Millisecond)
	s.mu.Unlock()
	s.sweep()
}

func TestRoomVoteUDPStrictMajorityAndHostVeto(t *testing.T) {
	_, clients := roomVoteUDPClients(t, 3)
	ids := roomVoteUDPIDs(clients)
	host, initiator, voter := clients[0], clients[1], clients[2]
	voteID := startRoomVoteUDP(t, initiator, ids[0], proto.RoomVoteVoting)
	roomVoteUDPViews(t, clients, ids[0], ids[0], ids, voteID, proto.RoomVoteVoting)
	if vote := initiator.Status().RoomVote; vote.Eligible != 2 || vote.Yes != 1 || vote.No != 0 || vote.Ballot != 1 || vote.CanVote {
		t.Fatalf("initiator's automatic vote should leave exactly half in favor: %+v", vote)
	}
	if host.Status().RoomVote.CanVote || !voter.Status().RoomVote.CanVote {
		t.Fatal("target or remaining voter's eligibility is incorrect")
	}
	if err := voter.CastRoomVote(voteID, true); err != nil {
		t.Fatal(err)
	}
	roomVoteUDPViews(t, clients, ids[0], ids[0], ids, voteID, proto.RoomVoteVeto)
	if vote := host.Status().RoomVote; vote.Yes != 2 || vote.VetoHostID != ids[0] || vote.Kind != proto.RoomVoteTransfer {
		t.Fatalf("strict majority did not enter the original host's veto period: %+v", vote)
	}
	if err := host.VetoRoomVote(voteID); err != nil {
		t.Fatal(err)
	}
	roomVoteUDPViews(t, clients, ids[0], ids[0], ids, voteID, proto.RoomVoteVetoed)
}

func TestRoomVoteUDPNoResponseTransfersAdminAndKeepsGameHost(t *testing.T) {
	s, clients := roomVoteUDPClients(t, 4)
	ids := roomVoteUDPIDs(clients)
	voteID := startRoomVoteUDP(t, clients[1], ids[0], proto.RoomVoteVoting)
	roomVoteUDPViews(t, clients, ids[0], ids[0], ids, voteID, proto.RoomVoteVoting)
	if err := clients[2].CastRoomVote(voteID, true); err != nil {
		t.Fatal(err)
	}
	roomVoteUDPViews(t, clients, ids[0], ids[0], ids, voteID, proto.RoomVoteVeto)
	if vote := clients[1].Status().RoomVote; vote.Eligible != 3 || vote.Yes != 2 || vote.VetoHostID != ids[0] {
		t.Fatalf("unexpected majority: %+v", vote)
	}
	expireRoomVoteUDP(t, s, clients[1].Status().RoomID, voteID, proto.RoomVoteVeto)
	roomVoteUDPViews(t, clients, ids[1], ids[0], ids, voteID, proto.RoomVoteExecuted)
}

func TestRoomVoteUDPKicksNormalTargetAndDisconnectsCleanly(t *testing.T) {
	s, clients := roomVoteUDPClients(t, 4)
	ids := roomVoteUDPIDs(clients)
	target := clients[2]
	voteID := startRoomVoteUDP(t, clients[1], ids[2], proto.RoomVoteVoting)
	roomVoteUDPViews(t, clients, ids[0], ids[0], ids, voteID, proto.RoomVoteVoting)
	if vote := target.Status().RoomVote; vote.CanVote || vote.Eligible != 3 || vote.Kind != proto.RoomVoteKick {
		t.Fatalf("normal target was not excluded from the kick electorate: %+v", vote)
	}
	if err := clients[3].CastRoomVote(voteID, true); err != nil {
		t.Fatal(err)
	}
	roomVoteUDPViews(t, clients, ids[0], ids[0], ids, voteID, proto.RoomVoteVeto)
	expireRoomVoteUDP(t, s, clients[1].Status().RoomID, voteID, proto.RoomVoteVeto)
	remaining := []*tunnel.Client{clients[0], clients[1], clients[3]}
	roomVoteUDPViews(t, remaining, ids[1], ids[0], []uint32{ids[0], ids[1], ids[3]}, voteID, proto.RoomVoteExecuted)
	waitRoomVoteUDPStatus(t, target, "kicked target disconnect", func(st tunnel.Status) bool { return !st.Connected })
	roomVoteDisconnect(t, target)
}

func TestRoomVoteUDPTwoMembersAutoVoteEntersVeto(t *testing.T) {
	_, clients := roomVoteUDPClients(t, 2)
	ids := roomVoteUDPIDs(clients)
	voteID := startRoomVoteUDP(t, clients[1], ids[0], proto.RoomVoteVeto)
	roomVoteUDPViews(t, clients, ids[0], ids[0], ids, voteID, proto.RoomVoteVeto)
	if vote := clients[1].Status().RoomVote; vote.Eligible != 1 || vote.Yes != 1 || vote.Ballot != 1 || vote.CanVote || vote.VetoHostID != ids[0] {
		t.Fatalf("two-member automatic majority is incorrect: %+v", vote)
	}
	if err := clients[0].VetoRoomVote(voteID); err != nil {
		t.Fatal(err)
	}
	roomVoteUDPViews(t, clients, ids[0], ids[0], ids, voteID, proto.RoomVoteVetoed)
}

func TestRoomVoteUDPHostDepartureCancelsVoteAndPromotesSuccessor(t *testing.T) {
	_, clients := roomVoteUDPClients(t, 3)
	ids := roomVoteUDPIDs(clients)
	voteID := startRoomVoteUDP(t, clients[1], ids[0], proto.RoomVoteVoting)
	roomVoteUDPViews(t, clients, ids[0], ids[0], ids, voteID, proto.RoomVoteVoting)
	roomVoteDisconnect(t, clients[0])
	roomVoteUDPViews(t, clients[1:], ids[1], ids[1], ids[1:], voteID, proto.RoomVoteCancelled)
}
