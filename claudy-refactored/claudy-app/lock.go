package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

var lockFile *os.File

func lockFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claudy", "daemon.lock")
}

// acquireLock takes an exclusive flock on ~/.claudy/daemon.lock.
// Returns an error (instead of exiting) when another instance holds it,
// so the GUI can surface a dialog.
func acquireLock() error {
	path := lockFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return errors.New("another Claudy instance is already running")
	}
	lockFile = f
	return nil
}

func releaseLock() {
	if lockFile == nil {
		return
	}
	_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	_ = lockFile.Close()
	_ = os.Remove(lockFilePath())
	lockFile = nil
}
