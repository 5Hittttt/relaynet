package tunnel

import (
	"errors"
	"net"
	"time"

	"relaynet/internal/proto"
)

// StartRoomVote asks the server to begin a vote against another room member.
func (c *Client) StartRoomVote(targetID uint32) error {
	return c.queueRoomControl(proto.RoomControl{Action: proto.RoomControlStart, TargetID: targetID})
}

// CastRoomVote records one final ballot for the specified vote.
func (c *Client) CastRoomVote(voteID uint64, agree bool) error {
	action := proto.RoomControlNo
	if agree {
		action = proto.RoomControlYes
	}
	return c.queueRoomControl(proto.RoomControl{Action: action, VoteID: voteID})
}

// VetoRoomVote asks the server to cancel a vote during its host veto period.
func (c *Client) VetoRoomVote(voteID uint64) error {
	return c.queueRoomControl(proto.RoomControl{Action: proto.RoomControlVeto, VoteID: voteID})
}

func (c *Client) queueRoomControl(command proto.RoomControl) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return errors.New("未连接")
	}
	if !c.roomVotingSupported {
		return errors.New("服务端不支持投票")
	}
	if c.roomControlPending != nil {
		return errors.New("上一条请求仍在等待确认")
	}
	c.roomControlNextID++
	command.RequestID = c.roomControlNextID
	c.roomControlPending = &command
	c.roomControlError = ""
	_, err := c.conn.Write(proto.EncodeRoomControl(command))
	return err
}

func (c *Client) roomControlLoop(conn *net.UDPConn, stop <-chan struct{}) {
	defer c.wg.Done()
	c.sendRoomControl(conn)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			c.sendRoomControl(conn)
		}
	}
}

func (c *Client) sendRoomControl(conn *net.UDPConn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if conn == nil || c.conn != conn {
		return
	}
	command := proto.RoomControl{Action: proto.RoomControlSync}
	if c.roomControlPending != nil {
		command = *c.roomControlPending
	}
	conn.Write(proto.EncodeRoomControl(command))
}

func (c *Client) handleRoomState(packet []byte) {
	state, err := proto.DecodeRoomState(packet)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.roomVotingSupported && state.Sequence <= c.roomStateSequence {
		return
	}
	c.roomVotingSupported = true
	c.roomStateSequence = state.Sequence
	c.roomStateReceived = time.Now()
	c.roomVote = state.Vote
	if state.AckRequestID >= c.roomControlAckID {
		c.roomControlAckID = state.AckRequestID
		c.roomControlError = state.Error
	}
	if c.roomControlPending != nil && state.AckRequestID >= c.roomControlPending.RequestID {
		c.roomControlPending = nil
	}
	members := make(map[uint32]Member, len(state.Members))
	for _, m := range state.Members {
		rtt := -1
		if old, ok := c.members[m.ID]; ok {
			rtt = old.RTT
		}
		members[m.ID] = Member{ID: m.ID, IsHost: m.ID == state.HostID, Name: m.Name, Hostname: m.Hostname, RTT: rtt}
	}
	for id := range c.members {
		if _, present := members[id]; !present {
			c.removeMemberLocked(id)
		}
	}
	c.members = members
	c.hostID = state.HostID
	c.setGameHostLocked(state.GameHostID)
	c.isHost = c.clientID == state.HostID
}

func (c *Client) setGameHostLocked(id uint32) {
	if id == c.clientID && c.gameHostID != id {
		// A joiner's listeners occupy the ports the replacement game server
		// needs. Administrative transfers never enter this branch.
		for _, fs := range c.fwd {
			fs.listener.Close()
		}
		c.fwd = make(map[int]*forwardState)
		for _, fs := range c.tcpForwards {
			fs.listener.Close()
		}
		c.tcpForwards = make(map[int]*tcpForwardState)
	}
	c.gameHostID = id
}

func (c *Client) roomVoteSnapshotLocked() proto.RoomVote {
	vote := c.roomVote
	if !c.roomStateReceived.IsZero() {
		elapsed := time.Since(c.roomStateReceived).Milliseconds()
		if elapsed >= int64(vote.RemainingMS) {
			vote.RemainingMS = 0
		} else if elapsed > 0 {
			vote.RemainingMS -= uint32(elapsed)
		}
	}
	return vote
}

func (c *Client) resetRoomVotingLocked() {
	c.roomVotingSupported = false
	c.roomStateSequence = 0
	c.roomStateReceived = time.Time{}
	c.roomVote = proto.RoomVote{}
	c.roomControlAckID, c.roomControlNextID = 0, 0
	c.roomControlError = ""
	c.roomControlPending = nil
}

// removeMemberLocked also closes game sockets tied to a departed peer. Existing
// TCP connections retain their peer ID and are never reassigned to a new source.
func (c *Client) removeMemberLocked(id uint32) {
	delete(c.members, id)
	delete(c.screenSubscriptionSources, id)
	delete(c.peerPingSent, id)
	delete(c.peerVolumes, id)
	c.videoSharerMu.Lock()
	delete(c.videoSharers, id)
	c.videoSharerMu.Unlock()
	for key, es := range c.expose {
		if es.joinerID == id {
			es.gameConn.Close()
			delete(c.expose, key)
		}
	}
	for connID, ts := range c.tcpConns {
		if ts.joinerID == id {
			ts.gameConn.Close()
			delete(c.tcpConns, connID)
		}
	}
	c.fragMu.Lock()
	for key := range c.fragBuf {
		if uint32(key>>32) == id {
			delete(c.fragBuf, key)
		}
	}
	c.fragMu.Unlock()
}

// Background game readers can finish after the session has begun closing.
func (c *Client) writeGamePacket(packet []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		c.conn.Write(packet)
	}
}
