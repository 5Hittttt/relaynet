package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
)

const (
	ScreenVideo      byte = 1
	ScreenAudio      byte = 2
	screenHeaderSize      = 26
)

var screenMagic = []byte("RAV1")

// ScreenPayload is encrypted together with media data. Video subscriptions
// use separate routing controls; the relay cannot read this A/V metadata.
type ScreenPayload struct {
	Kind           byte
	HasAudio       bool
	StreamID       uint64
	Sequence       uint32
	TimestampNS    int64
	SubscriptionID uint64 // RAV2 video: rejects delayed pictures after a playback change
}

func EncodeScreenPayload(meta ScreenPayload, data []byte) []byte {
	headerSize := screenHeaderSize
	if meta.SubscriptionID != 0 {
		headerSize += 8
	}
	b := make([]byte, headerSize, headerSize+len(data))
	copy(b, screenMagic)
	if meta.SubscriptionID != 0 {
		b[3] = '2'
		binary.BigEndian.PutUint64(b[26:34], meta.SubscriptionID)
	}
	b[4] = meta.Kind
	if meta.HasAudio {
		b[5] = 1
	}
	binary.BigEndian.PutUint64(b[6:14], meta.StreamID)
	binary.BigEndian.PutUint32(b[14:18], meta.Sequence)
	binary.BigEndian.PutUint64(b[18:26], uint64(meta.TimestampNS))
	return append(b, data...)
}

// marked distinguishes an old client's media from a malformed new packet.
func DecodeScreenPayload(b []byte) (meta ScreenPayload, data []byte, marked bool, err error) {
	if len(b) < 4 || (!bytes.Equal(b[:4], screenMagic) && !bytes.Equal(b[:4], []byte("RAV2"))) {
		return meta, b, false, nil
	}
	marked = true
	headerSize := screenHeaderSize
	if b[3] == '2' {
		headerSize += 8
	}
	if len(b) <= headerSize || (b[4] != ScreenVideo && b[4] != ScreenAudio) || b[5] > 1 {
		return meta, nil, true, errors.New("invalid screen media header")
	}
	meta = ScreenPayload{Kind: b[4], HasAudio: b[5] == 1, StreamID: binary.BigEndian.Uint64(b[6:14]),
		Sequence: binary.BigEndian.Uint32(b[14:18]), TimestampNS: int64(binary.BigEndian.Uint64(b[18:26]))}
	if b[3] == '2' {
		meta.SubscriptionID = binary.BigEndian.Uint64(b[26:34])
		if meta.Kind != ScreenVideo || meta.SubscriptionID == 0 {
			return meta, nil, true, errors.New("invalid subscribed video")
		}
	}
	if meta.StreamID == 0 || meta.Sequence == 0 || meta.TimestampNS < 0 {
		return meta, nil, true, errors.New("invalid screen media timestamp")
	}
	return meta, b[headerSize:], true, nil
}
