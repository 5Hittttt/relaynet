package main

import "fyne.io/fyne/v2"

func (u *uiState) playbackSettingsChanged() {
	resolution := max(u.playbackResolutionSel.SelectedIndex(), 0)
	fpsIndex := max(u.playbackFPSSel.SelectedIndex(), 0)
	fps := []int{0, 30, 60, 120}[fpsIndex]
	u.playbackOptions.Store(uint32(resolution<<8 | fps))
	fyne.CurrentApp().Preferences().SetInt("screenPlaybackResolution", resolution)
	fyne.CurrentApp().Preferences().SetInt("screenPlaybackFPS", fps)
	mu.Lock()
	c := client
	mu.Unlock()
	if c != nil {
		c.SetScreenPlayback(byte(resolution), fps)
	}
}

func (u *uiState) refreshPlaybackInfo() {
	mu.Lock()
	c := client
	mu.Unlock()
	if c != nil && u.selectedSharer.Load() != 0 && !c.SupportsScreenPlayback(u.selectedSharer.Load()) {
		u.playbackInfo.SetText("共享端版本较旧，当前按原画播放；升级共享端后可选档位")
	} else {
		u.playbackInfo.SetText("播放档位不超过共享端设置")
	}
}
