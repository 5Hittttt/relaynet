package proto

import (
	"bytes"
	"testing"
)

func TestScreenEnvelopePreservesTimeline(t *testing.T) {
	meta := ScreenPayload{Kind: ScreenVideo, HasAudio: true, StreamID: 123, Sequence: 17, TimestampNS: 900123400}
	data := []byte{0, 0, 0, 1, 0x65, 1, 2, 3}
	got, payload, marked, err := DecodeScreenPayload(EncodeScreenPayload(meta, data))
	if err != nil || !marked || got != meta || !bytes.Equal(data, payload) {
		t.Fatal("screen timestamps failed round trip")
	}
	if _, payload, marked, err := DecodeScreenPayload(data); err != nil || marked || !bytes.Equal(data, payload) {
		t.Fatal("old video payload was not preserved")
	}
	bad := EncodeScreenPayload(meta, data)[:20]
	if _, _, marked, err := DecodeScreenPayload(bad); !marked || err == nil {
		t.Fatal("truncated synchronized packet accepted")
	}
}
