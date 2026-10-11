package runner

import (
	"errors"
	"net"
	"net/url"
	"sort"
	"strings"
)

// ConnURL is a database or cache URL found for a worktree: a Postgres or a
// Redis. Like PGTarget it keeps the URL to itself. What it shows is the
// variables it was found in and where the server is; the URL, password and
// all, goes only to the daemon, in Attach.
type ConnURL struct {
	// Keys are the variables that hold it, as in DATABASE_URL, in order.
	Keys []string
	// Kind is "postgres" or "redis".
	Kind string
	// Addr is where the server listens, host:port, and Database which of
	// its databases: a name for Postgres, a number for Redis.
	Addr     string
	Database string
	// Local says the server is on this machine.
	Local bool

	raw string
}

// String names the connection without its user or password.
func (c ConnURL) String() string {
	what := "Postgres"
	if c.Kind == "redis" {
		what = "Redis"
	}
	s := what + " " + c.Addr
	if c.Database != "" {
		s += "/" + c.Database
	}
	return s
}

// FindConnURLs looks through an environment, and then a project's .env
// files, for variables that hold the URL of a Postgres or a Redis. Nothing
// else is taken from either: a variable is looked at only to see whether it
// is such a URL. The same URL under two names (DATABASE_URL and
// DIRECT_URL) is one connection with both names.
func FindConnURLs(environ []string, dotenvs ...Dotenv) []ConnURL {
	var out []ConnURL
	at := map[string]int{}
	add := func(key, value string) {
		c, ok := connURL(value)
		if !ok {
			return
		}
		if i, seen := at[c.raw]; seen {
			out[i].Keys = append(out[i].Keys, key)
			return
		}
		c.Keys = []string{key}
		at[c.raw] = len(out)
		out = append(out, c)
	}
	seen := map[string]bool{}
	var keys []string
	values := map[string]string{}
	for _, kv := range environ {
		key, value, _ := strings.Cut(kv, "=")
		if !seen[key] {
			keys = append(keys, key)
		}
		// The last one wins, as it does for the process that gets them.
		seen[key], values[key] = true, value
	}
	for _, d := range dotenvs {
		for key, value := range d.Values {
			// The environment outranks the files, and the first file the
			// later ones.
			if !seen[key] {
				seen[key], values[key] = true, value
				keys = append(keys, key)
			}
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		add(key, values[key])
	}
	return out
}

// ParseConnFlag reads one --url KEY=URL.
func ParseConnFlag(arg string) (ConnURL, error) {
	key, value, ok := strings.Cut(arg, "=")
	key = strings.TrimSpace(key)
	// A URL has an = of its own only after its ://, in its options.
	if !ok || key == "" || strings.Contains(key, "://") {
		return ConnURL{}, errors.New("--url takes KEY=URL, as in DATABASE_URL=postgres://localhost:5432/acme")
	}
	c, ok := connURL(strings.TrimSpace(value))
	if !ok {
		// Never the value: it has a password in it.
		return ConnURL{}, errors.New("--url " + key + ": not a postgres:// or redis:// URL")
	}
	c.Keys = []string{key}
	return c, nil
}

// connURL reads a value as a Postgres or Redis URL.
func connURL(value string) (ConnURL, bool) {
	value = strings.TrimSpace(value)
	i := strings.Index(value, "://")
	if i <= 0 {
		return ConnURL{}, false
	}
	// postgresql+psycopg:// is SQLAlchemy's way of naming the driver; the
	// database is the same one.
	scheme, _, _ := strings.Cut(strings.ToLower(value[:i]), "+")
	c := ConnURL{}
	port := ""
	switch scheme {
	case "postgres", "postgresql":
		c.Kind, port = "postgres", "5432"
	case "redis", "rediss":
		c.Kind, port = "redis", "6379"
	default:
		return ConnURL{}, false
	}
	// The scheme is rewritten, never the rest: a password keeps its every
	// byte.
	c.raw = scheme + value[i:]
	u, err := url.Parse(c.raw)
	if err != nil || strings.Contains(u.Host, ",") {
		return ConnURL{}, false
	}
	host := u.Hostname()
	if host == "" {
		// libpq's "the socket, or localhost"; Redis's localhost.
		host = "localhost"
	}
	if p := u.Port(); p != "" {
		port = p
	}
	c.Addr = net.JoinHostPort(host, port)
	c.Database = strings.Trim(u.Path, "/")
	if c.Kind == "redis" && c.Database == "" {
		c.Database = "0"
	}
	c.Local = isLocalHost(host) && u.Query().Get("host") == ""
	return c, true
}
