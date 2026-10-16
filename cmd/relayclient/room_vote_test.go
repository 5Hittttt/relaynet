package main

import (
	"strings"
	"testing"

	"fyne.io/fyne/v2/test"
	"relaynet/internal/proto"
	"relaynet/internal/tunnel"
)

func TestRoomVoteBannerAndVetoDialog(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	u := newUI(a.NewWindow("room vote"))
	u.window.SetContent(u.build())
	c := tunnel.New(tunnel.Config{}, nil)
	st := tunnel.Status{Connected: true, RoomVotingSupported: true, ClientID: 1, IsHost: true}
	st.RoomVote = proto.RoomVote{ID: 7, Kind: proto.RoomVoteTransfer, Phase: proto.RoomVoteVoting,
		InitiatorID: 2, TargetID: 1, InitiatorName: "Alice", TargetName: "Host",
		Eligible: 3, Yes: 1, RemainingMS: 60000}
	u.refreshRoomVote(c, st)
	if !u.voteBar.Visible() || u.voteYes.Visible() || u.vetoDialog != nil {
		t.Fatal("excluded target received a ballot or a premature veto dialog")
	}
	st.RoomVote.Phase, st.RoomVote.VetoHostID, st.RoomVote.Yes, st.RoomVote.RemainingMS = proto.RoomVoteVeto, 1, 2, 30000
	u.refreshRoomVote(c, st)
	first := u.vetoDialog
	if first == nil || !u.voteVeto.Visible() || !strings.Contains(u.voteLabel.Text, "30") {
		t.Fatal("majority did not show the host veto and countdown")
	}
	st.RoomVote.RemainingMS = 29000
	u.refreshRoomVote(c, st)
	if u.vetoDialog != first {
		t.Fatal("periodic updates opened another veto dialog")
	}
	first.Hide() // Closing the dialog must never send a veto or reopen it.
	u.refreshRoomVote(c, st)
	if u.vetoDialog != nil || c.Status().RoomControlPending {
		t.Fatal("dismissal sent a veto or reopened the popup")
	}
	st.RoomVote.Phase = proto.RoomVoteExecuted
	u.refreshRoomVote(c, st)
	if u.voteVeto.Visible() || !strings.Contains(u.voteLabel.Text, "Alice") {
		t.Fatal("execution did not clear voting controls and show the successor")
	}
	u.refreshRoomVote(nil, tunnel.Status{})
	if u.voteBar.Visible() || u.vetoDialog != nil {
		t.Fatal("disconnect retained a vote popup")
	}
}

func TestRoomVoteBallotControlsFollowSnapshot(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	u := newUI(a.NewWindow("vote controls"))
	u.build()
	c := tunnel.New(tunnel.Config{}, nil)
	st := tunnel.Status{Connected: true, RoomVotingSupported: true, ClientID: 3,
		RoomVote: proto.RoomVote{ID: 2, Kind: proto.RoomVoteKick, Phase: proto.RoomVoteVoting,
			InitiatorName: "Alice", TargetName: "Bob", Eligible: 4, Yes: 2, CanVote: true, RemainingMS: 42000}}
	u.refreshRoomVote(c, st)
	if !u.voteYes.Visible() || !u.voteNo.Visible() || u.vetoDialog != nil || !strings.Contains(u.voteDetails.Text, "需 3 票") {
		t.Fatal("eligible voter did not receive controls and strict majority threshold")
	}
	st.RoomVote.Ballot, st.RoomVote.CanVote = 1, false
	u.refreshRoomVote(c, st)
	if u.voteYes.Visible() || u.voteNo.Visible() {
		t.Fatal("already submitted ballot can be cast again")
	}
	st.RoomVote.Phase, st.RoomVote.VetoHostID = proto.RoomVoteVeto, 1
	u.refreshRoomVote(c, st)
	if u.vetoDialog != nil || u.voteVeto.Visible() {
		t.Fatal("ordinary member received the host veto")
	}
}

func TestRoomVoteOldDialogCannotAffectNewConnection(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	u := newUI(a.NewWindow("vote connection isolation"))
	u.window.SetContent(u.build())
	oldClient, newClient := tunnel.New(tunnel.Config{}, nil), tunnel.New(tunnel.Config{}, nil)
	mu.Lock()
	previous := client
	client = oldClient
	mu.Unlock()
	defer func() { mu.Lock(); client = previous; mu.Unlock() }()
	st := tunnel.Status{Connected: true, RoomVotingSupported: true, ClientID: 1, IsHost: true,
		RoomVote: proto.RoomVote{ID: 7, Kind: proto.RoomVoteTransfer, Phase: proto.RoomVoteVeto,
			VetoHostID: 1, InitiatorID: 2, InitiatorName: "Alice", Eligible: 1, Yes: 1, RemainingMS: 30000}}
	u.refreshRoomVote(oldClient, st)
	oldDialog := u.vetoDialog
	mu.Lock()
	client = newClient
	mu.Unlock()
	u.refreshRoomVote(newClient, st)
	newDialog := u.vetoDialog
	if oldDialog == nil || newDialog == nil || oldDialog == newDialog {
		t.Fatal("new connection did not replace its veto dialog")
	}
	oldDialog.Confirm()
	if u.vetoDialog != newDialog || len(u.window.Canvas().Overlays().List()) != 1 || newClient.Status().RoomControlPending {
		t.Fatal("old connection's veto callback affected the new connection")
	}
	st.IsHost = false
	u.refreshRoomVote(newClient, st)
	if u.vetoDialog != nil || u.voteVeto.Visible() {
		t.Fatal("former host retained a veto dialog")
	}
}
