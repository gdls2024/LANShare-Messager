//go:build !windows

package main

import (
	"fmt"
	"os/exec"
)

// setDetachedProcess is a no-op on non-Windows platforms.
// On Unix-like systems, child processes survive parent exit by default.
func setDetachedProcess(cmd *exec.Cmd) {}

// launchRestartHelper is not needed on non-Windows — direct restart works fine
// because Unix doesn't have file locking issues or WebView2 child processes.
func launchRestartHelper(targetPath string) error {
	return fmt.Errorf("not supported on this platform")
}
