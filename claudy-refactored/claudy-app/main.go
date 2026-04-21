package main

import (
	"context"
	"embed"
	"log/slog"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/appicon.png
var trayIcon []byte

func main() {
	initLogger()

	app := NewApp()

	start := runTray(app)
	start()

	err := wails.Run(&options.App{
		Title:             "Claudy",
		Width:             960,
		Height:            680,
		MinWidth:          760,
		MinHeight:         520,
		HideWindowOnClose: true,
		AssetServer:       &assetserver.Options{Assets: assets},
		BackgroundColour:  &options.RGBA{R: 15, G: 15, B: 15, A: 1},
		OnStartup:         app.startup,
		OnShutdown:        app.shutdown,
		Bind:              []interface{}{app},
	})
	if err != nil {
		slog.Error("wails exited with error", "err", err)
	}
}

// showFatalDialog surfaces a fatal error to the user before Claudy gives up.
// Used by App.startup when the single-instance check fails.
func showFatalDialog(ctx context.Context, msg string) {
	_, _ = wailsruntime.MessageDialog(ctx, wailsruntime.MessageDialogOptions{
		Type:    wailsruntime.ErrorDialog,
		Title:   "Claudy",
		Message: msg,
	})
	wailsruntime.Quit(ctx)
}
