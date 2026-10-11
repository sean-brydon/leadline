// Package connurl is what Leadline does to a database URL before it
// connects with it: the one rule, in one place, for a URL typed by hand,
// edited, attached from a worktree or found by a scan alike. It is a
// package of its own, with nothing of the database engine in it, so that
// anything that has to agree with the rule (a test of discovery, say) can
// name it without linking the rest of the integrations package.
package connurl

import "strings"

// ormParams are query parameters an ORM reads from a database URL and
// Postgres itself does not know: Prisma's, which its own setup puts in
// every .env (`?schema=public`). A Postgres driver sends what it does not
// recognise to the server as a setting, and the server refuses the
// connection ("unrecognized configuration parameter"). So Leadline
// connects without them.
var ormParams = map[string]bool{
	"schema": true, "connection_limit": true, "pool_timeout": true, "pgbouncer": true,
	"socket_timeout": true, "statement_cache_size": true, "sslaccept": true, "sslidentity": true,
}

// ORMParams lists the parameters ForPostgres leaves out.
func ORMParams() []string {
	return []string{"schema", "connection_limit", "pool_timeout", "pgbouncer",
		"socket_timeout", "statement_cache_size", "sslaccept", "sslidentity"}
}

// ForPostgres is a Postgres URL as Leadline connects with it: without
// the parameters an ORM put there for itself. It cuts them out of the
// string and leaves every other byte, the password's among them, as it
// was written. A URL with none of them comes back as it was.
func ForPostgres(raw string) string {
	q := strings.IndexByte(raw, '?')
	if q < 0 {
		return raw
	}
	query, fragment := raw[q+1:], ""
	if h := strings.IndexByte(query, '#'); h >= 0 {
		query, fragment = query[:h], query[h:]
	}
	var keep []string
	dropped := false
	for _, pair := range strings.Split(query, "&") {
		name, _, _ := strings.Cut(pair, "=")
		if pair == "" || ormParams[strings.ToLower(name)] {
			dropped = dropped || pair != ""
			continue
		}
		keep = append(keep, pair)
	}
	if !dropped {
		return raw
	}
	if len(keep) == 0 {
		return raw[:q] + fragment
	}
	return raw[:q+1] + strings.Join(keep, "&") + fragment
}
