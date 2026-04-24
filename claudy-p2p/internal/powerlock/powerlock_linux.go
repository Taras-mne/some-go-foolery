//go:build linux

package powerlock

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
)

type linuxLock struct {
	cmd *exec.Cmd
}

func (l *linuxLock) release() {
	if l.cmd != nil && l.cmd.Process != nil {
		_ = l.cmd.Process.Kill()
		_ = l.cmd.Wait()
	}
}

func (l *linuxLock) name() string { return "systemd-inhibit" }

func acquirePlatform(ctx context.Context, log *slog.Logger) (releaser, error) {
	// systemd-inhibit holds a "block" lock while its child process runs.
	// We give it `sleep infinity` as the child and kill it on Release.
	// If systemd-inhibit isn't available (e.g. minimal container, musl
	// distro without systemd) we return a distinguishable error the
	// caller will log as "prevent-sleep not active" — owner still runs.
	if _, err := exec.LookPath("systemd-inhibit"); err != nil {
		return nil, errors.New("systemd-inhibit not found; sleep prevention disabled")
	}
	cmd := exec.CommandContext(ctx,
		"systemd-inhibit",
		"--what=sleep:idle",
		"--who=claudy",
		"--why=active share",
		"--mode=block",
		"sleep", "infinity",
	)
	if err := cmd.Start(); err != nil {
		return nil, fmtErr("start systemd-inhibit", err)
	}
	return &linuxLock{cmd: cmd}, nil
}
