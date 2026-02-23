//go:build !windows

package main

func setWindowIcon(skinId string) {
	// No-op on non-Windows platforms.
}
