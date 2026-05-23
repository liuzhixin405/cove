//go:build !windows

package main

func windowsConsoleEncodingNotice(platform string) string {
	return windowsUTF8Notice(platform)
}
