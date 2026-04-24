//go:build windows

package powerlock

import (
	"context"
	"log/slog"
	"runtime"
	"syscall"
)

// SetThreadExecutionState flags (winbase.h).
const (
	esSystemRequired = 0x00000001
	esContinuous     = 0x80000000
)

var (
	kernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procSetThreadExecutionState = kernel32.NewProc("SetThreadExecutionState")
)

type windowsLock struct {
	// threadDone closes when the locking goroutine exits. We need a
	// dedicated goroutine because ES_CONTINUOUS is per-thread state in
	// Windows' API, and if the Go runtime schedules the caller onto a
	// different OS thread the flag silently stops applying.
	threadDone chan struct{}
	stop       chan struct{}
}

func (w *windowsLock) release() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.threadDone
}

func (w *windowsLock) name() string { return "SetThreadExecutionState" }

func acquirePlatform(ctx context.Context, log *slog.Logger) (releaser, error) {
	w := &windowsLock{
		threadDone: make(chan struct{}),
		stop:       make(chan struct{}),
	}

	ready := make(chan error, 1)
	go func() {
		// Critical: this goroutine is pinned to the same OS thread for
		// its entire life, so every SetThreadExecutionState call targets
		// the same kernel thread.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(w.threadDone)

		r, _, err := procSetThreadExecutionState.Call(
			uintptr(esContinuous | esSystemRequired),
		)
		if r == 0 {
			ready <- fmtErr("SetThreadExecutionState(SYSTEM_REQUIRED)", err)
			return
		}
		ready <- nil

		// Hold the flag until stop or parent context cancel. The actual
		// "keep awake" is the flag itself; this select just parks the
		// thread so it stays pinned.
		select {
		case <-w.stop:
		case <-ctx.Done():
		}

		// Drop the flag: ES_CONTINUOUS alone, without SYSTEM_REQUIRED,
		// clears the request and lets Windows return to normal policy.
		procSetThreadExecutionState.Call(uintptr(esContinuous))
	}()

	if err := <-ready; err != nil {
		return nil, err
	}
	return w, nil
}
