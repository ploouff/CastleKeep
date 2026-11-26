//go:build windows

package main

import (
	"os"
	"syscall"
)

// Each program start runs gets its own process group, so start can send it
// Ctrl+Break (which Go programs take as Ctrl+C) on its own. Ctrl+C in the
// window reaches only start, which then stops them in order.
func childAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

var generateConsoleCtrlEvent = syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")

// interrupt asks a program to stop as Ctrl+C would.
func interrupt(p *os.Process) {
	const ctrlBreak = 1
	if r, _, _ := generateConsoleCtrlEvent.Call(ctrlBreak, uintptr(p.Pid)); r == 0 {
		p.Kill()
	}
}
