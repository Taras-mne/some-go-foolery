package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

const defaultRelayURL = "http://23.172.217.149"

// Config is persisted to ~/.claudy/config.json.
// Password is intentionally omitted from JSON — it lives in the OS keyring.
type Config struct {
	RelayURL string `json:"relay_url"`
	Username string `json:"username"`
	Password string `json:"-"`
	ShareDir string `json:"share_dir"`
}

// ConfigStore guards Config with a mutex and handles on-disk persistence.
type ConfigStore struct {
	mu   sync.RWMutex
	cfg  Config
	path string
}

func NewConfigStore() *ConfigStore {
	home, _ := os.UserHomeDir()
	s := &ConfigStore{
		path: filepath.Join(home, ".claudy", "config.json"),
		cfg:  Config{RelayURL: defaultRelayURL},
	}
	s.load()
	return s
}

func (s *ConfigStore) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update mutates the config under the write lock and persists the result.
// If fn changes Username, the caller is responsible for keyring updates.
func (s *ConfigStore) Update(fn func(c *Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	return s.persist()
}

func (s *ConfigStore) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		slog.Debug("no config file, using defaults", "path", s.path)
		return
	}
	if err := json.Unmarshal(data, &s.cfg); err != nil {
		slog.Warn("corrupt config file, using defaults", "err", err)
		s.cfg = Config{RelayURL: defaultRelayURL}
		return
	}
	if s.cfg.RelayURL == "" {
		s.cfg.RelayURL = defaultRelayURL
	}

	// Migration: older builds stored the password in plaintext under "password".
	// If present, move it into the keyring and rewrite the file without the field.
	var legacy struct {
		Password string `json:"password"`
	}
	_ = json.Unmarshal(data, &legacy)
	if legacy.Password != "" && s.cfg.Username != "" {
		if err := savePassword(s.cfg.Username, legacy.Password); err != nil {
			slog.Warn("keyring migration failed; password stays in memory only", "err", err)
			s.cfg.Password = legacy.Password
			return
		}
		slog.Info("migrated plaintext password to keyring", "user", s.cfg.Username)
		s.cfg.Password = legacy.Password
		if err := s.persist(); err != nil {
			slog.Warn("failed to rewrite config after migration", "err", err)
		}
		return
	}
	s.cfg.Password = loadPassword(s.cfg.Username)
}

// persist expects the caller to hold s.mu.
func (s *ConfigStore) persist() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(&s.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0600)
}
