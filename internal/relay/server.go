// Package relay implements the UDP relay server.
//
// The server is stateless across restarts: rooms and members live in memory.
// It authenticates joiners with a SHA-256 password token, assigns client IDs,
// broadcasts membership updates, and forwards encrypted DATA packets between
// room members without ever decrypting their payloads.
package relay

import (
	"log"
	"net"
	"sync"
	"time"

	"relaynet/internal/crypto"
	"relaynet/internal/proto"
)

// Member is a connected client on the server side.
type Member struct {
	ID               uint32
	Name             string
	Hostname         string
	IsHost           bool
	Addr             *net.UDPAddr
	LastSeen         time.Time
	Room             *Room
	ScreenNegotiated bool
	ScreenSource     uint32
	ScreenRequestID  uint64
	RoomControlAckID uint64
	RoomControlError string
}

// Room groups members that share a password token.
type Room struct {
	Name       string
	AuthToken  []byte
	HostID     uint32
	GameHostID uint32
	Members    map[uint32]*Member
	NextID     uint32
	Vote       *roomVote
}

// Server is the relay server.
type Server struct {
	conn *net.UDPConn

	mu               sync.Mutex
	rooms            map[string]*Room
	byAddr           map[string]*Member // key = addr.String()
	timeout          time.Duration
	nextVoteID       uint64
	nextRoomSequence uint64
}

// New creates a Server bound to addr (e.g. ":19132").
func New(addr string) (*Server, error) {
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}
	conn.SetReadBuffer(4 * 1024 * 1024)
	conn.SetWriteBuffer(4 * 1024 * 1024)
	s := &Server{
		conn:    conn,
		rooms:   make(map[string]*Room),
		byAddr:  make(map[string]*Member),
		timeout: 20 * time.Second,
	}
	return s, nil
}

// Serve runs the read loop until the server is stopped.
func (s *Server) Serve() {
	log.Printf("relay server listening on %s", s.conn.LocalAddr())
	go s.sweeper()

	buf := make([]byte, 65535)
	for {
		n, addr, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("read error: %v", err)
			return
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		s.handle(pkt, addr)
	}
}

func (s *Server) handle(p []byte, addr *net.UDPAddr) {
	if len(p) == 0 {
		return
	}
	switch p[0] {
	case proto.TypeHello:
		s.handleHello(p, addr)
	case proto.TypeData:
		s.handleData(p, addr)
	case proto.TypeTCPConnect:
		s.handleTCPConnect(p, addr)
	case proto.TypeTCPData:
		s.handleTCPData(p, addr)
	case proto.TypeTCPClose:
		s.handleTCPClose(p, addr)
	case proto.TypeChat:
		s.handleChat(p, addr)
	case proto.TypeAudioData:
		s.handleAudioData(p, addr)
	case proto.TypeSystemAudioData:
		s.handleSystemAudioData(p, addr)
	case proto.TypeVideoData:
		s.handleVideoData(p, addr)
	case proto.TypeVideoFragment:
		s.handleVideoFragment(p, addr)
	case proto.TypeMediaControl:
		s.handleMediaControl(p, addr)
	case proto.TypePing:
		s.handlePing(p, addr)
	case proto.TypeLeave:
		s.handleLeave(addr)
	case proto.TypeKick:
		s.handleKick(p, addr)
	case proto.TypeRoomControl:
		s.handleRoomControl(p, addr)
	}
}

func (s *Server) handleHello(p []byte, addr *net.UDPAddr) {
	h, err := proto.DecodeHello(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// If this address already had a member, detach it first (reconnect).
	if old, ok := s.byAddr[addr.String()]; ok {
		s.advanceRoomVoteLocked(old.Room, time.Now())
		s.removeMemberLocked(old)
	}

	room, exists := s.rooms[h.Room]
	if !exists {
		if !h.IsHost {
			s.send(addr, proto.EncodeReject("room does not exist; the host must create it first"))
			return
		}
		room = &Room{
			Name:      h.Room,
			AuthToken: append([]byte(nil), h.AuthToken...),
			Members:   make(map[uint32]*Member),
			NextID:    1,
		}
		s.rooms[h.Room] = room
	} else {
		if !crypto.EqualToken(room.AuthToken, h.AuthToken) {
			s.send(addr, proto.EncodeReject("wrong room password"))
			return
		}
		if h.IsHost && room.HostID != 0 {
			s.send(addr, proto.EncodeReject("room already has a host; join as a member"))
			return
		}
	}

	m := &Member{
		ID:       room.NextID,
		Name:     h.Name,
		Hostname: h.Hostname,
		IsHost:   h.IsHost,
		Addr:     addr,
		LastSeen: time.Now(),
		Room:     room,
	}
	room.NextID++
	room.Members[m.ID] = m
	if h.IsHost {
		room.HostID = m.ID
		room.GameHostID = m.ID
	}
	s.byAddr[addr.String()] = m

	// Build member list snapshot.
	members := s.memberListLocked(room)
	joined := proto.EncodeJoined(m.ID, m.IsHost, room.HostID, members)
	s.send(addr, joined)

	// Broadcast join to everyone else.
	update := proto.EncodeMemberUpdate(proto.EventJoin, proto.Member{ID: m.ID, IsHost: m.IsHost, Name: m.Name, Hostname: m.Hostname})
	s.broadcastLocked(room, m.ID, update)
	s.broadcastRoomStateLocked(room, time.Now())

	log.Printf("join: room=%s member=%s id=%d addr=%s (members=%d)", h.Room, h.Name, m.ID, addr, len(room.Members))
}

func (s *Server) handleData(p []byte, addr *net.UDPAddr) {
	srcID, dstID, err := proto.DataHeader(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	m, ok := s.byAddr[addr.String()]
	if !ok {
		return
	}
	m.LastSeen = time.Now()
	// Anti-spoof: the packet's srcID must match the sender's assigned ID.
	if srcID != m.ID {
		return
	}

	if dstID == proto.BroadcastID {
		s.broadcastLocked(m.Room, m.ID, p)
		return
	}
	if target, ok := m.Room.Members[dstID]; ok {
		s.send(target.Addr, p)
	}
}

func (s *Server) handlePing(p []byte, addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.byAddr[addr.String()]; ok {
		m.LastSeen = time.Now()
	}
	ts, err := proto.DecodePing(p)
	if err != nil {
		s.send(addr, proto.EncodePong(0))
		return
	}
	s.send(addr, proto.EncodePong(ts))
}

func (s *Server) handleTCPConnect(p []byte, addr *net.UDPAddr) {
	srcID, dstID, _, _, err := proto.DecodeTCPConnect(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byAddr[addr.String()]
	if !ok || srcID != m.ID {
		return
	}
	m.LastSeen = time.Now()
	if dstID == proto.BroadcastID {
		s.broadcastLocked(m.Room, m.ID, p)
		return
	}
	if target, ok := m.Room.Members[dstID]; ok {
		s.send(target.Addr, p)
	}
}

func (s *Server) handleTCPData(p []byte, addr *net.UDPAddr) {
	srcID, dstID, _, err := proto.TCPDataHeader(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byAddr[addr.String()]
	if !ok || srcID != m.ID {
		return
	}
	m.LastSeen = time.Now()
	if dstID == proto.BroadcastID {
		s.broadcastLocked(m.Room, m.ID, p)
		return
	}
	if target, ok := m.Room.Members[dstID]; ok {
		s.send(target.Addr, p)
	}
}

func (s *Server) handleTCPClose(p []byte, addr *net.UDPAddr) {
	srcID, dstID, _, err := proto.DecodeTCPClose(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byAddr[addr.String()]
	if !ok || srcID != m.ID {
		return
	}
	m.LastSeen = time.Now()
	if dstID == proto.BroadcastID {
		s.broadcastLocked(m.Room, m.ID, p)
		return
	}
	if target, ok := m.Room.Members[dstID]; ok {
		s.send(target.Addr, p)
	}
}

func (s *Server) handleChat(p []byte, addr *net.UDPAddr) {
	msg, err := proto.DecodeChatClient(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byAddr[addr.String()]
	if !ok {
		return
	}
	m.LastSeen = time.Now()
	bcast := proto.EncodeChatBroadcast(m.ID, m.Name, msg)
	s.broadcastLocked(m.Room, 0, bcast)
}

func (s *Server) handleAudioData(p []byte, addr *net.UDPAddr) {
	srcID, dstID, err := proto.AudioDataHeader(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byAddr[addr.String()]
	if !ok || srcID != m.ID {
		return
	}
	m.LastSeen = time.Now()
	if dstID == proto.BroadcastID {
		s.broadcastLocked(m.Room, m.ID, p)
		return
	}
	if target, ok := m.Room.Members[dstID]; ok {
		s.send(target.Addr, p)
	}
}

func (s *Server) handleSystemAudioData(p []byte, addr *net.UDPAddr) {
	srcID, dstID, err := proto.SystemAudioDataHeader(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byAddr[addr.String()]
	if !ok || srcID != m.ID {
		return
	}
	m.LastSeen = time.Now()
	if dstID == proto.BroadcastID {
		s.broadcastLocked(m.Room, m.ID, p)
		return
	}
	if target, ok := m.Room.Members[dstID]; ok {
		s.send(target.Addr, p)
	}
}

func (s *Server) handleVideoData(p []byte, addr *net.UDPAddr) {
	srcID, dstID, err := proto.VideoDataHeader(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byAddr[addr.String()]
	if !ok || srcID != m.ID {
		return
	}
	m.LastSeen = time.Now()
	if dstID == proto.BroadcastID {
		s.broadcastVideoLocked(m.Room, m.ID, p)
		return
	}
	if target, ok := m.Room.Members[dstID]; ok && (!target.ScreenNegotiated || target.ScreenSource == srcID) {
		s.send(target.Addr, p)
	}
}

// handleVideoFragment forwards fragment packets the same way as video data.
func (s *Server) handleVideoFragment(p []byte, addr *net.UDPAddr) {
	srcID, dstID, _, _, _, _, err := proto.VideoFragmentHeader(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byAddr[addr.String()]
	if !ok || srcID != m.ID {
		return
	}
	m.LastSeen = time.Now()
	if dstID == proto.BroadcastID {
		s.broadcastVideoLocked(m.Room, m.ID, p)
		return
	}
	if target, ok := m.Room.Members[dstID]; ok && (!target.ScreenNegotiated || target.ScreenSource == srcID) {
		s.send(target.Addr, p)
	}
}

func (s *Server) handleMediaControl(p []byte, addr *net.UDPAddr) {
	srcID, dstID, ctrl, data, err := proto.DecodeMediaControl(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byAddr[addr.String()]
	if !ok || srcID != m.ID {
		return
	}
	m.LastSeen = time.Now()
	if ctrl == proto.MediaCtrlScreenSubscribe {
		request, err := proto.DecodeScreenSubscription(data)
		if err != nil || request.RequestID < m.ScreenRequestID {
			return
		}
		if request.Legacy && dstID == proto.BroadcastID {
			m.ScreenSource = 0 // Preserve discovery of older, non-announcing sharers.
		} else if request.Active || request.Legacy {
			if _, ok := m.Room.Members[dstID]; !ok || dstID == srcID {
				return
			}
			m.ScreenSource = dstID
		} else if request.RequestID > m.ScreenRequestID || m.ScreenSource == dstID {
			m.ScreenSource = 0
		}
		m.ScreenNegotiated, m.ScreenRequestID = true, request.RequestID
		if request.Legacy {
			m.ScreenNegotiated = false
		}
	}
	if dstID == proto.BroadcastID {
		s.broadcastLocked(m.Room, m.ID, p)
		return
	}
	if target, ok := m.Room.Members[dstID]; ok {
		s.send(target.Addr, p)
	}
}

func (s *Server) broadcastVideoLocked(room *Room, except uint32, packet []byte) {
	for id, member := range room.Members {
		if id != except && !member.ScreenNegotiated {
			s.send(member.Addr, packet)
		}
	}
}

func (s *Server) handleLeave(addr *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.byAddr[addr.String()]; ok {
		s.advanceRoomVoteLocked(m.Room, time.Now())
		s.removeMemberLocked(m)
	}
}

// handleKick processes a host's request to remove a member from the room.
// Only the room host may kick; the target receives a Reject so its client
// disconnects cleanly.
func (s *Server) handleKick(p []byte, addr *net.UDPAddr) {
	targetID, err := proto.DecodeKick(p)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byAddr[addr.String()]
	if !ok {
		return
	}
	s.advanceRoomVoteLocked(m.Room, time.Now())
	if s.byAddr[addr.String()] != m || !m.IsHost {
		return // only the host can kick
	}
	target, ok := m.Room.Members[targetID]
	if !ok || targetID == m.ID {
		return // target must exist and cannot be self
	}
	s.send(target.Addr, proto.EncodeReject("kicked by host"))
	s.removeMemberLocked(target)
	log.Printf("kick: room=%s host=%s kicked id=%d (%s)", m.Room.Name, m.Name, targetID, target.Name)
}

// sweeper periodically drops inactive members.
func (s *Server) sweeper() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		s.sweep()
	}
}

func (s *Server) sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, room := range s.rooms {
		for id, m := range room.Members {
			if now.Sub(m.LastSeen) > s.timeout {
				log.Printf("timeout: room=%s member=%s id=%d", room.Name, m.Name, id)
				s.removeMemberLocked(m)
			}
		}
		if s.advanceRoomVoteLocked(room, now) {
			s.broadcastRoomStateLocked(room, now)
		}
	}
}

// removeMemberLocked removes m from its room and broadcasts a leave update.
// If m was the host, the oldest remaining member is promoted.
func (s *Server) removeMemberLocked(m *Member) {
	room := m.Room
	if room == nil {
		return
	}
	if room.Members[m.ID] != m {
		return
	}
	now := time.Now()
	if room.Vote != nil && room.Vote.active() && (room.Vote.InitiatorID == m.ID || room.Vote.TargetID == m.ID) {
		room.Vote.finish(proto.RoomVoteCancelled, now)
	}
	delete(room.Members, m.ID)
	delete(s.byAddr, m.Addr.String())

	// Promote a new host if needed.
	if room.HostID == m.ID {
		var next *Member
		for _, mm := range room.Members {
			if next == nil || mm.ID < next.ID {
				next = mm
			}
		}
		if next != nil {
			next.IsHost = true
			room.HostID = next.ID
			upd := proto.EncodeMemberUpdate(proto.EventJoin, proto.Member{ID: next.ID, IsHost: true, Name: next.Name, Hostname: next.Hostname})
			s.broadcastLocked(room, 0, upd)
		} else {
			room.HostID = 0
		}
	}
	if room.GameHostID == m.ID {
		room.GameHostID = 0
		for id := range room.Members {
			if room.GameHostID == 0 || id < room.GameHostID {
				room.GameHostID = id
			}
		}
	}

	// Broadcast leave.
	leave := proto.EncodeMemberUpdate(proto.EventLeave, proto.Member{ID: m.ID, IsHost: m.IsHost, Name: m.Name, Hostname: m.Hostname})
	s.broadcastLocked(room, 0, leave)
	s.broadcastRoomStateLocked(room, now)

	// Remove empty rooms.
	if len(room.Members) == 0 {
		delete(s.rooms, room.Name)
		log.Printf("room empty: %s", room.Name)
	}
}

func (s *Server) memberListLocked(room *Room) []proto.Member {
	out := make([]proto.Member, 0, len(room.Members))
	for _, m := range room.Members {
		out = append(out, proto.Member{ID: m.ID, IsHost: m.IsHost, Name: m.Name, Hostname: m.Hostname})
	}
	return out
}

// broadcastLocked sends p to every room member except skipID (0 = send to all).
func (s *Server) broadcastLocked(room *Room, skipID uint32, p []byte) {
	for _, m := range room.Members {
		if m.ID == skipID {
			continue
		}
		s.send(m.Addr, p)
	}
}

func (s *Server) send(addr *net.UDPAddr, p []byte) {
	_, _ = s.conn.WriteToUDP(p, addr)
}
