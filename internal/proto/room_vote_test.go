package proto

import (
	"reflect"
	"testing"
)

func TestRoomControlRoundTripAndValidation(t *testing.T) {
	for action := RoomControlSync; action <= RoomControlVeto; action++ {
		want := RoomControl{Action: action, RequestID: 0x1020304050607080, VoteID: 13, TargetID: 19}
		packet := EncodeRoomControl(want)
		got, err := DecodeRoomControl(packet)
		if err != nil || got != want {
			t.Fatalf("round trip: got %+v, %v", got, err)
		}
		for i := 0; i < len(packet); i++ {
			if _, err := DecodeRoomControl(packet[:i]); err == nil {
				t.Fatalf("accepted truncated control %d", i)
			}
		}
		packet[1] = 255
		if _, err := DecodeRoomControl(packet); err == nil {
			t.Fatal("accepted unknown action")
		}
	}
}

func TestRoomStateRoundTripAndValidation(t *testing.T) {
	want := RoomState{Sequence: 123, AckRequestID: 88, HostID: 7, GameHostID: 3,
		Members: []Member{{ID: 3, Name: "原房主", Hostname: "pc"}, {ID: 7, IsHost: true, Name: "new"}},
		Vote: RoomVote{ID: 55, Kind: RoomVoteKick, Phase: RoomVoteVeto, InitiatorID: 7, TargetID: 9, VetoHostID: 3,
			InitiatorName: "new", TargetName: "target", Eligible: 4, Yes: 3, No: 1, RemainingMS: 29999, CanVote: true, Ballot: 2}, Error: "already voted"}
	packet := EncodeRoomState(want)
	got, err := DecodeRoomState(packet)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip: got %+v, %v", got, err)
	}
	for i := 0; i < len(packet); i++ {
		if _, err := DecodeRoomState(packet[:i]); err == nil {
			t.Fatalf("accepted truncated state %d", i)
		}
	}
	if _, err := DecodeRoomState(append(packet, 0)); err == nil {
		t.Fatal("accepted trailing bytes")
	}
	packet[0] = TypeRoomControl
	if _, err := DecodeRoomState(packet); err == nil {
		t.Fatal("accepted wrong type")
	}
}
