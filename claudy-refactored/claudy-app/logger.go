package main

import (
	"log/slog"
	"os"
	"strings"
)

// initLogger wires the default slog logger to stderr.
// Set CLAUDY_LOG=debug to enable verbose output.
func initLogger() {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("CLAUDY_LOG"), "debug") {
		level = slog.LevelDebug
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))
}
