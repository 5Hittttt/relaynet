package main

import (
	"testing"

	"fyne.io/fyne/v2/test"
	"relaynet/internal/tunnel"
)

func TestScreenSourceCanBeSelectedAfterReset(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	mu.Lock()
	previous := client
	mu.Unlock()
	defer func() { mu.Lock(); client = previous; mu.Unlock() }()
	for _, name := range []string{"disconnect refresh", "disconnect screen tab", "source temporarily absent"} {
		t.Run(name, func(t *testing.T) {
			u := newUI(a.NewWindow("selection regression"))
			u.build()
			u.currentSharers = []uint32{17}
			u.screenSharersList.Length = func() int { return len(u.currentSharers) }
			u.screenSharersList.Select(0)
			if u.selectedSharer.Load() != 17 {
				t.Fatal("initial list click did not select source")
			}
			mu.Lock()
			client = nil
			if name == "source temporarily absent" {
				client = tunnel.New(tunnel.Config{}, func(string, ...interface{}) {})
			}
			mu.Unlock()
			if name == "disconnect refresh" {
				u.refresh()
			} else {
				u.refreshScreenTab()
			}
			if u.selectedSharer.Load() != 0 {
				t.Fatal("reset did not clear viewing source")
			}
			// Reappearing in the same list row must be clickable again.
			u.currentSharers = []uint32{17}
			u.screenSharersList.Length = func() int { return len(u.currentSharers) }
			u.screenSharersList.Refresh()
			u.screenSharersList.Select(0)
			if u.selectedSharer.Load() != 17 {
				t.Fatal("row stays highlighted but no source is selected; clicking it leaves 0 FPS")
			}
		})
	}
}
