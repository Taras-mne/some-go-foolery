package main

import (
	"context"
	"log/slog"
	"sync"
)

// App is the root object bound to the Wails frontend.
// Exported methods (Login, Register, …) live in handlers.go.
type App struct {
	ctx   context.Context
	store *ConfigStore

	mu           sync.RWMutex
	running      bool
	connected    bool
	cancelDaemon context.CancelFunc // cancels the currently running daemon goroutine

	// OnStatus is invoked when the connection state changes; set by the tray.
	OnStatus func(connected bool)
}

func NewApp() *App {
	return &App{store: NewConfigStore()}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	if err := acquireLock(); err != nil {
		slog.Error("single-instance check failed", "err", err)
		showFatalDialog(ctx, err.Error())
		return
	}

	cfg := a.store.Get()
	if cfg.Username != "" && cfg.Password != "" && cfg.ShareDir != "" {
		a.restartDaemon()
	}
}

func (a *App) shutdown(_ context.Context) {
	a.stopDaemon()
	releaseLock()
}

func (a *App) setStatus(running, connected bool) {
	a.mu.Lock()
	a.running = running
	a.connected = connected
	cb := a.OnStatus
	a.mu.Unlock()
	if cb != nil {
		cb(connected)
	}
}

// stopDaemon cancels the current daemon goroutine (if any).
func (a *App) stopDaemon() {
	a.mu.Lock()
	cancel := a.cancelDaemon
	a.cancelDaemon = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// restartDaemon replaces any running daemon with a fresh one. Safe to call
// concurrently: each goroutine owns its own context so there's no channel-swap race.
func (a *App) restartDaemon() {
	cfg := a.store.Get()
	ready := cfg.Username != "" && cfg.Password != "" && cfg.ShareDir != ""

	a.mu.Lock()
	if a.cancelDaemon != nil {
		a.cancelDaemon()
		a.cancelDaemon = nil
	}
	if !ready || a.ctx == nil {
		a.mu.Unlock()
		a.setStatus(false, false)
		return
	}
	dctx, cancel := context.WithCancel(a.ctx)
	a.cancelDaemon = cancel
	a.mu.Unlock()

	go a.runDaemon(dctx)
}
