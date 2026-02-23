//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Windows process creation flags not in syscall package.
const (
	createBreakawayFromJob = 0x01000000
	detachedProcess        = 0x00000008
)

// setDetachedProcess configures the command to start as a fully detached process on Windows.
// CREATE_NEW_PROCESS_GROUP: new Ctrl+C group
// CREATE_BREAKAWAY_FROM_JOB: escape parent's implicit job object (prevents kill-on-parent-exit)
// DETACHED_PROCESS: no inherited console
func setDetachedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createBreakawayFromJob | detachedProcess,
	}
}

// launchRestartHelper creates a batch script that waits for the current process
// to fully terminate, cleans up the .old file, then starts the new exe.
// This is more reliable than direct child process launch on Windows because
// WebView2 may spawn child processes that keep the mutex alive after os.Exit.
func launchRestartHelper(targetPath string) error {
	oldPath := targetPath + ".old"
	scriptPath := filepath.Join(filepath.Dir(targetPath), "_lanshare_restart.bat")
	pid := os.Getpid()

	script := fmt.Sprintf("@echo off\r\n"+
		"REM LANShare restart helper\r\n"+
		":wait\r\n"+
		"tasklist /FI \"PID eq %d\" 2>NUL | find /I \"%d\" >NUL\r\n"+
		"if not errorlevel 1 (\r\n"+
		"    timeout /t 1 /nobreak >NUL\r\n"+
		"    goto wait\r\n"+
		")\r\n"+
		"timeout /t 2 /nobreak >NUL\r\n"+
		"if exist \"%s\" del /F /Q \"%s\"\r\n"+
		"start \"\" \"%s\"\r\n"+
		"del \"%%~f0\"\r\n",
		pid, pid, oldPath, oldPath, targetPath)

	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		return fmt.Errorf("创建重启脚本失败: %v", err)
	}

	cmd := exec.Command("cmd", "/C", scriptPath)
	cmd.Dir = filepath.Dir(targetPath)
	setDetachedProcess(cmd)
	if err := cmd.Start(); err != nil {
		os.Remove(scriptPath)
		return fmt.Errorf("启动重启脚本失败: %v", err)
	}

	Log.Info("已启动重启脚本", "script", scriptPath, "waitingForPID", pid)
	return nil
}
