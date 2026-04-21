package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gorilla/websocket"
)

const (
	daemonAuthRetry    = 10 * time.Second
	daemonMinBackoff   = 1 * time.Second
	daemonMaxBackoff   = 30 * time.Second
	daemonPauseOnClose = 1 * time.Second
)

// runDaemon is the main daemon lifecycle: authenticate, open a relay tunnel,
// serve WebDAV over it, reconnect on failure. Exits on ctx cancellation.
func (a *App) runDaemon(ctx context.Context) {
	slog.Info("daemon starting")
	defer slog.Info("daemon stopped")
	defer a.setStatus(false, false)

	for {
		a.setStatus(false, false)
		cfg := a.store.Get()

		token, err := relayLogin(cfg.RelayURL, cfg.Username, cfg.Password)
		if err != nil {
			slog.Warn("relay login failed", "err", err)
			if sleepOrDone(ctx, daemonAuthRetry) {
				return
			}
			continue
		}

		wsURL, err := buildWSURL(cfg.RelayURL, token)
		if err != nil {
			slog.Warn("bad relay url", "err", err)
			if sleepOrDone(ctx, daemonAuthRetry) {
				return
			}
			continue
		}

		davPrefix := "/dav/" + cfg.Username
		dav := newDynamicDAV(davPrefix, cfg.ShareDir)

		a.setStatus(true, false)
		if done := a.reconnectLoop(ctx, wsURL, dav); done {
			return
		}
	}
}

// reconnectLoop dials the tunnel and serves it until disconnect, then retries
// with exponential backoff. Returns true if ctx was cancelled.
func (a *App) reconnectLoop(ctx context.Context, wsURL string, dav *dynamicDAV) bool {
	backoff := daemonMinBackoff
	for {
		ws, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return true
			}
			slog.Warn("tunnel dial failed", "err", err, "backoff", backoff)
			a.setStatus(true, false)
			if sleepOrDone(ctx, backoff) {
				return true
			}
			backoff = min(backoff*2, daemonMaxBackoff)
			continue
		}
		backoff = daemonMinBackoff
		slog.Info("tunnel connected")
		a.setStatus(true, true)

		serveTunnel(ctx, ws, dav)

		a.setStatus(true, false)
		slog.Info("tunnel disconnected")
		if sleepOrDone(ctx, daemonPauseOnClose) {
			return true
		}
	}
}

// sleepOrDone returns true if ctx finishes before d elapses.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}
