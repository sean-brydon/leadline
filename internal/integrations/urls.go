package integrations

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sean-brydon/leadline/internal/integrations/connurl"
)

const (
	KindPostgres = "postgres"
	KindRedis    = "redis"

	// Mask stands where a password was in every URL that leaves the
	// daemon. A URL that comes back in with it means "the password you
	// already have".
	Mask = "****"
)

// target is a connection URL taken apart.
type target struct {
	kind string
	// raw is the URL as it was given, and what is saved, shown (masked)
	// and pointed at the proxy for the app to use.
	raw string
	u   *url.URL
	// dial is the URL Leadline itself connects with. For Postgres that
	// is raw without the parameters an ORM put there for itself
	// (Prisma's `?schema=public`), which the server refuses; the app
	// that owns the URL still needs them, so raw keeps them.
	dial string

	host     string // a name, an address, or the directory of a unix socket
	port     int
	user     string
	password string
	database string // Postgres: its name. Redis: its number.
	tls      bool   // Redis: rediss://
}

// errURL is what a person sees for a URL that can't be read. It never
// quotes the URL: that would put the password in an error message.
func errURL(why string) error {
	return &Problem{Code: "bad_url", Message: why,
		Hint: "It should look like postgres://user:password@localhost:5432/dbname or redis://localhost:6379."}
}

// parseURL reads a connection URL. kind may be empty, to go by the scheme.
func parseURL(kind, raw string) (*target, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errURL("The URL is empty.")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return nil, errURL("That is not a URL Leadline can read.")
	}
	t := &target{raw: raw, dial: raw, u: u}
	switch strings.ToLower(u.Scheme) {
	case "postgres", "postgresql":
		t.kind = KindPostgres
	case "redis", "rediss":
		t.kind = KindRedis
		t.tls = strings.EqualFold(u.Scheme, "rediss")
	default:
		return nil, errURL("Leadline connects to Postgres (postgres://) and Redis (redis://). This URL is for neither.")
	}
	if kind != "" && kind != t.kind {
		return nil, errURL("The URL is for " + t.kind + ", not " + kind + ".")
	}
	if t.kind == KindPostgres {
		return t, t.readPostgres()
	}
	return t, t.readRedis()
}

func (t *target) readPostgres() error {
	// pgx reads the URL the way libpq does, defaults and all (the user's
	// name, PGPASSWORD, ~/.pgpass), so what connects here is what would
	// connect from psql.
	t.dial = connurl.ForPostgres(t.raw)
	cfg, err := pgconn.ParseConfig(t.dial)
	if err != nil {
		return errURL("That Postgres URL can't be read: " + lastClause(err) + ".")
	}
	t.host, t.port = cfg.Host, int(cfg.Port)
	t.user, t.password, t.database = cfg.User, cfg.Password, cfg.Database
	return nil
}

func (t *target) readRedis() error {
	t.host, t.port = t.u.Hostname(), 6379
	if t.host == "" {
		t.host = "localhost"
	}
	if p := t.u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return errURL("The port in that URL is not a number between 1 and 65535.")
		}
		t.port = n
	}
	if t.u.User != nil {
		t.user = t.u.User.Username()
		t.password, _ = t.u.User.Password()
	}
	if pw := t.u.Query().Get("password"); pw != "" && t.password == "" {
		t.password = pw
	}
	t.database = "0"
	if db := strings.Trim(t.u.Path, "/"); db != "" {
		n, err := strconv.Atoi(db)
		if err != nil || n < 0 {
			return errURL("A Redis URL ends in a database number, as in redis://localhost:6379/0.")
		}
		t.database = strconv.Itoa(n)
	}
	return nil
}

// lastClause is the end of a wrapped error, which is the part that says
// what was wrong rather than where.
func lastClause(err error) string {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			break
		}
		err = next
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return strings.TrimRight(s, ".")
}

// socket is the unix socket's path, or "" for a TCP address.
func (t *target) socket() string {
	if !strings.HasPrefix(t.host, "/") {
		return ""
	}
	if t.kind == KindPostgres {
		return t.host + "/.s.PGSQL." + strconv.Itoa(t.port)
	}
	return t.host
}

// addr is where the server is, as a person would write it.
func (t *target) addr() string {
	if s := t.socket(); s != "" {
		return s
	}
	return net.JoinHostPort(t.host, strconv.Itoa(t.port))
}

// remote reports whether the server is on another machine.
func (t *target) remote() bool { return !localHost(t.host) }

// localHost reports whether host is this machine. It never asks DNS: a
// name other than localhost counts as somewhere else.
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

// secretParams are the query parameters that carry a credential.
var secretParams = []string{"password", "sslpassword"}

// masked is the URL with every password replaced by Mask.
func (t *target) masked() string {
	return maskURL(t.u)
}

func maskURL(in *url.URL) string {
	u := *in
	// The mask goes in as a token and is swapped afterwards: url.URL
	// would percent-encode the stars.
	const token = "LEADLINEMASKTOKEN"
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), token)
		}
	}
	if u.RawQuery != "" {
		q := u.Query()
		changed := false
		for _, name := range secretParams {
			if q.Get(name) != "" {
				q.Set(name, token)
				changed = true
			}
		}
		if changed {
			u.RawQuery = q.Encode()
		}
	}
	return strings.ReplaceAll(u.String(), token, Mask)
}

// withSavedPassword fills a URL that came back from the app, where the
// password is the mask, with the password of the saved one. So editing a
// connection's host doesn't need the password typed again.
func withSavedPassword(raw string, saved *target) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || saved == nil {
		return raw
	}
	changed := false
	if u.User != nil {
		if pw, _ := u.User.Password(); pw == Mask {
			if spw, has := userPassword(saved.u); has {
				u.User = url.UserPassword(u.User.Username(), spw)
				changed = true
			}
		}
	}
	if u.RawQuery != "" {
		q, sq := u.Query(), saved.u.Query()
		for _, name := range secretParams {
			if q.Get(name) == Mask && sq.Get(name) != "" {
				q.Set(name, sq.Get(name))
				u.RawQuery = q.Encode()
				changed = true
			}
		}
	}
	if !changed {
		return raw
	}
	return u.String()
}

func userPassword(u *url.URL) (string, bool) {
	if u.User == nil {
		return "", false
	}
	return u.User.Password()
}

// tlsParams are the Postgres URL parameters that only mean something with
// TLS, which the proxy doesn't speak.
var tlsParams = []string{"host", "port", "sslmode", "sslcert", "sslkey", "sslrootcert", "sslpassword",
	"sslnegotiation", "sslsni", "channel_binding", "gssencmode", "target_session_attrs"}

// proxyURL is the person's own URL pointed at the proxy instead: same
// user, database and options, on 127.0.0.1 and without TLS. An ORM's own
// parameters stay: this is the line for the app's .env, and Prisma reads
// its schema from there. The password is masked like everywhere else;
// the app puts back what the person typed.
func (t *target) proxyURL(port int) string {
	u := *t.u
	u.Host = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	q := u.Query()
	for _, name := range tlsParams {
		q.Del(name)
	}
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	if u.Path == "" && t.database != "" {
		u.Path = "/" + t.database
	}
	return maskURL(&u)
}

// upstreamKey makes two spellings of one local server the same server:
// localhost:5432 and 127.0.0.1:5432.
func upstreamKey(upstream string) string {
	host, port, err := net.SplitHostPort(upstream)
	if err != nil {
		return upstream
	}
	host = strings.ToLower(host)
	if host == "localhost" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// scrub takes a password out of a message, should a driver have put the
// URL in an error. A password that is also an ordinary word here (the user
// is "postgres" and so is the password) is only removed where it stands as
// a password, or every message about that user would be full of stars.
func scrub(msg string, t *target) string {
	if t == nil || t.password == "" {
		return msg
	}
	pw := t.password
	msg = strings.ReplaceAll(msg, t.raw, t.masked())
	if t.dial != t.raw {
		msg = strings.ReplaceAll(msg, t.dial, t.masked())
	}
	for _, form := range []string{pw, url.QueryEscape(pw), url.PathEscape(pw)} {
		msg = strings.ReplaceAll(msg, ":"+form+"@", ":"+Mask+"@")
		msg = strings.ReplaceAll(msg, "password="+form, "password="+Mask)
		if distinctive(pw, t) {
			msg = strings.ReplaceAll(msg, form, Mask)
		}
	}
	return msg
}

func distinctive(pw string, t *target) bool {
	if len(pw) < 6 {
		return false
	}
	for _, word := range []string{t.user, t.database, t.host, "postgres", "postgresql", "password", "redis", "localhost"} {
		if strings.EqualFold(pw, word) {
			return false
		}
	}
	return true
}
