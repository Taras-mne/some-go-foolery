package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configTestDir points the store at a per-test directory via $HOME.
func configTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestConfigStore_MissingFileDefaults(t *testing.T) {
	configTestDir(t)
	s := NewConfigStore()
	cfg := s.Get()
	if cfg.RelayURL != defaultRelayURL {
		t.Errorf("RelayURL = %q, want default", cfg.RelayURL)
	}
	if cfg.Username != "" || cfg.ShareDir != "" {
		t.Errorf("unexpected non-empty fields: %+v", cfg)
	}
}

func TestConfigStore_UpdatePersists(t *testing.T) {
	home := configTestDir(t)
	s := NewConfigStore()
	err := s.Update(func(c *Config) {
		c.RelayURL = "http://x.example"
		c.Username = "alice"
		c.ShareDir = "/tmp/share"
		c.Password = "secret"
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".claudy", "config.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "secret") {
		t.Errorf("password leaked into file: %s", data)
	}
	var disk map[string]any
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := disk["password"]; ok {
		t.Errorf("password key present in persisted JSON: %v", disk)
	}
	if disk["username"] != "alice" || disk["share_dir"] != "/tmp/share" {
		t.Errorf("unexpected persisted fields: %v", disk)
	}
}

func TestConfigStore_LegacyPlaintextMigration(t *testing.T) {
	home := configTestDir(t)
	// Simulate old-format config with plaintext password.
	dir := filepath.Join(home, ".claudy")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	legacy := `{
  "relay_url": "http://relay.example",
  "username": "alice",
  "password": "legacy-secret",
  "share_dir": "/tmp/share"
}`
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}

	s := NewConfigStore()
	cfg := s.Get()
	if cfg.Username != "alice" || cfg.ShareDir != "/tmp/share" {
		t.Errorf("load lost fields: %+v", cfg)
	}

	// Keyring is unavailable in CI; tolerate either migration-performed
	// (file rewritten without password) or migration-failed (password still
	// in memory, file untouched). Either way, Password must be loaded.
	if cfg.Password != "legacy-secret" {
		t.Errorf("Password not carried into memory: %q", cfg.Password)
	}

	// Load the file again and verify that if keyring succeeded, the file
	// no longer contains the plaintext password.
	data, _ := os.ReadFile(path)
	if savePassword("alice", "legacy-secret") == nil {
		// Keyring works on this machine — migration must have stripped the field.
		if strings.Contains(string(data), "legacy-secret") {
			t.Errorf("keyring available but password still in file: %s", data)
		}
		_ = deletePasswordErr("alice")
	}
}

// deletePasswordErr is a wrapper used only by the test above.
func deletePasswordErr(username string) error {
	deletePassword(username)
	return nil
}

func TestConfigStore_CorruptFileFallsBack(t *testing.T) {
	home := configTestDir(t)
	dir := filepath.Join(home, ".claudy")
	_ = os.MkdirAll(dir, 0700)
	_ = os.WriteFile(filepath.Join(dir, "config.json"), []byte("not json"), 0600)

	s := NewConfigStore()
	cfg := s.Get()
	if cfg.RelayURL != defaultRelayURL {
		t.Errorf("expected default relay on corrupt file, got %q", cfg.RelayURL)
	}
}

func TestConfig_JSONTagStripsPassword(t *testing.T) {
	c := Config{Username: "u", Password: "p"}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "password") || strings.Contains(string(data), "\"p\"") {
		t.Errorf("Password leaked via json.Marshal: %s", data)
	}
}
