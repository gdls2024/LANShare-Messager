//go:build !windows

package main

// setupNativeFileDrop is a no-op on non-Windows platforms.
// WebView2-specific drag-drop interception is only needed on Windows.
func setupNativeFileDrop(onDrop func([]string)) {}
