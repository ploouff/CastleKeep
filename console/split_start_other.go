//go:build !windows

package main

import (
	"os"
	"syscall"
)

// Each program start runs gets its own process group, so Ctrl+C in the
// terminal reaches only start, which then stops them in order.
func childAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// interrupt asks a program to stop as Ctrl+C would.
func interrupt(p *os.Process) { p.Signal(os.Interrupt) }
