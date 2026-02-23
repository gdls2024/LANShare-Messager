//go:build !windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ensureSingleInstance checks that no other LANShare instance is running.
// Returns a cleanup function to call on exit, or exits the process if another instance is found.
// waitSeconds is used on Windows for restart retry; on other platforms it is ignored.
func ensureSingleInstance(waitSeconds int) func() {
	lockPath := filepath.Join(AppDataDir(), "lanshare.lock")

	// Check if lock file exists and process is still alive
	if data, err := os.ReadFile(lockPath); err == nil {
		pidStr := strings.TrimSpace(string(data))
		if pid, err := strconv.Atoi(pidStr); err == nil {
			process, err := os.FindProcess(pid)
			if err == nil {
				// On Unix, FindProcess always succeeds; check if process is alive
				if err := process.Signal(syscall.Signal(0)); err == nil {
					fmt.Println("LANShare 已在运行中")
					os.Exit(0)
				}
			}
		}
	}

	// Write our PID
	lockFile, _ := os.Create(lockPath)
	if lockFile != nil {
		fmt.Fprintf(lockFile, "%d", os.Getpid())
		lockFile.Close()
	}

	return func() {
		os.Remove(lockPath)
	}
}
