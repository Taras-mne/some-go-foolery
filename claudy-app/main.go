package main

import (
	"embed"
	_ "embed"
	"os"

	"fyne.io/systray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/appicon.png
var trayIcon []byte

func main() {
	app := NewApp()

	var mStatus   *systray.MenuItem
	var mOpen     *systray.MenuItem
	var mAutostart *systray.MenuItem
	var mQuit     *systray.MenuItem

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

		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					app.ShowWindow()
				case <-mAutostart.ClickedCh:
					if mAutostart.Checked() {
						mAutostart.Uncheck()
						app.SetAutostart(false)
					} else {
						mAutostart.Check()
						app.SetAutostart(true)
					}
				case <-mQuit.ClickedCh:
					systray.Quit()
					os.Exit(0)
				}
			}
		}()
	}

	onExit := func() {}

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

	start, _ := systray.RunWithExternalLoop(onReady, onExit)
	start()

	err := wails.Run(&options.App{
		Title:             "Claudy",
		Width:             960,
		Height:            680,
		MinWidth:          760,
		MinHeight:         520,
		HideWindowOnClose: true,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		BackgroundColour: &options.RGBA{R: 15, G: 15, B: 15, A: 1},
		OnStartup:        app.startup,
		OnShutdown:       app.shutdown,
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		println("Error:", err.Error())
	}
}
