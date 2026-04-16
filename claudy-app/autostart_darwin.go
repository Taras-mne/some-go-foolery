//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const autostartLabel = "app.claudy"

func autostartPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", autostartLabel+".plist")
}

func enableAutostart(execPath string) error {
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array><string>%s</string></array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><false/>
	<key>StandardOutPath</key><string>/tmp/claudy.log</string>
	<key>StandardErrorPath</key><string>/tmp/claudy.log</string>
</dict>
</plist>
`, autostartLabel, execPath)

	path := autostartPath()
	os.MkdirAll(filepath.Dir(path), 0755)
	if err := os.WriteFile(path, []byte(plist), 0644); err != nil {
		return err
	}
	return exec.Command("launchctl", "load", "-w", path).Run()
}

func disableAutostart() error {
	path := autostartPath()
	exec.Command("launchctl", "unload", "-w", path).Run()
	return os.Remove(path)
}

func isAutostartEnabled() bool {
	_, err := os.Stat(autostartPath())
	return err == nil
}
