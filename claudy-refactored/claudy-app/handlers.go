package main

import (
	"fmt"
	"log/slog"
	"os"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// DaemonStatus is returned to the frontend.
type DaemonStatus struct {
	Running   bool   `json:"running"`
	Connected bool   `json:"connected"`
	Username  string `json:"username"`
	ShareDir  string `json:"share_dir"`
	RelayURL  string `json:"relay_url"`
}

// GetConfig returns a copy of the config with the password stripped — the
// password must never reach the frontend.
func (a *App) GetConfig() Config {
	cfg := a.store.Get()
	cfg.Password = ""
	return cfg
}

func (a *App) GetStatus() DaemonStatus {
	a.mu.RLock()
	running, connected := a.running, a.connected
	a.mu.RUnlock()
	cfg := a.store.Get()
	return DaemonStatus{
		Running:   running,
		Connected: connected,
		Username:  cfg.Username,
		ShareDir:  cfg.ShareDir,
		RelayURL:  cfg.RelayURL,
	}
}

func (a *App) Login(relayURL, username, password string) (string, error) {
	relayURL = normalizeRelayURL(relayURL)
	token, err := relayLogin(relayURL, username, password)
	if err != nil {
		return "", err
	}
	if err := savePassword(username, password); err != nil {
		return "", fmt.Errorf("cannot save credentials to keyring: %w", err)
	}
	if err := a.store.Update(func(c *Config) {
		c.RelayURL = relayURL
		c.Username = username
		c.Password = password
	}); err != nil {
		return "", fmt.Errorf("cannot persist config: %w", err)
	}
	slog.Info("login succeeded", "user", username)
	a.restartDaemon()
	return token, nil
}

func (a *App) Register(relayURL, username, password string) error {
	return relayRegister(normalizeRelayURL(relayURL), username, password)
}

func (a *App) PickFolder() string {
	dir, err := wailsruntime.OpenDirectoryDialog(a.ctx, wailsruntime.OpenDialogOptions{
		Title: "Choose a folder to share with Claudy",
	})
	if err != nil || dir == "" {
		return ""
	}
	if err := a.setShareDir(dir); err != nil {
		slog.Warn("pick folder failed", "err", err)
		return ""
	}
	return dir
}

func (a *App) SetShareDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("invalid directory: %s", dir)
	}
	return a.setShareDir(dir)
}

func (a *App) setShareDir(dir string) error {
	if err := a.store.Update(func(c *Config) { c.ShareDir = dir }); err != nil {
		return err
	}
	a.restartDaemon()
	return nil
}

func (a *App) Logout() {
	user := a.store.Get().Username
	deletePassword(user)
	_ = a.store.Update(func(c *Config) {
		c.Username = ""
		c.Password = ""
	})
	a.stopDaemon()
	a.setStatus(false, false)
	slog.Info("logout", "user", user)
}

func (a *App) OpenWebUI() {
	openInOS(a.store.Get().RelayURL)
}

func (a *App) OpenFolder() {
	if dir := a.store.Get().ShareDir; dir != "" {
		openInOS(dir)
	}
}

func (a *App) ShowWindow() {
	wailsruntime.WindowShow(a.ctx)
}

func (a *App) GetAutostart() bool {
	return isAutostartEnabled()
}

func (a *App) SetAutostart(enable bool) error {
	execPath, err := os.Executable()
	if err != nil {
		return err
	}
	if enable {
		return enableAutostart(execPath)
	}
	return disableAutostart()
}
