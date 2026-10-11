package discover

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/sean-brydon/leadline/internal/runner"
)

// system is how the operating system is asked what is listening.
// listen_darwin.go and procfs.go answer for real; tests have their own.
type system interface {
	// sockets lists listening TCP sockets, and says what it could not see.
	sockets() ([]rawSocket, Limits)
	// process describes a process; false when it is gone or hidden.
	process(pid int) (procInfo, bool)
}

// rawSocket is one listening socket. pid is 0 when the owner is hidden.
type rawSocket struct {
	pid  int
	port int
	ip   net.IP
	uid  int
}

type procInfo struct {
	pid, ppid int
	name      string
	exe       string
	args      []string
	cwd       string
	started   time.Time
}

const maxCommand = 400

// listening builds the list of listeners from what the system says.
// self is the daemon's own process: it, any other leadlined, and anything
// either started are left out, proxies and all.
func listening(sys system, self int, now time.Time) ([]Listener, Limits) {
	socks, limits := sys.sockets()
	procs := map[int]*procInfo{}
	lookup := func(pid int) *procInfo {
		if p, ok := procs[pid]; ok {
			return p
		}
		var p *procInfo
		if info, ok := sys.process(pid); ok {
			p = &info
		}
		procs[pid] = p
		return p
	}
	// ours reports whether a process is Leadline's daemon or under it.
	ours := func(pid int) bool {
		for depth := 0; pid > 1 && depth < 32; depth++ {
			if pid == self {
				return true
			}
			p := lookup(pid)
			if p == nil {
				return false
			}
			if base := processBase(p); base == "leadlined" {
				return true
			}
			pid = p.ppid
		}
		return false
	}

	// One listener per port: a server on 127.0.0.1 and ::1 has two
	// sockets, and one that forks workers has the same socket in each.
	type key struct{ port int }
	byPort := map[key]*Listener{}
	owner := map[key]*procInfo{}
	var order []key
	for _, s := range socks {
		if s.port <= 0 {
			continue
		}
		var p *procInfo
		if s.pid > 0 {
			if ours(s.pid) {
				continue
			}
			p = lookup(s.pid)
		}
		k := key{s.port}
		l, seen := byPort[k]
		if !seen {
			l = &Listener{Port: s.port, Address: ipText(s.ip), Scope: scopeOf(s.ip)}
			byPort[k] = l
			order = append(order, k)
		} else if wider(scopeOf(s.ip), l.Scope) {
			l.Address, l.Scope = ipText(s.ip), scopeOf(s.ip)
		}
		// Of several processes holding it, the one that started first is
		// the server; the others are its workers.
		if p != nil && (owner[k] == nil || older(p, owner[k])) {
			owner[k] = p
		}
	}

	out := make([]Listener, 0, len(order))
	for _, k := range order {
		l := *byPort[k]
		if p := owner[k]; p != nil {
			l.PID, l.Process = p.pid, processName(p)
			l.Command = clip(redactCommand(strings.Join(p.args, " ")), maxCommand)
			l.Cwd = p.cwd
			if !p.started.IsZero() {
				l.StartedMs = float64(p.started.UnixMilli())
				if up := now.Sub(p.started); up > 0 {
					l.UptimeS = int64(up / time.Second)
				}
			}
			l.Kind, l.What, l.Framework, l.Basis = classify(p)
		}
		if l.Kind == "" {
			l.Kind, l.What = "other", l.Process
			if kind, what := byWellKnownPort(l.Port); kind != "" {
				l.Kind, l.What, l.Basis = kind, what, "port"
			}
		}
		out = append(out, l)
	}
	// Counted in ports, as they are listed: a hidden server on 0.0.0.0
	// and on :: is two sockets and one port.
	if limits.UnownedPorts > 0 {
		limits.UnownedPorts = 0
		for _, l := range out {
			if l.PID == 0 {
				limits.UnownedPorts++
			}
		}
	}
	sortListeners(out)
	return out, limits
}

func sortListeners(ls []Listener) {
	rank := func(kind string) int {
		switch kind {
		case "dev-server":
			return 0
		case "other":
			return 2
		}
		return 1
	}
	sort.SliceStable(ls, func(i, j int) bool {
		if a, b := rank(ls[i].Kind), rank(ls[j].Kind); a != b {
			return a < b
		}
		return ls[i].Port < ls[j].Port
	})
}

func older(a, b *procInfo) bool {
	if a.started.Equal(b.started) {
		return a.pid < b.pid
	}
	if a.started.IsZero() || b.started.IsZero() {
		return b.started.IsZero() && !a.started.IsZero()
	}
	return a.started.Before(b.started)
}

func ipText(ip net.IP) string {
	if len(ip) == 0 {
		return ""
	}
	if v4 := ip.To4(); v4 != nil && len(ip) == 4 {
		return v4.String()
	}
	return ip.String()
}

func scopeOf(ip net.IP) string {
	switch {
	case len(ip) == 0 || ip.IsUnspecified():
		return "all"
	case ip.IsLoopback():
		return "loopback"
	}
	return "address"
}

// wider reports whether scope a reaches further than b.
func wider(a, b string) bool {
	rank := map[string]int{"loopback": 0, "address": 1, "all": 2}
	return rank[a] > rank[b]
}

// processBase is the name a process goes by, lower-cased: its
// executable's file name, else what the system calls it.
func processBase(p *procInfo) string {
	name := p.name
	if p.exe != "" {
		name = filepath.Base(p.exe)
	}
	return strings.ToLower(name)
}

// processName is the name to show. The executable's name is truer than
// the kernel's, which is cut at 15 characters and which a process may
// change (a Next.js dev server calls itself "next-server (v16.0.1)").
func processName(p *procInfo) string {
	if p.exe != "" {
		return filepath.Base(p.exe)
	}
	return p.name
}

// A flag that takes a secret, in either spelling: --token=x, --token x.
var secretFlag = regexp.MustCompile(`(?i)(-{1,2}[a-z0-9_-]*(?:password|passwd|secret|token|api[-_]?key|auth)[a-z0-9_-]*[= ])("[^"]*"|'[^']*'|\S+)`)

// A variable set in front of a command (`sh -c "API_KEY=abc node x"`).
var secretAssign = regexp.MustCompile(`(?i)\b([a-z0-9_]*(?:password|passwd|secret|token|api_?key)[a-z0-9_]*=)("[^"]*"|'[^']*'|\S+)`)

// redactCommand hides what looks like a credential in a command line: a
// URL's password (runner.Redact), and the value of a flag or variable
// whose name says it is one.
func redactCommand(s string) string {
	s = runner.Redact(s)
	s = secretFlag.ReplaceAllString(s, "${1}***")
	return secretAssign.ReplaceAllString(s, "${1}***")
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Not in the middle of a character.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

func byWellKnownPort(port int) (kind, what string) {
	switch port {
	case 5432:
		return "postgres", "Postgres"
	case 6379:
		return "redis", "Redis"
	case 3306:
		return "mysql", "MySQL"
	case 27017:
		return "mongodb", "MongoDB"
	}
	return "", ""
}

// servers are processes that are a database or cache by their name.
var servers = map[string][2]string{
	"postgres": {"postgres", "Postgres"}, "postmaster": {"postgres", "Postgres"},
	"redis-server": {"redis", "Redis"}, "valkey-server": {"redis", "Valkey"}, "keydb-server": {"redis", "KeyDB"}, "dragonfly": {"redis", "Dragonfly"},
	"mysqld": {"mysql", "MySQL"}, "mariadbd": {"mysql", "MariaDB"},
	"mongod": {"mongodb", "MongoDB"}, "mongos": {"mongodb", "MongoDB"},
}

// runtimes are processes that run someone's code: listening from inside a
// project's folder, one is taken for that project's dev server.
var runtimes = map[string]string{
	"node": "node", "bun": "bun", "deno": "deno", "ruby": "ruby", "php": "php",
	"java": "java", "beam.smp": "elixir", "dotnet": "dotnet", "uvicorn": "python", "gunicorn": "python",
}

// devCommands recognise a dev server by its command line. Each is matched
// against the arguments joined by spaces, lower-cased, with every path
// reduced to its last part ("node /x/node_modules/.bin/next dev" reads
// "node next dev").
var devCommands = []struct {
	re        *regexp.Regexp
	what      string
	framework string
}{
	{regexp.MustCompile(`^next-server\b`), "next dev", "next"},
	{regexp.MustCompile(`\bnext (dev|start)\b`), "next $1", "next"},
	{regexp.MustCompile(`\bnux[ti] (dev|preview)\b`), "nuxt $1", "nuxt"},
	{regexp.MustCompile(`\bastro (dev|preview)\b`), "astro $1", "astro"},
	{regexp.MustCompile(`\bremix(-serve| dev| vite:dev)\b`), "remix dev", "remix"},
	{regexp.MustCompile(`\breact-router dev\b`), "react-router dev", "remix"},
	{regexp.MustCompile(`\bgatsby develop\b`), "gatsby develop", "gatsby"},
	{regexp.MustCompile(`\bng serve\b`), "ng serve", "angular"},
	{regexp.MustCompile(`\b(storybook dev|start-storybook)\b`), "storybook", "storybook"},
	{regexp.MustCompile(`\bwrangler (dev|pages dev)\b`), "wrangler dev", "wrangler"},
	{regexp.MustCompile(`\bexpo start\b`), "expo start", "expo"},
	{regexp.MustCompile(`\bvite(\.js)?( (dev|serve|preview))?( -|$)`), "vite", "vite"},
	{regexp.MustCompile(`\b(webpack serve|webpack-dev-server)\b`), "webpack serve", "webpack"},
	{regexp.MustCompile(`\breact-scripts start\b`), "react-scripts start", "create-react-app"},
	{regexp.MustCompile(`\bparcel( serve)?\b`), "parcel", "parcel"},
	{regexp.MustCompile(`\bnest start\b`), "nest start", "nest"},
	{regexp.MustCompile(`\bmanage\.py runserver\b`), "manage.py runserver", "django"},
	{regexp.MustCompile(`\bfastapi (dev|run)\b`), "fastapi $1", "fastapi"},
	{regexp.MustCompile(`\bflask\b.* run\b`), "flask run", "flask"},
	{regexp.MustCompile(`\buvicorn\b`), "uvicorn", "python"},
	{regexp.MustCompile(`\bgunicorn\b`), "gunicorn", "python"},
	{regexp.MustCompile(`\bhypercorn\b`), "hypercorn", "python"},
	{regexp.MustCompile(`\bstreamlit run\b`), "streamlit run", "streamlit"},
	{regexp.MustCompile(`-m http\.server\b`), "python -m http.server", "python"},
	{regexp.MustCompile(`\brails (server|s)\b`), "rails server", "rails"},
	{regexp.MustCompile(`^puma\b|\bpuma\b.*\btcp://`), "puma", "rails"},
	{regexp.MustCompile(`\bartisan serve\b`), "artisan serve", "laravel"},
	{regexp.MustCompile(`\bphp -s\b`), "php -S", "php"},
	{regexp.MustCompile(`\bphx\.server\b`), "mix phx.server", "phoenix"},
	{regexp.MustCompile(`\b(nodemon|tsx|ts-node|ts-node-dev)\b`), "node", "node"},
	{regexp.MustCompile(`\bbun (run|dev|--hot|--watch)\b`), "bun", "bun"},
	{regexp.MustCompile(`\bdeno (run|task|serve)\b`), "deno", "deno"},
}

// classify says what a listening process probably is.
func classify(p *procInfo) (kind, what, framework, basis string) {
	base := processBase(p)
	if s, ok := servers[base]; ok {
		return s[0], s[1], "", "process"
	}
	// The process's own title can be the giveaway (argv[0] of a Next.js
	// dev server is "next-server (v16.0.1)").
	line := commandWords(p.args)
	for _, d := range devCommands {
		if m := d.re.FindStringSubmatchIndex(line); m != nil {
			what := string(d.re.ExpandString(nil, d.what, line, m))
			return "dev-server", strings.TrimSpace(what), d.framework, "command"
		}
	}
	// What `go run` and `cargo run` build and start.
	exe := filepath.ToSlash(p.exe)
	switch {
	case strings.Contains(exe, "/go-build") && strings.Contains(exe, "/exe/"):
		return "dev-server", "go run", "go", "command"
	case strings.Contains(exe, "/target/debug/"):
		return "dev-server", "cargo run", "rust", "command"
	}
	return "", "", "", ""
}

// runtimeOf is the runtime a process is, or "": for a listener nothing
// else explained, whose working directory may yet make it a dev server.
func runtimeOf(process string) string {
	base := strings.ToLower(process)
	if r, ok := runtimes[base]; ok {
		return r
	}
	if strings.HasPrefix(base, "python") {
		return "python"
	}
	return ""
}

// commandWords joins arguments for matching: lower-cased, each path
// reduced to its file name.
func commandWords(args []string) string {
	words := make([]string, 0, len(args))
	for i, a := range args {
		if i > 12 {
			break
		}
		a = strings.ToLower(a)
		if !strings.HasPrefix(a, "-") && strings.ContainsRune(a, '/') && !strings.Contains(a, "://") {
			a = a[strings.LastIndexByte(a, '/')+1:]
		}
		words = append(words, a)
	}
	return strings.Join(words, " ")
}

func thisMachine() Machine {
	name, _ := os.Hostname()
	return Machine{OS: runtime.GOOS, Hostname: name}
}
