package main

import "github.com/zalando/go-keyring"

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
		return ""
	}
	return pw
}

func deletePassword(username string) {
	if username == "" {
		return
	}
	_ = keyring.Delete(keyringService, username)
}
