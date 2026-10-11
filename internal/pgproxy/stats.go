package pgproxy

// Stats is what one proxy has carried, for whoever shows a person that
// their app really is connected through it.
type Stats struct {
	// Accepted is every client connection since the proxy opened, Open
	// those still up.
	Accepted, Open int64
	// Unread is connections that were relayed without being read: their
	// statements were not recorded.
	Unread int64
	// LastConnNs is when the newest connection arrived, in unix
	// nanoseconds; 0 when none has.
	LastConnNs int64
}

func (p *Proxy) Stats() Stats {
	return Stats{
		Accepted:   p.accepted.Load(),
		Open:       p.open.Load(),
		Unread:     p.fallbacks.Load(),
		LastConnNs: p.lastConn.Load(),
	}
}

// Upstream normalises an upstream address the way the -pg flag does: a bare
// port is that port on 127.0.0.1, a host alone gets Postgres's port, and a
// path is a unix socket.
func Upstream(s string) (string, error) { return upstreamAddr(s) }
