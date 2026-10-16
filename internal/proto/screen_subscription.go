package proto

import (
	"encoding/binary"
	"errors"
)

const MediaCtrlScreenSubscribe byte = 7

// Resolution: 0 follows the source, 1..4 select 720/1080/1440/2160p.
// FPS: 0 follows the source, otherwise 30, 60 or 120.
type ScreenSubscription struct {
	RequestID  uint64
	Active     bool
	Legacy     bool
	Resolution byte
	FPS        byte
}

func EncodeScreenSubscription(s ScreenSubscription) []byte {
	b := make([]byte, 12)
	b[0], b[2], b[3] = 1, s.Resolution, s.FPS
	if s.Active {
		b[1] = 1
	}
	if s.Legacy {
		b[1] = 2
	}
	binary.BigEndian.PutUint64(b[4:12], s.RequestID)
	return b
}

func DecodeScreenSubscription(b []byte) (ScreenSubscription, error) {
	if len(b) != 12 || b[0] != 1 || b[1] > 2 || b[2] > 4 || (b[3] != 0 && b[3] != 30 && b[3] != 60 && b[3] != 120) {
		return ScreenSubscription{}, errors.New("invalid screen subscription")
	}
	s := ScreenSubscription{RequestID: binary.BigEndian.Uint64(b[4:12]), Active: b[1] == 1, Legacy: b[1] == 2, Resolution: b[2], FPS: b[3]}
	if s.RequestID == 0 {
		return s, errors.New("missing screen subscription ID")
	}
	return s, nil
}
