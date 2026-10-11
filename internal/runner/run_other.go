//go:build !unix

package runner

import (
	"fmt"
	"os"
)

// Options is one `leadline run`.
type Options struct {
	Command                         []string
	Name, URL                       string
	NoProxy, Proxy, NoInject, Quiet bool
	Dir                             string
	Env                             []string
	Stdin, Stdout, Stderr           *os.File
}

// Run is not there yet on this system: it needs a Unix terminal.
func Run(opts Options) int {
	fmt.Fprintln(os.Stderr, "leadline: run is not supported on this system yet")
	return 1
}
