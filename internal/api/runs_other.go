//go:build !unix

package api

// runnerAlive cannot tell here, so a run is never taken for lost.
func runnerAlive(pid int) bool { return true }
