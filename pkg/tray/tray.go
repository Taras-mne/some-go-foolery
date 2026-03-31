// Package tray manages the system tray icon and menu for the Claudy daemon.
package tray

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/claudy-app/claudy-core/pkg/assets"
	"github.com/getlantern/systray"
)

// Status represents the daemon connection state.
type Status int

const (
	StatusDisconnected Status = iota
	StatusConnecting
	StatusConnected
)

// Bus is a non-blocking status channel between the connection loop and tray.
type Bus struct {
	ch chan Status
}

func NewBus() *Bus { return &Bus{ch: make(chan Status, 8)} }

func (b *Bus) Send(s Status) {
	select {
	case b.ch <- s:
	default:
	}
}

func (b *Bus) C() <-chan Status { return b.ch }

// Options configures the tray.
type Options struct {
	RelayURL    string
	ShareDir    string
	Username    string
	Bus         *Bus
	OnQuit      func()
	OnFolder    func() string       // returns new folder or "" if cancelled
	OnAutostart func(bool) error    // enable/disable autostart
	AutostartOn bool                // initial checked state
}

// Run starts the systray event loop — must be called on the main goroutine.
func Run(opts Options) {
	systray.Run(func() { onReady(opts) }, opts.OnQuit)
}

func onReady(opts Options) {
	systray.SetIcon(assets.Icon)
	systray.SetTooltip("Claudy")

	mStatus := systray.AddMenuItem("Claudy: Connecting…", "Connection status")
	mStatus.Disable()

	systray.AddSeparator()

	folder := opts.ShareDir
	if len(folder) > 35 {
		folder = "…" + folder[len(folder)-33:]
	}
	mFolder := systray.AddMenuItem("📁 "+folder, opts.ShareDir)
	mFolder.Disable()

	mOpenWeb := systray.AddMenuItem("Open Web UI", "Open drive in browser")
	mChange := systray.AddMenuItem("Change Folder…", "Select a different folder to share")

	systray.AddSeparator()

	mAutostart := systray.AddMenuItemCheckbox("Launch on Login", "Start automatically when you log in", opts.AutostartOn)

	systray.AddSeparator()

	mQuit := systray.AddMenuItem("Quit Claudy", "Stop daemon and quit")

	go func() {
		for {
			select {
			case s := <-opts.Bus.C():
				switch s {
				case StatusConnected:
					mStatus.SetTitle(fmt.Sprintf("● Connected as %s", opts.Username))
				case StatusDisconnected:
					mStatus.SetTitle("○ Disconnected — reconnecting…")
				case StatusConnecting:
					mStatus.SetTitle("◌ Connecting…")
				}

			case <-mOpenWeb.ClickedCh:
				openBrowser(opts.RelayURL)

			case <-mChange.ClickedCh:
				if dir := opts.OnFolder(); dir != "" {
					opts.ShareDir = dir
					short := dir
					if len(short) > 35 {
						short = "…" + short[len(short)-33:]
					}
					mFolder.SetTitle("📁 " + short)
					mFolder.SetTooltip(dir)
				}

			case <-mAutostart.ClickedCh:
				enable := !mAutostart.Checked()
				if opts.OnAutostart != nil {
					if err := opts.OnAutostart(enable); err == nil {
						if enable {
							mAutostart.Check()
						} else {
							mAutostart.Uncheck()
						}
					}
				}

			case <-mQuit.ClickedCh:
				systray.Quit()
			}
		}
	}()
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
