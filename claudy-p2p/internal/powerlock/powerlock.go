// Package powerlock keeps the host awake while a Claudy owner process
// is active. A sleeping laptop tears down the WebDAV server, kills
// DataChannels, and the viewer sees EOF at the worst possible moment
// (mid-transfer, Finder showing "disconnected"). There is no way to
// serve files while the CPU is halted — sleep must be blocked.
//
// Semantics:
//   - Acquire() registers a "system-required" lock with the OS. While
//     held, the machine will not idle-sleep. Display can still sleep
//     (we don't care about screen), user can still explicitly sleep
//     from the menu (we don't override manual intent).
//   - Release() drops the lock; OS returns to its normal sleep policy.
//   - Acquire is idempotent; Release after a failed Acquire is a no-op.
//
// Platform notes:
//   - Windows: SetThreadExecutionState(ES_CONTINUOUS|ES_SYSTEM_REQUIRED).
//     Must be called on an OS-locked thread; otherwise the Go runtime
//     can migrate the goroutine and the "continuous" flag becomes a
//     one-shot. We dedicate a goroutine with runtime.LockOSThread.
//   - macOS: we shell out to /usr/bin/caffeinate -is -w <pid>. The -w
//     makes caffeinate exit as soon as the claudy process dies, so
//     there's no dangling lock after a crash. Much simpler than calling
//     IOPMAssertion via cgo, and equally effective.
//   - Linux: systemd-inhibit --what=sleep --mode=block sleep infinity,
//     auto-killed by the wrapper goroutine when the context is cancelled.
//     Falls back to a no-op on systems without systemd (returning nil
//     so we don't fail the owner on minimal containers).
package powerlock

import (
	"context"
	"fmt"
	"log/slog"
)

// Lock is a handle representing an active "prevent sleep" request.
// Release is safe to call on a nil or zero-value Lock.
type Lock struct {
	cancel context.CancelFunc
	log    *slog.Logger
	impl   releaser
}

// releaser is the platform-specific tear-down for an acquired lock.
type releaser interface {
	release()
	name() string
}

// Acquire returns a Lock that prevents the host from idle-sleeping
// until Release is called. Failures (unsupported OS, missing helper
// binary) are logged and a no-op Lock is returned; the caller always
// gets something Release-able and does not need to handle errors,
// because a best-effort prevent-sleep should never be the reason the
// owner refuses to start.
func Acquire(log *slog.Logger) *Lock {
	ctx, cancel := context.WithCancel(context.Background())
	impl, err := acquirePlatform(ctx, log)
	if err != nil {
		log.Warn("prevent-sleep not active", "reason", err)
		cancel()
		return &Lock{cancel: cancel, log: log}
	}
	log.Info("prevent-sleep acquired", "backend", impl.name())
	return &Lock{cancel: cancel, log: log, impl: impl}
}

// Release drops the sleep-prevention lock. Idempotent.
func (l *Lock) Release() {
	if l == nil {
		return
	}
	if l.impl != nil {
		l.impl.release()
	}
	if l.cancel != nil {
		l.cancel()
	}
	if l.log != nil && l.impl != nil {
		l.log.Info("prevent-sleep released", "backend", l.impl.name())
	}
}

// fmtErr is a tiny helper for platform files that need to wrap a
// syscall errno cleanly without importing fmt everywhere.
func fmtErr(msg string, err error) error { return fmt.Errorf("%s: %w", msg, err) }
