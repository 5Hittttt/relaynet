package main

import (
	"fmt"
	"image/color"
	"strings"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"relaynet/internal/tunnel"
)

func inset(obj fyne.CanvasObject, vertical, horizontal float32) *fyne.Container {
	return container.New(layout.NewCustomPaddedLayout(vertical, vertical, horizontal, horizontal), obj)
}

func surface(fill color.Color, obj fyne.CanvasObject) *fyne.Container {
	return container.NewStack(canvas.NewRectangle(fill), obj)
}

func spacer(height float32) fyne.CanvasObject {
	r := canvas.NewRectangle(color.Transparent)
	r.SetMinSize(fyne.NewSize(0, height))
	return r
}

func caption(text string) *widget.Label {
	l := widget.NewLabel(text)
	l.Importance = widget.LowImportance
	l.SizeName = theme.SizeNameCaptionText
	l.Wrapping = fyne.TextWrapWord
	return l
}

func field(label string, control fyne.CanvasObject) fyne.CanvasObject {
	return container.NewVBox(caption(label), control)
}

func section(title, description string, content fyne.CanvasObject) fyne.CanvasObject {
	heading := widget.NewLabelWithStyle(title, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	return inset(container.NewVBox(heading, caption(description), spacer(6), content), 14, 18)
}

func avatar(name string, size float32, fill color.Color) fyne.CanvasObject {
	letter := "R"
	if name = strings.TrimSpace(name); name != "" {
		r, _ := utf8.DecodeRuneInString(name)
		letter = strings.ToUpper(string(r))
	}
	bg := canvas.NewCircle(fill)
	frame := canvas.NewRectangle(color.Transparent)
	frame.SetMinSize(fyne.NewSize(size, size))
	text := canvas.NewText(letter, color.White)
	text.TextSize = size * .42
	text.TextStyle.Bold = true
	return container.NewStack(frame, bg, container.NewCenter(text))
}

// Border layouts use the sidebar's width and let its height fill the window.
type fixedWidthLayout struct{ width float32 }

func (l fixedWidthLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(l.width, objects[0].MinSize().Height)
}

func (l fixedWidthLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	objects[0].Move(fyne.NewPos(0, 0))
	objects[0].Resize(size)
}

func fixedWidth(width float32, obj fyne.CanvasObject) fyne.CanvasObject {
	return container.New(fixedWidthLayout{width}, obj)
}

// Keep the rail, room sidebar and page flush, without theme padding between them.
type clientShellLayout struct{}

func (clientShellLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	left, side, page := objects[0].MinSize(), objects[1].MinSize(), objects[2].MinSize()
	return fyne.NewSize(left.Width+side.Width+page.Width, fyne.Max(left.Height, fyne.Max(side.Height, page.Height)))
}

func (clientShellLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	x := float32(0)
	for i, obj := range objects {
		width := obj.MinSize().Width
		if i == 2 {
			width = fyne.Max(0, size.Width-x)
		}
		obj.Move(fyne.NewPos(x, 0))
		obj.Resize(fyne.NewSize(width, size.Height))
		x += width
	}
}

func (u *uiState) buildShell(pages ...fyne.CanvasObject) fyne.CanvasObject {
	u.pages = pages
	u.pageTitle = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	u.pageSubtitle = caption("")
	u.pageSubtitle.Truncation = fyne.TextTruncateEllipsis
	u.pageSubtitle.Wrapping = fyne.TextWrapOff
	header := surface(flatBackground, inset(container.NewBorder(nil, nil, u.pageTitle, nil, u.pageSubtitle), 8, 18))
	body := container.NewStack(pages...)
	main := container.NewBorder(container.NewVBox(header, widget.NewSeparator(), u.buildRoomVoteBar()), nil, nil, nil, body)

	labels := []string{"房间聊天", "语音与共享", "屏幕直播", "运行日志", "房间设置"}
	icons := []fyne.Resource{theme.MailComposeIcon(), theme.VolumeUpIcon(), theme.DesktopIcon(), theme.FileTextIcon(), theme.SettingsIcon()}
	for i, label := range labels {
		index := i
		button := widget.NewButtonWithIcon(label, icons[i], func() { u.selectPage(index) })
		button.Alignment = widget.ButtonAlignLeading
		button.Importance = widget.LowImportance
		u.navButtons = append(u.navButtons, button)
	}
	roomIcon := widget.NewButtonWithIcon("", theme.SettingsIcon(), func() { u.selectPage(4) })
	roomIcon.Importance = widget.LowImportance
	u.roomName = widget.NewLabelWithStyle("RelayNet 房间", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	u.roomName.Truncation = fyne.TextTruncateEllipsis
	u.roomConnectionText = caption("尚未连接")
	roomHeader := inset(container.NewBorder(nil, nil, nil, roomIcon, container.NewVBox(u.roomName, u.roomConnectionText)), 14, 12)

	nav := container.NewVBox(caption("房间频道"), u.navButtons[0], u.navButtons[2], u.navButtons[1], spacer(20), caption("管理"), u.navButtons[4], u.navButtons[3])
	nav = container.NewVBox(inset(container.NewThemeOverride(nav, navigationTheme{newFlatTheme()}), 14, 12))
	u.userName = widget.NewLabelWithStyle(u.nameEntry.Text, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	u.userName.Truncation = fyne.TextTruncateEllipsis
	u.nameEntry.OnChanged = func(name string) { u.userName.SetText(name) }
	userSettings := widget.NewButtonWithIcon("", theme.SettingsIcon(), func() { u.selectPage(4) })
	userSettings.Importance = widget.LowImportance
	userCaption := caption("RelayNet 客户端")
	userCaption.Wrapping = fyne.TextWrapOff
	userCaption.Truncation = fyne.TextTruncateEllipsis
	user := container.NewBorder(nil, nil, avatar("R", 34, flatAccent), container.NewCenter(userSettings), container.NewVBox(u.userName, userCaption))
	u.statusLabel.Truncation = fyne.TextTruncateEllipsis
	footer := surface(flatRail, inset(container.NewVBox(u.statusLabel, u.btnConnect, u.btnDisconnect, spacer(4), user), 12, 12))
	sidebar := fixedWidth(224, surface(flatSidebar, container.NewBorder(roomHeader, footer, nil, nil, container.NewVScroll(nav))))

	logo := widget.NewButton("R", func() { u.selectPage(0) })
	logo.Importance = widget.HighImportance
	chat := widget.NewButtonWithIcon("", theme.HomeIcon(), func() { u.selectPage(0) })
	stream := widget.NewButtonWithIcon("", theme.DesktopIcon(), func() { u.selectPage(2) })
	settings := widget.NewButtonWithIcon("", theme.SettingsIcon(), func() { u.selectPage(4) })
	settings.Importance = widget.LowImportance
	rail := fixedWidth(64, surface(flatRail, inset(container.NewBorder(container.NewVBox(logo, spacer(6), widget.NewSeparator(), spacer(6), chat, stream), settings, nil, nil), 14, 8)))
	u.selectPage(0)
	return container.New(clientShellLayout{}, rail, sidebar, main)
}

func (u *uiState) selectPage(index int) {
	if index < 0 || index >= len(u.pages) {
		return
	}
	u.selectedPage = index
	for i, page := range u.pages {
		if i == index {
			page.Show()
			u.navButtons[i].Importance = widget.HighImportance
		} else {
			page.Hide()
			u.navButtons[i].Importance = widget.LowImportance
		}
		u.navButtons[i].Refresh()
	}
	titles := []string{"#  房间聊天", "语音与共享", "屏幕直播", "运行日志", "房间设置"}
	subtitles := []string{"和房间成员保持联系", "设置麦克风、电脑声音和屏幕共享", "选择成员，观看正在共享的屏幕", "查看连接和媒体运行情况", "管理连接信息与游戏端口"}
	u.pageTitle.SetText(titles[index])
	u.pageSubtitle.SetText(subtitles[index])
}

func (u *uiState) showConnectionError(message string) {
	u.errLabel.SetText(message)
	u.selectPage(4)
	u.connectionScroll.ScrollToTop()
}

func (u *uiState) updateRoomChrome(st tunnel.Status) {
	if !st.Connected {
		u.roomName.SetText("RelayNet 房间")
		u.roomConnectionText.SetText("尚未连接 · 前往房间设置")
		u.statusLabel.Importance = widget.LowImportance
		u.memberHeading.SetText("房间成员 — 0")
		u.membersEmpty.Show()
		u.chatEmpty.Show()
		u.chatBox.Segments = nil
		u.chatBox.Refresh()
		return
	}
	u.roomName.SetText(st.RoomID)
	u.roomConnectionText.SetText("已连接 · " + roleStr(st.IsHost))
	u.statusLabel.Importance = widget.SuccessImportance
	u.memberHeading.SetText(fmt.Sprintf("房间成员 — %d", len(st.Members)))
	u.membersEmpty.Hide()
}
