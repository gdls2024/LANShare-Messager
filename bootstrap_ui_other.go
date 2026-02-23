//go:build !windows

package main

import "fmt"

// BootstrapSplash is a no-op on non-Windows platforms.
type BootstrapSplash struct{}

func NewBootstrapSplash() *BootstrapSplash { return &BootstrapSplash{} }
func (s *BootstrapSplash) Show()           {}
func (s *BootstrapSplash) SetText(t string) { fmt.Println(t) }
func (s *BootstrapSplash) Close()           {}
func (s *BootstrapSplash) ShowError(msg string) {
	fmt.Println("错误:", msg)
}

// isWebView2SystemInstalled is a no-op on non-Windows (WebView2 is Windows-only).
func isWebView2SystemInstalled() bool {
	return false
}
