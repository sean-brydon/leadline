package pgproxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"time"
)

// What a client can open with instead of a StartupMessage. Each is a
// length, then one of these in the place of a protocol version.
const (
	cancelRequest = 80877102
	sslRequest    = 80877103
	gssEncRequest = 80877104

	// The server refuses startup packets longer than this.
	maxStartup = 10000
)

func (p *Proxy) handle(client net.Conn) {
	defer p.release(client)

	first, m, err := p.startup(client)
	if err != nil {
		return
	}
	if m != nil && p.route.Service != "" {
		m.named = p.route.Service
		m.service = serviceName(m.app, m.named, m.database)
	}
	server, err := p.dial()
	if err != nil {
		p.warn("pg proxy: can't reach postgres", "upstream", p.route.Upstream, "err", err)
		if m != nil {
			client.Write(fatal("08001", fmt.Sprintf("Leadline's proxy on %s could not reach Postgres at %s: %v",
				p.route.Listen, p.route.Upstream, err)))
		}
		return
	}
	if !p.hold(server) {
		server.Close()
		return
	}
	defer p.release(server)
	if _, err := server.Write(first); err != nil {
		return
	}

	c := &conn{proxy: p}
	var front, back *scanner
	if m != nil {
		m.upstream = p.route.Upstream
		c.t = &tracker{rec: p.rec, meta: m}
		front = &scanner{tap: frontTap{c.t}}
		back = &scanner{tap: backTap{c.t}}
	} else if !isCancel(first) {
		p.fallbacks.Add(1)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.pump(server, client, front)
	}()
	c.pump(client, server, back)
	<-done

	if c.t != nil {
		c.t.mu.Lock()
		c.t.now = time.Now().UnixNano()
		c.t.stop(true)
		c.t.mu.Unlock()
	}
}

// startup reads what the client opens with, up to and including its
// StartupMessage, and answers requests for encryption itself. first is
// what to send the server. A nil meta means the proxy doesn't know what
// this connection is (or doesn't need to: a cancel request) and should
// relay it as it comes.
func (p *Proxy) startup(client net.Conn) (first []byte, m *meta, err error) {
	var head [8]byte
	// A client may ask for GSS encryption, then TLS, then give up on both.
	for range 4 {
		if _, err := io.ReadFull(client, head[:4]); err != nil {
			return nil, nil, err
		}
		n := int(binary.BigEndian.Uint32(head[:4]))
		if n < 8 || n > maxStartup {
			return bytes.Clone(head[:4]), nil, nil
		}
		if _, err := io.ReadFull(client, head[4:]); err != nil {
			return nil, nil, err
		}
		code := binary.BigEndian.Uint32(head[4:])
		switch {
		case n == 8 && (code == sslRequest || code == gssEncRequest):
			// "No": the client carries on in plaintext, which is the only
			// way the proxy can read the queries. A client that insists
			// on TLS (sslmode=require) gives up here.
			if _, err := client.Write([]byte{'N'}); err != nil {
				return nil, nil, err
			}
			continue
		case code>>16 == 3 || code == cancelRequest:
			msg := make([]byte, n)
			copy(msg, head[:])
			if _, err := io.ReadFull(client, msg[8:]); err != nil {
				return nil, nil, err
			}
			if code == cancelRequest {
				return msg, nil, nil
			}
			return msg, startupMeta(msg[8:]), nil
		}
		return bytes.Clone(head[:]), nil, nil
	}
	return nil, nil, errors.New("client kept asking for encryption")
}

// isCancel reports whether a connection's opening is a CancelRequest, which
// is relayed unread by design rather than for want of understanding.
func isCancel(first []byte) bool {
	return len(first) >= 8 && binary.BigEndian.Uint32(first[4:]) == cancelRequest
}

// startupMeta reads the parameters of a StartupMessage: names and values,
// each NUL-terminated, then a final NUL.
func startupMeta(b []byte) *meta {
	m := &meta{}
	app := ""
	for len(b) > 1 {
		name, rest, cut := cstring(b)
		if cut {
			return nil
		}
		value, rest, cut := cstring(rest)
		if cut {
			return nil
		}
		b = rest
		switch name {
		case "user":
			m.user = value
		case "database":
			m.database = value
		case "application_name":
			app = value
		case "replication":
			// A replication connection speaks its own protocol.
			if value != "false" && value != "off" && value != "0" && value != "no" {
				return nil
			}
		}
	}
	if m.database == "" {
		m.database = m.user
	}
	m.app = app
	m.service = serviceName(app, "", m.database)
	return m
}

// fatal builds the ErrorResponse a client sees when the proxy itself has
// to refuse it.
func fatal(code, message string) []byte {
	b := []byte{'E', 0, 0, 0, 0}
	for _, f := range []struct {
		field byte
		value string
	}{{'S', "FATAL"}, {'V', "FATAL"}, {'C', code}, {'M', message}} {
		b = append(b, f.field)
		b = append(b, f.value...)
		b = append(b, 0)
	}
	b = append(b, 0)
	binary.BigEndian.PutUint32(b[1:], uint32(len(b)-1))
	return b
}

// conn is one client connection and its connection to the server.
type conn struct {
	proxy *Proxy
	t     *tracker
	// plain is set once the tracker has given up, so the relay stops
	// calling it.
	plain atomic.Bool
}

// pump relays src to dst until either ends. The scanner, if there is one,
// is shown each chunk before it is forwarded: before, so that the tracker
// has queued a question by the time its answer can come back.
func (c *conn) pump(dst, src net.Conn, sc *scanner) {
	buf := make([]byte, 64<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if sc != nil && !c.plain.Load() {
				c.tap(sc, buf[:n])
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				src.Close()
				dst.Close()
				return
			}
		}
		if err != nil {
			if err == io.EOF {
				// Pass the end of the stream on, and let the other
				// direction finish what it is saying.
				closeWrite(dst)
			} else {
				src.Close()
				dst.Close()
			}
			return
		}
	}
}

func closeWrite(c net.Conn) {
	if hc, ok := c.(interface{ CloseWrite() error }); ok {
		hc.CloseWrite()
		return
	}
	c.Close()
}

// tap shows the tracker a chunk. Nothing that goes wrong in here may reach
// the relay: an error or a panic ends the tracking, not the connection.
func (c *conn) tap(sc *scanner, p []byte) {
	t := c.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.off {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			c.giveUp(fmt.Errorf("panic: %v", r))
		}
	}()
	t.now = time.Now().UnixNano()
	if err := sc.feed(p); err != nil {
		c.giveUp(err)
	}
}

func (c *conn) giveUp(err error) {
	c.plain.Store(true)
	c.t.stop(false)
	if err == errReplication {
		return
	}
	c.proxy.fallbacks.Add(1)
	slog.Debug("pg proxy: stopped reading a connection; it is relayed as it is", "upstream", c.proxy.route.Upstream, "reason", err.Error())
}
