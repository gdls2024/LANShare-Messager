//go:build !windows

package main

// trayIcon returns the PNG icon bytes for non-Windows systray.
func trayIcon() []byte {
	return appIconPNG
}

func subclassSystray(dblClickFn func()) {}

func isAppWindowVisible() (visible bool, minimized bool) {
	return false, false
}
