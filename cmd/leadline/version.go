package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sean-brydon/leadline/internal/version"
)

// runVersion prints this binary's version and, when a daemon answers, the
// daemon's: the two are installed together, so a difference means one of
// them is stale (an upgrade the daemon hasn't been restarted for, say).
func runVersion(args []string) error {
	fset := flag.NewFlagSet("version", flag.ExitOnError)
	base := fset.String("url", envOr("LEADLINE_URL", "http://127.0.0.1:4320"), "the daemon's address")
	daemon := fset.Bool("daemon", false, "print only the running daemon's version, and fail when none answers (for scripts)")
	fset.Parse(args)
	url := strings.TrimRight(*base, "/")

	if *daemon {
		running, ok := daemonVersion(url)
		if !ok {
			return fmt.Errorf("no daemon is answering at %s", url)
		}
		if running == "" {
			running = "unknown" // a daemon from before versions
		}
		fmt.Println(running)
		return nil
	}

	fmt.Println(version.Line("leadline"))
	switch running, ok := daemonVersion(url); {
	case !ok:
		fmt.Printf("no daemon is answering at %s\n", url)
	case running == "":
		fmt.Printf("the daemon at %s is from before versions; restart it to run this one\n", url)
	case running != version.Version:
		fmt.Printf("the daemon at %s is %s; restart it to run this version\n", url, running)
	default:
		fmt.Printf("the daemon at %s is the same version\n", url)
	}
	return nil
}

// daemonVersion asks the daemon's health for its version. ok is whether a
// Leadline daemon answered at all; one from before versions answers with
// none.
func daemonVersion(base string) (v string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/health", nil)
	if err != nil {
		return "", false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var health struct {
		OK      bool   `json:"ok"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&health) != nil {
		return "", false
	}
	if !health.OK || health.Name != "leadline" {
		return "", false
	}
	return health.Version, true
}
