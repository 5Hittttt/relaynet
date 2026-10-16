// relayclient is the client entry point. It runs as a native desktop
// application (no browser needed) and drives the tunnel client.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"fyne.io/systray"

	"relaynet/internal/media"
	"relaynet/internal/tunnel"
)

var (
	mu       sync.Mutex
	client   *tunnel.Client
	logMu    sync.Mutex
	logLines []string
)

const maxLogs = 300

// mbpsDivisor converts bytes/second to megabits/second (Mbps = bits / 1e6).
const mbpsDivisor = 1_000_000

func addLog(format string, args ...interface{}) {
	line := time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	logMu.Lock()
	logLines = append(logLines, line)
	if len(logLines) > maxLogs {
		logLines = logLines[len(logLines)-maxLogs:]
	}
	logMu.Unlock()
}

func main() {
	// Route Media Foundation H.264 encoder/decoder diagnostics into the Log tab.
	media.SetH264LogCallback(func(msg string) { addLog("h264: %s", msg) })

	a := app.NewWithID("com.relaynet.client")
	a.Settings().SetTheme(newFlatTheme())
	w := a.NewWindow("RelayNet")
	w.SetPadded(false)
	w.Resize(fyne.NewSize(1280, 800))

	ui := newUI(w)
	w.SetContent(ui.build())

	var quitOnce sync.Once
	quitFunc := func() {
		quitOnce.Do(func() {
			go func() {
				mu.Lock()
				c := client
				client = nil
				mu.Unlock()
				if c != nil {
					c.Disconnect()
				}
				systray.Quit()
				fyne.Do(func() { a.Quit() })
			}()
		})
	}

	// 系统托盘图标：左键单击打开窗口，右键弹出菜单
	go func() {
		systray.Run(func() {
			systray.SetIcon(generateTrayIcon())
			systray.SetTitle("RelayNet")
			systray.SetTooltip("RelayNet - 联机中继")

			// 左键单击/双击 → 显示窗口
			systray.SetOnTapped(func() {
				fyne.Do(func() {
					w.Show()
					w.RequestFocus()
				})
			})

			// 右键菜单
			mShow := systray.AddMenuItem("显示窗口", "显示主窗口")
			mSetting := systray.AddMenuItem("设置关闭行为...", "修改关闭时的默认行为")
			systray.AddSeparator()
			mQuit := systray.AddMenuItem("退出", "退出程序")

			go func() {
				for {
					select {
					case <-mShow.ClickedCh:
						fyne.Do(func() {
							w.Show()
							w.RequestFocus()
						})
					case <-mSetting.ClickedCh:
						fyne.Do(func() {
							w.Show()
							showCloseDialog(a, w, quitFunc)
						})
					case <-mQuit.ClickedCh:
						quitFunc()
						return
					}
				}
			}()
		}, func() {})
	}()

	w.SetCloseIntercept(func() {
		// 读取已保存的选择，空字符串表示未设置
		choice := a.Preferences().String("closeAction")
		if choice == "minimize" {
			w.Hide()
			return
		}
		if choice == "quit" {
			quitFunc()
			return
		}
		// 首次使用，弹出选择对话框
		showCloseDialog(a, w, quitFunc)
	})

	addLog("relaynet client ready")
	go ui.refreshLoop()

	// 菜单栏
	w.SetMainMenu(fyne.NewMainMenu(
		fyne.NewMenu("文件",
			fyne.NewMenuItem("显示窗口", func() { w.Show() }),
			fyne.NewMenuItem("设置关闭行为", func() {
				showCloseDialog(a, w, quitFunc)
			}),
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("退出", quitFunc),
		),
	))

	w.SetOnClosed(func() {
		mu.Lock()
		c := client
		mu.Unlock()
		if c != nil {
			c.Disconnect()
		}
	})

	w.ShowAndRun()
}

// generateTrayIcon 生成一个 32x32 的 ICO 图标（蓝底白色 R）
func generateTrayIcon() []byte {
	const w, h = 32, 32

	// 像素数据 (BGRA, bottom-up)
	pixels := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := ((h-1-y)*w + x) * 4

			// 默认蓝色背景 #1F6FEB
			b, g, r, a := byte(0xEB), byte(0x6F), byte(0x1F), byte(0xFF)

			// 画白色圆角矩形边框
			if x < 2 || x >= w-2 || y < 2 || y >= h-2 {
				b, g, r = 0xFF, 0xFF, 0xFF
			}

			// 画字母 R 的竖线
			if (x >= 8 && x <= 11) && (y >= 8 && y <= 23) {
				b, g, r = 0xFF, 0xFF, 0xFF
			}
			// R 的上半弯弧
			if (x >= 8 && x <= 19) && (y >= 8 && y <= 12) {
				b, g, r = 0xFF, 0xFF, 0xFF
			}
			if (x >= 16 && x <= 19) && (y >= 8 && y <= 15) {
				b, g, r = 0xFF, 0xFF, 0xFF
			}
			// R 的斜腿
			if y >= 15 && y <= 23 {
				if x == y+4 || x == y+5 {
					b, g, r = 0xFF, 0xFF, 0xFF
				}
			}

			pixels[idx] = b
			pixels[idx+1] = g
			pixels[idx+2] = r
			pixels[idx+3] = a
		}
	}

	// AND mask: 全零表示全部可见
	andMask := make([]byte, w*h/8)

	// BITMAPINFOHEADER (40 bytes)
	bih := make([]byte, 40)
	binary.LittleEndian.PutUint32(bih[0:4], 40)
	binary.LittleEndian.PutUint32(bih[4:8], uint32(w))
	binary.LittleEndian.PutUint32(bih[8:12], uint32(h*2)) // height = xor + and
	binary.LittleEndian.PutUint16(bih[12:14], 1)          // planes
	binary.LittleEndian.PutUint16(bih[14:16], 32)         // bpp

	imgData := append(bih, pixels...)
	imgData = append(imgData, andMask...)

	// ICO 文件头 (6 bytes) + 目录项 (16 bytes) = 22 bytes offset
	ico := make([]byte, 6+16)
	binary.LittleEndian.PutUint16(ico[0:2], 0) // reserved
	binary.LittleEndian.PutUint16(ico[2:4], 1) // type = icon
	binary.LittleEndian.PutUint16(ico[4:6], 1) // count = 1

	// 目录项
	entry := ico[6:]
	entry[0] = byte(w)                                               // width (0 = 256, but 32 is fine)
	entry[1] = byte(h)                                               // height
	entry[2] = 0                                                     // colors
	entry[3] = 0                                                     // reserved
	binary.LittleEndian.PutUint16(entry[4:6], 1)                     // planes
	binary.LittleEndian.PutUint16(entry[6:8], 32)                    // bpp
	binary.LittleEndian.PutUint32(entry[8:12], uint32(len(imgData))) // image size
	binary.LittleEndian.PutUint32(entry[12:16], 22)                  // offset to image data

	return append(ico, imgData...)
}

// showCloseDialog 弹出关闭行为选择对话框，带"记住选择"和确定/取消
func showCloseDialog(a fyne.App, w fyne.Window, quitFunc func()) {
	minimize := widget.NewRadioGroup([]string{"最小化到后台", "直接退出"}, nil)
	minimize.SetSelected("最小化到后台")
	remember := widget.NewCheck("记住选择，不再询问", nil)
	remember.SetChecked(true)

	content := container.NewVBox(
		minimize,
		widget.NewLabel(""),
		remember,
	)

	d := dialog.NewCustomConfirm("关闭程序", "确定", "取消", content, func(ok bool) {
		if !ok {
			return
		}
		choice := "minimize"
		if minimize.Selected == "直接退出" {
			choice = "quit"
		}
		if remember.Checked {
			a.Preferences().SetString("closeAction", choice)
		}
		if choice == "minimize" {
			w.Hide()
		} else {
			quitFunc()
		}
	}, w)
	d.Resize(fyne.NewSize(350, 200))
	d.Show()
}

type uiState struct {
	window             fyne.Window
	pages              []fyne.CanvasObject
	navButtons         []*widget.Button
	selectedPage       int
	pageTitle          *widget.Label
	pageSubtitle       *widget.Label
	roomName           *widget.Label
	roomConnectionText *widget.Label
	userName           *widget.Label
	statusDetails      *widget.Label
	memberHeading      *widget.Label
	membersEmpty       fyne.CanvasObject
	chatEmpty          fyne.CanvasObject
	voteBar            *fyne.Container
	voteLabel          *widget.Label
	voteDetails        *widget.Label
	voteYes            *widget.Button
	voteNo             *widget.Button
	voteVeto           *widget.Button
	voteClient         *tunnel.Client
	voteID             uint64
	votePhase          byte
	voteAckID          uint64
	vetoShownID        uint64
	vetoDialog         *dialog.ConfirmDialog
	vetoCountdown      *widget.Label

	// connection fields
	serverEntry      *widget.Entry
	nameEntry        *widget.Entry
	hostnameEntry    *widget.Entry
	roomEntry        *widget.Entry
	passEntry        *widget.Entry
	protoUDP         *widget.Button
	protoTCP         *widget.Button
	modeHost         *widget.Button
	modeJoin         *widget.Button
	exposeEntry      *widget.Entry
	forwardsEntry    *widget.Entry
	hostBox          *fyne.Container
	joinBox          *fyne.Container
	btnConnect       *widget.Button
	btnDisconnect    *widget.Button
	errLabel         *widget.Label
	connectionScroll *container.Scroll

	// status
	statusLabel      *widget.Label
	membersContainer *fyne.Container

	// chat
	chatBox   *widget.RichText
	chatEntry *widget.Entry

	// media
	audioDeviceSel        *widget.Select
	playbackDeviceSel     *widget.Select
	micVolumeSlider       *widget.Slider
	btnAudio              *widget.Button
	btnSystemAudio        *widget.Button
	displaySel            *widget.Select
	qualitySel            *widget.Select
	bitrateSel            *widget.Select
	screenAudioCheck      *widget.Check
	screenAudioQualitySel *widget.Select
	fpsSel                *widget.Select
	captureMethodSel      *widget.Select
	btnVideo              *widget.Button
	btnPreview            *widget.Button

	// local screen preview window
	previewWin     fyne.Window
	previewImage   *canvas.Image
	previewPlaying atomic.Bool
	previewBounds  atomic.Uint64
	previewStop    chan struct{}
	previewDone    chan struct{}

	// screen viewing
	screenSharersList     *widget.List
	screenImage           *canvas.Image
	screenBounds          atomic.Uint64
	playbackResolutionSel *widget.Select
	playbackFPSSel        *widget.Select
	playbackOptions       atomic.Uint32
	playbackInfo          *widget.Label
	screenStatusLabel     *widget.Label
	selectedSharer        atomic.Uint32
	currentSharers        []uint32
	h264Viewer            *media.H264Decoder // H.264 decoder for the screen viewer (incoming frames)

	// FPS display
	showFPSCheck    *widget.Check
	fpsLabel        *widget.Label // overlay on the screen viewer (top-left)
	fpsOverlay      fyne.CanvasObject
	fpsPreviewLabel *widget.Label // overlay on the local preview window
	fpsFrameCount   int32         // atomic: frames rendered to screen viewer in the current second
	fpsPreviewCount int32         // atomic: frames rendered to preview window in the current second
	fpsEncodedCount int32         // atomic: encoded frames handed to the video sender in the current second
	fpsBytes        int64         // atomic: encoded video bytes received for screen viewer
	fpsEncodedBytes int64         // atomic: encoded video payload bytes handed to the video sender

	// logs
	logText *widget.TextGrid

	// state
	isHost             bool
	proto              string
	audioRunning       bool
	systemAudioRunning bool
	videoRunning       bool
}

func newUI(w fyne.Window) *uiState {
	return &uiState{
		window: w,
		isHost: true,
		proto:  "udp",
	}
}

func (u *uiState) build() fyne.CanvasObject {
	u.serverEntry = widget.NewEntry()
	u.serverEntry.SetText("127.0.0.1:19132")

	u.nameEntry = widget.NewEntry()
	u.nameEntry.SetText("Player1")

	u.hostnameEntry = widget.NewEntry()
	u.hostnameEntry.SetPlaceHolder("留空用系统名")

	u.roomEntry = widget.NewEntry()
	u.roomEntry.SetText("game1")

	u.passEntry = widget.NewPasswordEntry()
	u.passEntry.SetText("secret")

	u.protoUDP = widget.NewButton("UDP (默认)", nil)
	u.protoTCP = widget.NewButton("TCP (MC等)", nil)
	u.protoUDP.OnTapped = func() {
		u.proto = "udp"
		u.protoUDP.Importance = widget.HighImportance
		u.protoTCP.Importance = widget.MediumImportance
		u.protoUDP.Refresh()
		u.protoTCP.Refresh()
	}
	u.protoTCP.OnTapped = func() {
		u.proto = "tcp"
		u.protoTCP.Importance = widget.HighImportance
		u.protoUDP.Importance = widget.MediumImportance
		u.protoUDP.Refresh()
		u.protoTCP.Refresh()
	}
	u.protoUDP.Importance = widget.HighImportance

	u.modeHost = widget.NewButton("创建房间", nil)
	u.modeJoin = widget.NewButton("加入者", nil)
	u.modeHost.OnTapped = func() {
		u.isHost = true
		u.modeHost.Importance = widget.HighImportance
		u.modeJoin.Importance = widget.MediumImportance
		u.hostBox.Show()
		u.joinBox.Hide()
		u.modeHost.Refresh()
		u.modeJoin.Refresh()
	}
	u.modeJoin.OnTapped = func() {
		u.isHost = false
		u.modeJoin.Importance = widget.HighImportance
		u.modeHost.Importance = widget.MediumImportance
		u.joinBox.Show()
		u.hostBox.Hide()
		u.modeHost.Refresh()
		u.modeJoin.Refresh()
	}
	u.modeHost.Importance = widget.HighImportance

	u.exposeEntry = widget.NewEntry()
	u.exposeEntry.SetText("7777")

	u.forwardsEntry = widget.NewMultiLineEntry()
	u.forwardsEntry.SetText("7777->7777")
	u.forwardsEntry.SetMinRowsVisible(2)

	u.hostBox = container.NewVBox(
		widget.NewLabel("暴露的游戏端口 (逗号分隔)"),
		u.exposeEntry,
	)
	u.joinBox = container.NewVBox(
		widget.NewLabel("端口转发 (每行: 本地端口->房主端口)"),
		u.forwardsEntry,
	)
	u.joinBox.Hide()

	u.btnConnect = widget.NewButtonWithIcon("连接房间", theme.LoginIcon(), u.doConnect)
	u.btnConnect.Importance = widget.HighImportance
	u.btnDisconnect = widget.NewButtonWithIcon("断开连接", theme.LogoutIcon(), u.doDisconnect)
	u.btnDisconnect.Importance = widget.DangerImportance
	u.btnDisconnect.Hide()

	u.errLabel = widget.NewLabel("")
	u.errLabel.Importance = widget.DangerImportance
	u.errLabel.Wrapping = fyne.TextWrapWord
	u.statusDetails = caption("连接房间后显示详细状态")

	connCard := container.NewVScroll(container.NewVBox(
		section("连接到你的房间", "设置中继服务器、房间 ID 与密码，然后点击左侧的连接房间。", container.NewVBox(
			field("中继服务器地址", u.serverEntry),
			container.NewGridWithColumns(2, field("昵称", u.nameEntry), field("主机名（可选）", u.hostnameEntry)),
			container.NewGridWithColumns(2, field("房间 ID", u.roomEntry), field("房间密码", u.passEntry)),
			u.errLabel,
		)),
		widget.NewSeparator(),
		section("游戏组网", "选择自己的角色与转发方式。", container.NewVBox(
			field("连接协议", container.NewHBox(u.protoUDP, u.protoTCP)),
			field("房间角色", container.NewHBox(u.modeHost, u.modeJoin)),
			u.hostBox, u.joinBox,
		)),
		widget.NewSeparator(),
		section("连接状态", "房间、成员与服务器信息", u.statusDetails),
	))
	u.connectionScroll = connCard

	u.statusLabel = widget.NewLabel("● 尚未连接")
	u.statusLabel.Importance = widget.MediumImportance
	u.membersContainer = container.NewVBox()

	u.chatBox = widget.NewRichText()
	u.chatBox.Wrapping = fyne.TextWrapWord
	u.chatEntry = widget.NewEntry()
	u.chatEntry.SetPlaceHolder("发送消息到房间 · 按 Enter 发送")
	u.chatEntry.OnSubmitted = u.sendChat

	u.memberHeading = widget.NewLabelWithStyle("房间成员 — 0", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	u.membersEmpty = caption("连接房间后，成员会显示在这里。")
	members := fixedWidth(208, surface(flatSidebar, inset(container.NewBorder(
		container.NewVBox(u.memberHeading, caption("右键成员可调节音量")),
		nil, nil, nil, container.NewVScroll(container.NewVBox(u.membersEmpty, u.membersContainer)),
	), 18, 12)))
	chatWelcome := widget.NewLabelWithStyle("欢迎来到你的房间", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	chatWelcome.SizeName = theme.SizeNameHeadingText
	configure := widget.NewButtonWithIcon("设置房间", theme.SettingsIcon(), func() { u.selectPage(4) })
	configure.Importance = widget.HighImportance
	u.chatEmpty = container.NewBorder(nil, inset(container.NewVBox(
		container.NewHBox(avatar("#", 64, flatSelected)), spacer(8), chatWelcome,
		caption("连接房间，与朋友聊天、语音通话或共享屏幕。"),
		container.NewHBox(configure), spacer(12),
	), 14, 24), nil, nil)
	send := widget.NewButtonWithIcon("发送", theme.MailSendIcon(), func() { u.sendChat(u.chatEntry.Text) })
	send.Importance = widget.HighImportance
	composer := inset(container.NewBorder(nil, nil, nil, send, u.chatEntry), 16, 18)
	chatPanel := container.NewBorder(nil, composer, nil, nil, container.NewStack(inset(container.NewVScroll(u.chatBox), 14, 18), u.chatEmpty))
	combinedPage := container.NewBorder(nil, nil, nil, members, chatPanel)

	u.audioDeviceSel = widget.NewSelect([]string{"默认设备"}, nil)
	u.playbackDeviceSel = widget.NewSelect([]string{"默认设备"}, nil)
	u.audioDeviceSel.SetSelectedIndex(0)
	u.playbackDeviceSel.SetSelectedIndex(0)
	u.micVolumeSlider = widget.NewSlider(0, 100)
	u.micVolumeSlider.Value = 80
	u.micVolumeSlider.OnChanged = func(v float64) {
		if u.audioRunning {
			mu.Lock()
			c := client
			mu.Unlock()
			if c != nil {
				c.SetAudioVolume(float32(v / 100))
			}
		}
	}
	u.btnAudio = widget.NewButton("开启语音", u.toggleAudio)
	u.btnSystemAudio = widget.NewButton("共享电脑声音", u.toggleSystemAudio)
	u.btnAudio.Icon = theme.VolumeUpIcon()
	u.btnAudio.Importance = widget.HighImportance
	u.btnSystemAudio.Icon = theme.MediaMusicIcon()

	u.displaySel = widget.NewSelect([]string{"显示器 0"}, nil)
	u.displaySel.SetSelectedIndex(0)
	u.qualitySel = widget.NewSelect([]string{"720p", "1080p", "1440p", "2160p"}, nil)
	u.qualitySel.SetSelectedIndex(0) // default 720p — lightest encode budget for 120 FPS
	u.bitrateSel = widget.NewSelect([]string{"自动", "20 Mbps", "50 Mbps", "85 Mbps"}, nil)
	u.bitrateSel.SetSelectedIndex(0)
	u.screenAudioCheck = widget.NewCheck("同时共享电脑声音", nil)
	u.screenAudioCheck.SetChecked(true)
	u.screenAudioQualitySel = widget.NewSelect([]string{"低", "中", "高"}, nil)
	u.screenAudioQualitySel.SetSelectedIndex(1)
	u.fpsSel = widget.NewSelect([]string{"30 FPS", "60 FPS", "120 FPS"}, nil)
	u.fpsSel.SetSelectedIndex(2) // default 120 FPS
	u.captureMethodSel = widget.NewSelect(media.CaptureMethodNames, nil)
	u.captureMethodSel.SetSelectedIndex(0) // Auto
	u.btnVideo = widget.NewButton("开启屏幕", u.toggleVideo)
	u.btnPreview = widget.NewButton("预览", u.togglePreview)
	u.btnVideo.Icon = theme.DesktopIcon()
	u.btnVideo.Importance = widget.HighImportance
	u.btnPreview.Icon = theme.VisibilityIcon()

	mediaCard := container.NewVBox(
		section("语音通话", "与房间成员保持实时语音交流。", container.NewVBox(
			container.NewHBox(widget.NewButtonWithIcon("刷新设备", theme.ViewRefreshIcon(), u.loadDevices)),
			field("麦克风设备", u.audioDeviceSel), field("播放设备", u.playbackDeviceSel),
			field("麦克风音量", u.micVolumeSlider), container.NewHBox(u.btnAudio),
		)),
		widget.NewSeparator(),
		section("电脑声音", "单独共享电脑正在播放的声音。", container.NewHBox(u.btnSystemAudio)),
		widget.NewSeparator(),
		section("屏幕共享", "分享画面与电脑声音，让房间成员一起观看。", container.NewVBox(
			field("共享的显示器", u.displaySel),
			container.NewGridWithColumns(3, field("分辨率", u.qualitySel), field("帧率", u.fpsSel), field("目标码率", u.bitrateSel)),
			container.NewHBox(u.screenAudioCheck, widget.NewLabel("音质"), u.screenAudioQualitySel),
			field("捕获方式", u.captureMethodSel),
			caption("单端发送上限 100 Mbps · 已预留传输开销"),
			container.NewHBox(u.btnVideo, u.btnPreview),
		)),
	)

	u.logText = widget.NewTextGrid()

	logCard := inset(container.NewBorder(caption("连接与媒体运行日志"), nil, nil, nil, container.NewScroll(u.logText)), 14, 18)

	// --- Screen viewing page ---
	u.screenSharersList = widget.NewList(
		func() int { return 0 },
		func() fyne.CanvasObject {
			label := widget.NewLabel("")
			label.Truncation = fyne.TextTruncateEllipsis
			return label
		},
		func(i widget.ListItemID, o fyne.CanvasObject) {},
	)
	u.screenSharersList.OnSelected = func(id widget.ListItemID) {
		u.refreshSelectedSharer(id)
	}

	u.screenImage = canvas.NewImageFromImage(nil)
	u.screenImage.FillMode = canvas.ImageFillContain
	u.screenImage.ScaleMode = canvas.ImageScaleFastest
	u.screenImage.SetMinSize(fyne.NewSize(320, 180))

	u.screenStatusLabel = widget.NewLabel("等待屏幕共享...")
	u.screenStatusLabel.Alignment = fyne.TextAlignCenter

	// FPS overlay: bold label on a semi-transparent dark rounded background,
	// anchored to the top-left of the screen viewer via a top-border layout.
	u.fpsLabel = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	u.fpsLabel.Hide()
	fpsBg := canvas.NewRectangle(color.NRGBA{R: 0, G: 0, B: 0, A: 150})
	fpsBg.CornerRadius = 6
	fpsBox := container.NewStack(fpsBg, container.NewPadded(u.fpsLabel))
	fpsOverlay := container.NewBorder(container.NewHBox(fpsBox), nil, nil, nil, nil)
	u.fpsOverlay = fpsOverlay
	u.fpsOverlay.Hide()

	screenRight := container.New(&videoDisplayLayout{base: layout.NewStackLayout(), bounds: &u.screenBounds, scale: u.window.Canvas().Scale}, canvas.NewRectangle(flatRail), u.screenImage, container.NewCenter(u.screenStatusLabel), fpsOverlay)
	sharers := fixedWidth(160, surface(flatSidebar, inset(container.NewBorder(caption("正在直播的成员"), nil, nil, nil, u.screenSharersList), 8, 8)))
	screenSplit := container.NewBorder(nil, nil, sharers, nil, screenRight)

	// "显示帧率" toggle: persists to app preferences so the choice survives
	// restarts. Controls both the screen-viewer overlay and the local preview.
	u.showFPSCheck = widget.NewCheck("显示帧率", func(checked bool) {
		fyne.CurrentApp().Preferences().SetBool("showFPS", checked)
		if !checked {
			u.fpsLabel.Hide()
			u.fpsOverlay.Hide()
			if u.fpsPreviewLabel != nil {
				u.fpsPreviewLabel.Hide()
			}
		}
	})
	u.showFPSCheck.SetChecked(fyne.CurrentApp().Preferences().BoolWithFallback("showFPS", false))
	u.playbackResolutionSel = widget.NewSelect([]string{"原画", "720p", "1080p", "1440p", "2160p"}, func(string) { u.playbackSettingsChanged() })
	u.playbackFPSSel = widget.NewSelect([]string{"跟随共享端", "30 FPS", "60 FPS", "120 FPS"}, func(string) { u.playbackSettingsChanged() })
	resolution := fyne.CurrentApp().Preferences().IntWithFallback("screenPlaybackResolution", 0)
	fps := fyne.CurrentApp().Preferences().IntWithFallback("screenPlaybackFPS", 0)
	if resolution < 0 || resolution > 4 {
		resolution = 0
	}
	u.playbackResolutionSel.SetSelectedIndex(resolution)
	fpsIndex := 0
	switch fps {
	case 30:
		fpsIndex = 1
	case 60:
		fpsIndex = 2
	case 120:
		fpsIndex = 3
	}
	u.playbackFPSSel.SetSelectedIndex(fpsIndex)
	u.playbackInfo = caption("播放档位不超过共享端设置")

	screenControls := container.NewVBox(
		container.NewBorder(nil, nil, nil, container.NewCenter(widget.NewButtonWithIcon("刷新", theme.ViewRefreshIcon(), u.refreshScreenTab)), container.NewHBox(
			field("播放画质", u.playbackResolutionSel), field("播放帧率", u.playbackFPSSel), container.NewCenter(u.showFPSCheck),
		)), u.playbackInfo,
	)
	screenCard := inset(container.NewBorder(nil, inset(screenControls, 10, 0), nil, nil, screenSplit), 14, 18)
	return u.buildShell(combinedPage, container.NewVScroll(mediaCard), screenCard, logCard, connCard)
}

func (u *uiState) doConnect() {
	u.errLabel.SetText("")
	cfg := tunnel.Config{
		ServerAddr: strings.TrimSpace(u.serverEntry.Text),
		Name:       strings.TrimSpace(u.nameEntry.Text),
		Hostname:   strings.TrimSpace(u.hostnameEntry.Text),
		RoomID:     strings.TrimSpace(u.roomEntry.Text),
		Password:   u.passEntry.Text,
		IsHost:     u.isHost,
		Protocol:   u.proto,
	}
	if cfg.Name == "" {
		cfg.Name = "Player"
	}
	if cfg.ServerAddr == "" || cfg.RoomID == "" || cfg.Password == "" {
		u.showConnectionError("服务器地址、房间号、密码不能为空")
		return
	}

	if cfg.IsHost {
		cfg.ExposePorts = parsePorts(u.exposeEntry.Text)
	} else {
		cfg.Forwards = parseForwards(u.forwardsEntry.Text)
	}

	u.btnConnect.Disable()
	go func() {
		mu.Lock()
		if client != nil {
			old := client
			mu.Unlock()
			old.Disconnect()
			mu.Lock()
		}
		client = tunnel.New(cfg, func(format string, args ...interface{}) {
			addLog(format, args...)
		})
		c := client
		mu.Unlock()

		if err := c.Connect(); err != nil {
			addLog("连接失败: %v", err)
			mu.Lock()
			client = nil
			mu.Unlock()
			fyne.Do(func() {
				u.showConnectionError("连接失败: " + err.Error())
				u.btnConnect.Enable()
			})
			return
		}
		addLog("连接成功")
		fyne.Do(func() { u.btnConnect.Enable() })
	}()
}

func (u *uiState) doDisconnect() {
	mu.Lock()
	c := client
	client = nil
	mu.Unlock()
	if c != nil {
		c.Disconnect()
	}
	addLog("已断开")
}

func (u *uiState) sendChat(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	u.chatEntry.SetText("")
	mu.Lock()
	c := client
	mu.Unlock()
	if c == nil {
		dialog.ShowError(errors.New("未连接"), u.window)
		return
	}
	if err := c.SendChat(text); err != nil {
		addLog("发送失败: %v", err)
	}
}

func (u *uiState) toggleAudio() {
	if u.audioRunning {
		mu.Lock()
		c := client
		mu.Unlock()
		if c != nil {
			c.StopAudio()
		}
		u.audioRunning = false
		u.btnAudio.SetText("开启语音")
	} else {
		mu.Lock()
		c := client
		mu.Unlock()
		if c == nil {
			dialog.ShowError(errors.New("未连接"), u.window)
			return
		}
		deviceID := -1
		if u.audioDeviceSel.SelectedIndex() > 0 {
			deviceID = u.audioDeviceSel.SelectedIndex()
		}
		playbackID := -1
		if u.playbackDeviceSel.SelectedIndex() > 0 {
			playbackID = u.playbackDeviceSel.SelectedIndex()
		}
		volume := float32(u.micVolumeSlider.Value / 100)
		if err := c.StartAudio(deviceID, playbackID, 48000, volume); err != nil {
			dialog.ShowError(errors.New("开启语音失败: "+err.Error()), u.window)
			return
		}
		u.audioRunning = true
		u.btnAudio.SetText("停止语音")
	}
}

func (u *uiState) toggleSystemAudio() {
	if u.systemAudioRunning {
		mu.Lock()
		c := client
		mu.Unlock()
		if c != nil {
			c.StopSystemAudio()
		}
		u.systemAudioRunning = false
		u.btnSystemAudio.SetText("共享电脑声音")
		return
	}

	mu.Lock()
	c := client
	mu.Unlock()
	if c == nil {
		dialog.ShowError(errors.New("未连接"), u.window)
		return
	}
	if err := c.StartSystemAudio(48000); err != nil {
		dialog.ShowError(errors.New("共享电脑声音失败: "+err.Error()), u.window)
		return
	}
	u.systemAudioRunning = true
	u.btnSystemAudio.SetText("停止共享电脑声音")
}

func (u *uiState) toggleVideo() {
	if u.videoRunning {
		mu.Lock()
		c := client
		mu.Unlock()
		if c != nil {
			c.StopVideo()
		}
		u.closePreview()
		u.videoRunning = false
		u.screenAudioCheck.Enable()
		u.screenAudioQualitySel.Enable()
		u.btnSystemAudio.Enable()
		u.btnVideo.SetText("开启屏幕")
	} else {
		mu.Lock()
		c := client
		mu.Unlock()
		if c == nil {
			dialog.ShowError(errors.New("未连接"), u.window)
			return
		}
		displayIdx := u.displaySel.SelectedIndex()
		if displayIdx < 0 {
			dialog.ShowError(errors.New("请先选择显示器（点击\"刷新设备\"）"), u.window)
			return
		}
		qualityKey := "1080p"
		switch u.qualitySel.SelectedIndex() {
		case 0:
			qualityKey = "720p"
		case 1:
			qualityKey = "1080p"
		case 2:
			qualityKey = "1440p"
		case 3:
			qualityKey = "2160p"
		}
		q, ok := media.VideoQualities[qualityKey]
		if !ok {
			dialog.ShowError(errors.New("无效的分辨率"), u.window)
			return
		}
		q.FPS = 30
		switch u.fpsSel.SelectedIndex() {
		case 1:
			q.FPS = 60
		case 2:
			q.FPS = 120
		}
		switch u.bitrateSel.SelectedIndex() {
		case 1:
			q.Bitrate = 20_000
		case 2:
			q.Bitrate = 50_000
		case 3:
			q.Bitrate = 85_000
		}
		method := media.CaptureMethod(u.captureMethodSel.SelectedIndex())
		audioQuality := media.ScreenAudioOff
		if u.screenAudioCheck.Checked {
			audioQuality = media.ScreenAudioQuality(u.screenAudioQualitySel.SelectedIndex() + 1)
			if u.systemAudioRunning {
				c.StopSystemAudio()
				u.systemAudioRunning = false
				u.btnSystemAudio.SetText("共享电脑声音")
			}
		}

		// open preview before StartVideo so frames aren't dropped
		u.openPreview()
		// Preview decoder runs in its own goroutine so frame decoding never
		// blocks the capture→encrypt→send path. Without this, the synchronous
		// preview decode in the video callback stalled the frame channel and
		// dropped the effective send rate far below target.
		previewChan := make(chan media.VideoFrame, 120)
		previewStop := make(chan struct{})
		previewDone := make(chan struct{})
		previewMissed := new(atomic.Bool)
		u.previewStop = previewStop
		u.previewDone = previewDone
		go u.previewDecodeLoop(previewChan, previewStop, previewDone, previewMissed)

		if err := c.StartVideo(displayIdx, q, method, audioQuality, func(data []byte, typ byte) {
			atomic.AddInt32(&u.fpsEncodedCount, 1)
			atomic.AddInt64(&u.fpsEncodedBytes, int64(len(data)))
			select {
			case <-previewStop:
			case previewChan <- media.VideoFrame{Data: data, Type: typ}:
			default:
				previewMissed.Store(true)
			}
		}); err != nil {
			u.closePreview()
			dialog.ShowError(errors.New("开启屏幕失败: "+err.Error()), u.window)
			return
		}
		u.videoRunning = true
		u.screenAudioCheck.Disable()
		u.screenAudioQualitySel.Disable()
		if audioQuality != media.ScreenAudioOff {
			u.btnSystemAudio.Disable()
		}
		u.btnVideo.SetText("停止屏幕")
	}
}

// openPreview creates a separate window showing the local screen capture in
// real-time. When the window loses focus, playback pauses; the user must click
// the play button to resume.
func (u *uiState) openPreview() {
	win := fyne.CurrentApp().NewWindow("屏幕预览")
	win.Resize(fyne.NewSize(960, 540))

	img := canvas.NewImageFromImage(nil)
	img.FillMode = canvas.ImageFillContain
	img.ScaleMode = canvas.ImageScaleFastest
	img.SetMinSize(fyne.NewSize(320, 180))
	u.previewImage = img
	u.previewPlaying.Store(true)

	playBtn := widget.NewButton("▶ 播放", nil)
	playBtn.Importance = widget.HighImportance

	pausedLabel := widget.NewLabel("已暂停 - 点击播放继续")
	pausedLabel.Alignment = fyne.TextAlignCenter

	playOverlay := container.NewCenter(
		container.NewVBox(pausedLabel, playBtn),
	)

	// FPS overlay for the local preview window (top-left), matching the
	// screen-viewer style. Hidden unless the "显示帧率" toggle is on.
	u.fpsPreviewLabel = widget.NewLabelWithStyle("", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	u.fpsPreviewLabel.Hide()
	prevFpsBg := canvas.NewRectangle(color.NRGBA{R: 0, G: 0, B: 0, A: 150})
	prevFpsBg.CornerRadius = 6
	prevFpsBox := container.NewStack(prevFpsBg, container.NewPadded(u.fpsPreviewLabel))
	prevFpsOverlay := container.NewBorder(container.NewHBox(prevFpsBox), nil, nil, nil, nil)

	content := container.New(&videoDisplayLayout{base: layout.NewStackLayout(), bounds: &u.previewBounds, scale: win.Canvas().Scale}, img, playOverlay, prevFpsOverlay)
	playOverlay.Hide()

	win.SetContent(content)
	u.previewWin = win

	playBtn.OnTapped = func() {
		u.previewPlaying.Store(true)
		playOverlay.Hide()
	}

	win.SetOnClosed(func() {
		u.previewWin = nil
		u.previewImage = nil
		u.previewPlaying.Store(false)
	})

	win.Show()

	// poll foreground window to detect focus loss (Fyne has no focus callback)
	go u.focusMonitor("屏幕预览", playOverlay)
}

// focusMonitor polls GetForegroundWindow and pauses the preview when the
// preview window is not the active window.
func (u *uiState) focusMonitor(title string, overlay *fyne.Container) {
	user32 := syscall.NewLazyDLL("user32.dll")
	findWindow := user32.NewProc("FindWindowW")
	getForeground := user32.NewProc("GetForegroundWindow")

	titlePtr, _ := syscall.UTF16PtrFromString(title)
	time.Sleep(500 * time.Millisecond) // wait for window creation

	wasFocused := true
	for {
		time.Sleep(200 * time.Millisecond)
		if u.previewWin == nil {
			return
		}

		hwnd, _, _ := findWindow.Call(0, uintptr(unsafe.Pointer(titlePtr)))
		if hwnd == 0 {
			continue
		}

		fgHwnd, _, _ := getForeground.Call()
		focused := hwnd == fgHwnd

		if !focused && wasFocused {
			u.previewPlaying.Store(false)
			fyne.Do(func() {
				overlay.Show()
			})
		}
		wasFocused = focused
	}
}

func (u *uiState) closePreview() {
	if u.previewWin != nil {
		u.previewWin.Close()
		u.previewWin = nil
		u.previewImage = nil
		u.fpsPreviewLabel = nil
		u.previewPlaying.Store(false)
	}
	if u.previewStop != nil {
		close(u.previewStop)
		<-u.previewDone
		u.previewStop = nil
		u.previewDone = nil
	}
}

// decodeFrame decodes one encoded frame into an image for display. H.264
// frames go through the Media Foundation decoder identified by *dec (created
// lazily); a broken H.264 stream closes the decoder so the caller rebuilds it
// and waits for the next IDR keyframe. JPEG frames decode via libjpeg-turbo.
func (u *uiState) decodeFrame(frame media.VideoFrame, dec **media.H264Decoder, discard bool, maxW, maxH int) (image.Image, int64, bool) {
	switch frame.Type {
	case media.FrameTypeH264IDR, media.FrameTypeH264P:
		if *dec == nil {
			d, err := media.NewH264Decoder()
			if err != nil {
				fmt.Println("h264: decoder unavailable:", err)
				return nil, 0, false
			}
			*dec = d
		}
		if discard {
			if err := (*dec).SkipAt(frame.Data, frame.TimestampNS); err != nil {
				(*dec).Close()
				*dec = nil
			}
			return nil, 0, false
		}
		rgba, w, h, pts, err := (*dec).SendSizedAt(frame.Data, frame.TimestampNS, maxW, maxH)
		if err != nil {
			// Stream broken (UDP loss broke the reference chain): drop the
			// decoder, it will be rebuilt and wait for the next keyframe.
			d := *dec
			*dec = nil
			d.Close()
			return nil, 0, false
		}
		if w <= 0 || h <= 0 || rgba == nil {
			return nil, 0, false // decoder still buffering / P-frame before keyframe
		}
		return &image.RGBA{Pix: rgba, Stride: w * 4, Rect: image.Rect(0, 0, w, h)}, pts, true
	default: // FrameTypeJPEG (legacy sender / encoder fallback)
		if discard {
			return nil, 0, false
		}
		img, err := media.TurboDecode(frame.Data)
		if err != nil {
			return nil, 0, false
		}
		return img, frame.TimestampNS, true
	}
}

// previewDecodeLoop feeds compressed frames to the decoder in order. H.264
// P-frames cannot be skipped without breaking the decoder's reference chain.
func (u *uiState) previewDecodeLoop(ch <-chan media.VideoFrame, stop <-chan struct{}, done chan<- struct{}, missed *atomic.Bool) {
	defer close(done)
	var decoder *media.H264Decoder
	defer func() {
		if decoder != nil {
			decoder.Close()
		}
	}()
	var displayMu sync.Mutex
	var pending image.Image
	var displayQueued bool
	for {
		select {
		case <-stop:
			return
		case frame := <-ch:
			if missed.Swap(false) && decoder != nil {
				decoder.Close()
				decoder = nil
			}
			if len(frame.Data) == 0 || (frame.Type == media.FrameTypeH264P && decoder == nil) {
				continue
			}
			maxW, maxH := loadDisplaySize(&u.previewBounds)
			img, _, ok := u.decodeFrame(frame, &decoder, !u.previewPlaying.Load(), maxW, maxH)
			if !ok {
				continue
			}
			displayMu.Lock()
			pending = img
			if displayQueued {
				displayMu.Unlock()
				continue
			}
			displayQueued = true
			displayMu.Unlock()
			fyne.Do(func() {
				displayMu.Lock()
				shown := pending
				pending = nil
				displayQueued = false
				displayMu.Unlock()
				if u.previewImage != nil && u.previewPlaying.Load() {
					u.previewImage.Image = shown
					u.previewImage.Refresh()
					atomic.AddInt32(&u.fpsPreviewCount, 1)
				}
			})
		}
	}
}

// togglePreview opens or focuses the preview window. Only works while screen
// sharing is active.
func (u *uiState) togglePreview() {
	if !u.videoRunning {
		dialog.ShowError(errors.New("屏幕共享未开启"), u.window)
		return
	}
	if u.previewWin != nil {
		u.previewWin.RequestFocus()
		return
	}
	u.openPreview()
}

func (u *uiState) refreshLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	// initial device load
	u.loadDevices()

	// Feed every H.264 frame to one decoder in receive order. Only decoded
	// images are coalesced when the UI cannot paint as quickly as they arrive.
	go func() {
		frameTicker := time.NewTicker(8 * time.Millisecond)
		defer frameTicker.Stop()
		var lastSeq uint64
		var activeClient *tunnel.Client
		var activeSharer uint32
		var activeStream uint64
		var activeSubscription uint64
		var lastFrameSequence uint32
		var order media.ScreenVideoOrder
		type displayFrame struct {
			img    image.Image
			video  media.VideoFrame
			client *tunnel.Client
		}
		var scheduled []displayFrame
		var encoded []media.VideoFrame
		var displayMu sync.Mutex
		var pending displayFrame
		var displayQueued bool
		queueDisplay := func(frame displayFrame) {
			displayMu.Lock()
			pending = frame
			if displayQueued {
				displayMu.Unlock()
				return
			}
			displayQueued = true
			displayMu.Unlock()
			fyne.Do(func() {
				displayMu.Lock()
				shown := pending
				pending = displayFrame{}
				displayQueued = false
				displayMu.Unlock()
				mu.Lock()
				current := client
				mu.Unlock()
				if shown.img == nil || current != shown.client || u.selectedSharer.Load() != shown.video.SourceID || shown.video.SubscriptionID != shown.client.ScreenSubscriptionID() {
					return
				}
				position, ready := shown.client.ScreenPosition(shown.video)
				if !ready || (shown.video.StreamID != 0 && position-shown.video.TimestampNS > int64(100*time.Millisecond)) {
					return
				}
				u.screenImage.Image = shown.img
				u.screenImage.Refresh()
				u.screenStatusLabel.Hide()
				atomic.AddInt32(&u.fpsFrameCount, 1)
			})
		}
		resetDecoder := func() {
			if u.h264Viewer != nil {
				u.h264Viewer.Close()
				u.h264Viewer = nil
			}
			scheduled = nil
		}
		for range frameTicker.C {
			sharer := u.selectedSharer.Load()
			mu.Lock()
			c := client
			mu.Unlock()
			if sharer != activeSharer || c != activeClient {
				resetDecoder()
				if activeClient != nil {
					activeClient.SelectScreenSource(0)
				}
				if c != nil {
					options := u.playbackOptions.Load()
					c.SetScreenPlayback(byte(options>>8), int(options&255))
					c.SelectScreenSource(sharer)
				}
				lastSeq, activeStream, lastFrameSequence = 0, 0, 0
				order = media.ScreenVideoOrder{}
				encoded = nil
				activeSharer, activeClient = sharer, c
			}
			if sharer == 0 || c == nil {
				continue
			}
			requestID := c.ScreenSubscriptionID()
			if requestID != activeSubscription {
				resetDecoder()
				encoded = nil
				order = media.ScreenVideoOrder{}
				lastFrameSequence = 0
				activeSubscription = requestID
			}
			seq, frames, missed := c.GetVideoFramesSince(lastSeq)
			lastSeq = seq
			if missed {
				resetDecoder()
				lastFrameSequence = 0
			}
			var ordered []media.VideoFrame
			for _, frame := range frames {
				if frame.SourceID != sharer || len(frame.Data) == 0 || frame.SubscriptionID != activeSubscription {
					continue
				}
				atomic.AddInt64(&u.fpsBytes, int64(len(frame.Data)))
				if frame.StreamID == 0 {
					ordered = append(ordered, frame)
				} else {
					order.Push(frame)
				}
			}
			ordered = append(ordered, order.Ready(time.Now())...)
			for _, frame := range ordered {
				if frame.StreamID != 0 && frame.StreamID < activeStream {
					continue
				}
				if frame.StreamID > activeStream {
					resetDecoder()
					encoded = nil
					activeStream, lastFrameSequence = frame.StreamID, 0
				}
				encoded = append(encoded, frame)
			}
			if len(encoded) > 240 {
				encoded = encoded[len(encoded)-240:]
				resetDecoder()
			}
			for len(encoded) > 0 {
				frame := encoded[0]
				c.ScreenPosition(frame)
				gap := frame.Discontinuity || (lastFrameSequence != 0 && frame.Sequence != 0 && frame.Sequence != lastFrameSequence+1)
				if frame.StreamID != 0 && u.h264Viewer != nil && !gap && frame.TimestampNS > c.ScreenDecodeLimit(frame) {
					break
				}
				encoded = encoded[1:]
				if frame.SourceID != sharer || len(frame.Data) == 0 {
					continue
				}
				if frame.StreamID != 0 && frame.StreamID < activeStream {
					continue
				}
				if frame.Discontinuity {
					resetDecoder()
				}
				if frame.StreamID != activeStream {
					resetDecoder()
					activeStream, lastFrameSequence = frame.StreamID, 0
				}
				if frame.Sequence != 0 {
					if frame.Sequence <= lastFrameSequence {
						continue
					}
					if lastFrameSequence != 0 && frame.Sequence != lastFrameSequence+1 {
						resetDecoder()
					}
					lastFrameSequence = frame.Sequence
				}
				if frame.Type == media.FrameTypeH264P && u.h264Viewer == nil {
					continue
				}
				maxW, maxH := loadDisplaySize(&u.screenBounds)
				img, pts, ok := u.decodeFrame(frame, &u.h264Viewer, frame.TimestampNS < c.ScreenDiscardBefore(frame), maxW, maxH)
				if ok {
					frame.TimestampNS = pts
					c.ScreenVideoDecoded(frame)
					scheduled = append(scheduled, displayFrame{img, frame, c})
					if len(scheduled) > 120 {
						scheduled = scheduled[len(scheduled)-120:]
					}
					// Once playback starts, submit due pictures after each decode.
					// Draining a whole 4K batch can take seconds and make every
					// picture stale before the UI gets its first update.
					if _, ready := c.ScreenPosition(frame); ready {
						break
					}
				}
			}
			if len(scheduled) == 0 {
				continue
			}
			position, ready := c.ScreenPosition(scheduled[len(scheduled)-1].video)
			if !ready {
				continue
			}
			due := -1
			for i, frame := range scheduled {
				if frame.video.StreamID == 0 || frame.video.TimestampNS <= position {
					due = i
				}
			}
			if due >= 0 {
				shown := scheduled[due]
				scheduled = scheduled[due+1:]
				if shown.video.StreamID == 0 || position-shown.video.TimestampNS <= int64(100*time.Millisecond) {
					queueDisplay(shown)
				}
			}
		}
	}()
	for range ticker.C {
		fyne.Do(func() { u.refresh() })
	}
}

func (u *uiState) loadDevices() {
	go func() {
		audioDevices := media.ListAudioDevices()
		audioNames := make([]string, len(audioDevices))
		for i, d := range audioDevices {
			audioNames[i] = d.Name
		}
		if len(audioNames) == 0 {
			audioNames = []string{"默认设备"}
		}

		playbackDevices := media.ListPlaybackDevices()
		playbackNames := make([]string, len(playbackDevices))
		for i, d := range playbackDevices {
			playbackNames[i] = d.Name
		}
		if len(playbackNames) == 0 {
			playbackNames = []string{"默认设备"}
		}

		displays := media.ListDisplays()
		if len(displays) == 0 {
			displays = []string{"显示器 0"}
		}

		fyne.Do(func() {
			u.audioDeviceSel.Options = audioNames
			u.audioDeviceSel.SetSelectedIndex(0)
			u.playbackDeviceSel.Options = playbackNames
			u.playbackDeviceSel.SetSelectedIndex(0)
			u.displaySel.Options = displays
			u.displaySel.SetSelectedIndex(0)
		})
	}()
}

func (u *uiState) refresh() {
	u.refreshPlaybackInfo()
	// Refresh the FPS overlays once per second. The counters are incremented
	// atomically by the decode goroutines for every frame that actually gets
	// painted to the screen. We always swap (reset) the counters so that a
	// later toggle-on doesn't read a stale accumulated value.
	screenFPS := atomic.SwapInt32(&u.fpsFrameCount, 0)
	previewFPS := atomic.SwapInt32(&u.fpsPreviewCount, 0)
	encodedFPS := atomic.SwapInt32(&u.fpsEncodedCount, 0)
	screenBytes := atomic.SwapInt64(&u.fpsBytes, 0)
	encodedBytes := atomic.SwapInt64(&u.fpsEncodedBytes, 0)
	showFPS := u.showFPSCheck != nil && u.showFPSCheck.Checked
	if u.fpsLabel != nil {
		if showFPS {
			// Measured encoded payload bytes received this second — contrast with the nominal quality label
			// (e.g. "720p (4000k)").
			u.fpsLabel.SetText(fmt.Sprintf("%d FPS · %.1f Mbps", screenFPS,
				float64(screenBytes)*8/mbpsDivisor))
			u.fpsLabel.Show()
			u.fpsOverlay.Show()
		} else {
			u.fpsLabel.Hide()
			u.fpsOverlay.Hide()
		}
	}
	if u.fpsPreviewLabel != nil {
		if showFPS {
			// Keep UI refresh rate separate from encoded stream rate and bitrate.
			u.fpsPreviewLabel.SetText(fmt.Sprintf("预览 %d FPS · 码流 %d FPS / %.1f Mbps", previewFPS,
				encodedFPS, float64(encodedBytes)*8/mbpsDivisor))
			u.fpsPreviewLabel.Show()
		} else {
			u.fpsPreviewLabel.Hide()
		}
	}

	mu.Lock()
	c := client
	mu.Unlock()

	st := tunnel.Status{}
	if c != nil {
		st = c.Status()
	}
	u.refreshRoomVote(c, st)
	if c == nil || !st.Connected {
		u.updateRoomChrome(tunnel.Status{})
		u.statusLabel.SetText("● 未连接")
		u.statusDetails.SetText("连接房间后显示详细状态")
		u.btnConnect.Show()
		u.btnDisconnect.Hide()
		u.membersContainer.Objects = nil
		u.membersContainer.Refresh()
		u.currentSharers = nil
		u.screenSharersList.Length = func() int { return 0 }
		u.screenSharersList.Refresh()
		u.selectedSharer.Store(0)
		u.screenSharersList.UnselectAll()
		u.screenImage.Image = nil
		u.screenImage.Refresh()
		u.screenStatusLabel.SetText("等待屏幕共享...")
		u.screenStatusLabel.Show()
		// reset media state on disconnect
		u.screenAudioCheck.Enable()
		u.screenAudioQualitySel.Enable()
		u.btnSystemAudio.Enable()
		u.closePreview()
		if u.videoRunning {
			u.videoRunning = false
			u.btnVideo.SetText("开启屏幕")
		}
		if u.audioRunning {
			u.audioRunning = false
			u.btnAudio.SetText("开启语音")
		}
		if u.systemAudioRunning {
			u.systemAudioRunning = false
			u.btnSystemAudio.SetText("共享电脑声音")
		}
		return
	}

	u.btnConnect.Hide()
	u.btnDisconnect.Show()

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("● 已连接 | ID: %d | 角色: %s | 房主ID: %d | 成员: %d | 服务器延迟: ",
		st.ClientID, roleStr(st.IsHost), st.HostID, len(st.Members)))
	if st.ServerRTT >= 0 {
		sb.WriteString(fmt.Sprintf("%d ms", st.ServerRTT))
	} else {
		sb.WriteString("测量中")
	}
	sb.WriteString("\n房间: " + st.RoomID + " | 服务器: " + st.ServerAddr)
	u.statusDetails.SetText(sb.String())
	u.updateRoomChrome(st)
	u.statusLabel.SetText("● 已连接 · 测量延迟中")
	if st.ServerRTT >= 0 {
		u.statusLabel.SetText(fmt.Sprintf("● 已连接 · %d ms", st.ServerRTT))
	}

	members := st.Members
	items := make([]fyne.CanvasObject, 0, len(members))
	for _, m := range members {
		items = append(items, newMemberItem(m, u.showMemberMenu))
	}
	u.membersContainer.Objects = items
	u.membersContainer.Refresh()

	// screen sharers list
	memberMap := make(map[uint32]tunnel.Member, len(st.Members))
	for _, m := range st.Members {
		memberMap[m.ID] = m
	}
	u.currentSharers = st.VideoSharers
	u.screenSharersList.Length = func() int { return len(u.currentSharers) }
	u.screenSharersList.UpdateItem = func(i widget.ListItemID, o fyne.CanvasObject) {
		if i >= len(u.currentSharers) {
			return
		}
		id := u.currentSharers[i]
		name := fmt.Sprintf("#%d", id)
		if m, ok := memberMap[id]; ok {
			name = m.Name
			if m.Hostname != "" {
				name += " (" + m.Hostname + ")"
			}
		}
		o.(*widget.Label).SetText(name)
	}
	u.screenSharersList.Refresh()

	// if the selected sharer stopped sharing, reset
	if u.selectedSharer.Load() != 0 {
		found := false
		for _, id := range u.currentSharers {
			if id == u.selectedSharer.Load() {
				found = true
				break
			}
		}
		if !found {
			u.selectedSharer.Store(0)
			u.screenSharersList.UnselectAll()
			u.screenImage.Image = nil
			u.screenImage.Refresh()
			u.screenStatusLabel.SetText("等待屏幕共享...")
			u.screenStatusLabel.Show()
		}
	}

	// (frame display is handled by the high-frequency frame ticker goroutine)

	// chat
	if len(st.Chat) > 0 {
		u.chatEmpty.Hide()
		segs := make([]widget.RichTextSegment, 0, len(st.Chat)*2)
		for _, msg := range st.Chat {
			name := msg.SenderName
			style := widget.RichTextStyleParagraph
			style.TextStyle = fyne.TextStyle{Bold: true}
			if msg.Mine {
				style.ColorName = theme.ColorNameSuccess
			} else {
				style.ColorName = theme.ColorNamePrimary
			}
			segs = append(segs, &widget.TextSegment{Text: name + ": ", Style: style})
			segs = append(segs, &widget.TextSegment{Text: msg.Text + "\n", Style: widget.RichTextStyleParagraph})
		}
		u.chatBox.Segments = segs
		u.chatBox.Refresh()
	} else {
		u.chatEmpty.Show()
		u.chatBox.Segments = nil
		u.chatBox.Refresh()
	}

	// logs
	logMu.Lock()
	logs := append([]string(nil), logLines...)
	logMu.Unlock()
	if len(logs) > 80 {
		logs = logs[len(logs)-80:]
	}
	u.logText.SetText(strings.Join(logs, "\n"))
}

// memberItemWidget is a custom list-row widget that supports right-click
// (secondary tap) to trigger a context menu for the member.
type memberItemWidget struct {
	widget.BaseWidget
	label   *widget.Label
	content fyne.CanvasObject
	member  tunnel.Member
	onRight func(tunnel.Member, fyne.Position)
}

func newMemberItem(m tunnel.Member, onRight func(tunnel.Member, fyne.Position)) *memberItemWidget {
	mi := &memberItemWidget{
		label:   widget.NewLabel(""),
		member:  m,
		onRight: onRight,
	}
	mi.label.Truncation = fyne.TextTruncateEllipsis
	mi.ExtendBaseWidget(mi)

	name := m.Name
	role := ""
	if m.IsHost {
		role = " · 房主"
	}
	rtt := "测量中"
	if m.RTT > 0 {
		rtt = fmt.Sprintf("%d ms", m.RTT)
	}
	mi.label.TextStyle.Bold = true
	mi.label.SetText(name + role)
	detail := caption(fmt.Sprintf("#%d · %s", m.ID, rtt))
	if m.Hostname != "" {
		detail.SetText(fmt.Sprintf("#%d · %s · %s", m.ID, m.Hostname, rtt))
	}
	detail.Wrapping = fyne.TextWrapOff
	detail.Truncation = fyne.TextTruncateEllipsis
	mi.content = inset(container.NewBorder(nil, nil, avatar(name, 32, flatAccent), nil, container.NewVBox(mi.label, detail)), 4, 0)
	return mi
}

func (mi *memberItemWidget) Tapped(*fyne.PointEvent) {}

func (mi *memberItemWidget) TappedSecondary(e *fyne.PointEvent) {
	if mi.onRight != nil {
		mi.onRight(mi.member, e.AbsolutePosition)
	}
}

func (mi *memberItemWidget) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(mi.content)
}

// showMemberMenu displays a context menu at the given position with options
// to adjust the member's playback volume and (for host) kick the member.
func (u *uiState) showMemberMenu(m tunnel.Member, pos fyne.Position) {
	items := []*fyne.MenuItem{
		fyne.NewMenuItem("调节音量...", func() { u.showVolumeDialog(m) }),
	}
	c := currentClient()
	st := tunnel.Status{}
	if c != nil {
		st = c.Status()
	}
	if st.Connected && m.ID != st.ClientID {
		text := "发起踢人投票"
		if m.ID == st.HostID {
			text = "投票接任房主"
		}
		items = append(items, fyne.NewMenuItem(text, func() {
			if currentClient() == c {
				u.startRoomVote(m)
			}
		}))
	}
	if st.Connected && st.IsHost && m.ID != st.ClientID && !m.IsHost {
		items = append(items,
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("踢出房间", func() { u.kickMember(m) }),
		)
	}
	menu := fyne.NewMenu("", items...)
	widget.ShowPopUpMenuAtPosition(menu, u.window.Canvas(), pos)
}

// showVolumeDialog opens a dialog with a slider to control the local playback
// volume for a specific member.
func (u *uiState) showVolumeDialog(m tunnel.Member) {
	slider := widget.NewSlider(0, 100)
	volLabel := widget.NewLabel("")

	mu.Lock()
	c := client
	mu.Unlock()
	currentVol := float32(1.0)
	if c != nil {
		currentVol = c.GetPeerVolume(m.ID)
	}
	slider.Value = float64(currentVol * 100)
	volLabel.SetText(fmt.Sprintf("音量: %d%%", int(slider.Value)))

	slider.OnChanged = func(v float64) {
		volLabel.SetText(fmt.Sprintf("音量: %d%%", int(v)))
		mu.Lock()
		c := client
		mu.Unlock()
		if c != nil {
			c.SetPeerVolume(m.ID, float32(v/100))
		}
	}

	content := container.NewVBox(
		widget.NewLabel(fmt.Sprintf("%s (#%d)", m.Name, m.ID)),
		slider,
		volLabel,
	)
	d := dialog.NewCustom("音量控制", "关闭", content, u.window)
	d.Resize(fyne.NewSize(300, 180))
	d.Show()
}

// kickMember asks for confirmation then sends a kick request to the server.
func (u *uiState) kickMember(m tunnel.Member) {
	dialog.ShowConfirm("踢出房间",
		fmt.Sprintf("确定要将 %s 踢出房间吗?", m.Name),
		func(ok bool) {
			if !ok {
				return
			}
			mu.Lock()
			c := client
			mu.Unlock()
			if c == nil {
				return
			}
			if err := c.KickMember(m.ID); err != nil {
				dialog.ShowError(err, u.window)
			} else {
				addLog("踢出请求已发送: %s", m.Name)
			}
		}, u.window)
}

// refreshSelectedSharer handles list selection by storing the chosen member ID
// and immediately updating the displayed frame.
func (u *uiState) refreshSelectedSharer(id widget.ListItemID) {
	if id < 0 || id >= len(u.currentSharers) {
		return
	}
	u.selectedSharer.Store(u.currentSharers[id])
	u.updateScreenFrame()
}

// updateScreenFrame is called from UI callbacks (main thread) to reset the
// status label when a new sharer is selected. The actual frame rendering is
// handled by the high-frequency frame ticker goroutine.
func (u *uiState) updateScreenFrame() {
	u.screenImage.Image = nil
	u.screenImage.Refresh()
	u.screenStatusLabel.SetText("等待画面数据...")
	u.screenStatusLabel.Show()
}

// refreshScreenTab resets the screen viewing area and re-pulls the sharer
// list + latest frame from the client.
func (u *uiState) refreshScreenTab() {
	mu.Lock()
	c := client
	mu.Unlock()
	if c == nil {
		u.screenImage.Image = nil
		u.screenImage.Refresh()
		u.screenStatusLabel.SetText("等待屏幕共享...")
		u.screenStatusLabel.Show()
		u.selectedSharer.Store(0)
		u.screenSharersList.UnselectAll()
		return
	}

	st := c.Status()
	u.currentSharers = st.VideoSharers
	u.screenSharersList.Length = func() int { return len(u.currentSharers) }

	memberMap := make(map[uint32]tunnel.Member, len(st.Members))
	for _, m := range st.Members {
		memberMap[m.ID] = m
	}
	u.screenSharersList.UpdateItem = func(i widget.ListItemID, o fyne.CanvasObject) {
		if i >= len(u.currentSharers) {
			return
		}
		id := u.currentSharers[i]
		name := fmt.Sprintf("#%d", id)
		if m, ok := memberMap[id]; ok {
			name = m.Name
			if m.Hostname != "" {
				name += " (" + m.Hostname + ")"
			}
		}
		o.(*widget.Label).SetText(name)
	}
	u.screenSharersList.Refresh()

	// If the previously selected sharer is no longer active, reset.
	if u.selectedSharer.Load() != 0 {
		found := false
		for _, id := range u.currentSharers {
			if id == u.selectedSharer.Load() {
				found = true
				break
			}
		}
		if !found {
			u.selectedSharer.Store(0)
			u.screenSharersList.UnselectAll()
		}
	}
	if u.selectedSharer.Load() == 0 {
		u.screenImage.Image = nil
		u.screenImage.Refresh()
		u.screenStatusLabel.SetText("等待屏幕共享...")
		u.screenStatusLabel.Show()
	} else {
		u.updateScreenFrame()
	}
}

func parsePorts(s string) []int {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
	result := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err == nil {
			result = append(result, n)
		}
	}
	return result
}

func parseForwards(s string) []tunnel.Forward {
	lines := strings.Split(s, "\n")
	result := make([]tunnel.Forward, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.FieldsFunc(line, func(r rune) bool {
			return r == '-' || r == '>' || r == ':' || r == '='
		})
		if len(parts) == 1 {
			n, err := strconv.Atoi(strings.TrimSpace(parts[0]))
			if err == nil {
				result = append(result, tunnel.Forward{LocalPort: n, RemotePort: n})
			}
		} else if len(parts) == 2 {
			a, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
			b, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err1 == nil && err2 == nil {
				result = append(result, tunnel.Forward{LocalPort: a, RemotePort: b})
			}
		}
	}
	return result
}

func roleStr(isHost bool) string {
	if isHost {
		return "房主"
	}
	return "加入者"
}
