package proto

import (
	"encoding/binary"
	"testing"
)

func TestScreenSubscribedEnvelope(t *testing.T) {
	b := make([]byte, 35)
	copy(b, "RAV2")
	b[4], b[5] = ScreenVideo, 1
	binary.BigEndian.PutUint64(b[6:14], 100)
	binary.BigEndian.PutUint32(b[14:18], 7)
	binary.BigEndian.PutUint64(b[18:26], 900)
	binary.BigEndian.PutUint64(b[26:34], 42)
	b[34] = 9
	meta, data, marked, err := DecodeScreenPayload(b)
	if err != nil || !marked || len(data) != 1 || data[0] != 9 || meta.SubscriptionID != 42 {
		t.Fatal("subscribed video envelope is not understood")
	}
	got, _, _, err := DecodeScreenPayload(EncodeScreenPayload(meta, data))
	if err != nil || got != meta {
		t.Fatal("subscription does not round-trip")
	}
	for length := 4; length <= 34; length++ {
		if _, _, _, err := DecodeScreenPayload(b[:length]); err == nil {
			t.Fatalf("truncated RAV2 accepted: %d", length)
		}
	}
}

func TestScreenSubscriptionValidation(t *testing.T) {
	for _, resolution := range []byte{0, 1, 2, 3, 4} {
		for _, fps := range []byte{0, 30, 60, 120} {
			want := ScreenSubscription{RequestID: 99, Active: true, Resolution: resolution, FPS: fps}
			got, err := DecodeScreenSubscription(EncodeScreenSubscription(want))
			if err != nil || got != want {
				t.Fatalf("valid playback option rejected: %+v", want)
			}
		}
	}
	valid := EncodeScreenSubscription(ScreenSubscription{RequestID: 1, Active: true})
	for _, index := range []int{0, 1, 2, 3} {
		invalid := append([]byte(nil), valid...)
		invalid[index] = 255
		if _, err := DecodeScreenSubscription(invalid); err == nil {
			t.Fatalf("invalid subscription field %d accepted", index)
		}
	}
	for _, invalid := range [][]byte{valid[:11], append(valid, 0), EncodeScreenSubscription(ScreenSubscription{})} {
		if _, err := DecodeScreenSubscription(invalid); err == nil {
			t.Fatal("invalid subscription size or ID accepted")
		}
	}
}
