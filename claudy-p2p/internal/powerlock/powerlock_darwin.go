//go:build darwin

package powerlock

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
)

type darwinLock struct {
	cmd *exec.Cmd
}

func (d *darwinLock) release() {
	if d.cmd != nil && d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
		_ = d.cmd.Wait()
	}
}

func (d *darwinLock) name() string { return "caffeinate" }

func acquirePlatform(ctx context.Context, log *slog.Logger) (releaser, error) {
	// `caffeinate -i` prevents idle sleep. `-w <pid>` makes caffeinate
	// terminate automatically when our claudy process dies, so a crash
	// does not leave the user's Mac wide-awake forever. Cheap, robust,
	// and avoids dragging cgo into the build just for one IOKit call.
	cmd := exec.CommandContext(ctx, "/usr/bin/caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, fmtErr("start caffeinate", err)
	}
	return &darwinLock{cmd: cmd}, nil
}
