package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestLine(t *testing.T) {
	oldV, oldC := Version, Commit
	defer func() { Version, Commit = oldV, oldC }()

	Version, Commit = "1.2.3", ""
	got := Line("leadlined")
	if want := "leadlined 1.2.3 (" + runtime.GOOS + "/" + runtime.GOARCH + ")"; got != want {
		t.Errorf("Line = %q, want %q", got, want)
	}
	// The installer reads the second word as the version.
	if f := strings.Fields(got); f[1] != "1.2.3" {
		t.Errorf("second word = %q", f[1])
	}

	Commit = "abc1234"
	if got := Line("leadline"); !strings.HasSuffix(got, ", abc1234)") {
		t.Errorf("Line with a commit = %q", got)
	}
}
