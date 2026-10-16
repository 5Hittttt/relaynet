package main

import (
	"errors"
	"fmt"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
	"relaynet/internal/proto"
	"relaynet/internal/tunnel"
)

func currentClient() *tunnel.Client {
	mu.Lock()
	defer mu.Unlock()
	return client
}

func (u *uiState) buildRoomVoteBar() fyne.CanvasObject {
	u.voteLabel = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	u.voteLabel.Truncation = fyne.TextTruncateEllipsis
	u.voteDetails = caption("")
	u.voteDetails.Wrapping = fyne.TextWrapOff
	u.voteDetails.Truncation = fyne.TextTruncateEllipsis
	u.voteYes = widget.NewButton("赞成", func() { u.submitRoomBallot(true) })
	u.voteYes.Importance = widget.HighImportance
	u.voteNo = widget.NewButton("反对", func() { u.submitRoomBallot(false) })
	u.voteVeto = widget.NewButton("否决本轮投票", func() { u.submitRoomVeto(u.voteClient, u.voteID) })
	u.voteVeto.Importance = widget.DangerImportance
	actions := container.NewCenter(container.NewHBox(u.voteYes, u.voteNo, u.voteVeto))
	u.voteBar = surface(flatSidebar, inset(container.NewBorder(nil, nil, nil, actions, container.NewVBox(u.voteLabel, u.voteDetails)), 6, 18))
	u.voteBar.Hide()
	return u.voteBar
}

func (u *uiState) closeVetoDialog() {
	if u.vetoDialog != nil {
		d := u.vetoDialog
		u.vetoDialog = nil
		d.Hide()
	}
}

func (u *uiState) refreshRoomVote(c *tunnel.Client, st tunnel.Status) {
	if c != u.voteClient {
		u.closeVetoDialog()
		u.voteClient, u.voteID, u.vetoShownID, u.voteAckID = c, 0, 0, 0
	}
	if c == nil || !st.Connected || !st.RoomVotingSupported {
		u.closeVetoDialog()
		u.voteBar.Hide()
		u.voteID = 0
		return
	}
	if st.RoomControlAckID > u.voteAckID {
		u.voteAckID = st.RoomControlAckID
		if st.RoomControlError != "" {
			dialog.ShowError(errors.New(st.RoomControlError), u.window)
		}
	}
	v := st.RoomVote
	if v.ID == 0 {
		u.closeVetoDialog()
		u.voteBar.Hide()
		u.voteID = 0
		return
	}
	if v.ID != u.voteID || v.Phase != u.votePhase {
		u.closeVetoDialog()
	}
	if v.Phase != proto.RoomVoteVeto || !st.IsHost || st.ClientID != v.VetoHostID {
		u.closeVetoDialog()
	}
	u.voteID, u.votePhase = v.ID, v.Phase
	u.voteYes.Hide()
	u.voteNo.Hide()
	u.voteVeto.Hide()
	u.voteBar.Show()
	seconds := (v.RemainingMS + 999) / 1000
	required := int(v.Eligible)/2 + 1
	action := "接任房主"
	if v.Kind == proto.RoomVoteKick {
		action = fmt.Sprintf("移除 %s 并接任房主", v.TargetName)
	}
	details := fmt.Sprintf("赞成 %d / 需 %d 票 · 反对 %d · 本轮 %d 人有投票资格", v.Yes, required, v.No, v.Eligible)
	switch v.Phase {
	case proto.RoomVoteVoting:
		u.voteLabel.SetText(fmt.Sprintf("%s 提议%s · 剩余 %d 秒", v.InitiatorName, action, seconds))
		if v.CanVote && v.Ballot == 0 && v.RemainingMS > 0 {
			u.voteYes.Show()
			u.voteNo.Show()
		}
	case proto.RoomVoteVeto:
		u.voteLabel.SetText(fmt.Sprintf("投票已过半 · %s将%s · 房主否决剩余 %d 秒", v.InitiatorName, action, seconds))
		details += " · 未明确否决将在期限结束后执行"
		if st.IsHost && st.ClientID == v.VetoHostID {
			if v.RemainingMS > 0 {
				u.voteVeto.Show()
			}
			message := fmt.Sprintf("%s提议%s，已获 %d / %d 票赞成。\n\n房主权限将移交给发起人，原房主留在房间。\n你可在剩余 %d 秒内否决；关闭此窗口不算否决。", v.InitiatorName, action, v.Yes, v.Eligible, seconds)
			if u.vetoDialog != nil {
				u.vetoCountdown.SetText(message)
			} else if u.vetoShownID != v.ID && v.RemainingMS > 0 {
				u.vetoShownID = v.ID
				u.showVetoDialog(c, v.ID, message)
			}
		}
	case proto.RoomVoteExecuted:
		u.voteLabel.SetText(fmt.Sprintf("投票已执行 · %s已%s", v.InitiatorName, action))
	case proto.RoomVoteVetoed:
		u.voteLabel.SetText("本轮投票已被房主否决")
	case proto.RoomVoteExpired:
		u.voteLabel.SetText("本轮投票已结束 · 未获严格过半赞成")
	case proto.RoomVoteCancelled:
		u.voteLabel.SetText("本轮投票已取消 · 发起人或目标已离开")
	default:
		u.voteBar.Hide()
	}
	if v.Ballot == 1 {
		details += " · 你已赞成"
	} else if v.Ballot == 2 {
		details += " · 你已反对"
	}
	if st.RoomControlPending {
		details += " · 请求已发送，等待服务器确认"
		u.voteYes.Disable()
		u.voteNo.Disable()
		u.voteVeto.Disable()
	} else {
		u.voteYes.Enable()
		u.voteNo.Enable()
		u.voteVeto.Enable()
	}
	u.voteDetails.SetText(details)
}

func (u *uiState) showVetoDialog(c *tunnel.Client, id uint64, message string) {
	u.vetoCountdown = widget.NewLabel(message)
	u.vetoCountdown.Wrapping = fyne.TextWrapWord
	d := dialog.NewCustomConfirm("房间投票已通过，等待房主处理", "否决本轮投票", "暂不处理", u.vetoCountdown,
		func(veto bool) {
			if veto {
				u.submitRoomVeto(c, id)
			}
		}, u.window)
	u.vetoDialog = d
	d.SetConfirmImportance(widget.DangerImportance)
	d.SetOnClosed(func() {
		if u.vetoDialog == d {
			u.vetoDialog = nil
		}
	})
	d.Resize(fyne.NewSize(480, 270))
	u.window.Show()
	u.window.RequestFocus()
	d.Show()
}

func (u *uiState) submitRoomBallot(agree bool) {
	c, id := u.voteClient, u.voteID
	if c == nil || currentClient() != c {
		return
	}
	if err := c.CastRoomVote(id, agree); err != nil {
		dialog.ShowError(err, u.window)
	} else {
		u.voteYes.Disable()
		u.voteNo.Disable()
		u.voteDetails.SetText("投票已发送，等待服务器确认")
	}
}

func (u *uiState) submitRoomVeto(c *tunnel.Client, id uint64) {
	if c == nil || currentClient() != c || u.voteClient != c || u.voteID != id || u.votePhase != proto.RoomVoteVeto {
		return
	}
	if err := c.VetoRoomVote(id); err != nil {
		dialog.ShowError(err, u.window)
	} else {
		u.voteVeto.Disable()
		u.voteDetails.SetText("否决请求已发送，等待服务器确认")
	}
}

func (u *uiState) startRoomVote(m tunnel.Member) {
	c := currentClient()
	if c == nil {
		return
	}
	st := c.Status()
	if !st.RoomVotingSupported {
		dialog.ShowInformation("投票暂不可用", "暂未收到服务端的投票信息，请稍后重试；若一直不可用，请更新中继服务端。", u.window)
		return
	}
	action := fmt.Sprintf("移除 %s，并由你接任房主", m.Name)
	if m.ID == st.HostID {
		action = "由你接任房主，原房主保留成员身份"
	}
	message := fmt.Sprintf("%s。\n\n投票限时 60 秒，目标不参与，你自动赞成。\n本轮发起时的其余成员须严格过半赞成；通过后房主可在 30 秒内否决，未否决则执行。", action)
	dialog.ShowConfirm("发起房间投票", message, func(confirmed bool) {
		if !confirmed || currentClient() != c {
			return
		}
		if err := c.StartRoomVote(m.ID); err != nil {
			dialog.ShowError(err, u.window)
		} else {
			addLog("投票请求已发送，等待服务器确认")
		}
	}, u.window)
}
