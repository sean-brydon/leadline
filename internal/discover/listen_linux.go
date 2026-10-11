//go:build linux

package discover

func hostSystem() system { return procfs{root: "/proc", userHZ: 100} }
