//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// alive says whether a process with this pid is running: a status file left
// by an agent that was killed still says connected.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
