//go:build windows

package main

import "os"

// processAlive reports whether pid is running: on Windows FindProcess opens
// a handle and fails when the process is gone.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
