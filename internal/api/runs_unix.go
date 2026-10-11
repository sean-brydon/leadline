//go:build unix

package api

import (
	"errors"
	"syscall"
)

// runnerAlive asks the system whether a process exists. Signal 0 delivers
// nothing; "not permitted" still means there is one.
func runnerAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
