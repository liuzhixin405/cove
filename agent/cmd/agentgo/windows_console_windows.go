//go:build windows

package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

func windowsConsoleCodePages() (uint32, uint32, error) {
	in, err := windows.GetConsoleCP()
	if err != nil {
		return 0, 0, err
	}
	out, err := windows.GetConsoleOutputCP()
	if err != nil {
		return 0, 0, err
	}
	return in, out, nil
}

func windowsConsoleEncodingActive(inputCP, outputCP uint32) bool {
	return inputCP != 65001 || outputCP != 65001
}

func windowsConsoleEncodingWarning(inputCP, outputCP uint32) string {
	if !windowsConsoleEncodingActive(inputCP, outputCP) {
		return ""
	}
	return fmt.Sprintf("Console code page warning / 控制台代码页提醒: input CP=%d, output CP=%d. Current console is not full UTF-8, so Chinese input/output may still look garbled. Run chcp 65001 before starting agentgo, or use Windows Terminal / UTF-8. 当前控制台还不是完整 UTF-8，所以中文输入输出仍可能乱码。请先执行 chcp 65001，再启动 agentgo，或直接使用 Windows Terminal / UTF-8。", inputCP, outputCP)
}

func windowsConsoleEncodingNotice(platform string) string {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(platform)), "windows") {
		return ""
	}
	if inputCP, outputCP, err := windowsConsoleCodePages(); err == nil {
		if msg := windowsConsoleEncodingWarning(inputCP, outputCP); msg != "" {
			return msg
		}
	}
	return ""
}

func tryEnableWindowsUTF8Console() {
	inputCP, outputCP, err := windowsConsoleCodePages()
	if err != nil {
		return
	}
	if !windowsConsoleEncodingActive(inputCP, outputCP) {
		return
	}
	_ = windows.SetConsoleCP(65001)
	_ = windows.SetConsoleOutputCP(65001)
}

func tryEnableVirtualTerminal() {
	stdout := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(stdout, &mode); err != nil {
		return
	}
	_ = windows.SetConsoleMode(stdout, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}

func init() {
	if len(os.Args) > 0 {
		tryEnableWindowsUTF8Console()
		tryEnableVirtualTerminal()
	}
}
