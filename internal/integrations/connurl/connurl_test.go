package connurl

import (
	"strings"
	"testing"
)

// Prisma writes parameters of its own into the URL (`?schema=public` is
// in every .env it makes), which Postgres refuses. The URL Leadline
// connects with goes without them and is otherwise untouched.
func TestORMParametersAreLeftOut(t *testing.T) {
	cases := map[string]string{
		"postgresql://app:p%40ss@localhost:5432/acme?schema=public":                       "postgresql://app:p%40ss@localhost:5432/acme",
		"postgresql://app:pw@localhost/acme?schema=public&sslmode=disable":                "postgresql://app:pw@localhost/acme?sslmode=disable",
		"postgresql://app:pw@localhost/acme?sslmode=disable&connection_limit=5&pgbouncer": "postgresql://app:pw@localhost/acme?sslmode=disable",
		"postgresql://app:pw@localhost/acme?SCHEMA=public&pool_timeout=10":                "postgresql://app:pw@localhost/acme",
		"postgres://app:pw@localhost/acme?schema=public#frag":                             "postgres://app:pw@localhost/acme#frag",
		// Nothing of an ORM's: every byte as it was, an empty pair and all.
		"postgresql://app:pw@localhost/acme?application_name=acme": "postgresql://app:pw@localhost/acme?application_name=acme",
		"postgresql://app:pw@localhost/acme?sslmode=disable&":      "postgresql://app:pw@localhost/acme?sslmode=disable&",
		"postgresql://app:pw@localhost/acme":                       "postgresql://app:pw@localhost/acme",
		// A password is never read as a parameter, whatever is in it.
		"postgresql://app:schema=public@localhost/acme": "postgresql://app:schema=public@localhost/acme",
	}
	for raw, want := range cases {
		if got := ForPostgres(raw); got != want {
			t.Errorf("ForPostgres(%q)\n  = %q\nwant %q", raw, got, want)
		}
	}
}

func TestEveryListedParameterIsLeftOut(t *testing.T) {
	if len(ORMParams()) != len(ormParams) {
		t.Fatalf("ORMParams lists %d, the table has %d", len(ORMParams()), len(ormParams))
	}
	for _, name := range ORMParams() {
		got := ForPostgres("postgres://app:pw@localhost/acme?sslmode=disable&" + name + "=1")
		if strings.Contains(got, name) || got != "postgres://app:pw@localhost/acme?sslmode=disable" {
			t.Errorf("%s was kept: %q", name, got)
		}
	}
}
