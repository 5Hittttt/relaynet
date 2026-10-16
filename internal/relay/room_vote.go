package relay

import (
	"net"
	"sort"
	"time"

	"relaynet/internal/proto"
)

const (
	roomVotingTime     = 60 * time.Second
	roomVetoTime       = 30 * time.Second
	roomVoteResultTime = 30 * time.Second
)

type roomVote struct {
	proto.RoomVote
	ballots  map[uint32]byte // Fixed electorate; departed members remain here.
	deadline time.Time
	clearAt  time.Time
}

func (v *roomVote) active() bool {
	return v.Phase == proto.RoomVoteVoting || v.Phase == proto.RoomVoteVeto
}

func (v *roomVote) finish(phase byte, now time.Time) {
	v.Phase = phase
	v.clearAt = now.Add(roomVoteResultTime)
}

func (s *Server) handleRoomControl(packet []byte, addr *net.UDPAddr) {
	command, err := proto.DecodeRoomControl(packet)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.byAddr[addr.String()]
	if m == nil {
		s.send(addr, proto.EncodeReject("已离开房间，请重新连接"))
		return
	}
	now := time.Now()
	m.LastSeen = now
	s.applyRoomControlLocked(m, command, now)
	// A deadline can execute a kick before the command is considered.
	if s.byAddr[addr.String()] != m {
		s.send(addr, proto.EncodeReject("已被移出房间"))
		return
	}
	if command.Action == proto.RoomControlSync {
		s.send(addr, proto.EncodeRoomState(s.roomStateLocked(m, now)))
	} else {
		s.broadcastRoomStateLocked(m.Room, now)
	}
}

func (s *Server) applyRoomControlLocked(m *Member, c proto.RoomControl, now time.Time) {
	r := m.Room
	s.advanceRoomVoteLocked(r, now)
	if r.Members[m.ID] != m || c.Action == proto.RoomControlSync {
		return
	}
	if c.RequestID == 0 || c.RequestID <= m.RoomControlAckID {
		return
	}
	m.RoomControlAckID, m.RoomControlError = c.RequestID, ""
	if c.Action == proto.RoomControlStart {
		if r.Vote != nil && r.Vote.active() {
			m.RoomControlError = "已有投票正在进行"
			return
		}
		target := r.Members[c.TargetID]
		if c.VoteID != 0 || target == nil || target == m {
			m.RoomControlError = "投票目标无效"
			return
		}
		s.nextVoteID++
		kind := proto.RoomVoteKick
		if target.ID == r.HostID {
			kind = proto.RoomVoteTransfer
		}
		v := &roomVote{RoomVote: proto.RoomVote{ID: s.nextVoteID, Kind: kind, Phase: proto.RoomVoteVoting,
			InitiatorID: m.ID, TargetID: target.ID, InitiatorName: m.Name, TargetName: target.Name},
			ballots: make(map[uint32]byte), deadline: now.Add(roomVotingTime)}
		for id := range r.Members {
			if id != target.ID {
				v.ballots[id] = 0
			}
		}
		v.Eligible = uint16(len(v.ballots))
		v.ballots[m.ID], v.Yes = 1, 1
		r.Vote = v
		s.maybeRoomVoteMajorityLocked(r, now)
		return
	}
	v := r.Vote
	if v == nil || c.VoteID != v.ID || !v.active() {
		m.RoomControlError = "投票已结束或已过期"
		return
	}
	switch c.Action {
	case proto.RoomControlYes, proto.RoomControlNo:
		ballot, eligible := v.ballots[m.ID]
		if v.Phase != proto.RoomVoteVoting || !eligible || ballot != 0 {
			m.RoomControlError = "当前无法投票或已经投过票"
			return
		}
		if c.Action == proto.RoomControlYes {
			v.ballots[m.ID] = 1
			v.Yes++
		} else {
			v.ballots[m.ID] = 2
			v.No++
		}
		s.maybeRoomVoteMajorityLocked(r, now)
	case proto.RoomControlVeto:
		if v.Phase != proto.RoomVoteVeto || m.ID != v.VetoHostID || m.ID != r.HostID {
			m.RoomControlError = "仅本轮原房主可以否决"
			return
		}
		v.finish(proto.RoomVoteVetoed, now)
	default:
		m.RoomControlError = "无效的投票操作"
	}
}

func (s *Server) maybeRoomVoteMajorityLocked(r *Room, now time.Time) {
	v := r.Vote
	if int(v.Yes)*2 > int(v.Eligible) {
		v.Phase, v.VetoHostID = proto.RoomVoteVeto, r.HostID
		v.deadline = now.Add(roomVetoTime)
	}
}

// advanceRoomVoteLocked enforces deadlines before accepting any new command.
func (s *Server) advanceRoomVoteLocked(r *Room, now time.Time) bool {
	v := r.Vote
	if v == nil {
		return false
	}
	changed := false
	if v.active() && !now.Before(v.deadline) {
		changed = true
		if v.Phase == proto.RoomVoteVoting {
			v.finish(proto.RoomVoteExpired, v.deadline)
		} else if r.Members[v.InitiatorID] == nil || r.Members[v.TargetID] == nil {
			v.finish(proto.RoomVoteCancelled, v.deadline)
		} else {
			v.finish(proto.RoomVoteExecuted, v.deadline)
			s.setRoomHostLocked(r, v.InitiatorID)
			if v.Kind == proto.RoomVoteKick {
				target := r.Members[v.TargetID]
				s.send(target.Addr, proto.EncodeReject("房间投票已将你移出"))
				s.removeMemberLocked(target)
			}
		}
	}
	if !v.active() && !now.Before(v.clearAt) {
		r.Vote = nil
		changed = true
	}
	return changed
}

func (s *Server) setRoomHostLocked(r *Room, hostID uint32) {
	r.HostID = hostID
	for _, m := range r.Members {
		host := m.ID == hostID
		if m.IsHost == host {
			continue
		}
		m.IsHost = host
		s.broadcastLocked(r, 0, proto.EncodeMemberUpdate(proto.EventJoin, proto.Member{
			ID: m.ID, IsHost: host, Name: m.Name, Hostname: m.Hostname}))
	}
}

func (s *Server) roomStateLocked(m *Member, now time.Time) proto.RoomState {
	r := m.Room
	s.nextRoomSequence++
	state := proto.RoomState{Sequence: s.nextRoomSequence, AckRequestID: m.RoomControlAckID,
		HostID: r.HostID, GameHostID: r.GameHostID, Members: s.memberListLocked(r), Error: m.RoomControlError}
	sort.Slice(state.Members, func(i, j int) bool { return state.Members[i].ID < state.Members[j].ID })
	if r.Vote != nil {
		v := r.Vote
		state.Vote = v.RoomVote
		state.Vote.Ballot = v.ballots[m.ID]
		_, eligible := v.ballots[m.ID]
		state.Vote.CanVote = v.Phase == proto.RoomVoteVoting && eligible && state.Vote.Ballot == 0
		if v.active() && now.Before(v.deadline) {
			state.Vote.RemainingMS = uint32((v.deadline.Sub(now) + time.Millisecond - 1) / time.Millisecond)
		}
	}
	return state
}

func (s *Server) broadcastRoomStateLocked(r *Room, now time.Time) {
	for _, m := range r.Members {
		s.send(m.Addr, proto.EncodeRoomState(s.roomStateLocked(m, now)))
	}
}
