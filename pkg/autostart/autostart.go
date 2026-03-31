// Package autostart manages launch-on-login for the Claudy daemon.
package autostart

// Manager enables or disables auto-start at login.
type Manager interface {
	Enable(execPath string) error
	Disable() error
	IsEnabled() (bool, error)
}
