package discover

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary stand in for a dev server: started with
// LEADLINE_DISCOVER_HELPER set, it listens on a port, prints it, and
// waits for its input to close.
func TestMain(m *testing.M) {
	if os.Getenv("LEADLINE_DISCOVER_HELPER") == "listen" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println(ln.Addr().(*net.TCPAddr).Port)
		bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeSystem is a machine made up for a test.
type fakeSystem struct {
	socks  []rawSocket
	procs  map[int]procInfo
	limits Limits
}

func (f fakeSystem) sockets() ([]rawSocket, Limits) { return f.socks, f.limits }
func (f fakeSystem) process(pid int) (procInfo, bool) {
	p, ok := f.procs[pid]
	return p, ok
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name                        string
		p                           procInfo
		kind, what, framework, base string
	}{
		{"next by its bin", procInfo{exe: "/usr/local/bin/node", args: []string{"node", "/Users/acme/code/acme/node_modules/.bin/next", "dev", "--turbopack"}},
			"dev-server", "next dev", "next", "command"},
		{"next by its title", procInfo{exe: "/usr/local/bin/node", args: []string{"next-server (v16.0.1)"}},
			"dev-server", "next dev", "next", "command"},
		{"next start", procInfo{exe: "/usr/bin/node", args: []string{"node", "node_modules/next/dist/bin/next", "start"}},
			"dev-server", "next start", "next", "command"},
		{"vite", procInfo{exe: "/usr/bin/node", args: []string{"node", "/srv/acme-admin/node_modules/.bin/vite", "--port", "5173"}},
			"dev-server", "vite", "vite", "command"},
		{"vite bare", procInfo{exe: "/usr/bin/node", args: []string{"node", "/srv/acme-admin/node_modules/vite/bin/vite.js"}},
			"dev-server", "vite", "vite", "command"},
		{"vitest is not vite", procInfo{exe: "/usr/bin/node", args: []string{"node", "node_modules/.bin/vitest", "--ui"}},
			"", "", "", ""},
		{"nuxt", procInfo{exe: "/usr/bin/node", args: []string{"node", "node_modules/.bin/nuxt", "dev"}}, "dev-server", "nuxt dev", "nuxt", "command"},
		{"astro", procInfo{exe: "/usr/bin/node", args: []string{"node", "node_modules/.bin/astro", "dev"}}, "dev-server", "astro dev", "astro", "command"},
		{"tsx watch", procInfo{exe: "/usr/bin/node", args: []string{"node", "node_modules/.bin/tsx", "watch", "src/server.ts"}}, "dev-server", "node", "node", "command"},
		{"django", procInfo{exe: "/usr/bin/python3.13", args: []string{"python", "manage.py", "runserver", "8000"}}, "dev-server", "manage.py runserver", "django", "command"},
		{"uvicorn", procInfo{exe: "/usr/bin/python3.13", args: []string{"/srv/api/.venv/bin/python", "/srv/api/.venv/bin/uvicorn", "main:app", "--reload"}}, "dev-server", "uvicorn", "python", "command"},
		{"flask", procInfo{exe: "/usr/bin/python3", args: []string{"python", "-m", "flask", "--app", "acme", "run"}}, "dev-server", "flask run", "flask", "command"},
		{"http.server", procInfo{exe: "/usr/bin/python3", args: []string{"python3", "-m", "http.server", "8080"}}, "dev-server", "python -m http.server", "python", "command"},
		{"rails", procInfo{exe: "/usr/bin/ruby", args: []string{"ruby", "bin/rails", "server"}}, "dev-server", "rails server", "rails", "command"},
		{"puma by its title", procInfo{exe: "/usr/bin/ruby", args: []string{"puma 6.4.3 (tcp://localhost:3000) [acme]"}}, "dev-server", "puma", "rails", "command"},
		{"go run", procInfo{exe: "/tmp/go-build123/b001/exe/billing", args: []string{"/tmp/go-build123/b001/exe/billing"}}, "dev-server", "go run", "go", "command"},
		{"cargo run", procInfo{exe: "/srv/api/target/debug/api", args: []string{"target/debug/api"}}, "dev-server", "cargo run", "rust", "command"},
		{"postgres", procInfo{exe: "/usr/lib/postgresql/17/bin/postgres", args: []string{"postgres", "-D", "/var/lib/postgresql/17/main"}}, "postgres", "Postgres", "", "process"},
		{"redis", procInfo{exe: "/usr/bin/redis-server", args: []string{"redis-server *:6379"}}, "redis", "Redis", "", "process"},
		{"mysql", procInfo{name: "mysqld"}, "mysql", "MySQL", "", "process"},
		{"mongodb", procInfo{name: "mongod"}, "mongodb", "MongoDB", "", "process"},
		{"a plain node script", procInfo{exe: "/usr/bin/node", args: []string{"node", "server.js"}}, "", "", "", ""},
		{"something else", procInfo{exe: "/usr/sbin/sshd", args: []string{"sshd: /usr/sbin/sshd -D"}}, "", "", "", ""},
	}
	for _, c := range cases {
		kind, what, framework, basis := classify(&c.p)
		if kind != c.kind || what != c.what || framework != c.framework || basis != c.base {
			t.Errorf("%s: got (%q, %q, %q, %q), want (%q, %q, %q, %q)", c.name, kind, what, framework, basis, c.kind, c.what, c.framework, c.base)
		}
	}
}

func TestListeningMergesAndLeavesLeadlineOut(t *testing.T) {
	now := time.Unix(1791650000, 0)
	started := now.Add(-3 * time.Hour)
	sys := fakeSystem{
		socks: []rawSocket{
			// A dev server on both loopbacks: one listener.
			{pid: 4242, port: 3000, ip: net.ParseIP("127.0.0.1").To4()},
			{pid: 4242, port: 3000, ip: net.ParseIP("::1")},
			// Postgres with a worker that holds the same socket: the
			// one that started first is the server.
			{pid: 612, port: 5432, ip: net.IPv4zero.To4()},
			{pid: 611, port: 5432, ip: net.IPv4zero.To4()},
			// The daemon itself, its proxy, another leadlined, and a
			// thing the daemon started.
			{pid: 100, port: 4320, ip: net.ParseIP("127.0.0.1").To4()},
			{pid: 100, port: 15432, ip: net.ParseIP("127.0.0.1").To4()},
			{pid: 200, port: 4399, ip: net.ParseIP("127.0.0.1").To4()},
			{pid: 101, port: 9100, ip: net.ParseIP("127.0.0.1").To4()},
			// A port whose process is hidden, on a number that says
			// what it probably is.
			{pid: 0, port: 6379, ip: net.IPv4zero.To4(), uid: 0},
			{pid: 0, port: 8443, ip: net.ParseIP("192.0.2.7").To4(), uid: 0},
			// A command line with secrets in it.
			{pid: 700, port: 7000, ip: net.ParseIP("127.0.0.1").To4()},
		},
		procs: map[int]procInfo{
			4242: {pid: 4242, ppid: 4000, name: "node", exe: "/usr/local/bin/node", cwd: "/srv/acme", started: started,
				args: []string{"node", "/srv/acme/node_modules/.bin/next", "dev"}},
			4000: {pid: 4000, ppid: 1, name: "zsh", exe: "/bin/zsh"},
			611:  {pid: 611, ppid: 1, name: "postgres", exe: "/usr/bin/postgres", started: started.Add(-time.Hour), args: []string{"postgres", "-D", "/var/lib/pg"}},
			612:  {pid: 612, ppid: 611, name: "postgres", exe: "/usr/bin/postgres", started: started, args: []string{"postgres: checkpointer"}},
			100:  {pid: 100, ppid: 1, name: "leadlined", exe: "/Applications/Leadline.app/Contents/MacOS/leadlined"},
			200:  {pid: 200, ppid: 1, name: "leadlined", exe: "/usr/local/bin/leadlined"},
			101:  {pid: 101, ppid: 100, name: "helper", exe: "/usr/local/bin/helper"},
			700: {pid: 700, ppid: 1, name: "worker", exe: "/usr/local/bin/worker", cwd: "/srv/worker", started: started,
				args: []string{"worker", "--db", "postgres://app:CMDSECRET1@localhost/acme", "--api-token=CMDSECRET2", "--password", "CMDSECRET3"}},
		},
		limits: Limits{Method: "procfs", UnownedPorts: 2},
	}
	got, limits := listening(sys, 100, now)
	if limits.UnownedPorts != 2 {
		t.Errorf("limits = %+v", limits)
	}
	byPort := map[int]Listener{}
	for _, l := range got {
		if _, twice := byPort[l.Port]; twice {
			t.Errorf("port %d is listed twice", l.Port)
		}
		byPort[l.Port] = l
	}
	for _, port := range []int{4320, 15432, 4399, 9100} {
		if l, ok := byPort[port]; ok {
			t.Errorf("Leadline's own port %d is listed: %+v", port, l)
		}
	}
	dev := byPort[3000]
	if dev.PID != 4242 || dev.Process != "node" || dev.Cwd != "/srv/acme" || dev.Kind != "dev-server" || dev.What != "next dev" ||
		dev.Framework != "next" || dev.Scope != "loopback" || dev.UptimeS != 3*3600 || dev.StartedMs != float64(started.UnixMilli()) {
		t.Errorf("dev server = %+v", dev)
	}
	if pg := byPort[5432]; pg.PID != 611 || pg.Kind != "postgres" || pg.Scope != "all" || pg.Basis != "process" {
		t.Errorf("postgres = %+v", pg)
	}
	if rd := byPort[6379]; rd.PID != 0 || rd.Kind != "redis" || rd.Basis != "port" || rd.Process != "" {
		t.Errorf("hidden redis = %+v", rd)
	}
	if other := byPort[8443]; other.Kind != "other" || other.Scope != "address" || other.Address != "192.0.2.7" {
		t.Errorf("hidden other = %+v", other)
	}
	cmd := byPort[7000].Command
	for _, secret := range []string{"CMDSECRET1", "CMDSECRET2", "CMDSECRET3"} {
		if strings.Contains(cmd, secret) {
			t.Errorf("the command line shows %s: %q", secret, cmd)
		}
	}
	if !strings.Contains(cmd, "postgres://app:") || !strings.Contains(cmd, "@localhost/acme") {
		t.Errorf("the command line lost more than its secrets: %q", cmd)
	}
	// Dev servers first, then databases, then the rest.
	var kinds []string
	for _, l := range got {
		kinds = append(kinds, l.Kind)
	}
	if want := "dev-server postgres redis other other"; strings.Join(kinds, " ") != want {
		t.Errorf("order = %v, want %s", kinds, want)
	}
}

// A made-up /proc, read by the code that reads the real one on Linux.
func TestProcfs(t *testing.T) {
	root := t.TempDir()
	const header = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
	line := func(n int, local, state string, uid, inode int) string {
		return fmt.Sprintf("%4d: %s 00000000:0000 %s 00000000:00000000 00:00000000 00000000 %5d        0 %d 1 0000000000000000 100 0 0 10 0\n",
			n, local, state, uid, inode)
	}
	writeFiles(t, root, map[string]string{
		"stat": "cpu  1 2 3 4\nbtime 1791600000\nprocesses 99\n",
		"net/tcp": header +
			line(0, "0100007F:1538", "0A", 1000, 11111) + // 127.0.0.1:5432
			line(1, "00000000:0BB8", "0A", 1000, 22222) + // 0.0.0.0:3000
			line(2, "0100007F:9999", "01", 1000, 33333) + // an open connection, not a listener
			line(3, "00000000:0050", "0A", 0, 44444), // 0.0.0.0:80, root's
		"net/tcp6": header +
			line(0, "00000000000000000000000001000000:0BB8", "0A", 1000, 22223), // [::1]:3000
		// starttime is the 22nd field: 360000 ticks is an hour after boot.
		"4242/stat":    "4242 (node) S 4000 4242 4000 0 -1 4194304 1 0 0 0 5 1 0 0 20 0 11 0 360000 1 1\n",
		"4242/cmdline": "node\x00/srv/acme/node_modules/.bin/next\x00dev\x00",
		// A name with a space and a bracket in it must not shift the
		// fields.
		"611/stat":    "611 (postgres: main (x)) S 1 611 611 0 -1 4194304 1 0 0 0 5 1 0 0 20 0 1 0 100 1 1\n",
		"611/cmdline": "/usr/lib/postgresql/17/bin/postgres\x00-D\x00/var/lib/postgresql/17/main\x00",
		"self/stat":   "not a number: skipped\n",
	})
	link := func(target, name string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	link("/dev/null", "4242/fd/0")
	link("socket:[22222]", "4242/fd/20")
	link("socket:[22223]", "4242/fd/21")
	link("socket:[33333]", "4242/fd/22")
	link("/srv/acme", "4242/cwd")
	link("/usr/bin/node", "4242/exe")
	link("socket:[11111]", "611/fd/5")
	link("/var/lib/postgresql/17/main", "611/cwd")
	link("/usr/lib/postgresql/17/bin/postgres (deleted)", "611/exe")

	p := procfs{root: root, userHZ: 100}
	socks, limits := p.sockets()
	if limits.Method != "procfs" || limits.Complete || limits.UnownedPorts != 1 || limits.Note == "" {
		t.Errorf("limits = %+v", limits)
	}
	type sock struct {
		pid, port int
		ip        string
	}
	got := map[sock]bool{}
	for _, s := range socks {
		got[sock{s.pid, s.port, s.ip.String()}] = true
	}
	want := []sock{{611, 5432, "127.0.0.1"}, {4242, 3000, "0.0.0.0"}, {4242, 3000, "::1"}, {0, 80, "0.0.0.0"}}
	if len(got) != len(want) {
		t.Errorf("sockets = %v", got)
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing %+v in %v", w, got)
		}
	}

	node, ok := p.process(4242)
	if !ok || node.ppid != 4000 || node.name != "node" || node.cwd != "/srv/acme" || node.exe != "/usr/bin/node" ||
		strings.Join(node.args, " ") != "node /srv/acme/node_modules/.bin/next dev" || node.started.Unix() != 1791600000+3600 {
		t.Errorf("node = %+v", node)
	}
	pg, ok := p.process(611)
	if !ok || pg.ppid != 1 || pg.name != "postgres: main (x)" || pg.exe != "/usr/lib/postgresql/17/bin/postgres" || pg.started.Unix() != 1791600001 {
		t.Errorf("postgres = %+v", pg)
	}
	if _, ok := p.process(999); ok {
		t.Error("a process that is not there was described")
	}

	// And the whole of it, through the same path the daemon takes.
	ls, _ := listening(p, 1, time.Unix(1791610000, 0))
	var seen []string
	for _, l := range ls {
		seen = append(seen, fmt.Sprintf("%d %s %s pid=%d %s", l.Port, l.Kind, l.Scope, l.PID, l.What))
	}
	wantList := "3000 dev-server all pid=4242 next dev|5432 postgres loopback pid=611 Postgres|80 other all pid=0 "
	if strings.Join(seen, "|") != wantList {
		t.Errorf("listeners:\n got %s\nwant %s", strings.Join(seen, "|"), wantList)
	}
}

func TestProcAddr(t *testing.T) {
	cases := map[string]string{
		"0100007F:1538":                         "127.0.0.1:5432",
		"00000000:0BB8":                         "0.0.0.0:3000",
		"0A00020F:01BB":                         "15.2.0.10:443",
		"00000000000000000000000000000000:1F90": "[::]:8080",
		"00000000000000000000000001000000:0BB8": "[::1]:3000",
		"0000000000000000FFFF00000100007F:0050": "[::ffff:127.0.0.1]:80",
	}
	for in, want := range cases {
		ip, port, ok := procAddr(in)
		if !ok {
			t.Errorf("procAddr(%q) failed", in)
			continue
		}
		if got := net.JoinHostPort(net.IP(append([]byte{}, ip...)).String(), strconv.Itoa(port)); got != want && !(len(ip) == 16 && ip.To4() != nil) {
			t.Errorf("procAddr(%q) = %s, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"", "nonsense", "0100007F", "0100007F:zz", "01:0050"} {
		if _, _, ok := procAddr(bad); ok {
			t.Errorf("procAddr(%q) was accepted", bad)
		}
	}
}

// The real thing, on whatever this runs on: this process listens, and so
// does a child of it in another folder, and both are found with what is
// true of them. It needs no network: the sockets are loopback and nothing
// connects to them.
func TestListeningForReal(t *testing.T) {
	sys := hostSystem()
	if _, limits := sys.sockets(); limits.Method == "none" {
		t.Skip("no way to list listeners on this system")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ownPort := ln.Addr().(*net.TCPAddr).Port

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(exe, "-test.run=^$", "--database-url", "postgres://app:CHILDSECRET@localhost/acme")
	child.Dir = dir
	child.Env = append(os.Environ(), "LEADLINE_DISCOVER_HELPER=listen", "CHILD_ENV_SECRET=ENVSECRETVALUE")
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stdin.Close()
		child.Wait()
	}()
	first, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	childPort, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil {
		t.Fatalf("the helper said %q", first)
	}

	now := time.Now()
	find := func(self int) map[int]Listener {
		ls, _ := listening(sys, self, now)
		out := map[int]Listener{}
		for _, l := range ls {
			out[l.Port] = l
		}
		return out
	}

	all := find(-1)
	own, ok := all[ownPort]
	if !ok {
		t.Fatalf("this process's own listener on %d was not found among %d", ownPort, len(all))
	}
	cwd, _ := os.Getwd()
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	if own.PID != os.Getpid() || own.Scope != "loopback" || own.Address != "127.0.0.1" || own.Cwd != cwd {
		t.Errorf("own listener = %+v (want pid %d, cwd %s)", own, os.Getpid(), cwd)
	}
	kid, ok := all[childPort]
	if !ok {
		t.Fatalf("the child's listener on %d was not found", childPort)
	}
	if kid.PID != child.Process.Pid || kid.Cwd != dir || kid.Process != filepath.Base(exe) {
		t.Errorf("child listener = %+v (want pid %d, cwd %s, process %s)", kid, child.Process.Pid, dir, filepath.Base(exe))
	}
	if kid.StartedMs == 0 || kid.UptimeS < 0 || kid.UptimeS > 300 || now.Sub(time.UnixMilli(int64(kid.StartedMs))) > 5*time.Minute {
		t.Errorf("child start time is off: started_ms=%v uptime_s=%d", kid.StartedMs, kid.UptimeS)
	}
	if !strings.Contains(kid.Command, "--database-url") {
		t.Errorf("child command line = %q", kid.Command)
	}
	// Its arguments are read; a URL's password in them is hidden, and its
	// environment is not read at all.
	if strings.Contains(kid.Command, "CHILDSECRET") || strings.Contains(kid.Command, "ENVSECRETVALUE") || strings.Contains(kid.Command, "CHILD_ENV_SECRET") {
		t.Errorf("child command line shows a secret: %q", kid.Command)
	}

	// With this process as the daemon, it and what it started are left
	// out.
	mine := find(os.Getpid())
	if _, ok := mine[ownPort]; ok {
		t.Error("the daemon's own listener was listed")
	}
	if _, ok := mine[childPort]; ok {
		t.Error("a listener the daemon started was listed")
	}
}
