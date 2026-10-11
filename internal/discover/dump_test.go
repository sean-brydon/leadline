package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDump prints what discovery finds on the machine it runs on. It does
// nothing unless asked, and is for looking at a system by hand, a Linux
// box say, where a test binary is all that can be brought along:
//
//	GOOS=linux CGO_ENABLED=0 go test -c -o discover.test ./internal/discover
//	LEADLINE_DISCOVER_DUMP=listening ./discover.test -test.run TestDump -test.v
//	LEADLINE_DISCOVER_DUMP=scan LEADLINE_DISCOVER_FOLDERS=/srv ./discover.test -test.run TestDump -test.v
func TestDump(t *testing.T) {
	what := os.Getenv("LEADLINE_DISCOVER_DUMP")
	if what == "" {
		t.Skip("set LEADLINE_DISCOVER_DUMP=listening or =scan to print what is found here")
	}
	s := New(Options{Berth: func() string { return "" }})
	var out any = s.Listening()
	if what == "scan" {
		var folders []string
		if f := os.Getenv("LEADLINE_DISCOVER_FOLDERS"); f != "" {
			folders = strings.Split(f, ":")
		}
		if _, err := s.Start(ScanOptions{Folders: folders}); err != nil {
			t.Fatal(err)
		}
		scan, _ := s.Wait(context.Background(), 30*time.Second)
		out = scan
		// LEADLINE_DISCOVER_ENV names the projects whose env files to
		// read, by path, with colons between; without it none is opened.
		if names := os.Getenv("LEADLINE_DISCOVER_ENV"); names != "" {
			withEnv, err := s.ReadEnv(context.Background(), strings.Split(names, ":"))
			if err != nil {
				t.Fatal(err)
			}
			out = withEnv
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
