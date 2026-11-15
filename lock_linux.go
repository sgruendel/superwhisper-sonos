package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func lockState(statePath string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		return nil, err
	}
	// The lock sits next to the journal: recovery.json -> recovery.lock.
	lockPath := strings.TrimSuffix(statePath, filepath.Ext(statePath)) + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another bridge may be using %s: %w", lockPath, err)
	}
	return f, nil
}
