//go:build linux

package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type linuxManager struct{}

func New() Manager { return &linuxManager{} }

func unitPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user", "claudy-daemon.service")
}

func desktopPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "autostart", "claudy-daemon.desktop")
}

func (m *linuxManager) Enable(execPath string) error {
	// Try systemd user unit first.
	unit := fmt.Sprintf(`[Unit]
Description=Claudy Daemon

[Service]
ExecStart=%s
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, execPath)

	path := unitPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err == nil {
		if err := os.WriteFile(path, []byte(unit), 0644); err == nil {
			if exec.Command("systemctl", "--user", "enable", "--now", "claudy-daemon").Run() == nil {
				return nil
			}
		}
	}

	// Fallback: XDG autostart .desktop entry.
	desktop := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Claudy Daemon
Exec=%s
Hidden=false
X-GNOME-Autostart-enabled=true
`, execPath)
	dp := desktopPath()
	if err := os.MkdirAll(filepath.Dir(dp), 0755); err != nil {
		return err
	}
	return os.WriteFile(dp, []byte(desktop), 0644)
}

func (m *linuxManager) Disable() error {
	_ = exec.Command("systemctl", "--user", "disable", "--now", "claudy-daemon").Run()
	_ = os.Remove(unitPath())
	_ = os.Remove(desktopPath())
	return nil
}

func (m *linuxManager) IsEnabled() (bool, error) {
	if exec.Command("systemctl", "--user", "is-enabled", "claudy-daemon").Run() == nil {
		return true, nil
	}
	_, err := os.Stat(desktopPath())
	return err == nil, nil
}
