package runner

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NodeProvider finds @leadline/node the way Node would from dir: the nearest
// node_modules that has it. It returns the file `--import` should load, as
// a file: URL. An absolute URL, and not the package's name, because
// NODE_OPTIONS reaches every Node process the command starts, in whatever
// folder: one that could not resolve the name would refuse to start at all.
func NodeProvider(dir string) (importURL string, ok bool) {
	for at := dir; ; at = filepath.Dir(at) {
		pkgDir := filepath.Join(at, "node_modules", "@leadline", "node")
		if entry := providerEntry(pkgDir); entry != "" {
			return (&url.URL{Scheme: "file", Path: filepath.ToSlash(entry)}).String(), true
		}
		if filepath.Dir(at) == at {
			return "", false
		}
	}
}

// providerEntry is the file the package's "./register" export names, when
// it is there to load.
func providerEntry(pkgDir string) string {
	b, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Exports map[string]json.RawMessage `json:"exports"`
	}
	rel := "dist/register.js"
	if json.Unmarshal(b, &pkg) == nil {
		if target := exportTarget(pkg.Exports["./register"]); target != "" {
			rel = target
		}
	}
	entry := filepath.Join(pkgDir, filepath.FromSlash(rel))
	if st, err := os.Stat(entry); err != nil || st.IsDir() {
		// Linked from a checkout that was never built, say.
		return ""
	}
	return entry
}

// exportTarget reads one entry of package.json's "exports": a path, or
// conditions of which "import" and then "default" are the ones Node takes
// for --import.
func exportTarget(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var conditions map[string]json.RawMessage
	if json.Unmarshal(raw, &conditions) != nil {
		return ""
	}
	for _, key := range []string{"import", "node", "default"} {
		if target := exportTarget(conditions[key]); target != "" {
			return target
		}
	}
	return ""
}

// MergeNodeOptions adds `--import <importURL>` to a NODE_OPTIONS value,
// keeping what is there. If the provider is already being loaded, by name
// or by path, the value comes back unchanged.
func MergeNodeOptions(existing, importURL string) string {
	if strings.Contains(existing, "@leadline/node") || strings.Contains(existing, importURL) {
		return existing
	}
	add := "--import " + importURL
	// NODE_OPTIONS splits on spaces unless a value is in double quotes. A
	// file: URL has its spaces escaped already; quote it all the same if
	// something else in it would need it.
	if strings.ContainsAny(importURL, " \t\"") {
		add = `--import "` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(importURL) + `"`
	}
	if strings.TrimSpace(existing) == "" {
		return add
	}
	return strings.TrimRight(existing, " \t") + " " + add
}

var nodeVersion = regexp.MustCompile(`^v(\d+)\.(\d+)\.`)

// nodeTakesImport reports whether the `node` on PATH is one the provider
// runs on: 20.6 and later, where --import is allowed in NODE_OPTIONS. An
// older Node given the flag refuses to start, so when in doubt the answer
// is no.
func nodeTakesImport(env []string, dir string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// The node the command will find, on its PATH and not this process's.
	node, err := lookPath("node", env, dir)
	if err != nil {
		return false
	}
	cmd := exec.CommandContext(ctx, node, "--version")
	cmd.Env, cmd.Dir = withoutEnv(env, "NODE_OPTIONS"), dir
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return importSupported(strings.TrimSpace(string(out)))
}

func importSupported(version string) bool {
	m := nodeVersion.FindStringSubmatch(version)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	switch {
	case major > 20:
		return true
	case major == 20:
		return minor >= 6
	}
	return false
}

func withoutEnv(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}
