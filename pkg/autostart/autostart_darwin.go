//go:build darwin

package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const label = "app.claudy.daemon"

type darwinManager struct{}

func New() Manager { return &darwinManager{} }

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
}

func (m *darwinManager) Enable(execPath string) error {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array><string>%s</string></array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><false/>
	<key>StandardOutPath</key><string>/tmp/claudy-daemon.log</string>
	<key>StandardErrorPath</key><string>/tmp/claudy-daemon.log</string>
</dict>
</plist>
`, label, execPath)

	path := plistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plist), 0644); err != nil {
		return err
	}
	return exec.Command("launchctl", "load", "-w", path).Run()
}

func (m *darwinManager) Disable() error {
	path := plistPath()
	_ = exec.Command("launchctl", "unload", "-w", path).Run()
	return os.Remove(path)
}

func (m *darwinManager) IsEnabled() (bool, error) {
	_, err := os.Stat(plistPath())
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}
