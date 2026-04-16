package main

import (
	"log"
	"os"
	"path/filepath"
	"syscall"
)

var lockFile *os.File

func lockFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claudy", "daemon.lock")
}

// acquireLock grabs an exclusive flock on ~/.claudy/daemon.lock.
// If another process holds the lock, we log.Fatal immediately.
func acquireLock() {
	path := lockFilePath()
	os.MkdirAll(filepath.Dir(path), 0700)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		log.Fatalf("[lock] cannot open lock file: %v", err)
	}

	// Non-blocking exclusive lock
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		f.Close()
		log.Fatalf("[lock] another Claudy daemon is already running. Exiting.")
	}

	lockFile = f
}

// releaseLock releases the flock and removes the file.
func releaseLock() {
	if lockFile != nil {
		syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		lockFile.Close()
		os.Remove(lockFilePath())
	}
}
