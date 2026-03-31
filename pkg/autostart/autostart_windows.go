//go:build windows

package autostart

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type windowsManager struct{}

func New() Manager { return &windowsManager{} }

const taskName = `Claudy\Daemon`

func (m *windowsManager) Enable(execPath string) error {
	xml := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers>
  <Actions Context="Author">
    <Exec><Command>%s</Command></Exec>
  </Actions>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
  </Settings>
</Task>`, execPath)

	tmpFile := filepath.Join(os.TempDir(), "claudy-task.xml")
	if err := os.WriteFile(tmpFile, []byte(xml), 0644); err != nil {
		return err
	}
	defer os.Remove(tmpFile)
	return exec.Command("schtasks", "/Create", "/XML", tmpFile, "/TN", taskName, "/F").Run()
}

func (m *windowsManager) Disable() error {
	return exec.Command("schtasks", "/Delete", "/TN", taskName, "/F").Run()
}

func (m *windowsManager) IsEnabled() (bool, error) {
	err := exec.Command("schtasks", "/Query", "/TN", taskName).Run()
	return err == nil, nil
}
