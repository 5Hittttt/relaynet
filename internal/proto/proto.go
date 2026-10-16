// Package proto defines the relay wire protocol: message types and encode/decode helpers.
//
// All messages share a leading 1-byte Type. Numeric fields are big-endian.
//
//	Hello        (C->S): [01][roomLen:1][room][nameLen:1][name][hostnameLen:1][hostname][authToken:32][isHost:1]
//	Challenge    (S->C): [02][nonce:16]                                   (reserved, unused in token mode)
//	Joined       (S->C): [04][clientID:4][isHost:1][hostID:4][memberCount:2][member...]
//	                  member = [id:4][isHost:1][nameLen:1][name][hostnameLen:1][hostname]
//	Reject       (S->C): [05][reasonLen:1][reason]
//	Data         (C<->S):[06][srcID:4][dstID:4][nonce:12][ciphertext]
//	                     plaintext = [port:2][payload]
//	                     port 0 is reserved for peer ping (payload = 8-byte big-endian timestamp ns)
//	Ping         (C->S): [07][timestamp:8]   (big-endian nanoseconds, echoed back in pong)
//	Pong         (S->C): [08][timestamp:8]   (mirrors the ping timestamp so client can compute RTT)
//	MemberUpdate (S->C): [09][event:1][id:4][isHost:1][nameLen:1][name][hostnameLen:1][hostname]
//	                     event: 0=join, 1=leave
//	Leave        (C->S): [0A]
//	Kick         (C->S): [12][targetID:4]   (host-only: server removes the target member)
package proto

import (
	"encoding/binary"
	"errors"

	"relaynet/internal/crypto"
)

const (
	TypeHello           byte = 0x01
	TypeChallenge       byte = 0x02
	TypeJoined          byte = 0x04
	TypeReject          byte = 0x05
	TypeData            byte = 0x06
	TypePing            byte = 0x07
	TypePong            byte = 0x08
	TypeMemberUpdate    byte = 0x09
	TypeLeave           byte = 0x0A
	TypeTCPConnect      byte = 0x0B
	TypeTCPData         byte = 0x0C
	TypeTCPClose        byte = 0x0D
	TypeChat            byte = 0x0E
	TypeAudioData       byte = 0x0F
	TypeVideoData       byte = 0x10
	TypeMediaControl    byte = 0x11
	TypeKick            byte = 0x12
	TypeVideoFragment   byte = 0x13
	TypeSystemAudioData byte = 0x14

	// Video codec: 0 for legacy JPEG, 1/2 for H.264.
	VideoFrameJPEG    byte = 0 // JPEG frame (legacy / fallback path)
	VideoFrameH264IDR byte = 1 // H.264 IDR keyframe
	VideoFrameH264P   byte = 2 // H.264 P-frame
)

const (
	EventJoin  byte = 0
	EventLeave byte = 1
)

// BroadcastID means "all room members except sender".
const BroadcastID uint32 = 0xFFFFFFFF

// PeerPingPort is the reserved port number for peer latency measurements.
// Payload = 8-byte big-endian UnixNano timestamp. The receiver echoes it back verbatim.
const PeerPingPort uint16 = 0

const authTokenSize = 32

// Hello is the room join request.
type Hello struct {
	Room      string
	Name      string
	Hostname  string
	AuthToken []byte // len 32
	IsHost    bool
}

// EncodeHello builds a Hello packet.
func EncodeHello(h Hello) ([]byte, error) {
	if len(h.AuthToken) != authTokenSize {
		return nil, errors.New("auth token must be 32 bytes")
	}
	if len(h.Room) > 255 || len(h.Name) > 255 || len(h.Hostname) > 255 {
		return nil, errors.New("room, name or hostname too long")
	}
	b := make([]byte, 0, 1+1+len(h.Room)+1+len(h.Name)+1+len(h.Hostname)+authTokenSize+1)
	b = append(b, TypeHello)
	b = append(b, byte(len(h.Room)))
	b = append(b, h.Room...)
	b = append(b, byte(len(h.Name)))
	b = append(b, h.Name...)
	b = append(b, byte(len(h.Hostname)))
	b = append(b, h.Hostname...)
	b = append(b, h.AuthToken...)
	if h.IsHost {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	return b, nil
}

// DecodeHello parses a Hello packet (including the leading type byte).
func DecodeHello(p []byte) (Hello, error) {
	if len(p) < 1 || p[0] != TypeHello {
		return Hello{}, errors.New("not hello")
	}
	off := 1
	if off+1 > len(p) {
		return Hello{}, errors.New("short hello")
	}
	rl := int(p[off])
	off++
	if off+rl > len(p) {
		return Hello{}, errors.New("short hello room")
	}
	room := string(p[off : off+rl])
	off += rl
	if off+1 > len(p) {
		return Hello{}, errors.New("short hello name")
	}
	nl := int(p[off])
	off++
	if off+nl > len(p) {
		return Hello{}, errors.New("short hello name")
	}
	name := string(p[off : off+nl])
	off += nl
	if off+1 > len(p) {
		return Hello{}, errors.New("short hello hostname")
	}
	hl := int(p[off])
	off++
	if off+hl > len(p) {
		return Hello{}, errors.New("short hello hostname")
	}
	hostname := string(p[off : off+hl])
	off += hl
	if off+authTokenSize+1 > len(p) {
		return Hello{}, errors.New("short hello token")
	}
	token := make([]byte, authTokenSize)
	copy(token, p[off:off+authTokenSize])
	off += authTokenSize
	isHost := p[off] != 0
	return Hello{Room: room, Name: name, Hostname: hostname, AuthToken: token, IsHost: isHost}, nil
}

// Member is a room member entry.
type Member struct {
	ID       uint32
	IsHost   bool
	Name     string
	Hostname string
}

// EncodeJoined builds a Joined packet.
func EncodeJoined(clientID uint32, isHost bool, hostID uint32, members []Member) []byte {
	b := []byte{TypeJoined}
	b = appendU32(b, clientID)
	if isHost {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	b = appendU32(b, hostID)
	b = appendU16(b, uint16(len(members)))
	for _, m := range members {
		b = appendU32(b, m.ID)
		if m.IsHost {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
		b = append(b, byte(len(m.Name)))
		b = append(b, m.Name...)
		b = append(b, byte(len(m.Hostname)))
		b = append(b, m.Hostname...)
	}
	return b
}

// DecodeJoined parses a Joined packet (including the leading type byte).
func DecodeJoined(p []byte) (clientID uint32, isHost bool, hostID uint32, members []Member, err error) {
	if len(p) < 1 || p[0] != TypeJoined {
		err = errors.New("not joined")
		return
	}
	off := 1
	if off+4+1+4+2 > len(p) {
		err = errors.New("short joined")
		return
	}
	clientID = binary.BigEndian.Uint32(p[off:])
	off += 4
	isHost = p[off] != 0
	off++
	hostID = binary.BigEndian.Uint32(p[off:])
	off += 4
	mc := int(binary.BigEndian.Uint16(p[off:]))
	off += 2
	members = make([]Member, 0, mc)
	for i := 0; i < mc; i++ {
		if off+4+1+1 > len(p) {
			err = errors.New("short joined member")
			return
		}
		m := Member{}
		m.ID = binary.BigEndian.Uint32(p[off:])
		off += 4
		m.IsHost = p[off] != 0
		off++
		nl := int(p[off])
		off++
		if off+nl > len(p) {
			err = errors.New("short joined member name")
			return
		}
		m.Name = string(p[off : off+nl])
		off += nl
		if off+1 > len(p) {
			err = errors.New("short joined member hostname")
			return
		}
		hl := int(p[off])
		off++
		if off+hl > len(p) {
			err = errors.New("short joined member hostname")
			return
		}
		m.Hostname = string(p[off : off+hl])
		off += hl
		members = append(members, m)
	}
	return
}

// EncodeReject builds a Reject packet.
func EncodeReject(reason string) []byte {
	if len(reason) > 255 {
		reason = reason[:255]
	}
	b := []byte{TypeReject}
	b = append(b, byte(len(reason)))
	b = append(b, reason...)
	return b
}

// DecodeReject parses a Reject packet.
func DecodeReject(p []byte) (string, error) {
	if len(p) < 2 || p[0] != TypeReject {
		return "", errors.New("not reject")
	}
	rl := int(p[1])
	if 2+rl > len(p) {
		return "", errors.New("short reject")
	}
	return string(p[2 : 2+rl]), nil
}

// EncodeMemberUpdate builds a MemberUpdate packet.
func EncodeMemberUpdate(event byte, m Member) []byte {
	b := []byte{TypeMemberUpdate}
	b = append(b, event)
	b = appendU32(b, m.ID)
	if m.IsHost {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	b = append(b, byte(len(m.Name)))
	b = append(b, m.Name...)
	b = append(b, byte(len(m.Hostname)))
	b = append(b, m.Hostname...)
	return b
}

// DecodeMemberUpdate parses a MemberUpdate packet.
func DecodeMemberUpdate(p []byte) (event byte, m Member, err error) {
	if len(p) < 1+1+4+1+1+1 || p[0] != TypeMemberUpdate {
		err = errors.New("short member update")
		return
	}
	off := 1
	event = p[off]
	off++
	m.ID = binary.BigEndian.Uint32(p[off:])
	off += 4
	m.IsHost = p[off] != 0
	off++
	nl := int(p[off])
	off++
	if off+nl > len(p) {
		err = errors.New("short member update name")
		return
	}
	m.Name = string(p[off : off+nl])
	off += nl
	if off+1 > len(p) {
		err = errors.New("short member update hostname")
		return
	}
	hl := int(p[off])
	off++
	if off+hl > len(p) {
		err = errors.New("short member update hostname")
		return
	}
	m.Hostname = string(p[off : off+hl])
	return
}

// EncodeData builds a DATA packet. The routing header (src/dst) is plaintext so the
// relay can forward without decrypting; the port + payload are encrypted.
func EncodeData(srcID, dstID uint32, key []byte, port uint16, payload []byte) ([]byte, error) {
	plain := make([]byte, 2+len(payload))
	binary.BigEndian.PutUint16(plain, port)
	copy(plain[2:], payload)
	blob, err := crypto.Seal(key, plain)
	if err != nil {
		return nil, err
	}
	b := []byte{TypeData}
	b = appendU32(b, srcID)
	b = appendU32(b, dstID)
	b = append(b, blob...)
	return b, nil
}

// DataHeader extracts src/dst from a DATA packet without decrypting (server-side routing).
func DataHeader(p []byte) (srcID, dstID uint32, err error) {
	if len(p) < 9 || p[0] != TypeData {
		err = errors.New("not data")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	return
}

// DecodeData parses and decrypts a DATA packet, returning port + payload.
func DecodeData(p, key []byte) (srcID, dstID uint32, port uint16, payload []byte, err error) {
	if len(p) < 9 || p[0] != TypeData {
		err = errors.New("not data")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	plain, err := crypto.Open(key, p[9:])
	if err != nil {
		return
	}
	if len(plain) < 2 {
		err = errors.New("short data plaintext")
		return
	}
	port = binary.BigEndian.Uint16(plain[:2])
	payload = plain[2:]
	return
}

// EncodePing builds a Ping packet carrying a client timestamp in nanoseconds.
// The server echoes back the same 8 bytes so the client can compute RTT.
func EncodePing(timestampNs int64) []byte {
	b := make([]byte, 9)
	b[0] = TypePing
	binary.BigEndian.PutUint64(b[1:], uint64(timestampNs))
	return b
}

// DecodePing extracts the timestamp from a Ping packet.
func DecodePing(p []byte) (int64, error) {
	if len(p) != 9 || p[0] != TypePing {
		return 0, errors.New("bad ping")
	}
	return int64(binary.BigEndian.Uint64(p[1:])), nil
}

// EncodePong builds a Pong packet echoing the ping timestamp.
func EncodePong(timestampNs int64) []byte {
	b := make([]byte, 9)
	b[0] = TypePong
	binary.BigEndian.PutUint64(b[1:], uint64(timestampNs))
	return b
}

// DecodePong extracts the timestamp from a Pong packet.
func DecodePong(p []byte) (int64, error) {
	if len(p) != 9 || p[0] != TypePong {
		return 0, errors.New("bad pong")
	}
	return int64(binary.BigEndian.Uint64(p[1:])), nil
}

// EncodeLeave builds a Leave packet.
func EncodeLeave() []byte { return []byte{TypeLeave} }

// EncodeTCPConnect builds a TCP connection request.
// connID identifies this connection uniquely; the host echoes it back.
func EncodeTCPConnect(srcID, dstID, connID uint32, port uint16) []byte {
	b := []byte{TypeTCPConnect}
	b = appendU32(b, srcID)
	b = appendU32(b, dstID)
	b = appendU32(b, connID)
	b = appendU16(b, port)
	return b
}

// DecodeTCPConnect parses a TCPConnect packet.
func DecodeTCPConnect(p []byte) (srcID, dstID, connID uint32, port uint16, err error) {
	if len(p) < 1+4+4+4+2 || p[0] != TypeTCPConnect {
		err = errors.New("short tcp connect")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	connID = binary.BigEndian.Uint32(p[9:13])
	port = binary.BigEndian.Uint16(p[13:15])
	return
}

// EncodeTCPData builds a TCP data packet.
func EncodeTCPData(srcID, dstID, connID uint32, key []byte, payload []byte) ([]byte, error) {
	blob, err := crypto.Seal(key, payload)
	if err != nil {
		return nil, err
	}
	b := []byte{TypeTCPData}
	b = appendU32(b, srcID)
	b = appendU32(b, dstID)
	b = appendU32(b, connID)
	b = append(b, blob...)
	return b, nil
}

// TCPDataHeader extracts routing info from a TCPData packet without decrypting.
func TCPDataHeader(p []byte) (srcID, dstID, connID uint32, err error) {
	if len(p) < 1+4+4+4 || p[0] != TypeTCPData {
		err = errors.New("not tcp data")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	connID = binary.BigEndian.Uint32(p[9:13])
	return
}

// DecodeTCPData parses and decrypts a TCPData packet.
func DecodeTCPData(p, key []byte) (srcID, dstID, connID uint32, payload []byte, err error) {
	if len(p) < 1+4+4+4 || p[0] != TypeTCPData {
		err = errors.New("not tcp data")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	connID = binary.BigEndian.Uint32(p[9:13])
	payload, err = crypto.Open(key, p[13:])
	return
}

// EncodeTCPClose builds a TCP close notification.
func EncodeTCPClose(srcID, dstID, connID uint32) []byte {
	b := []byte{TypeTCPClose}
	b = appendU32(b, srcID)
	b = appendU32(b, dstID)
	b = appendU32(b, connID)
	return b
}

// DecodeTCPClose parses a TCPClose packet.
func DecodeTCPClose(p []byte) (srcID, dstID, connID uint32, err error) {
	if len(p) < 1+4+4+4 || p[0] != TypeTCPClose {
		err = errors.New("short tcp close")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	connID = binary.BigEndian.Uint32(p[9:13])
	return
}

// IsPing reports whether p is a Ping.
func IsPing(p []byte) bool { return len(p) >= 9 && p[0] == TypePing }

// IsLeave reports whether p is a Leave.
func IsLeave(p []byte) bool { return len(p) > 0 && p[0] == TypeLeave }

// EncodeChat builds a chat message from a client to the server.
// The server fills in the sender's ID and name before broadcasting.
func EncodeChat(msg string) []byte {
	b := []byte{TypeChat}
	b = appendU16(b, uint16(len(msg)))
	b = append(b, msg...)
	return b
}

// DecodeChatClient parses a chat message sent by a client (no sender info).
func DecodeChatClient(p []byte) (msg string, err error) {
	if len(p) < 3 || p[0] != TypeChat {
		err = errors.New("bad chat")
		return
	}
	ml := int(binary.BigEndian.Uint16(p[1:3]))
	if 3+ml > len(p) {
		err = errors.New("short chat msg")
		return
	}
	msg = string(p[3 : 3+ml])
	return
}

// EncodeChatBroadcast builds a chat message for broadcasting to room members.
// The server fills in srcID and senderName before forwarding.
func EncodeChatBroadcast(srcID uint32, senderName, msg string) []byte {
	b := []byte{TypeChat}
	b = appendU32(b, srcID)
	b = append(b, byte(len(senderName)))
	b = append(b, senderName...)
	b = appendU16(b, uint16(len(msg)))
	b = append(b, msg...)
	return b
}

// DecodeChatBroadcast parses a broadcasted chat message (with sender info).
func DecodeChatBroadcast(p []byte) (srcID uint32, senderName, msg string, err error) {
	if len(p) < 1+4+1+2 || p[0] != TypeChat {
		err = errors.New("bad chat broadcast")
		return
	}
	off := 1
	srcID = binary.BigEndian.Uint32(p[off:])
	off += 4
	nl := int(p[off])
	off++
	if off+nl+2 > len(p) {
		err = errors.New("short chat broadcast")
		return
	}
	senderName = string(p[off : off+nl])
	off += nl
	ml := int(binary.BigEndian.Uint16(p[off:]))
	off += 2
	if off+ml > len(p) {
		err = errors.New("short chat broadcast msg")
		return
	}
	msg = string(p[off : off+ml])
	return
}

func appendU16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

func appendU32(b []byte, v uint32) []byte {
	return append(b, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// EncodeAudioData builds an audio data packet.
func EncodeAudioData(srcID, dstID uint32, key []byte, payload []byte) ([]byte, error) {
	blob, err := crypto.Seal(key, payload)
	if err != nil {
		return nil, err
	}
	b := []byte{TypeAudioData}
	b = appendU32(b, srcID)
	b = appendU32(b, dstID)
	b = append(b, blob...)
	return b, nil
}

// DecodeAudioData parses and decrypts an audio data packet.
func DecodeAudioData(p, key []byte) (srcID, dstID uint32, payload []byte, err error) {
	if len(p) < 9 || p[0] != TypeAudioData {
		err = errors.New("not audio data")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	payload, err = crypto.Open(key, p[9:])
	return
}

// AudioDataHeader extracts routing info without decrypting.
func AudioDataHeader(p []byte) (srcID, dstID uint32, err error) {
	if len(p) < 9 || p[0] != TypeAudioData {
		err = errors.New("not audio data")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	return
}

// EncodeSystemAudioData builds a system playback audio packet.
func EncodeSystemAudioData(srcID, dstID uint32, key []byte, payload []byte) ([]byte, error) {
	blob, err := crypto.Seal(key, payload)
	if err != nil {
		return nil, err
	}
	b := []byte{TypeSystemAudioData}
	b = appendU32(b, srcID)
	b = appendU32(b, dstID)
	b = append(b, blob...)
	return b, nil
}

// DecodeSystemAudioData parses and decrypts a system playback audio packet.
func DecodeSystemAudioData(p, key []byte) (srcID, dstID uint32, payload []byte, err error) {
	if len(p) < 9 || p[0] != TypeSystemAudioData {
		err = errors.New("not system audio data")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	payload, err = crypto.Open(key, p[9:])
	return
}

// SystemAudioDataHeader extracts routing info without decrypting.
func SystemAudioDataHeader(p []byte) (srcID, dstID uint32, err error) {
	if len(p) < 9 || p[0] != TypeSystemAudioData {
		err = errors.New("not system audio data")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	return
}

// EncodeVideoData builds a video data packet.
func EncodeVideoData(srcID, dstID uint32, key []byte, frameType byte, payload []byte) ([]byte, error) {
	blob, err := crypto.Seal(key, payload)
	if err != nil {
		return nil, err
	}
	b := []byte{TypeVideoData}
	b = appendU32(b, srcID)
	b = appendU32(b, dstID)
	b = append(b, frameType)
	b = append(b, blob...)
	return b, nil
}

// DecodeVideoData parses and decrypts a video data packet.
func DecodeVideoData(p, key []byte) (srcID, dstID uint32, frameType byte, payload []byte, err error) {
	if len(p) < 10 || p[0] != TypeVideoData {
		err = errors.New("not video data")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	frameType = p[9]
	payload, err = crypto.Open(key, p[10:])
	return
}

// VideoDataHeader extracts routing info without decrypting.
func VideoDataHeader(p []byte) (srcID, dstID uint32, err error) {
	if len(p) < 10 || p[0] != TypeVideoData {
		err = errors.New("not video data")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	return
}

// MaxFragmentPayload is the maximum encrypted-chunk size per fragment packet.
// 1300 keeps the total UDP packet (17-byte header + payload) well under the
// 1500-byte internet MTU, avoiding IP fragmentation which is reliably dropped
// by most internet routers and firewalls.
const MaxFragmentPayload = 1300

// EncodeVideoFragments splits an encrypted frame blob into UDP-safe fragments.
// frameID identifies the frame so the receiver can reassemble.
// frameType is one of the VideoFrame* constants (carried so the receiver can
// route H.264 frames to the decoder and JPEG frames to the JPEG path).
func EncodeVideoFragments(srcID, dstID, frameID uint32, frameType byte, blob []byte) [][]byte {
	total := (len(blob) + MaxFragmentPayload - 1) / MaxFragmentPayload
	if total == 0 {
		total = 1
	}
	pkts := make([][]byte, total)
	for i := 0; i < total; i++ {
		start := i * MaxFragmentPayload
		end := start + MaxFragmentPayload
		if end > len(blob) {
			end = len(blob)
		}
		b := []byte{TypeVideoFragment}
		b = appendU32(b, srcID)
		b = appendU32(b, dstID)
		b = appendU32(b, frameID)
		b = appendU16(b, uint16(i))
		b = appendU16(b, uint16(total))
		b = append(b, frameType)
		b = append(b, blob[start:end]...)
		pkts[i] = b
	}
	return pkts
}

// VideoFragmentHeader extracts routing + fragment metadata without decrypting.
func VideoFragmentHeader(p []byte) (srcID, dstID, frameID uint32, frameType byte, fragIndex, fragTotal uint16, err error) {
	if len(p) < 18 || p[0] != TypeVideoFragment {
		err = errors.New("not video fragment")
		return
	}
	srcID = binary.BigEndian.Uint32(p[1:5])
	dstID = binary.BigEndian.Uint32(p[5:9])
	frameID = binary.BigEndian.Uint32(p[9:13])
	fragIndex = binary.BigEndian.Uint16(p[13:15])
	fragTotal = binary.BigEndian.Uint16(p[15:17])
	frameType = p[17]
	return
}

// VideoFragmentChunk returns the encrypted chunk from a fragment packet.
func VideoFragmentChunk(p []byte) []byte {
	if len(p) < 18 || p[0] != TypeVideoFragment {
		return nil
	}
	return p[18:]
}

// Media control types
const (
	MediaCtrlStartAudio      byte = 0x01
	MediaCtrlStopAudio       byte = 0x02
	MediaCtrlStartVideo      byte = 0x03
	MediaCtrlStopVideo       byte = 0x04
	MediaCtrlSetAudioVolume  byte = 0x05
	MediaCtrlSetVideoQuality byte = 0x06
)

// EncodeMediaControl builds a media control packet.
func EncodeMediaControl(srcID, dstID uint32, ctrlType byte, data []byte) []byte {
	b := []byte{TypeMediaControl}
	b = appendU32(b, srcID)
	b = appendU32(b, dstID)
	b = append(b, ctrlType)
	b = append(b, byte(len(data)))
	b = append(b, data...)
	return b
}

// DecodeMediaControl parses a media control packet.
func DecodeMediaControl(p []byte) (srcID, dstID uint32, ctrlType byte, data []byte, err error) {
	if len(p) < 1+4+4+1+1 || p[0] != TypeMediaControl {
		err = errors.New("not media control")
		return
	}
	off := 1
	srcID = binary.BigEndian.Uint32(p[off:])
	off += 4
	dstID = binary.BigEndian.Uint32(p[off:])
	off += 4
	ctrlType = p[off]
	off++
	dl := int(p[off])
	off++
	if off+dl > len(p) {
		err = errors.New("short media control data")
		return
	}
	data = p[off : off+dl]
	return
}

// EncodeKick builds a Kick packet. Only the room host may send this; the server
// validates the sender's identity before removing the target member.
//
//	Kick (C->S): [12][targetID:4]
func EncodeKick(targetID uint32) []byte {
	b := []byte{TypeKick}
	b = appendU32(b, targetID)
	return b
}

// DecodeKick parses a Kick packet, returning the target member ID.
func DecodeKick(p []byte) (targetID uint32, err error) {
	if len(p) < 1+4 || p[0] != TypeKick {
		err = errors.New("not kick")
		return
	}
	targetID = binary.BigEndian.Uint32(p[1:5])
	return
}
