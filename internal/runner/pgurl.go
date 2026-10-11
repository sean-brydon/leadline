package runner

import (
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// PGTarget is one environment variable that holds the URL of a Postgres on
// this machine. It never carries the URL's user or password out of this
// file: String and the fields for display hold the address alone.
type PGTarget struct {
	// The variable, as in DATABASE_URL.
	Key string
	// Where the database listens, host:port.
	Upstream string
	// The .env file the variable came from; empty when it was in the
	// process's own environment.
	File string

	raw string
}

func (t PGTarget) String() string { return t.Key + " → " + t.Upstream }

// Through returns the variable's value with the database's address replaced
// by listen, and everything else (user, password, database, options) as it
// was written.
func (t PGTarget) Through(listen string) string {
	start, end, ok := hostSpan(t.raw)
	if !ok {
		return t.raw
	}
	return t.raw[:start] + listen + t.raw[end:]
}

// FindPostgres looks through the environment the command will get, and then
// the project's .env files, for variables holding a Postgres URL that points
// at this machine. A remote database, a Unix socket, a URL naming several
// hosts and a connection that insists on TLS (which the proxy cannot read)
// are all left out, so they are left alone.
func FindPostgres(environ []string, dotenv Dotenv) []PGTarget {
	var out []PGTarget
	seen := map[string]bool{}
	for _, kv := range environ {
		key, value, _ := strings.Cut(kv, "=")
		seen[key] = true
		if upstream, ok := localPostgres(value); ok {
			out = append(out, PGTarget{Key: key, Upstream: upstream, raw: value})
		}
	}
	for key, value := range dotenv.Values {
		if seen[key] {
			continue
		}
		if upstream, ok := localPostgres(value); ok {
			out = append(out, PGTarget{Key: key, Upstream: upstream, File: dotenv.Source[key], raw: value})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// localPostgres reports the host:port of a Postgres URL on this machine.
func localPostgres(value string) (string, bool) {
	if !strings.Contains(value, "://") {
		return "", false
	}
	u, err := url.Parse(strings.TrimPrefix(value, "jdbc:"))
	if err != nil {
		return "", false
	}
	// postgres, postgresql, and SQLAlchemy's postgresql+psycopg and the like.
	scheme, _, _ := strings.Cut(strings.ToLower(u.Scheme), "+")
	if scheme != "postgres" && scheme != "postgresql" {
		return "", false
	}
	host := u.Hostname()
	if host == "" || strings.Contains(u.Host, ",") || !isLocalHost(host) {
		return "", false
	}
	q := u.Query()
	// libpq lets the query string name the real host, or a socket's folder.
	if q.Get("host") != "" || q.Get("hostaddr") != "" {
		return "", false
	}
	switch strings.ToLower(q.Get("sslmode")) {
	case "require", "verify-ca", "verify-full":
		return "", false
	}
	switch strings.ToLower(q.Get("ssl")) {
	case "true", "1", "require":
		return "", false
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	if _, _, ok := hostSpan(value); !ok {
		return "", false
	}
	// "Any address" is where a server listens, not somewhere to dial.
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), true
}

// spellUpstream spells a local server's address one way, so that
// localhost:5432 and 127.0.0.1:5432 are found to be the same server.
func spellUpstream(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if host = strings.ToLower(host); host == "localhost" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func isLocalHost(host string) bool {
	host = strings.ToLower(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// hostSpan finds the host and port in a URL as it was written: after the
// last @ of the authority, up to the path. Cutting the string there, and
// not printing a parsed URL back, keeps a password's every byte as it was.
func hostSpan(raw string) (start, end int, ok bool) {
	i := strings.Index(raw, "://")
	if i < 0 {
		return 0, 0, false
	}
	start = i + 3
	end = len(raw)
	if j := strings.IndexAny(raw[start:], "/?#"); j >= 0 {
		end = start + j
	}
	if at := strings.LastIndex(raw[start:end], "@"); at >= 0 {
		start += at + 1
	}
	return start, end, start < end
}

// scheme://user:password@, with the password as the part to hide. It runs
// to the last @ before the path, since a password may hold one.
var urlPassword = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://[^\s/:@'"]*:)[^\s/'"]+@`)

// Redact hides the password of any URL in s, for text that is shown or
// sent: a command line can carry one (`psql postgres://app:secret@host/db`).
func Redact(s string) string {
	return urlPassword.ReplaceAllString(s, "${1}***@")
}
