package fake

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Redis is a pretend Redis server.
type Redis struct {
	// Password, when not empty, must be given with AUTH before anything
	// else.
	Password string
	Version  string

	ln net.Listener
	mu sync.Mutex
	// The keyspace: name to size in bytes. Every key is a string.
	keys map[string]int64
	// Every command clients have sent.
	ran [][]string
	// The connections that asked to MONITOR.
	monitors []net.Conn
	watching chan struct{}
}

// NewRedis starts a pretend Redis on a loopback port, with a password of
// "acme-redis-secret" and a few keys.
func NewRedis(t testing.TB) *Redis { return newRedis(t, "acme-redis-secret") }

// NewOpenRedis starts one that asks for no password.
func NewOpenRedis(t testing.TB) *Redis { return newRedis(t, "") }

func newRedis(t testing.TB, password string) *Redis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &Redis{Password: password, Version: "7.4.0", ln: ln, watching: make(chan struct{}, 16),
		keys: map[string]int64{"user:1": 72, "user:2": 72, "cart:1": 80, "big:blob": 229432}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go r.serve(c)
		}
	}()
	return r
}

// SetKeys replaces the keyspace: name to size in bytes.
func (r *Redis) SetKeys(keys map[string]int64) {
	r.mu.Lock()
	r.keys = keys
	r.mu.Unlock()
}

func (r *Redis) keyspace() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.keys
}

// Addr is host:port.
func (r *Redis) Addr() string { return r.ln.Addr().String() }

// URL is a URL that connects.
func (r *Redis) URL() string {
	if r.Password == "" {
		return "redis://" + r.Addr()
	}
	return "redis://:" + r.Password + "@" + r.Addr() + "/0"
}

// Ran is every command clients have sent, in order.
func (r *Redis) Ran() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.ran...)
}

// WaitMonitor returns once a client is in MONITOR.
func (r *Redis) WaitMonitor(t testing.TB) {
	t.Helper()
	select {
	case <-r.watching:
	case <-time.After(5 * time.Second):
		t.Fatal("no client asked to MONITOR")
	}
}

// Emit sends MONITOR lines to every monitoring client, as if clients had
// run these commands. A line is what follows the "+": see MonitorLine.
func (r *Redis) Emit(lines ...string) {
	var b strings.Builder
	for _, l := range lines {
		b.WriteString("+" + l + "\r\n")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.monitors {
		io.WriteString(c, b.String())
	}
}

// MonitorLine writes a command the way MONITOR reports it.
func MonitorLine(at time.Time, client string, args ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d.%06d [0 %s]", at.Unix(), at.Nanosecond()/1000, client)
	for _, a := range args {
		b.WriteString(" " + strconv.Quote(a))
	}
	return b.String()
}

func (r *Redis) serve(c net.Conn) {
	defer c.Close()
	in := bufio.NewReader(c)
	authed := r.Password == ""
	name := ""
	for {
		args, err := readCommand(in)
		if err != nil {
			return
		}
		r.mu.Lock()
		r.ran = append(r.ran, args)
		r.mu.Unlock()
		cmd := strings.ToUpper(args[0])
		arg := func(i int) string {
			if i < len(args) {
				return args[i]
			}
			return ""
		}
		var reply string
		switch {
		case cmd == "AUTH":
			pw := args[len(args)-1]
			switch {
			case r.Password == "":
				reply = "-ERR AUTH <password> called without any password configured for the default user. Are you sure your configuration is correct?\r\n"
			case pw != r.Password:
				reply = "-WRONGPASS invalid username-password pair or user is disabled.\r\n"
			default:
				authed = true
				reply = "+OK\r\n"
			}
		case !authed:
			reply = "-NOAUTH Authentication required.\r\n"
		case cmd == "PING":
			reply = "+PONG\r\n"
		case cmd == "SELECT":
			reply = "+OK\r\n"
		case cmd == "CLIENT" && strings.EqualFold(arg(1), "SETNAME"):
			name = arg(2)
			reply = "+OK\r\n"
		case cmd == "CLIENT" && strings.EqualFold(arg(1), "LIST"):
			reply = bulk(fmt.Sprintf("id=7 addr=%s laddr=%s fd=8 name=%s db=0 cmd=client|list\n", c.RemoteAddr(), c.LocalAddr(), name))
		case cmd == "INFO" && strings.EqualFold(arg(1), "commandstats"):
			reply = bulk("# Commandstats\r\ncmdstat_get:calls=4200,usec=16800,usec_per_call=4.00,rejected_calls=0,failed_calls=0\r\n" +
				"cmdstat_set:calls=300,usec=2400,usec_per_call=8.00,rejected_calls=0,failed_calls=0\r\n" +
				"cmdstat_config|get:calls=2,usec=30,usec_per_call=15.00,rejected_calls=0,failed_calls=0\r\n")
		case cmd == "INFO":
			reply = bulk(r.info())
		case cmd == "CONFIG" && strings.EqualFold(arg(1), "GET"):
			reply = array(bulk(arg(2)), bulk("10000"))
		case cmd == "SLOWLOG":
			reply = array(
				array(":14\r\n", ":1791643000\r\n", ":15234\r\n", array(bulk("KEYS"), bulk("user:*")), bulk("127.0.0.1:50112"), bulk("acme-web")),
				array(":13\r\n", ":1791642000\r\n", ":12001\r\n", array(bulk("AUTH"), bulk("default"), bulk(r.Password)), bulk("127.0.0.1:50110"), bulk("")),
			)
		case cmd == "DBSIZE":
			reply = fmt.Sprintf(":%d\r\n", len(r.keyspace()))
		case cmd == "SCAN":
			reply = r.scan(arg(1), arg(3))
		case cmd == "MEMORY":
			if size, ok := r.keyspace()[arg(2)]; ok {
				reply = fmt.Sprintf(":%d\r\n", size)
			} else {
				reply = "$-1\r\n"
			}
		case cmd == "TYPE":
			reply = "+string\r\n"
		case cmd == "MONITOR":
			if _, err := io.WriteString(c, "+OK\r\n"); err != nil {
				return
			}
			r.mu.Lock()
			r.monitors = append(r.monitors, c)
			r.mu.Unlock()
			r.watching <- struct{}{}
			io.Copy(io.Discard, in) // until the client hangs up
			r.mu.Lock()
			for i, m := range r.monitors {
				if m == c {
					r.monitors = append(r.monitors[:i], r.monitors[i+1:]...)
					break
				}
			}
			r.mu.Unlock()
			return
		default:
			reply = "-ERR unknown command '" + args[0] + "'\r\n"
		}
		if _, err := io.WriteString(c, reply); err != nil {
			return
		}
	}
}

func (r *Redis) info() string {
	return "# Server\r\nredis_version:" + r.Version + "\r\nredis_mode:standalone\r\nuptime_in_seconds:1100\r\n" +
		"# Clients\r\nconnected_clients:3\r\nblocked_clients:0\r\nmaxclients:10000\r\n" +
		"# Memory\r\nused_memory:1145744\r\nused_memory_rss:6270976\r\nused_memory_peak:1200000\r\nmaxmemory:0\r\nmaxmemory_policy:noeviction\r\nmem_fragmentation_ratio:5.94\r\n" +
		"# Stats\r\ntotal_commands_processed:4502\r\ninstantaneous_ops_per_sec:12\r\nkeyspace_hits:900\r\nkeyspace_misses:100\r\nexpired_keys:4\r\nevicted_keys:0\r\n" +
		"# Keyspace\r\ndb0:keys=" + strconv.Itoa(len(r.keyspace())) + ",expires=2,avg_ttl=60000\r\n"
}

// scan pages through the keys in name order; the cursor is an index.
func (r *Redis) scan(cursor, count string) string {
	keys := r.keyspace()
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	from, _ := strconv.Atoi(cursor)
	n, _ := strconv.Atoi(count)
	if n <= 0 {
		n = 10
	}
	to := min(from+n, len(names))
	next := strconv.Itoa(to)
	if to >= len(names) {
		next = "0"
	}
	items := make([]string, 0, to-from)
	for _, k := range names[min(from, len(names)):to] {
		items = append(items, bulk(k))
	}
	return array(bulk(next), array(items...))
}

func bulk(s string) string { return fmt.Sprintf("$%d\r\n%s\r\n", len(s), s) }

func array(items ...string) string {
	return fmt.Sprintf("*%d\r\n%s", len(items), strings.Join(items, ""))
}

// readCommand reads one command: an array of bulk strings.
func readCommand(in *bufio.Reader) ([]string, error) {
	line, err := in.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("not a command: %q", line)
	}
	n, err := strconv.Atoi(strings.TrimSpace(line[1:]))
	if err != nil || n < 1 {
		return nil, fmt.Errorf("bad array: %q", line)
	}
	args := make([]string, n)
	for i := range args {
		head, err := in.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(head, "$")))
		if err != nil {
			return nil, fmt.Errorf("bad bulk: %q", head)
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(in, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:size])
	}
	return args, nil
}
