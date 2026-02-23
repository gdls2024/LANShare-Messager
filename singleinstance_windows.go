//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex = kernel32.NewProc("CreateMutexW")
	user32          = syscall.NewLazyDLL("user32.dll")
	procFindWindow  = user32.NewProc("FindWindowW")
	procSetFGWindow = user32.NewProc("SetForegroundWindow")
	procShowWindow  = user32.NewProc("ShowWindow")
)

// ensureSingleInstance checks that no other LANShare instance is running.
// Returns a cleanup function to call on exit, or exits the process if another instance is found.
// When waitSeconds > 0 (restart scenario), retries the mutex check for up to that many seconds
// instead of exiting immediately, giving the old process time to release it.
func ensureSingleInstance(waitSeconds int) func() {
	mutexName, _ := syscall.UTF16PtrFromString("Global\\LANShare_SingleInstance")

	handle, _, err := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(mutexName)))
	if handle == 0 {
		fmt.Println("创建互斥锁失败")
		os.Exit(1)
	}

	if err == syscall.ERROR_ALREADY_EXISTS {
		if waitSeconds <= 0 {
			// Normal launch — another instance is running, bring it to front and exit
			fmt.Println("LANShare 已在运行中")
			bringExistingWindowToFront()
			os.Exit(0)
		}

		// Restart scenario — old process may still be shutting down.
		// Close the failed handle and retry with polling.
		syscall.CloseHandle(syscall.Handle(handle))
		handle = 0

		deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(500 * time.Millisecond)
			h, _, e := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(mutexName)))
			if h == 0 {
				continue
			}
			if e != syscall.ERROR_ALREADY_EXISTS {
				// Successfully acquired mutex
				handle = h
				fmt.Printf("互斥锁获取成功（等待了 %v）\n", time.Since(deadline.Add(-time.Duration(waitSeconds)*time.Second)))
				break
			}
			// Still held — close and retry
			syscall.CloseHandle(syscall.Handle(h))
		}

		if handle == 0 {
			fmt.Println("等待旧进程退出超时，无法获取互斥锁")
			os.Exit(1)
		}
	}

	// Also create a lock file as a secondary indicator
	lockPath := filepath.Join(AppDataDir(), "lanshare.lock")
	lockFile, _ := os.Create(lockPath)
	if lockFile != nil {
		fmt.Fprintf(lockFile, "%d", os.Getpid())
	}

	return func() {
		syscall.CloseHandle(syscall.Handle(handle))
		if lockFile != nil {
			lockFile.Close()
		}
		os.Remove(lockPath)
	}
}

func bringExistingWindowToFront() {
	title, _ := syscall.UTF16PtrFromString("LS Messager")
	hwnd, _, _ := procFindWindow.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd != 0 {
		const SW_RESTORE = 9
		procShowWindow.Call(hwnd, SW_RESTORE)
		procSetFGWindow.Call(hwnd)
	}
}
