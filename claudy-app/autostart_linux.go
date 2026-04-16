//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func desktopPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "autostart", "claudy.desktop")
}

func enableAutostart(execPath string) error {
	desktop := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=Claudy
Exec=%s
Hidden=false
X-GNOME-Autostart-enabled=true
`, execPath)
	dp := desktopPath()
	os.MkdirAll(filepath.Dir(dp), 0755)
	return os.WriteFile(dp, []byte(desktop), 0644)
}

func disableAutostart() error {
	return os.Remove(desktopPath())
}

func isAutostartEnabled() bool {
	_, err := os.Stat(desktopPath())
	return err == nil
}

var _ = exec.Command
