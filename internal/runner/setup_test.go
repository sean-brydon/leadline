package runner

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func noEnv(string) string { return "" }

func TestServiceName(t *testing.T) {
	root := t.TempDir()
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	// The folder, when nothing names the project.
	bare := filepath.Join(root, "bare-folder")
	write(t, filepath.Join(bare, "README"), "")
	if got := ServiceName("", noEnv, bare); got != "bare-folder" {
		t.Errorf("folder: got %q", got)
	}

	node := filepath.Join(root, "node")
	write(t, filepath.Join(node, "package.json"), `{"name":"@acme/orders","version":"1.0.0"}`)
	// A nested folder with a nameless package.json: keep walking up.
	write(t, filepath.Join(node, "dist", "package.json"), `{"type":"module"}`)
	if got := ServiceName("", noEnv, filepath.Join(node, "dist")); got != "@acme/orders" {
		t.Errorf("package.json: got %q", got)
	}
	if got := ServiceName("", env(map[string]string{"OTEL_SERVICE_NAME": "from-env"}), node); got != "from-env" {
		t.Errorf("OTEL_SERVICE_NAME: got %q", got)
	}
	if got := ServiceName(" api ", env(map[string]string{"OTEL_SERVICE_NAME": "from-env"}), node); got != "api" {
		t.Errorf("--name: got %q", got)
	}

	for file, want := range map[string]string{
		"module github.com/acme/billing\n\ngo 1.25\n": "billing",
		"// the API\nmodule github.com/acme/api/v2\n": "api",
		"module example.com\n":                        "example.com",
	} {
		dir := filepath.Join(root, "go-"+want)
		write(t, filepath.Join(dir, "go.mod"), file)
		if got := ServiceName("", noEnv, filepath.Join(dir)); got != want {
			t.Errorf("go.mod %q: got %q, want %q", file, got, want)
		}
	}

	for file, want := range map[string]string{
		"[build-system]\nrequires = [\"hatchling\"]\n\n[project]\nname = \"acme-reports\"\nversion = \"0.1\"\n": "acme-reports",
		"[tool.poetry]\nname = 'acme-jobs'\n": "acme-jobs",
	} {
		dir := filepath.Join(root, "py-"+want)
		write(t, filepath.Join(dir, "pyproject.toml"), file)
		if got := ServiceName("", noEnv, dir); got != want {
			t.Errorf("pyproject.toml: got %q, want %q", got, want)
		}
	}
	// A pyproject.toml that only configures a tool names nothing.
	lint := filepath.Join(root, "only-ruff")
	write(t, filepath.Join(lint, "pyproject.toml"), "[tool.ruff]\nline-length = 100\n")
	if got := ServiceName("", noEnv, lint); got != "only-ruff" {
		t.Errorf("tool-only pyproject.toml: got %q", got)
	}
}

func TestParseDotenv(t *testing.T) {
	src := strings.Join([]string{
		"# a comment",
		"",
		"PLAIN=value",
		"export EXPORTED=yes",
		"SPACED = padded value  ",
		`DOUBLE="two words # not a comment"`,
		`ESCAPED="line one\nline two"`,
		`SINGLE='$NOT_EXPANDED and \n kept'`,
		"TRAILING=value # a comment",
		"HASH_IN_VALUE=abc#def",
		"EMPTY=",
		"URL=postgres://app:p%40ss@localhost:5432/acme?sslmode=disable",
		"MULTI=\"first",
		"second\"",
		"1BAD=no",
		"not a pair",
		"YAML: style",
	}, "\n")
	got := map[string]string{}
	for _, kv := range parseDotenv(src) {
		got[kv.key] = kv.value
	}
	want := map[string]string{
		"PLAIN":         "value",
		"EXPORTED":      "yes",
		"SPACED":        "padded value",
		"DOUBLE":        "two words # not a comment",
		"ESCAPED":       "line one\nline two",
		"SINGLE":        `$NOT_EXPANDED and \n kept`,
		"TRAILING":      "value",
		"HASH_IN_VALUE": "abc#def",
		"EMPTY":         "",
		"URL":           "postgres://app:p%40ss@localhost:5432/acme?sslmode=disable",
		"MULTI":         "first\nsecond",
		"YAML":          "style",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %#v\nwant %#v", got, want)
	}
}

func TestReadDotenvPrecedenceAndExpansion(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".env"), "A=env\nB=env\nC=env\nD=env\nDB_USER=app\nDB_NAME=acme\nDATABASE_URL=postgres://${DB_USER}:${DB_PASS:-dev}@localhost/$DB_NAME\nLITERAL='${DB_USER}'\n")
	write(t, filepath.Join(dir, ".env.development"), "B=development\nC=development\nD=development\n")
	write(t, filepath.Join(dir, ".env.local"), "C=local\nD=local\n")
	write(t, filepath.Join(dir, ".env.development.local"), "D=development.local\n")
	write(t, filepath.Join(dir, ".env.production"), "A=production\nE=production\n")
	write(t, filepath.Join(dir, ".env.test"), "A=test\n")

	env := func(key string) string {
		if key == "DB_NAME" {
			return "from_process"
		}
		return ""
	}
	d := ReadDotenv(dir, "", env)
	for key, want := range map[string]string{
		"A": "env", "B": "development", "C": "local", "D": "development.local",
		// The process's environment is looked at first, as dotenv-expand does.
		"DATABASE_URL": "postgres://app:dev@localhost/from_process",
		"LITERAL":      "${DB_USER}",
	} {
		if d.Values[key] != want {
			t.Errorf("%s: got %q, want %q", key, d.Values[key], want)
		}
	}
	if _, ok := d.Values["E"]; ok {
		t.Error("read .env.production in development")
	}
	if d.Source["D"] != ".env.development.local" || d.Source["A"] != ".env" {
		t.Errorf("sources: %v", d.Source)
	}

	// Tests do not see .env.local.
	d = ReadDotenv(dir, "test", noEnv)
	if d.Values["A"] != "test" || d.Values["C"] != "env" {
		t.Errorf("test mode: A=%q C=%q", d.Values["A"], d.Values["C"])
	}
	// No files at all is not an error.
	if d := ReadDotenv(t.TempDir(), "", noEnv); len(d.Values) != 0 {
		t.Errorf("empty folder: %v", d.Values)
	}
}

const secret = "s3cr3t-P@ss+w0rd"

func TestFindPostgresLocalOnly(t *testing.T) {
	local := map[string]string{
		"DATABASE_URL":   "postgres://app:hunter2@localhost:5432/acme",
		"NO_PORT":        "postgresql://app@127.0.0.1/acme",
		"IPV6":           "postgres://app:hunter2@[::1]:5433/acme",
		"SQLALCHEMY":     "postgresql+psycopg://app:hunter2@localhost:5432/acme",
		"JDBC":           "jdbc:postgresql://localhost:5432/acme?user=app",
		"PREFER":         "postgres://app@localhost:5432/acme?sslmode=prefer",
		"ANY_ADDRESS":    "postgres://app@0.0.0.0:5432/acme",
		"SUBDOMAIN":      "postgres://app@db.localhost:5432/acme",
		"AT_IN_PASSWORD": "postgres://app:p@ss@localhost:5432/acme",
	}
	wantUpstream := map[string]string{
		"DATABASE_URL": "localhost:5432", "NO_PORT": "127.0.0.1:5432", "IPV6": "[::1]:5433",
		"SQLALCHEMY": "localhost:5432", "JDBC": "localhost:5432", "PREFER": "localhost:5432",
		"ANY_ADDRESS": "127.0.0.1:5432", "SUBDOMAIN": "db.localhost:5432", "AT_IN_PASSWORD": "localhost:5432",
	}
	leftAlone := map[string]string{
		"REMOTE":        "postgres://app:hunter2@db.acme.example:5432/acme",
		"REMOTE_IP":     "postgres://app:hunter2@10.0.0.5:5432/acme",
		"LOOKALIKE":     "postgres://app:hunter2@localhost.acme.example:5432/acme",
		"USER_IS_LOCAL": "postgres://localhost:hunter2@db.acme.example/acme",
		"IN_PASSWORD":   "postgres://app:@localhost@db.acme.example:5432/acme",
		"SOCKET":        "postgres:///acme?host=/var/run/postgresql",
		"HOST_PARAM":    "postgres://localhost/acme?host=db.acme.example",
		"MULTI_HOST":    "postgres://localhost:5432,localhost:5433/acme",
		"TLS_REQUIRED":  "postgres://app@localhost:5432/acme?sslmode=require",
		"TLS_VERIFY":    "postgres://app@localhost:5432/acme?sslmode=verify-full",
		"NODE_PG_SSL":   "postgres://app@localhost:5432/acme?ssl=true",
		"MYSQL":         "mysql://app:hunter2@localhost:3306/acme",
		"REDIS":         "redis://localhost:6379",
		"HTTP":          "http://localhost:5432",
		"NOT_A_URL":     "localhost:5432",
		"PATH":          "/usr/bin:/bin",
	}
	var environ []string
	for k, v := range local {
		environ = append(environ, k+"="+v)
	}
	for k, v := range leftAlone {
		environ = append(environ, k+"="+v)
	}
	found := map[string]PGTarget{}
	for _, target := range FindPostgres(environ, Dotenv{}) {
		found[target.Key] = target
	}
	for key := range leftAlone {
		if _, ok := found[key]; ok {
			t.Errorf("%s was taken for a local Postgres: %s", key, leftAlone[key])
		}
	}
	for key, want := range wantUpstream {
		target, ok := found[key]
		if !ok {
			t.Errorf("%s was not found: %s", key, local[key])
			continue
		}
		if target.Upstream != want {
			t.Errorf("%s: upstream %q, want %q", key, target.Upstream, want)
		}
	}
}

func TestThroughKeepsEverythingButTheAddress(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://app:hunter2@localhost:5432/acme":                               "postgres://app:hunter2@127.0.0.1:61000/acme",
		"postgresql://app@127.0.0.1/acme":                                          "postgresql://app@127.0.0.1:61000/acme",
		"postgres://app:hunter2@[::1]:5433/acme?application_name=x&sslmode=prefer": "postgres://app:hunter2@127.0.0.1:61000/acme?application_name=x&sslmode=prefer",
		// A password is carried byte for byte, odd escapes and all.
		"postgres://app:p%40ss%2fw%C3%B6rd+x@localhost:5432/acme": "postgres://app:p%40ss%2fw%C3%B6rd+x@127.0.0.1:61000/acme",
		"postgres://app:p@ss@localhost:5432/acme":                 "postgres://app:p@ss@127.0.0.1:61000/acme",
		"postgres://localhost":                                    "postgres://127.0.0.1:61000",
		"jdbc:postgresql://localhost:5432/acme?user=app":          "jdbc:postgresql://127.0.0.1:61000/acme?user=app",
		"postgresql+asyncpg://app:x@localhost/acme":               "postgresql+asyncpg://app:x@127.0.0.1:61000/acme",
	} {
		targets := FindPostgres([]string{"DATABASE_URL=" + in}, Dotenv{})
		if len(targets) != 1 {
			t.Errorf("%s: not found", in)
			continue
		}
		if got := targets[0].Through("127.0.0.1:61000"); got != want {
			t.Errorf("%s:\n got %s\nwant %s", in, got, want)
		}
	}
}

func TestFindPostgresReadsDotenvBehindTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".env"), "DATABASE_URL=postgres://app:"+secret+"@localhost:5432/acme\nSHADOW_URL=postgres://app@localhost:5434/shadow\nREMOTE_URL=postgres://app@db.acme.example/acme\n")
	dotenv := ReadDotenv(dir, "", noEnv)

	targets := FindPostgres(nil, dotenv)
	if len(targets) != 2 || targets[0].Key != "DATABASE_URL" || targets[0].File != ".env" || targets[1].Key != "SHADOW_URL" {
		t.Fatalf("got %v", targets)
	}
	// What the process already has is what the app will use: a remote URL
	// there wins over a local one in the file, and is left alone.
	targets = FindPostgres([]string{"DATABASE_URL=postgres://app@db.acme.example/acme"}, dotenv)
	if len(targets) != 1 || targets[0].Key != "SHADOW_URL" {
		t.Fatalf("the environment did not win: %v", targets)
	}
}

func TestPGTargetNeverShowsThePassword(t *testing.T) {
	targets := FindPostgres([]string{"DATABASE_URL=postgres://app:" + secret + "@localhost:5432/acme"}, Dotenv{})
	if len(targets) != 1 {
		t.Fatal("not found")
	}
	for _, shown := range []string{
		targets[0].String(),
		strings.Join([]string{targets[0].Key, targets[0].Upstream, targets[0].File}, " "),
	} {
		if strings.Contains(shown, secret) || strings.Contains(shown, "app:") {
			t.Errorf("credentials in %q", shown)
		}
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"psql postgres://app:hunter2@localhost:5432/acme -c 'select 1'": "psql postgres://app:***@localhost:5432/acme -c 'select 1'",
		"postgres://app:p@ss@localhost/acme":                            "postgres://app:***@localhost/acme",
		"curl https://user:tok3n@api.acme.example/v1":                   "curl https://user:***@api.acme.example/v1",
		"yarn dev --port 3000":                                          "yarn dev --port 3000",
		"postgres://app@localhost/acme":                                 "postgres://app@localhost/acme",
		"node server.js --at 10:30 --mail a@b.example":                  "node server.js --at 10:30 --mail a@b.example",
	} {
		if got := Redact(in); got != want {
			t.Errorf("%s:\n got %s\nwant %s", in, got, want)
		}
	}
}

func TestMergeNodeOptions(t *testing.T) {
	const u = "file:///app/node_modules/@acme/provider/dist/register.js"
	for _, tc := range []struct{ existing, want string }{
		{"", "--import " + u},
		{"   ", "--import " + u},
		{"--max-old-space-size=4096", "--max-old-space-size=4096 --import " + u},
		{"--inspect --enable-source-maps ", "--inspect --enable-source-maps --import " + u},
		{`--require "./my setup.js"`, `--require "./my setup.js" --import ` + u},
		// Already loaded, by name or by path: left exactly as it is.
		{"--import @leadline/node/register", "--import @leadline/node/register"},
		{"--inspect --import " + u, "--inspect --import " + u},
	} {
		if got := MergeNodeOptions(tc.existing, u); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.existing, got, tc.want)
		}
	}
	// A value Node would split is quoted.
	if got := MergeNodeOptions("", `C:\my app\register.js`); got != `--import "C:\\my app\\register.js"` {
		t.Errorf("quoting: got %q", got)
	}
}

func TestNodeProvider(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "my project", "apps", "web")
	write(t, filepath.Join(app, "package.json"), `{"name":"web"}`)
	if _, ok := NodeProvider(app); ok {
		t.Fatal("found a provider that is not installed")
	}

	// Installed at the workspace root, as a hoisting package manager does.
	pkg := filepath.Join(root, "my project", "node_modules", "@leadline", "node")
	write(t, filepath.Join(pkg, "package.json"), `{"name":"@leadline/node","exports":{".":{"default":"./dist/index.js"},"./register":{"types":"./dist/register.d.ts","default":"./dist/register.js"}}}`)
	if _, ok := NodeProvider(app); ok {
		t.Fatal("found a provider whose file is not built")
	}
	write(t, filepath.Join(pkg, "dist", "register.js"), "")
	got, ok := NodeProvider(app)
	if !ok {
		t.Fatal("not found")
	}
	// A file: URL, so a folder with a space needs no quoting in NODE_OPTIONS.
	if !strings.HasPrefix(got, "file:///") || !strings.HasSuffix(got, "/my%20project/node_modules/@leadline/node/dist/register.js") {
		t.Errorf("got %q", got)
	}
	if merged := MergeNodeOptions("", got); strings.ContainsAny(merged, `"`) || strings.Count(merged, " ") != 1 {
		t.Errorf("NODE_OPTIONS would be split wrongly: %q", merged)
	}

	// "exports" as a plain path, and with no exports at all.
	write(t, filepath.Join(pkg, "package.json"), `{"exports":{"./register":"./lib/reg.mjs"}}`)
	write(t, filepath.Join(pkg, "lib", "reg.mjs"), "")
	if got, _ := NodeProvider(app); !strings.HasSuffix(got, "/lib/reg.mjs") {
		t.Errorf("string export: got %q", got)
	}
	write(t, filepath.Join(pkg, "package.json"), `{}`)
	if got, _ := NodeProvider(app); !strings.HasSuffix(got, "/dist/register.js") {
		t.Errorf("no exports: got %q", got)
	}
}

func TestImportSupported(t *testing.T) {
	for version, want := range map[string]bool{
		"v22.22.2": true, "v20.6.0": true, "v24.0.0": true, "v21.1.0": true,
		"v20.5.1": false, "v18.20.0": false, "v16.20.2": false, "": false, "garbage": false,
	} {
		if got := importSupported(version); got != want {
			t.Errorf("%q: got %v", version, got)
		}
	}
}
