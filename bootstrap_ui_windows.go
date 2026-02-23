//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// Win32 API references for bootstrap splash window
var (
	bsUser32   = syscall.NewLazyDLL("user32.dll")
	bsKernel32 = syscall.NewLazyDLL("kernel32.dll")
	bsGdi32    = syscall.NewLazyDLL("gdi32.dll")

	bsRegisterClassExW = bsUser32.NewProc("RegisterClassExW")
	bsCreateWindowExW  = bsUser32.NewProc("CreateWindowExW")
	bsDefWindowProcW   = bsUser32.NewProc("DefWindowProcW")
	bsShowWindow       = bsUser32.NewProc("ShowWindow")
	bsUpdateWindow     = bsUser32.NewProc("UpdateWindow")
	bsGetMessageW      = bsUser32.NewProc("GetMessageW")
	bsTranslateMessage = bsUser32.NewProc("TranslateMessage")
	bsDispatchMessageW = bsUser32.NewProc("DispatchMessageW")
	bsPostMessageW     = bsUser32.NewProc("PostMessageW")
	bsSetWindowTextW   = bsUser32.NewProc("SetWindowTextW")
	bsGetSystemMetrics = bsUser32.NewProc("GetSystemMetrics")
	bsMessageBoxW      = bsUser32.NewProc("MessageBoxW")
	bsSendMessageW     = bsUser32.NewProc("SendMessageW")
	bsPostQuitMessage  = bsUser32.NewProc("PostQuitMessage")
	bsGetModuleHandleW = bsKernel32.NewProc("GetModuleHandleW")
	bsGetStockObject   = bsGdi32.NewProc("GetStockObject")
)

// Win32 constants
const (
	bsWsCaption   = 0x00C00000
	bsWsSysmenu   = 0x00080000
	bsWsVisible   = 0x10000000
	bsWsChild     = 0x40000000
	bsWsExTopmost = 0x00000008
	bsSsCenter    = 0x00000001
	bsSmCxscreen  = 0
	bsSmCyscreen  = 1
	bsSwShow      = 5
	bsWmDestroy   = 0x0002
	bsWmClose     = 0x0010
	bsWmSetfont   = 0x0030
	bsWmUser      = 0x0400
	bsMbOK        = 0x00000000
	bsMbIconError = 0x00000010
	bsDefGuiFont  = 17
	bsColorWindow = 5

	bsWmUpdateText = bsWmUser + 100
)

type bsWndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type bsPoint struct{ x, y int32 }
type bsMsg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      bsPoint
}

// Shared state for the splash window (single instance)
var (
	bsSplashMu    sync.Mutex
	bsSplashText  string
	bsSplashLabel uintptr
)

func bsSplashWndProc(hwnd, umsg, wParam, lParam uintptr) uintptr {
	switch umsg {
	case bsWmDestroy:
		bsPostQuitMessage.Call(0)
		return 0
	case bsWmUpdateText:
		bsSplashMu.Lock()
		t := bsSplashText
		bsSplashMu.Unlock()
		if bsSplashLabel != 0 {
			ptr, _ := syscall.UTF16PtrFromString(t)
			bsSetWindowTextW.Call(bsSplashLabel, uintptr(unsafe.Pointer(ptr)))
		}
		return 0
	}
	ret, _, _ := bsDefWindowProcW.Call(hwnd, umsg, wParam, lParam)
	return ret
}

// BootstrapSplash displays a simple native Windows splash during WebView2 bootstrap.
type BootstrapSplash struct {
	hwnd  uintptr
	ready chan struct{}
}

func NewBootstrapSplash() *BootstrapSplash {
	return &BootstrapSplash{ready: make(chan struct{})}
}

func (s *BootstrapSplash) Show() {
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		hInst, _, _ := bsGetModuleHandleW.Call(0)
		className, _ := syscall.UTF16PtrFromString("LANShareBootstrap")

		wc := bsWndClassEx{
			lpfnWndProc:   syscall.NewCallback(bsSplashWndProc),
			hInstance:     hInst,
			hbrBackground: bsColorWindow + 1,
			lpszClassName: className,
		}
		wc.cbSize = uint32(unsafe.Sizeof(wc))
		bsRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

		const w, h = 460, 150
		sw, _, _ := bsGetSystemMetrics.Call(bsSmCxscreen)
		sh, _, _ := bsGetSystemMetrics.Call(bsSmCyscreen)
		x := (int(sw) - w) / 2
		y := (int(sh) - h) / 2

		title, _ := syscall.UTF16PtrFromString("域信 LS Messager")
		hwnd, _, _ := bsCreateWindowExW.Call(
			bsWsExTopmost,
			uintptr(unsafe.Pointer(className)),
			uintptr(unsafe.Pointer(title)),
			bsWsCaption|bsWsSysmenu,
			uintptr(x), uintptr(y), w, h,
			0, 0, hInst, 0,
		)
		if hwnd == 0 {
			close(s.ready)
			return
		}
		s.hwnd = hwnd

		// Create centered static text label
		staticClass, _ := syscall.UTF16PtrFromString("STATIC")
		initText, _ := syscall.UTF16PtrFromString("正在初始化...")
		label, _, _ := bsCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(staticClass)),
			uintptr(unsafe.Pointer(initText)),
			bsWsChild|bsWsVisible|bsSsCenter,
			10, 35, w-20, 70,
			hwnd, 0, hInst, 0,
		)
		bsSplashLabel = label

		// Set default GUI font
		hFont, _, _ := bsGetStockObject.Call(bsDefGuiFont)
		bsSendMessageW.Call(label, bsWmSetfont, hFont, 1)

		bsShowWindow.Call(hwnd, bsSwShow)
		bsUpdateWindow.Call(hwnd)

		close(s.ready)

		// Message pump
		var m bsMsg
		for {
			ret, _, _ := bsGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if ret == 0 || int32(ret) == -1 {
				break
			}
			bsTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			bsDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
	}()
	<-s.ready
}

func (s *BootstrapSplash) SetText(text string) {
	bsSplashMu.Lock()
	bsSplashText = text
	bsSplashMu.Unlock()
	if s.hwnd != 0 {
		bsPostMessageW.Call(s.hwnd, bsWmUpdateText, 0, 0)
	}
	fmt.Println(text)
}

func (s *BootstrapSplash) Close() {
	if s.hwnd != 0 {
		bsPostMessageW.Call(s.hwnd, bsWmClose, 0, 0)
		s.hwnd = 0
	}
}

func (s *BootstrapSplash) ShowError(message string) {
	text, _ := syscall.UTF16PtrFromString(message)
	title, _ := syscall.UTF16PtrFromString("域信 LS Messager")
	bsMessageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), bsMbOK|bsMbIconError)
}

// isWebView2SystemInstalled checks for system-installed WebView2 Evergreen Runtime.
// Uses registry, Windows version, and filesystem checks for comprehensive detection.
// NOTE: This intentionally does NOT check for local Fixed Version (WebView2Runtime/ next to exe),
// because the caller needs to distinguish system vs local to set the correct WebviewBrowserPath.
func isWebView2SystemInstalled() bool {
	const wv2GUID = `{F3017226-FE2A-4295-8BEF-335AE1BC7588}`
	const keyRead = 0x20019

	keys := []struct {
		root syscall.Handle
		path string
	}{
		{0x80000002, `SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\` + wv2GUID}, // HKLM 64-bit
		{0x80000002, `SOFTWARE\Microsoft\EdgeUpdate\Clients\` + wv2GUID},              // HKLM 32-bit
		{0x80000001, `SOFTWARE\Microsoft\EdgeUpdate\Clients\` + wv2GUID},              // HKCU
	}

	for _, k := range keys {
		var h syscall.Handle
		sub, _ := syscall.UTF16PtrFromString(k.path)
		if syscall.RegOpenKeyEx(k.root, sub, 0, keyRead, &h) == nil {
			syscall.RegCloseKey(h)
			return true
		}
	}

	// Windows 11+ ships WebView2 as part of the OS
	if isWindows11OrLater() {
		return true
	}

	// Fallback: check filesystem for Evergreen Runtime (Edge-bundled WebView2).
	// Some Win10 systems have WebView2 via Edge updates but lack the EdgeUpdate registry key.
	if isWebView2EvergreenOnDisk() {
		return true
	}

	return false
}

// isWebView2EvergreenOnDisk checks known filesystem paths for system-installed WebView2.
// This catches cases where WebView2 is available through Edge but the registry key is absent.
func isWebView2EvergreenOnDisk() bool {
	candidates := []string{
		os.Getenv("ProgramFiles(x86)"),
		os.Getenv("ProgramFiles"),
		os.Getenv("LOCALAPPDATA"),
	}
	for _, root := range candidates {
		if root == "" {
			continue
		}
		appDir := filepath.Join(root, "Microsoft", "EdgeWebView", "Application")
		entries, err := os.ReadDir(appDir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if _, err := os.Stat(filepath.Join(appDir, entry.Name(), "msedgewebview2.exe")); err == nil {
				return true
			}
		}
	}
	return false
}

// isWindows11OrLater returns true if the OS is Windows 11 or later (build >= 22000).
// Windows 11 ships with WebView2 built into the OS via Microsoft Edge.
func isWindows11OrLater() bool {
	const keyRead = 0x20019
	var h syscall.Handle
	sub, _ := syscall.UTF16PtrFromString(`SOFTWARE\Microsoft\Windows NT\CurrentVersion`)
	if syscall.RegOpenKeyEx(0x80000002, sub, 0, keyRead, &h) != nil {
		return false
	}
	defer syscall.RegCloseKey(h)

	var buf [64]uint16
	n := uint32(len(buf) * 2)
	valName, _ := syscall.UTF16PtrFromString("CurrentBuildNumber")
	if syscall.RegQueryValueEx(h, valName, nil, nil, (*byte)(unsafe.Pointer(&buf[0])), &n) != nil {
		return false
	}
	buildStr := syscall.UTF16ToString(buf[:])
	var build int
	fmt.Sscanf(buildStr, "%d", &build)
	return build >= 22000
}
