package discover

import (
	"net/url"
	"strings"
	"testing"
)

func describe(kind, raw string) *target {
	return describeURL(envURL{key: "X_URL", kind: kind, raw: raw})
}

func TestDescribeURL(t *testing.T) {
	cases := []struct {
		kind, raw                   string
		masked, host, database, usr string
		remote, connectable         bool
		reason                      string
	}{
		{"postgres", "postgres://app:s3cret@localhost:5432/acme",
			"postgres://app:****@localhost:5432/acme", "localhost:5432", "acme", "app", false, true, ""},
		{"postgres", "postgresql://app:s3cret@db.acme.test/acme?sslmode=require",
			"postgresql://app:****@db.acme.test/acme?sslmode=require", "db.acme.test:5432", "acme", "app", true, true, ""},
		{"postgres", "postgres://app@127.0.0.1/acme",
			"postgres://app@127.0.0.1/acme", "127.0.0.1:5432", "acme", "app", false, true, ""},
		{"postgres", "postgresql+psycopg://app:s3cret@localhost/acme",
			"postgresql://app:****@localhost/acme", "localhost:5432", "acme", "app", false, true, ""},
		{"postgres", "jdbc:postgresql://localhost:5433/acme?user=app&password=s3cret",
			"postgresql://localhost:5433/acme?password=****&user=app", "localhost:5433", "acme", "app", false, true, ""},
		{"postgres", "postgres://app:s3cret@[::1]:5432/acme",
			"postgres://app:****@[::1]:5432/acme", "[::1]:5432", "acme", "app", false, true, ""},
		{"redis", "redis://localhost:6379",
			"redis://localhost:6379", "localhost:6379", "0", "", false, true, ""},
		{"redis", "rediss://default:s3cret@cache.acme.test:6380/2",
			"rediss://default:****@cache.acme.test:6380/2", "cache.acme.test:6380", "2", "default", true, true, ""},
		{"redis", "redis://:s3cret@localhost",
			"redis://:****@localhost", "localhost:6379", "0", "", false, true, ""},
		// A lone user part in a Redis URL is how some clients spell a
		// password, so it is hidden as one.
		{"redis", "redis://s3cret@localhost:6379",
			"redis://****@localhost:6379", "localhost:6379", "0", "", false, true, ""},
		{"mysql", "mysql://root:s3cret@127.0.0.1:3306/acme",
			"mysql://root:****@127.0.0.1:3306/acme", "127.0.0.1:3306", "acme", "root", false, false, "not_supported"},
		{"mongodb", "mongodb+srv://app:s3cret@cluster0.acme.test/acme",
			"mongodb+srv://app:****@cluster0.acme.test/acme", "cluster0.acme.test", "acme", "app", true, false, "not_supported"},
		// Several hosts: the first is the one shown.
		{"mongodb", "mongodb://app:s3cret@a.acme.test:27017,b.acme.test:27017/acme",
			"mongodb://app:****@a.acme.test:27017,b.acme.test:27017/acme", "a.acme.test:27017", "acme", "app", true, false, "not_supported"},
		// A password with a / in it, unescaped: no parser splits this
		// where it was meant, and no part of it is shown.
		{"postgres", "postgres://app:s3/cret@localhost:5432/acme",
			"postgres://****", "", "", "", false, false, "bad_url"},
		{"postgres", "postgres://app:s3cret%zz@db.acme.test:5432/acme",
			"postgres://app:****@db.acme.test:5432/acme", "db.acme.test:5432", "", "", true, false, "bad_url"},
		{"redis", "redis://default:${REDIS_PASSWORD}@cache.acme.test:6379/1",
			"redis://default:****@cache.acme.test:6379/1", "cache.acme.test:6379", "", "", true, false, "bad_url"},
	}
	for _, c := range cases {
		got := describe(c.kind, c.raw)
		if got.masked != c.masked || got.addr() != c.host || got.database != c.database || got.user != c.usr ||
			got.remote != c.remote || got.connectable != c.connectable || got.reason != c.reason {
			t.Errorf("%s\n got masked=%q host=%q db=%q user=%q remote=%v connectable=%v reason=%q\nwant masked=%q host=%q db=%q user=%q remote=%v connectable=%v reason=%q",
				c.raw, got.masked, got.addr(), got.database, got.user, got.remote, got.connectable, got.reason,
				c.masked, c.host, c.database, c.usr, c.remote, c.connectable, c.reason)
		}
		if strings.Contains(got.masked, "s3cret") {
			t.Errorf("%s: the mask let the password through: %q", c.raw, got.masked)
		}
		if got.connectable == (got.connect == "") {
			t.Errorf("%s: connectable=%v but connect=%q", c.raw, got.connectable, got.connect)
		}
	}
}

// oddPasswords are passwords with every character that means something
// in a URL. None may come through a mask, written as it is or
// percent-encoded.
var oddPasswords = []string{
	"p@ss", "p:ss", "p/ss", "p?ss", "p#ss", "p%ss", "p&ss=1", "p;ss", "p ss", "p+ss", "p$ss", "p{ss}", "p[ss]",
	"12/34", "12?34", "12#34", "@@@@", "::::", "a@b:c/d?e#f", "p\\ss", `p"ss'`, "пароль", "p%40ss", "%%%", "ends@",
	"@starts", "p|ss^", "p<ss>", "p`ss`", "://", "x@localhost:5432/acme",
}

func TestMaskNeverShowsAPassword(t *testing.T) {
	shapes := []struct{ kind, format string }{
		{"postgres", "postgres://app:%s@localhost:5432/acme"},
		{"postgres", "postgresql://app:%s@db.acme.test/acme?sslmode=require"},
		{"postgres", "postgres://localhost/acme?user=app&password=%s"},
		{"postgres", "postgres://localhost/acme?sslmode=disable;password=%s"},
		{"postgres", "jdbc:postgresql://localhost/acme?password=%s&user=app"},
		{"redis", "redis://:%s@localhost:6379/0"},
		{"redis", "redis://default:%s@localhost:6379"},
		{"redis", "redis://%s@localhost:6379"},
		{"mysql", "mysql://root:%s@127.0.0.1:3306/acme"},
		{"mongodb", "mongodb://app:%s@a.acme.test:27017,b.acme.test:27017/acme?authSource=admin"},
		{"mongodb", "mongodb+srv://app:%s@cluster0.acme.test/acme"},
	}
	for _, shape := range shapes {
		for _, pw := range oddPasswords {
			for _, written := range []string{pw, url.QueryEscape(pw), url.PathEscape(pw)} {
				raw := strings.Replace(shape.format, "%s", written, 1)
				if urlKind([]byte(raw)) == "" {
					// A value with a space in it is not taken for a URL
					// at all; it is counted and dropped.
					continue
				}
				got := describe(shape.kind, raw)
				if leaks(got.masked, written) || leaks(got.masked, pw) {
					t.Errorf("%s with password %q: masked as %q", shape.format, written, got.masked)
				}
				for _, shown := range []string{got.addr(), got.database, got.user, got.note, got.reason} {
					if leaks(shown, written) || leaks(shown, pw) {
						t.Errorf("%s with password %q: %q shows it", shape.format, written, shown)
					}
				}
			}
		}
	}
}

// leaks reports whether text shows a password. A password that is also
// part of the URL around it ("://", or one that repeats the host) can't
// be told from that part, so only a password of some substance counts,
// and only where the URL's own text would not have it anyway.
func leaks(text, pw string) bool {
	if len(pw) < 4 || pw == "://" || strings.Contains(pw, "localhost") {
		return false
	}
	return strings.Contains(text, pw)
}

func TestLocalHost(t *testing.T) {
	local := []string{"", "localhost", "LOCALHOST", "db.localhost", "localhost.", "127.0.0.1", "127.1.2.3", "::1", "0.0.0.0", "::", "/var/run/postgresql"}
	remote := []string{"db.acme.test", "example.com", "10.255.255.254", "192.0.2.10", "2001:db8::1", "localhost.acme.test", "notlocalhost"}
	for _, h := range local {
		if !localHost(h) {
			t.Errorf("localHost(%q) = false", h)
		}
	}
	for _, h := range remote {
		if localHost(h) {
			t.Errorf("localHost(%q) = true", h)
		}
	}
}

func TestNormalSchemeKeepsEveryOtherByte(t *testing.T) {
	cases := map[string]string{
		"postgres://app:p@ss/w#rd@localhost/acme":           "postgres://app:p@ss/w#rd@localhost/acme",
		"jdbc:postgresql://localhost/acme?password=a%2Fb":   "postgresql://localhost/acme?password=a%2Fb",
		"JDBC:postgresql://localhost/acme":                  "postgresql://localhost/acme",
		"postgresql+asyncpg://app:p+w@localhost/acme":       "postgresql://app:p+w@localhost/acme",
		"mongodb+srv://app:pw@cluster0.acme.test/acme":      "mongodb+srv://app:pw@cluster0.acme.test/acme",
		"mysql+pymysql://root:pw@127.0.0.1/acme?charset=u8": "mysql://root:pw@127.0.0.1/acme?charset=u8",
	}
	for raw, want := range cases {
		if got := normalScheme(raw); got != want {
			t.Errorf("normalScheme(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Prisma writes parameters of its own into the URL (`?schema=public` is
// in every .env it makes). The URL handed on to be saved has them still:
// it is the project's URL, and the line for the project's .env is made
// from it. Leaving them out of what Leadline itself connects with is the
// integrations package's job (its connurl), for a found URL as for one
// pasted by hand. Only the scheme's spelling is changed here.
func TestORMParametersAreHandedOn(t *testing.T) {
	cases := map[string]string{
		"postgresql://app:p%40ss@localhost:5432/acme?schema=public":                       "postgresql://app:p%40ss@localhost:5432/acme?schema=public",
		"postgresql://app:pw@localhost/acme?schema=tenant_a&sslmode=disable":              "postgresql://app:pw@localhost/acme?schema=tenant_a&sslmode=disable",
		"postgresql://app:pw@localhost/acme?sslmode=disable&connection_limit=5&pgbouncer": "postgresql://app:pw@localhost/acme?sslmode=disable&connection_limit=5&pgbouncer",
		"postgresql+psycopg://app:pw@localhost/acme?schema=public":                        "postgresql://app:pw@localhost/acme?schema=public",
		"jdbc:postgresql://localhost:5432/acme?user=app&password=pw&schema=public":        "postgresql://localhost:5432/acme?user=app&password=pw&schema=public",
		"postgresql://app:pw@localhost/acme":                                              "postgresql://app:pw@localhost/acme",
	}
	for raw, want := range cases {
		got := describe("postgres", raw)
		if got.connect != want {
			t.Errorf("%s\n connect = %q\n    want   %q", raw, got.connect, want)
		}
	}
	// What is shown has them too.
	if got := describe("postgres", "postgresql://app:pw@localhost/acme?schema=public"); !strings.Contains(got.masked, "schema=public") {
		t.Errorf("masked = %q", got.masked)
	}
}

// Two found URLs are one connection when they reach the same server and
// database as the same user with the same password, however each is
// spelled.
func TestIdentity(t *testing.T) {
	same := []string{
		"postgres://app:pw@localhost:5432/acme",
		"postgresql://app:pw@localhost/acme",
		"postgresql://app:pw@127.0.0.1:5432/acme?schema=public",
		"postgresql+psycopg://app:pw@localhost:5432/acme?sslmode=disable",
		"jdbc:postgresql://localhost:5432/acme?user=app&password=pw",
	}
	first := describe("postgres", same[0]).identity
	for _, raw := range same {
		if got := describe("postgres", raw).identity; got != first || got == "" {
			t.Errorf("%s is not taken for the same connection", raw)
		}
	}
	for _, raw := range []string{
		"postgres://app:other@localhost:5432/acme", // another password
		"postgres://admin:pw@localhost:5432/acme",  // another user
		"postgres://app:pw@localhost:5432/acme_test",
		"postgres://app:pw@localhost:5433/acme",
		"postgres://app:pw@db.acme.test:5432/acme",
	} {
		if describe("postgres", raw).identity == first {
			t.Errorf("%s is taken for the same connection", raw)
		}
	}
	if a, b := describe("redis", "redis://localhost:6379").identity, describe("redis", "redis://127.0.0.1:6379/0").identity; a != b {
		t.Error("two spellings of one Redis are two connections")
	}
	if describe("redis", "redis://localhost:6379").identity == first || describe("postgres", "postgres://app:p/w@localhost/acme").identity != "" {
		t.Error("identity of another kind, or of a URL that does not parse")
	}
}
