package discover

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// pairs runs the parser and returns what it called back with, as strings.
func pairs(src string) [][3]string {
	var out [][3]string
	parseEnv([]byte(src), func(key, value []byte, expand bool) {
		e := "expand"
		if !expand {
			e = "literal"
		}
		out = append(out, [3]string{string(key), string(value), e})
	})
	return out
}

func TestParseEnv(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want [][3]string
	}{
		{"bare", "A=1\nB=two\n", [][3]string{{"A", "1", "expand"}, {"B", "two", "expand"}}},
		{"no trailing newline", "A=1", [][3]string{{"A", "1", "expand"}}},
		{"crlf", "A=1\r\nB=2\r\n", [][3]string{{"A", "1", "expand"}, {"B", "2", "expand"}}},
		{"export", "export A=1\nexport\tB=2\n  export C=3", [][3]string{{"A", "1", "expand"}, {"B", "2", "expand"}, {"C", "3", "expand"}}},
		{"comments and blanks", "# a comment\n\n  # another\nA=1 # trailing\nB=#all\nC=a#b\n",
			[][3]string{{"A", "1", "expand"}, {"B", "", "expand"}, {"C", "a#b", "expand"}}},
		{"spaces around", "  A = 1  \nB=  two words  \n", [][3]string{{"A", "1", "expand"}, {"B", "two words", "expand"}}},
		{"double quotes", `A="one # not a comment" # but this is`, [][3]string{{"A", "one # not a comment", "expand"}}},
		{"single quotes are literal", `A='$HOME and ${X}'`, [][3]string{{"A", "$HOME and ${X}", "literal"}}},
		{"back quotes", "A=`it's \"fine\"`", [][3]string{{"A", `it's "fine"`, "expand"}}},
		{"escaped quote inside", `A="say \"hi\""` + "\nB=2", [][3]string{{"A", `say \"hi\"`, "expand"}, {"B", "2", "expand"}}},
		{"runs over lines", "KEY=\"line one\nline two\nline three\"\nAFTER=1\n",
			[][3]string{{"KEY", "line one\nline two\nline three", "expand"}, {"AFTER", "1", "expand"}}},
		{"never closed", "A=\"unclosed\nB=2\n", [][3]string{{"A", `"unclosed`, "expand"}, {"B", "2", "expand"}}},
		{"yaml-ish", "A: 1\n", [][3]string{{"A", "1", "expand"}}},
		{"not a variable", "just some text\n=nokey\n1BAD=x\nGOOD=1\n", [][3]string{{"GOOD", "1", "expand"}}},
		{"empty", "A=\nB=\"\"\n", [][3]string{{"A", "", "expand"}, {"B", "", "expand"}}},
		{"equals in value", "A=b=c=d\n", [][3]string{{"A", "b=c=d", "expand"}}},
	}
	for _, c := range cases {
		if got := pairs(c.src); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestURLKind(t *testing.T) {
	kinds := map[string]string{
		"postgres://localhost/acme":                         "postgres",
		"postgresql://app:pw@localhost:5432/acme":           "postgres",
		"POSTGRES://LOCALHOST/acme":                         "postgres",
		"postgresql+psycopg://app:pw@localhost/acme":        "postgres",
		"jdbc:postgresql://localhost:5432/acme?user=app":    "postgres",
		"redis://localhost:6379":                            "redis",
		"rediss://default:pw@cache.acme.test:6380/2":        "redis",
		"mysql://root:pw@127.0.0.1:3306/acme":               "mysql",
		"mysql+pymysql://root:pw@127.0.0.1/acme":            "mysql",
		"mariadb://root@localhost/acme":                     "mysql",
		"mongodb://localhost:27017/acme":                    "mongodb",
		"mongodb+srv://app:pw@cluster0.acme.test/acme":      "mongodb",
		"https://key@o0.ingest.acme.test/42":                "",
		"http://localhost:3000":                             "",
		"amqp://guest:guest@localhost:5672":                 "",
		"prisma+postgres://accelerate.acme.test/?api_key=k": "",
		"redis+sentinel://localhost:26379/mymaster":         "",
		"postgres":            "",
		"sk_test_acme_51Nxyz": "",
		"postgres://app:pw@localhost/acme and then some":     "",
		"see postgres://app:pw@localhost/acme":               "",
		"postgres://app:pw@localhost/acme\npostgres://other": "",
		"": "",
	}
	for value, want := range kinds {
		if got := urlKind([]byte(value)); got != want {
			t.Errorf("urlKind(%q) = %q, want %q", value, got, want)
		}
	}
}

// writeFiles makes a folder of files from a map of relative path to
// content.
func writeFiles(t *testing.T, dir string, tree map[string]string) {
	t.Helper()
	for name, content := range tree {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadEnvTakesURLsAndCountsTheRest(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		".env.local": strings.Join([]string{
			`# local`,
			`DATABASE_URL="postgres://acme:pw-local@localhost:5432/acme"`,
			`export REDIS_URL=redis://localhost:6379 # the cache`,
			`STRIPE_SECRET_KEY='sk_test_acme_OTHERVALUE1'`,
			`SESSION_SECRET=OTHERVALUE2`,
			`PRIVATE_KEY="-----BEGIN KEY-----`,
			`OTHERVALUE3`,
			`-----END KEY-----"`,
			`EMPTY=`,
		}, "\n"),
		".env": strings.Join([]string{
			`DATABASE_URL=postgres://acme:pw-shared@db.acme.test:5432/acme`,
			`MONGO_URL=mongodb://localhost:27017/acme`,
			`APP_URL=http://localhost:3000`,
		}, "\n"),
		// A file no dev tool reads in development: not opened.
		".env.production": "DATABASE_URL=postgres://acme:pw-prod@prod.acme.test/acme\n",
	})
	env := osFiles().readEnv(dir)

	wantFiles := []EnvFile{{File: ".env.local", Read: true, URLs: 2, OtherKeys: 4}, {File: ".env", Read: true, URLs: 2, OtherKeys: 1}}
	if !reflect.DeepEqual(env.files, wantFiles) {
		t.Errorf("files = %+v, want %+v", env.files, wantFiles)
	}
	type found struct {
		key, file, kind string
		effective       bool
	}
	var got []found
	for _, u := range env.urls {
		got = append(got, found{u.key, u.file, u.kind, u.effective})
	}
	want := []found{
		{"DATABASE_URL", ".env.local", "postgres", true},
		{"REDIS_URL", ".env.local", "redis", true},
		// .env.local outranks .env, so this one is not the one in use.
		{"DATABASE_URL", ".env", "postgres", false},
		{"MONGO_URL", ".env", "mongodb", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("urls = %+v\nwant   %+v", got, want)
	}
	if env.urls[0].raw != "postgres://acme:pw-local@localhost:5432/acme" || env.urls[1].raw != "redis://localhost:6379" {
		t.Errorf("the URLs were not taken as written: %q, %q", env.urls[0].raw, env.urls[1].raw)
	}
	for _, u := range env.urls {
		if strings.Contains(u.raw, "pw-prod") {
			t.Error(".env.production was read")
		}
	}
}

// The buffer an env file was read into is wiped when the parse ends, so
// the values that were passed over are not left lying in memory.
func TestReadEnvWipesTheFile(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{".env": "SECRET=OTHERVALUE\nDATABASE_URL=postgres://localhost/acme\n"})
	var buffers [][]byte
	f := osFiles()
	read := f.readFile
	f.readFile = func(path string, limit int64) ([]byte, error) {
		b, err := read(path, limit)
		if err == nil {
			buffers = append(buffers, b)
		}
		return b, err
	}
	env := f.readEnv(dir)
	if len(env.urls) != 1 || len(buffers) != 1 {
		t.Fatalf("urls %d, buffers %d", len(env.urls), len(buffers))
	}
	for _, c := range buffers[0] {
		if c != 0 {
			t.Fatalf("the file's buffer was not wiped: %q", buffers[0])
		}
	}
	// And the URL is its own string, not a view of the wiped buffer.
	if env.urls[0].raw != "postgres://localhost/acme" {
		t.Errorf("raw = %q", env.urls[0].raw)
	}
}

// Interpolation is left alone: a URL that refers to another variable is
// reported, marked, and not filled in.
func TestInterpolationIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("DB_PASSWORD", "from-the-daemons-own-environment")
	writeFiles(t, dir, map[string]string{".env": strings.Join([]string{
		`DB_PASSWORD=OTHERVALUE`,
		`A_URL=postgres://app:${DB_PASSWORD}@localhost:5432/acme`,
		`B_URL=postgres://app:$DB_PASSWORD@localhost:5432/acme`,
		`C_URL="redis://:${REDIS_PW:-fallback}@localhost:6379"`,
		// Single quotes and an escaped dollar are a dollar, not a
		// reference.
		`D_URL='postgres://app:pa$word@localhost:5432/acme'`,
		`E_URL=postgres://app:pa\$word@localhost:5432/acme`,
	}, "\n")})
	env := osFiles().readEnv(dir)
	if len(env.urls) != 5 {
		t.Fatalf("found %d URLs, want 5", len(env.urls))
	}
	wantRefs := map[string]bool{"A_URL": true, "B_URL": true, "C_URL": true, "D_URL": false, "E_URL": false}
	for _, u := range env.urls {
		if u.refs != wantRefs[u.key] {
			t.Errorf("%s: refs = %v, want %v", u.key, u.refs, wantRefs[u.key])
		}
		if strings.Contains(u.raw, "OTHERVALUE") || strings.Contains(u.raw, "from-the-daemons-own-environment") {
			t.Errorf("%s was interpolated: %q", u.key, u.raw)
		}
		tg := describeURL(u)
		if u.refs && (tg.connectable || tg.reason != "interpolated" || tg.connect != "") {
			t.Errorf("%s: an interpolated URL must not be connectable: %+v", u.key, tg)
		}
		if !u.refs && !tg.connectable {
			t.Errorf("%s: should be connectable: %+v", u.key, tg)
		}
	}
	if env.urls[4].raw != "postgres://app:pa$word@localhost:5432/acme" {
		t.Errorf(`\$ was not made a dollar: %q`, env.urls[4].raw)
	}
}
