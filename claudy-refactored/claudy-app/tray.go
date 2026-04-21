package main

import (
	"os"

	"fyne.io/systray"
)

// runTray sets up the tray icon and wires menu events back to the app.
// Returns the systray start function; call it before wails.Run.
func runTray(app *App) func() {
	var (
		mStatus    *systray.MenuItem
		mOpen      *systray.MenuItem
		mAutostart *systray.MenuItem
		mQuit      *systray.MenuItem
	)

	onReady := func() {
		systray.SetIcon(trayIcon)
		systray.SetTooltip("Claudy")

		mStatus = systray.AddMenuItem("⚪  Connecting…", "")
		mStatus.Disable()
		systray.AddSeparator()
		mOpen = systray.AddMenuItem("Open Claudy", "")
		systray.AddSeparator()
		mAutostart = systray.AddMenuItem("Launch at Login", "")
		if isAutostartEnabled() {
			mAutostart.Check()
		}
		systray.AddSeparator()
		mQuit = systray.AddMenuItem("Quit", "")

		go trayEventLoop(app, mOpen, mAutostart, mQuit)
	}

	app.OnStatus = func(connected bool) {
		if mStatus == nil {
			return
		}
		if connected {
			mStatus.SetTitle("🟢  Connected")
		} else {
			mStatus.SetTitle("🔴  Disconnected")
		}
	}

	start, _ := systray.RunWithExternalLoop(onReady, func() {})
	return start
}

func trayEventLoop(app *App, mOpen, mAutostart, mQuit *systray.MenuItem) {
	for {
		select {
		case <-mOpen.ClickedCh:
			app.ShowWindow()
		case <-mAutostart.ClickedCh:
			if mAutostart.Checked() {
				mAutostart.Uncheck()
				_ = app.SetAutostart(false)
			} else {
				mAutostart.Check()
				_ = app.SetAutostart(true)
			}
		case <-mQuit.ClickedCh:
			systray.Quit()
			os.Exit(0)
		}
	}
}
