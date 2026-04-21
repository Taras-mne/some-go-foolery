package main

import (
	"log/slog"

	"github.com/zalando/go-keyring"
)

const keyringService = "claudy"

func savePassword(username, password string) error {
	if username == "" {
		return nil
	}
	return keyring.Set(keyringService, username, password)
}

func loadPassword(username string) string {
	if username == "" {
		return ""
	}
	pw, err := keyring.Get(keyringService, username)
	if err != nil {
		slog.Debug("keyring lookup failed", "user", username, "err", err)
		return ""
	}
	return pw
}

func deletePassword(username string) {
	if username == "" {
		return
	}
	if err := keyring.Delete(keyringService, username); err != nil {
		slog.Debug("keyring delete failed", "user", username, "err", err)
	}
}
