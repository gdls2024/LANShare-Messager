//go:build windows

package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed build/windows/wisetalk-icon.ico
var wisetalkIconICO []byte

var (
	pSendMessageW = user32dll.NewProc("SendMessageW")
	pLoadImageW   = user32dll.NewProc("LoadImageW")
	pDestroyIcon  = user32dll.NewProc("DestroyIcon")
)

const (
	wmSetIcon      = 0x0080
	iconBig        = 1
	iconSmall      = 0
	imageIcon      = 1
	lrLoadFromFile = 0x0010
)

var (
	currentBigIcon   uintptr
	currentSmallIcon uintptr
	cachedHwnd       uintptr // cached main window handle
)

// findMainHwnd locates the main Wails window by trying known titles.
func findMainHwnd() uintptr {
	if cachedHwnd != 0 {
		// Verify the cached handle is still valid
		if ret, _, _ := pIsWindow.Call(cachedHwnd); ret != 0 {
			return cachedHwnd
		}
		cachedHwnd = 0
	}
	// Try "LS Messager" first, then empty title
	for _, t := range []string{"LS Messager", ""} {
		title, _ := windows.UTF16PtrFromString(t)
		hwnd, _, _ := pFindWindowW.Call(0, uintptr(unsafe.Pointer(title)))
		if hwnd != 0 {
			// Verify it belongs to our process
			var pid uint32
			pGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
			if pid == uint32(os.Getpid()) {
				cachedHwnd = hwnd
				return hwnd
			}
		}
	}
	return 0
}

var (
	pIsWindow                = user32dll.NewProc("IsWindow")
	pGetWindowThreadProcessId = user32dll.NewProc("GetWindowThreadProcessId")
)

// setWindowIcon changes the native window icon (title bar + taskbar).
// skinId: "telegram" (default icon) or "wisetalk" (hide title bar icon, keep taskbar icon).
func setWindowIcon(skinId string) {
	hwnd := findMainHwnd()
	if hwnd == 0 {
		return
	}

	if skinId == "wisetalk" {
		// WiseTalk: hide small icon from title bar, set big icon for taskbar
		pSendMessageW.Call(hwnd, wmSetIcon, iconSmall, 0)
		if currentSmallIcon != 0 {
			pDestroyIcon.Call(currentSmallIcon)
			currentSmallIcon = 0
		}
		// Set taskbar icon to wisetalk dove
		setIconFromData(hwnd, wisetalkIconICO, true, false)
	} else {
		// Telegram: set both big and small icons to default
		setIconFromData(hwnd, trayIconICO, true, true)
	}
}

// setIconFromData loads an ICO from byte data and sets it on the window.
func setIconFromData(hwnd uintptr, icoData []byte, setBig, setSmall bool) {
	tmpFile := filepath.Join(os.TempDir(), "lanshare_icon.ico")
	if err := os.WriteFile(tmpFile, icoData, 0644); err != nil {
		return
	}
	defer os.Remove(tmpFile)

	tmpPath, _ := windows.UTF16PtrFromString(tmpFile)

	if setBig {
		hBig, _, _ := pLoadImageW.Call(0, uintptr(unsafe.Pointer(tmpPath)), imageIcon, 32, 32, lrLoadFromFile)
		if currentBigIcon != 0 {
			pDestroyIcon.Call(currentBigIcon)
		}
		if hBig != 0 {
			pSendMessageW.Call(hwnd, wmSetIcon, iconBig, hBig)
			currentBigIcon = hBig
		}
	}

	if setSmall {
		hSmall, _, _ := pLoadImageW.Call(0, uintptr(unsafe.Pointer(tmpPath)), imageIcon, 16, 16, lrLoadFromFile)
		if currentSmallIcon != 0 {
			pDestroyIcon.Call(currentSmallIcon)
		}
		if hSmall != 0 {
			pSendMessageW.Call(hwnd, wmSetIcon, iconSmall, hSmall)
			currentSmallIcon = hSmall
		}
	}
}
