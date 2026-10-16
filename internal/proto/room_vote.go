package proto

import (
	"encoding/binary"
	"errors"
)

const (
	TypeRoomControl byte = 0x15
	TypeRoomState   byte = 0x16
)

const (
	RoomControlSync byte = iota
	RoomControlStart
	RoomControlYes
	RoomControlNo
	RoomControlVeto
)

const (
	RoomVoteKick byte = 1 + iota
	RoomVoteTransfer
)

const (
	RoomVoteNone byte = iota
	RoomVoteVoting
	RoomVoteVeto
	RoomVoteExecuted
	RoomVoteVetoed
	RoomVoteExpired
	RoomVoteCancelled
)

type RoomControl struct {
	Action    byte
	RequestID uint64
	VoteID    uint64
	TargetID  uint32
}

type RoomVote struct {
	ID                                uint64
	Kind, Phase                       byte
	InitiatorID, TargetID, VetoHostID uint32
	InitiatorName, TargetName         string
	Eligible, Yes, No                 uint16
	RemainingMS                       uint32
	CanVote                           bool
	Ballot                            byte // 0=unanswered, 1=yes, 2=no
}

type RoomState struct {
	Sequence, AckRequestID uint64
	HostID, GameHostID     uint32
	Members                []Member
	Vote                   RoomVote
	Error                  string
}

// RoomControl is [type:1][action:1][request:8][vote:8][target:4].
func EncodeRoomControl(c RoomControl) []byte {
	b := []byte{TypeRoomControl, c.Action}
	b = binary.BigEndian.AppendUint64(b, c.RequestID)
	b = binary.BigEndian.AppendUint64(b, c.VoteID)
	return appendU32(b, c.TargetID)
}

func DecodeRoomControl(p []byte) (RoomControl, error) {
	if len(p) != 22 || p[0] != TypeRoomControl || p[1] > RoomControlVeto {
		return RoomControl{}, errors.New("invalid room control")
	}
	return RoomControl{Action: p[1], RequestID: binary.BigEndian.Uint64(p[2:10]),
		VoteID: binary.BigEndian.Uint64(p[10:18]), TargetID: binary.BigEndian.Uint32(p[18:22])}, nil
}

// RoomState uses big-endian fields in struct order. Members use the Joined
// member format; member/vote strings have one-byte lengths, Error a two-byte
// length. Boolean fields are exactly 0 or 1.
func EncodeRoomState(s RoomState) []byte {
	b := []byte{TypeRoomState}
	b = binary.BigEndian.AppendUint64(b, s.Sequence)
	b = binary.BigEndian.AppendUint64(b, s.AckRequestID)
	b = appendU32(b, s.HostID)
	b = appendU32(b, s.GameHostID)
	b = appendU16(b, uint16(len(s.Members)))
	for _, m := range s.Members {
		b = appendU32(b, m.ID)
		b = append(b, roomBool(m.IsHost))
		b = appendRoomString(b, m.Name)
		b = appendRoomString(b, m.Hostname)
	}
	v := s.Vote
	b = binary.BigEndian.AppendUint64(b, v.ID)
	b = append(b, v.Kind, v.Phase)
	b = appendU32(b, v.InitiatorID)
	b = appendU32(b, v.TargetID)
	b = appendU32(b, v.VetoHostID)
	b = appendRoomString(b, v.InitiatorName)
	b = appendRoomString(b, v.TargetName)
	b = appendU16(b, v.Eligible)
	b = appendU16(b, v.Yes)
	b = appendU16(b, v.No)
	b = appendU32(b, v.RemainingMS)
	b = append(b, roomBool(v.CanVote), v.Ballot)
	if len(s.Error) > 65535 {
		s.Error = s.Error[:65535]
	}
	b = appendU16(b, uint16(len(s.Error)))
	return append(b, s.Error...)
}

func DecodeRoomState(p []byte) (RoomState, error) {
	r := roomStateReader{p: p}
	if r.byte() != TypeRoomState {
		return RoomState{}, errors.New("not room state")
	}
	s := RoomState{Sequence: r.u64(), AckRequestID: r.u64(), HostID: r.u32(), GameHostID: r.u32()}
	count := int(r.u16())
	// Each member needs at least seven bytes; bound allocation on corrupt input.
	if count > len(p)/7 {
		return RoomState{}, errors.New("invalid room members")
	}
	s.Members = make([]Member, 0, count)
	for i := 0; i < count; i++ {
		m := Member{ID: r.u32(), IsHost: r.boolean(), Name: r.str(), Hostname: r.str()}
		s.Members = append(s.Members, m)
	}
	v := RoomVote{ID: r.u64(), Kind: r.byte(), Phase: r.byte(), InitiatorID: r.u32(), TargetID: r.u32(), VetoHostID: r.u32(),
		InitiatorName: r.str(), TargetName: r.str(), Eligible: r.u16(), Yes: r.u16(), No: r.u16(), RemainingMS: r.u32(),
		CanVote: r.boolean(), Ballot: r.byte()}
	s.Vote = v
	s.Error = string(r.take(int(r.u16())))
	if r.bad || r.off != len(p) || v.Kind > RoomVoteTransfer || v.Phase > RoomVoteCancelled || v.Ballot > 2 {
		return RoomState{}, errors.New("invalid room state")
	}
	return s, nil
}

func roomBool(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func appendRoomString(b []byte, s string) []byte {
	if len(s) > 255 {
		s = s[:255]
	}
	b = append(b, byte(len(s)))
	return append(b, s...)
}

type roomStateReader struct {
	p   []byte
	off int
	bad bool
}

func (r *roomStateReader) take(n int) []byte {
	if r.bad || n > len(r.p)-r.off {
		r.bad = true
		return make([]byte, n)
	}
	b := r.p[r.off : r.off+n]
	r.off += n
	return b
}
func (r *roomStateReader) byte() byte { return r.take(1)[0] }
func (r *roomStateReader) boolean() bool {
	b := r.byte()
	if b > 1 {
		r.bad = true
	}
	return b == 1
}
func (r *roomStateReader) u16() uint16 { return binary.BigEndian.Uint16(r.take(2)) }
func (r *roomStateReader) u32() uint32 { return binary.BigEndian.Uint32(r.take(4)) }
func (r *roomStateReader) u64() uint64 { return binary.BigEndian.Uint64(r.take(8)) }
func (r *roomStateReader) str() string { return string(r.take(int(r.byte()))) }
