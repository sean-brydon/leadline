// Package version is the one version the daemon, the command line and the
// API report.
//
// The number lives in the VERSION file at the top of the repository. The
// Makefile and the scripts stamp it into both binaries as they build
// (-ldflags -X), so a plain `go build`, `go run` or `go test` says "dev".
package version

import "runtime"

var (
	// Version is the release this binary was built as, without a "v".
	Version = "dev"
	// Commit is the short commit it was built from, when the build knew.
	Commit = ""
)

// Line is what `leadline version` and `leadlined --version` print: the
// program, its version and what it was built for. The version is always
// the second word, which scripts/install.sh relies on.
func Line(program string) string {
	s := program + " " + Version + " (" + runtime.GOOS + "/" + runtime.GOARCH
	if Commit != "" {
		s += ", " + Commit
	}
	return s + ")"
}
