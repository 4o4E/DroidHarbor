package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var consoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// Explorer creates a console owned only by this process. Keep an error visible
// there; an existing CMD/PowerShell, redirected CLI, or service never waits.
func waitForFailureConsole() error {
	var processes [2]uint32
	count, _, callErr := consoleProcessList.Call(uintptr(unsafe.Pointer(&processes[0])), uintptr(len(processes)))
	if count == 0 {
		if errors.Is(callErr, windows.ERROR_INVALID_HANDLE) || errors.Is(callErr, windows.ERROR_SUCCESS) {
			return nil
		}
		return fmt.Errorf("读取控制台归属失败：%w", callErr)
	}
	if count != 1 || processes[0] != uint32(os.Getpid()) {
		return nil
	}
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode); err != nil {
		if errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			return nil
		}
		return err
	}
	if _, err := fmt.Fprintln(os.Stderr, "启动失败，按回车关闭窗口。"); err != nil {
		return err
	}
	var input [1]byte
	for {
		n, err := os.Stdin.Read(input[:])
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
		if input[0] == '\n' {
			return nil
		}
	}
}
