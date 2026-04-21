//go:build windows

package main

import "golang.org/x/sys/windows/registry"

const autostartKey = "Claudy"
const autostartRunKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func enableAutostart(execPath string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(autostartKey, execPath)
}

func disableAutostart() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.DeleteValue(autostartKey)
}

func isAutostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartRunKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(autostartKey)
	return err == nil
}
