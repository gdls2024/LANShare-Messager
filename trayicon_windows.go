//go:build windows

package main

import (
	_ "embed"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

//go:embed build/windows/icon.ico
var trayIconICO []byte

// trayIcon returns the ICO-format icon bytes for Windows systray.
func trayIcon() []byte {
	return trayIconICO
}

var (
	user32dll          = windows.NewLazySystemDLL("User32.dll")
	pFindWindowW       = user32dll.NewProc("FindWindowW")
	pIsWindowVisible   = user32dll.NewProc("IsWindowVisible")
	pIsIconic          = user32dll.NewProc("IsIconic")
	pCallWindowProcW   = user32dll.NewProc("CallWindowProcW")
	pSetWindowLongPtrW = user32dll.NewProc("SetWindowLongPtrW") // 64-bit
	pSetWindowLongW    = user32dll.NewProc("SetWindowLongW")    // 32-bit fallback
)

const (
	wmUser          = 0x0400
	wmSystrayMsg    = wmUser + 1 // must match systray-on-wails initInstance (WM_USER+1)
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
)

var (
	origWndProc    uintptr
	onTrayDblClick func()
)

// traySubclassProc is the replacement window procedure for the systray hidden window.
// It intercepts tray icon messages to:
//   - Suppress left single-click (no menu on left click)
//   - Fire toggle callback on left double-click
//   - Forward right-click to original handler (shows context menu)
func traySubclassProc(hWnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	if msg == wmSystrayMsg {
		switch lParam {
		case wmLButtonDblClk:
			if onTrayDblClick != nil {
				go onTrayDblClick()
			}
			return 0
		case wmLButtonUp:
			// Suppress left single-click so only right-click shows the menu.
			return 0
		}
	}
	ret, _, _ := pCallWindowProcW.Call(origWndProc, hWnd, uintptr(msg), wParam, lParam)
	return ret
}

// subclassSystray finds the systray hidden window (class "SystrayClass" created by
// the systray-on-wails library) and replaces its wndProc to intercept click events.
// Must be called from the systray onReady callback (after the window exists).
func subclassSystray(dblClickFn func()) {
	onTrayDblClick = dblClickFn

	className, _ := windows.UTF16PtrFromString("SystrayClass")
	hwnd, _, _ := pFindWindowW.Call(uintptr(unsafe.Pointer(className)), 0)
	if hwnd == 0 {
		return
	}

	cb := syscall.NewCallback(traySubclassProc)
	// GWLP_WNDPROC = -4, represented as ^uintptr(3) for unsigned conversion
	origWndProc = callSetWindowLongPtr(hwnd, ^uintptr(3), cb)
}

// callSetWindowLongPtr calls SetWindowLongPtrW (64-bit) or SetWindowLongW (32-bit).
// SetWindowLongPtrW does not exist in 32-bit user32.dll; the 32-bit equivalent is SetWindowLongW.
func callSetWindowLongPtr(hwnd, index, newLong uintptr) uintptr {
	if pSetWindowLongPtrW.Find() == nil {
		ret, _, _ := pSetWindowLongPtrW.Call(hwnd, index, newLong)
		return ret
	}
	ret, _, _ := pSetWindowLongW.Call(hwnd, index, newLong)
	return ret
}

// isAppWindowVisible checks if the main LANShare window is visible and not minimized.
// Uses Windows API so it reflects the actual window state (including HideWindowOnClose).
func isAppWindowVisible() (visible bool, minimized bool) {
	title, _ := windows.UTF16PtrFromString("LS Messager")
	hwnd, _, _ := pFindWindowW.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return false, false
	}
	v, _, _ := pIsWindowVisible.Call(hwnd)
	m, _, _ := pIsIconic.Call(hwnd)
	return v != 0, m != 0
}
