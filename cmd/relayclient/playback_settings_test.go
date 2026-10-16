package main

import (
	"fyne.io/fyne/v2/test"
	"testing"
)

func TestPlaybackSelectorsPublishIndependentSettings(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	u := newUI(a.NewWindow("playback settings"))
	u.build()
	u.playbackResolutionSel.SetSelected("720p")
	u.playbackFPSSel.SetSelected("30 FPS")
	if u.playbackOptions.Load() != 1<<8|30 {
		t.Fatal("playback controls did not publish requested profile")
	}
	if u.qualitySel.Selected != "720p" || u.fpsSel.Selected != "120 FPS" {
		t.Fatal("viewer controls changed sharing settings")
	}
	u.playbackResolutionSel.SetSelected("2160p")
	u.playbackFPSSel.SetSelected("120 FPS")
	if u.playbackOptions.Load() != 4<<8|120 {
		t.Fatal("higher playback settings were not published")
	}
	if a.Preferences().Int("screenPlaybackResolution") != 4 || a.Preferences().Int("screenPlaybackFPS") != 120 {
		t.Fatal("playback settings are not retained")
	}
}
