package main

import (
	"log/slog"
	"os/exec"
	"runtime"
)

// openInOS launches the OS handler for the given URL or path.
func openInOS(target string) {
	if target == "" {
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		slog.Warn("open failed", "target", target, "err", err)
	}
}
