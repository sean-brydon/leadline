package discover

import (
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// mask stands where a password was, as in the integrations API.
const mask = "****"

// target is a found connection URL taken apart. Everything in it but
// connect is safe to show.
type target struct {
	kind     string
	masked   string
	hostname string // the host alone; "" when the URL names none
	port     int
	database string
	user     string
	remote   bool

	connectable bool
	reason      string
	note        string

	// connect is the URL to hand the integrations manager: the URL as
	// written, with its scheme in the spelling integrations reads and
	// nothing else changed. What an ORM put in it for itself
	// (`?schema=public`) stays: the saved connection is the project's own
	// URL, and the line Leadline gives back for the project's .env has to
	// keep it. The manager leaves those parameters out when it connects.
	// It holds the password and never leaves the package.
	connect string
	// identity is what makes two found URLs the same connection: the
	// server, the database, the user and the password. Two spellings of
	// one (localhost and 127.0.0.1, with an ORM's parameter and without)
	// have one identity. It holds the password too. Empty for a URL that
	// would not parse, which is only ever the same as itself.
	identity string
}

// addr is where the server is, as a saved connection's `host` has it.
func (t *target) addr() string {
	if t.hostname == "" {
		return ""
	}
	if t.port == 0 {
		return t.hostname
	}
	return net.JoinHostPort(t.hostname, strconv.Itoa(t.port))
}

var defaultPorts = map[string]int{"postgres": 5432, "redis": 6379, "mysql": 3306, "mongodb": 27017}

const (
	noteNotSupported = "Leadline connects to Postgres and Redis. This one was found, and can't be connected yet."
	noteInterpolated = "This URL is filled in from other variables, which Leadline does not read. Add it by hand with the finished URL."
	noteBadURL       = "This URL can't be read as written. A password with / ? # or @ in it needs percent-encoding."
)

// describeURL takes a found URL apart for showing and for saving.
func describeURL(u envURL) *target {
	t := &target{kind: u.kind, port: defaultPorts[u.kind]}
	norm := normalScheme(u.raw)

	parsed, err := url.Parse(norm)
	if err != nil || parsed.Host == "" && parsed.Path == "" || misplacedAt(norm) {
		// Not a URL anything will read the way it was meant. It is shown,
		// masked the blunt way, and can't be connected.
		t.masked = maskRough(u.raw, u.kind)
		t.reason, t.note = "bad_url", noteBadURL
		if u.refs {
			t.reason, t.note = "interpolated", noteInterpolated
		}
		// Where it points is still worth saying when that much is plain:
		// what follows the last @, if it is a host and nothing cleverer.
		if host, port, ok := roughHost(t.masked); ok {
			t.hostname = host
			if port > 0 {
				t.port = port
			}
			t.remote = !localHost(host)
		}
		return t
	}

	q := parsed.Query()
	t.hostname = parsed.Hostname()
	if p, err := strconv.Atoi(parsed.Port()); err == nil && p > 0 {
		t.port = p
	}
	if first, _, several := strings.Cut(parsed.Host, ","); several {
		// Several hosts: the first is the one that is shown, as the
		// Postgres driver takes the first for its primary.
		t.hostname, t.port = first, defaultPorts[u.kind]
		if h, p, err := net.SplitHostPort(first); err == nil {
			t.hostname = h
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				t.port = n
			}
		}
	}
	if parsed.User != nil {
		t.user = parsed.User.Username()
	}
	t.database = strings.TrimPrefix(parsed.Path, "/")
	switch u.kind {
	case "postgres":
		// libpq lets the query string name what the authority leaves out.
		if t.hostname == "" {
			t.hostname = q.Get("host")
		}
		if t.user == "" {
			t.user = q.Get("user")
		}
		if t.database == "" {
			t.database = q.Get("dbname")
		}
	case "redis":
		if t.hostname == "" {
			t.hostname = "localhost"
		}
		if t.database == "" {
			t.database = "0"
		}
		// redis://secret@host is how some clients spell a password with
		// no user, so a lone user part is treated as one.
		if parsed.User != nil {
			if _, has := parsed.User.Password(); !has {
				t.user = ""
			}
		}
	case "mongodb":
		if strings.Contains(strings.ToLower(parsed.Scheme), "+srv") {
			t.port = 0
		}
	}
	t.masked = maskURL(parsed, u.kind)
	t.remote = !localHost(t.hostname)
	password := q.Get("password")
	if parsed.User != nil {
		if pw, has := parsed.User.Password(); has {
			password = pw
		} else if u.kind == "redis" {
			password = parsed.User.Username()
		}
	}
	t.identity = strings.Join([]string{u.kind, serverKey(t.addr()), t.database, t.user, password}, "\x00")

	switch {
	case u.refs:
		t.reason, t.note = "interpolated", noteInterpolated
	case u.kind != "postgres" && u.kind != "redis":
		t.reason, t.note = "not_supported", noteNotSupported
	default:
		t.connectable = true
		t.connect = norm
	}
	return t
}

var plainHost = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)

// roughHost reads the host and port out of a URL that would not parse,
// from its masked form: only when they are plainly a name and a number.
func roughHost(masked string) (host string, port int, ok bool) {
	i := strings.Index(masked, "://")
	if i < 0 {
		return "", 0, false
	}
	rest := masked[i+3:]
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	host = rest
	if h, p, found := strings.Cut(rest, ":"); found {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, false
		}
		host, port = h, n
	}
	if !plainHost.MatchString(host) {
		return "", 0, false
	}
	return host, port, true
}

// normalScheme rewrites a URL's scheme to the plain one: `jdbc:` off the
// front, `+driver` out of the scheme. It cuts the string and leaves every
// other byte, the password's among them, as it was written.
func normalScheme(raw string) string {
	if len(raw) > 5 && strings.EqualFold(raw[:5], "jdbc:") {
		raw = raw[5:]
	}
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	scheme := raw[:i]
	if plus := strings.IndexByte(scheme, '+'); plus > 0 && !strings.EqualFold(scheme[plus:], "+srv") {
		scheme = scheme[:plus]
	}
	return scheme + raw[i:]
}

// misplacedAt reports a URL whose authority has no user part although an
// @ comes later: a password with an unescaped / ? or # in it, which a
// parser splits in the wrong place. Parsed as it stands, the password
// would pass for a port or a path and be shown.
func misplacedAt(raw string) bool {
	i := strings.Index(raw, "://")
	if i < 0 {
		return false
	}
	rest := raw[i+3:]
	end := len(rest)
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		end = j
	}
	return !strings.Contains(rest[:end], "@") && strings.Contains(rest[end:], "@")
}

// secretParams are query parameters that carry a credential. The
// integrations package masks the first two; the rest are here because a
// found URL may be for a driver it has never met.
var secretParams = []string{"password", "sslpassword", "pass", "pwd", "passwd", "token", "authtoken", "auth_token",
	"access_token", "secret", "apikey", "api_key", "sslkey"}

func isSecretParam(name string) bool {
	name = strings.ToLower(name)
	for _, s := range secretParams {
		if name == s {
			return true
		}
	}
	return false
}

// maskURL is a parsed URL with every password replaced by the mask, the
// way integrations.maskURL does it.
func maskURL(in *url.URL, kind string) string {
	u := *in
	// The mask goes in as a token and is swapped afterwards: url.URL
	// would percent-encode the stars.
	const token = "LEADLINEMASKTOKEN"
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), token)
		} else if kind == "redis" {
			u.User = url.User(token)
		}
	}
	if u.Fragment != "" || u.RawFragment != "" {
		// A connection URL has no use for a fragment; a password with a
		// # in it ends up in one.
		u.Fragment, u.RawFragment = token, ""
	}
	if u.RawQuery != "" {
		q := u.Query()
		changed := false
		for name := range q {
			if isSecretParam(name) {
				q.Set(name, token)
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}
	// And once more by pattern: a query that separates with semicolons
	// is not one url.URL takes apart, and would pass through as written.
	return roughParam.ReplaceAllString(strings.ReplaceAll(u.String(), token, mask), "${1}"+mask)
}

var roughParam = regexp.MustCompile(`(?i)([?&;](?:` + strings.Join(secretParams, "|") + `)=)[^&;#]*`)

// maskRough masks a URL that can't be parsed, by its shape alone. It
// hides too much before it hides too little:
//
//   - With no @, there is no user part: only the query and the fragment
//     can hold a credential, and both are masked.
//   - With the last @ inside the authority (nothing before it that ends
//     one), the password is everything from the user part's first colon
//     to that @. A lone user part is hidden for Redis, where it is how a
//     password with no user is sometimes written.
//   - Otherwise a / ? or # comes before the @, and there is no telling
//     a password that holds one from a host followed by a query that
//     holds an @. Everything after the scheme is hidden.
func maskRough(raw, kind string) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return mask
	}
	head, rest := raw[:i+3], raw[i+3:]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return head + maskTail(rest)
	}
	user := rest[:at]
	if strings.ContainsAny(user, "/?#") {
		return head + mask
	}
	if colon := strings.IndexByte(user, ':'); colon >= 0 {
		user = user[:colon] + ":" + mask
	} else if kind == "redis" {
		user = mask
	}
	return head + user + maskTail(rest[at:])
}

// maskTail masks what follows the user part: a credential in the query,
// and the fragment whole, since a connection URL has no use for one and a
// password with a # in it ends up there.
func maskTail(tail string) string {
	if h := strings.IndexByte(tail, '#'); h >= 0 {
		tail = tail[:h] + "#" + mask
	}
	return roughParam.ReplaceAllString(tail, "${1}"+mask)
}

// localHost reports whether host is this machine. It is a copy of
// integrations.localHost, which is not exported: it never asks DNS, so a
// name other than localhost counts as somewhere else. A test checks the
// two agree.
func localHost(host string) bool {
	if host == "" || strings.HasPrefix(host, "/") {
		return true
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if i := strings.IndexByte(h, '%'); i >= 0 { // an IPv6 zone
		h = h[:i]
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	// One of this machine's own addresses, as in a database bound to the
	// LAN interface.
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// serverKey makes two spellings of one local server the same server:
// localhost:5432 and 127.0.0.1:5432. For telling whether a found URL is
// already a saved connection, and which listener it points at.
func serverKey(hostport string) string {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return strings.ToLower(hostport)
	}
	host = strings.ToLower(host)
	if host == "localhost" || host == "::1" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
