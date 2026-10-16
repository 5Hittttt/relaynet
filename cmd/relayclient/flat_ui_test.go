package main

import (
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"relaynet/internal/tunnel"
)

func TestClientUsesSidebarInsteadOfTopTabs(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	u := newUI(a.NewWindow("flat client"))
	root := u.build()
	if _, oldTabs := root.(*container.AppTabs); oldTabs {
		t.Fatal("client still uses the old top tab layout instead of a room sidebar")
	}
}

func TestFlatNavigationKeepsOnePageVisible(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	a.Settings().SetTheme(newFlatTheme())
	w := a.NewWindow("flat navigation")
	w.SetPadded(false)
	u := newUI(w)
	root := u.build()
	w.SetContent(root)
	for index, button := range u.navButtons {
		test.Tap(button)
		if u.selectedPage != index || button.Importance != widget.HighImportance {
			t.Fatalf("navigation %d did not select its page", index)
		}
		for i, page := range u.pages {
			if page.Visible() != (i == index) {
				t.Fatalf("page %d visibility is wrong after selecting %d", i, index)
			}
		}
		if size := root.MinSize(); size.Width > 1024 || size.Height > 720 {
			t.Fatalf("page %d forces an oversized window: %v", index, size)
		}
		w.Resize(fyne.NewSize(1024, 720))
		if u.pageSubtitle.Size().Width < 200 {
			t.Fatal("page description was squeezed into an ellipsis")
		}
	}
	if u.playbackResolutionSel.Selected != "原画" || u.fpsSel.Selected != "120 FPS" {
		t.Fatal("navigation changed playback or sharing settings")
	}
	test.Tap(u.modeJoin)
	if u.isHost || u.hostBox.Visible() || !u.joinBox.Visible() {
		t.Fatal("join controls were lost in the new settings page")
	}
	test.Tap(u.protoTCP)
	if u.proto != "tcp" {
		t.Fatal("protocol selection no longer works")
	}
	settingsScroll := u.pages[4].(*container.Scroll)
	settingsScroll.ScrollToBottom()
	if settingsScroll.Offset.Y == 0 {
		t.Fatal("could not reproduce scrolled room settings")
	}
	u.serverEntry.SetText("")
	test.Tap(u.btnConnect)
	if u.selectedPage != 4 || u.errLabel.Text == "" {
		t.Fatal("invalid connection no longer reveals its error in room settings")
	}
	if settingsScroll.Offset.Y != 0 {
		t.Fatal("connection error remains above the visible settings viewport")
	}
	u.showFPSCheck.SetChecked(false)
	u.refresh()
	if u.fpsOverlay.Visible() {
		t.Fatal("hidden FPS display left an empty black badge")
	}
	u.showFPSCheck.SetChecked(true)
	u.refresh()
	if !u.fpsOverlay.Visible() {
		t.Fatal("FPS toggle no longer displays its overlay")
	}
}

func TestFlatThemeIsDarkAndHasNoShadow(t *testing.T) {
	th := newFlatTheme()
	for _, variant := range []fyne.ThemeVariant{theme.VariantDark, theme.VariantLight} {
		if got := color.NRGBAModel.Convert(th.Color(theme.ColorNameBackground, variant)); got != flatBackground {
			t.Fatal("system light mode changed the requested dark palette")
		}
		_, _, _, alpha := th.Color(theme.ColorNameShadow, variant).RGBA()
		if alpha != 0 {
			t.Fatal("flat theme still draws shadows")
		}
	}
}

func TestFlatMemberRowRetainsIDWithHostname(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	row := newMemberItem(tunnel.Member{ID: 17, Name: "Player", Hostname: "DESKTOP"}, nil)
	found := false
	var inspect func(fyne.CanvasObject)
	inspect = func(obj fyne.CanvasObject) {
		switch value := obj.(type) {
		case *widget.Label:
			if strings.HasPrefix(value.Text, "#17") && strings.Contains(value.Text, "DESKTOP") {
				found = true
			}
		case *fyne.Container:
			for _, child := range value.Objects {
				inspect(child)
			}
		}
	}
	inspect(row.content)
	if !found {
		t.Fatal("member detail lost its stable ID when a hostname was present")
	}
}

func TestFlatRoomRoleUsesConnectedSession(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	u := newUI(a.NewWindow("connected role"))
	u.build()
	u.isHost = false // Settings can change while the existing session stays host.
	st := tunnel.Status{Connected: true, RoomID: "game1", IsHost: true}
	u.updateRoomChrome(st)
	if !strings.Contains(u.roomConnectionText.Text, roleStr(st.IsHost)) {
		t.Fatal("connected role follows draft settings instead of the real session")
	}
}

// Optional captures show the real widgets and layouts, without starting media.
func TestFlatClientSnapshots(t *testing.T) {
	dir := os.Getenv("RELAYNET_UI_SNAPSHOTS")
	if dir == "" {
		t.Skip("set RELAYNET_UI_SNAPSHOTS to save visual previews")
	}
	a := test.NewApp()
	defer a.Quit()
	a.Settings().SetTheme(newFlatTheme())
	w := a.NewWindow("RelayNet")
	w.SetPadded(false)
	u := newUI(w)
	w.SetContent(u.build())
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for index, name := range []string{"chat", "media", "screen", "logs", "settings"} {
		test.Tap(u.navButtons[index])
		w.Resize(fyne.NewSize(1280, 800))
		f, err := os.Create(filepath.Join(dir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		err = png.Encode(f, w.Canvas().Capture())
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("save %s: %v / %v", name, err, closeErr)
		}
	}
}
