//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid is running. EPERM means it exists but
// belongs to someone else — still alive.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
