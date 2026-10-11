package integrations

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// A small Redis client: the RESP2 protocol is five reply types, and what
// Leadline asks (INFO, SCAN, SLOWLOG, MONITOR) needs nothing a library
// would add.

// respError is an error reply from the server, as it sent it.
type respError string

func (e respError) Error() string { return string(e) }

var errNotRedis = errors.New("not a redis reply")

const (
	redisDial    = 5 * time.Second
	redisTimeout = 3 * time.Second
	// A reply larger than this is not one Leadline asked for.
	maxBulk  = 16 << 20
	maxArray = 1 << 20
)

type respConn struct {
	c net.Conn
	r *bufio.Reader
	// seenAs is this connection's address as the server sees it, when it
	// was asked for: MONITOR names clients by it.
	seenAs string
	// name is what the connection called itself to the server; see setName.
	name string
}

// dialRedis connects, authenticates and selects the URL's database.
func dialRedis(ctx context.Context, t *target) (*respConn, error) {
	d := net.Dialer{Timeout: redisDial}
	network, addr := "tcp", t.addr()
	if t.socket() != "" {
		network = "unix"
	}
	nc, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	if t.tls {
		tc := tls.Client(nc, &tls.Config{ServerName: t.host, MinVersion: tls.VersionTLS12})
		nc.SetDeadline(time.Now().Add(redisDial))
		if err := tc.HandshakeContext(ctx); err != nil {
			nc.Close()
			return nil, err
		}
		nc = tc
	}
	c := &respConn{c: nc, r: bufio.NewReaderSize(nc, 32<<10)}
	if err := c.hello(t); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *respConn) hello(t *target) error {
	if t.password != "" {
		auth := []string{"AUTH", t.password}
		if t.user != "" {
			auth = []string{"AUTH", t.user, t.password}
		}
		if _, err := c.do(auth...); err != nil {
			// A password the server has no use for is not a reason to
			// fail: everything else will work.
			var re respError
			if !errors.As(err, &re) || !strings.Contains(string(re), "no password") && !strings.Contains(string(re), "without any password") {
				return err
			}
		}
	}
	// Before anything else is said on it, so that a live capture watching
	// this server knows the connection for Leadline's from its first line.
	c.setName()
	if t.database != "" && t.database != "0" {
		if _, err := c.do("SELECT", t.database); err != nil {
			return err
		}
	}
	// The first real command: it is where a missing password, or a server
	// that isn't Redis, shows.
	if _, err := c.do("PING"); err != nil {
		return err
	}
	return nil
}

// ownNamePrefix begins the name every connection of Leadline's own gives
// itself. MONITOR shows the naming like any other command, and that is how
// a live capture tells Leadline's connections from an app's: by the first
// thing each one says, before the address it came from could be known any
// other way.
const ownNamePrefix = "leadline-"

// setName names the connection. A server that refuses (an old one, or a
// user that may not) is not a reason to fail: the name is only a courtesy
// to captures.
func (c *respConn) setName() {
	var b [6]byte
	rand.Read(b[:])
	name := ownNamePrefix + hex.EncodeToString(b[:])
	if _, err := c.do("CLIENT", "SETNAME", name); err == nil {
		c.name = name
	}
}

// isOwnName reports whether a command is a connection of Leadline's naming
// itself.
func isOwnName(args []string) bool {
	return len(args) == 3 && strings.EqualFold(args[0], "CLIENT") && strings.EqualFold(args[1], "SETNAME") &&
		strings.HasPrefix(args[2], ownNamePrefix)
}

// identify finds this connection's address as the server sees it, by
// looking up the name it gave itself.
func (c *respConn) identify() {
	if c.name == "" {
		if c.setName(); c.name == "" {
			return
		}
	}
	name := c.name
	list, err := c.do("CLIENT", "LIST")
	if err != nil {
		return
	}
	text, _ := list.(string)
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "name="+name) {
			continue
		}
		for _, field := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(field, "addr="); ok {
				c.seenAs = v
			}
		}
	}
}

func (c *respConn) close() { c.c.Close() }

// do sends one command and reads its reply. An error reply comes back as a
// respError; anything else that goes wrong means the connection is done.
func (c *respConn) do(args ...string) (any, error) {
	c.c.SetDeadline(time.Now().Add(redisTimeout))
	if err := c.send(args); err != nil {
		return nil, err
	}
	return c.read()
}

// pipeline sends several commands together and reads a reply for each. A
// command's error reply is its entry, as a respError, not the call's.
func (c *respConn) pipeline(cmds [][]string) ([]any, error) {
	c.c.SetDeadline(time.Now().Add(redisTimeout))
	var buf []byte
	for _, args := range cmds {
		buf = appendCommand(buf, args)
	}
	if _, err := c.c.Write(buf); err != nil {
		return nil, err
	}
	out := make([]any, len(cmds))
	for i := range cmds {
		v, err := c.read()
		var re respError
		switch {
		case errors.As(err, &re):
			out[i] = re
		case err != nil:
			return nil, err
		default:
			out[i] = v
		}
	}
	return out, nil
}

func (c *respConn) send(args []string) error {
	_, err := c.c.Write(appendCommand(nil, args))
	return err
}

func appendCommand(b []byte, args []string) []byte {
	b = append(b, '*')
	b = strconv.AppendInt(b, int64(len(args)), 10)
	b = append(b, '\r', '\n')
	for _, a := range args {
		b = append(b, '$')
		b = strconv.AppendInt(b, int64(len(a)), 10)
		b = append(b, '\r', '\n')
		b = append(b, a...)
		b = append(b, '\r', '\n')
	}
	return b
}

// read reads one reply: a string, an int64, nil, or a []any of those.
func (c *respConn) read() (any, error) {
	line, err := c.line()
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		return nil, errNotRedis
	}
	body := line[1:]
	switch line[0] {
	case '+':
		return body, nil
	case '-':
		return nil, respError(body)
	case ':':
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return nil, errNotRedis
		}
		return n, nil
	case '$':
		n, err := strconv.Atoi(body)
		if err != nil || n > maxBulk {
			return nil, errNotRedis
		}
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(c.r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(body)
		if err != nil || n > maxArray {
			return nil, errNotRedis
		}
		if n < 0 {
			return nil, nil
		}
		out := make([]any, n)
		for i := range out {
			v, err := c.read()
			var re respError
			if errors.As(err, &re) {
				v, err = re, nil
			}
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	return nil, errNotRedis
}

// line reads up to CRLF. A line with no end in 64 KB is not Redis talking.
func (c *respConn) line() (string, error) {
	var long []byte
	for {
		part, err := c.r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			long = append(long, part...)
			if len(long) > 64<<10 {
				return "", errNotRedis
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if long != nil {
			part = append(long, part...)
		}
		return strings.TrimRight(string(part), "\r\n"), nil
	}
}

// ---- replies, read loosely ----

func asInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	}
	return ""
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// parseInfo reads INFO's "name:value" lines, whatever the section.
func parseInfo(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[k] = v
		}
	}
	return out
}

func infoInt(info map[string]string, key string) int64 {
	n, _ := strconv.ParseInt(info[key], 10, 64)
	return n
}

func infoFloat(info map[string]string, key string) float64 {
	f, _ := strconv.ParseFloat(info[key], 64)
	return f
}

func redisCommandError(v any) error {
	if re, ok := v.(respError); ok {
		return re
	}
	return nil
}

func unexpected(cmd string, v any) error {
	return fmt.Errorf("%w: %s answered %T", errNotRedis, cmd, v)
}
