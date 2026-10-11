package discover

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sean-brydon/leadline/internal/runner"
)

// markers are the files that make a folder a project. A git root is one
// too, whatever is in it.
var markers = []string{"package.json", "go.mod", "pyproject.toml", "Gemfile", "Cargo.toml"}

// projectID is the same for the same path in every scan, so the app can
// keep a tick against a project across scans.
func projectID(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "p_" + hex.EncodeToString(sum[:5])
}

// detect reads dir as a project: what it is built with, how it is
// started, and whether it is wired to Leadline. It reads the project's own
// files and, for the lock file of a package inside a workspace, the names
// in the folders above it up to boundary. ok is false when dir is not a
// project.
func (f files) detect(dir, boundary string) (*Project, bool) {
	entries, err := f.readDir(dir)
	if err != nil {
		return nil, false
	}
	return f.detectIn(dir, boundary, entries)
}

// detectIn is detect for a folder whose entries are already listed.
func (f files) detectIn(dir, boundary string, entries []fs.DirEntry) (*Project, bool) {
	has := map[string]fs.DirEntry{}
	for _, e := range entries {
		has[e.Name()] = e
	}
	found := has[".git"] != nil
	for _, m := range markers {
		if e := has[m]; e != nil && !e.IsDir() {
			found = true
		}
	}
	if !found {
		return nil, false
	}

	p := &Project{ID: projectID(dir), Path: dir, Name: filepath.Base(dir), Stack: "unknown", boundary: boundary,
		Sources: []string{}, Frameworks: []Framework{}, Running: []Running{}, EnvFiles: []EnvFile{}, ConnectionIDs: []string{},
		Sends: Sends{Because: []string{}}}
	// Which env files it has, from the listing alone: none is opened, or
	// so much as stat'ed, here.
	for _, name := range envFileNames {
		if e := has[name]; e != nil && !e.IsDir() {
			p.EnvFiles = append(p.EnvFiles, EnvFile{File: name})
		}
	}
	named := false
	read := func(name string) []byte {
		if e := has[name]; e == nil || e.IsDir() {
			return nil
		}
		b, _ := f.readFile(filepath.Join(dir, name), maxProjectFile)
		return b
	}

	// The first stack that fits names it; a Rails app with a package.json
	// for its assets is Ruby.
	switch {
	case has["Gemfile"] != nil:
		f.ruby(p, read)
	case has["go.mod"] != nil:
		named = f.golang(p, dir, has, read) || named
	case has["pyproject.toml"] != nil || (has["requirements.txt"] != nil || has["manage.py"] != nil) && has["package.json"] == nil:
		// requirements.txt and manage.py do not make a folder a project
		// on their own, but they say what a git checkout is.
		named = f.python(p, has, read) || named
	case has["Cargo.toml"] != nil:
		f.rust(p, read)
	}
	if has["package.json"] != nil {
		named = f.node(p, dir, boundary, has, read, p.Stack == "unknown") || named
	}
	if named {
		// The name `leadline run` gives a command started here, by the
		// runner's own rule. Only asked when a project file here names
		// it: otherwise the runner would go looking in the folders
		// above, which a scan has no business reading.
		p.Name = runner.ServiceName("", func(string) string { return "" }, dir)
	}
	p.Label = label(p)
	return p, true
}

type packageJSON struct {
	Name            string            `json:"name"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	PackageManager  string            `json:"packageManager"`
}

// nodeFrameworks, in the order they are told: what the app is built as
// first, then what it is bundled with, then what serves it.
var nodeFrameworks = []struct{ id, name, pkg, class string }{
	{"next", "Next.js", "next", "app"},
	{"nuxt", "Nuxt", "nuxt", "app"},
	{"remix", "Remix", "@remix-run/dev", "app"},
	{"react-router", "React Router", "@react-router/dev", "app"},
	{"astro", "Astro", "astro", "app"},
	{"sveltekit", "SvelteKit", "@sveltejs/kit", "app"},
	{"gatsby", "Gatsby", "gatsby", "app"},
	{"angular", "Angular", "@angular/core", "app"},
	{"expo", "Expo", "expo", "app"},
	{"vite", "Vite", "vite", "bundler"},
	{"create-react-app", "Create React App", "react-scripts", "bundler"},
	{"nest", "NestJS", "@nestjs/core", "server"},
	{"express", "Express", "express", "server"},
	{"fastify", "Fastify", "fastify", "server"},
	{"hono", "Hono", "hono", "server"},
	{"koa", "Koa", "koa", "server"},
}

// devScripts are the script names that start a project in development,
// best first.
var devScripts = []string{"dev", "start:dev", "develop", "serve", "start"}

// node reads package.json. primary is false when another stack already
// named the project (a Rails app's package.json): then only what is
// missing is filled in.
func (f files) node(p *Project, dir, boundary string, has map[string]fs.DirEntry, read func(string) []byte, primary bool) (named bool) {
	var pkg packageJSON
	if json.Unmarshal(read("package.json"), &pkg) != nil {
		// Not JSON anyone can read: still a project, of which nothing is
		// known.
		if primary {
			p.Stack = "node"
		}
		return false
	}
	deps := map[string]string{}
	for name, v := range pkg.DevDependencies {
		deps[name] = v
	}
	for name, v := range pkg.Dependencies {
		deps[name] = v
	}
	for name := range deps {
		if strings.HasPrefix(name, "@opentelemetry/") || name == "@vercel/otel" {
			p.OpenTelemetry = true
		}
	}
	named = strings.TrimSpace(pkg.Name) != ""
	if !primary {
		return named
	}
	p.Stack = "node"
	p.PackageManager = f.nodePackageManager(pkg.PackageManager, dir, boundary, has)
	for _, fw := range nodeFrameworks {
		spec, ok := deps[fw.pkg]
		if !ok {
			continue
		}
		version := f.installedVersion(dir, fw.pkg)
		if version == "" {
			version = specVersion(spec)
		}
		p.Frameworks = append(p.Frameworks, Framework{ID: fw.id, Name: fw.name, Version: version})
	}
	for _, name := range devScripts {
		script, ok := pkg.Scripts[name]
		if !ok || strings.TrimSpace(script) == "" {
			continue
		}
		p.DevCommand = runScript(p.PackageManager, name)
		p.DevScript = &DevScript{File: "package.json", Name: name, Command: clip(redactCommand(script), maxCommand)}
		if usesLeadlineRun(script) {
			p.Sends.Because = append(p.Sends.Because, "dev-script")
		}
		break
	}

	// Wired to Leadline already? The plugin counts when next.config
	// names it, since a dependency that nothing loads sends nothing.
	p.nextConfig, _ = f.nextConfig(dir, has)
	if _, ok := deps["@leadline/next"]; ok {
		p.hasPlugin = true
		if _, wired := f.nextConfig(dir, has); p.nextConfig == "" || wired {
			p.Sends.Because = append(p.Sends.Because, "plugin")
		}
	}
	if _, ok := deps["@leadline/node"]; ok {
		p.Sends.Because = append(p.Sends.Because, "provider")
	}
	return named
}

var leadlineRun = regexp.MustCompile(`\bleadline(d)? run\b`)

func usesLeadlineRun(script string) bool { return leadlineRun.MatchString(script) }

// nextConfig finds the project's next.config and says whether it loads
// the Leadline plugin.
func (f files) nextConfig(dir string, has map[string]fs.DirEntry) (name string, wired bool) {
	for _, name := range []string{"next.config.ts", "next.config.mjs", "next.config.js", "next.config.mts", "next.config.cjs"} {
		if has[name] == nil {
			continue
		}
		b, _ := f.readFile(filepath.Join(dir, name), maxProjectFile)
		return name, strings.Contains(string(b), "@leadline/next")
	}
	return "", false
}

var lockFiles = []struct{ file, manager string }{
	{"pnpm-lock.yaml", "pnpm"}, {"yarn.lock", "yarn"}, {"bun.lock", "bun"}, {"bun.lockb", "bun"}, {"package-lock.json", "npm"},
}

// nodePackageManager is the package manager a Node project uses: what
// package.json says, else whose lock file is here, else whose is in a
// folder above (a package in a workspace), else npm.
func (f files) nodePackageManager(declared, dir, boundary string, has map[string]fs.DirEntry) string {
	if name, _, _ := strings.Cut(declared, "@"); name == "pnpm" || name == "yarn" || name == "bun" || name == "npm" {
		return name
	}
	for _, l := range lockFiles {
		if has[l.file] != nil {
			return l.manager
		}
	}
	for at := filepath.Dir(dir); within(boundary, at) && at != dir; at = filepath.Dir(at) {
		for _, l := range lockFiles {
			if f.exists(filepath.Join(at, l.file)) {
				return l.manager
			}
		}
		if filepath.Dir(at) == at {
			break
		}
	}
	return "npm"
}

// installedVersion is the version of a package in the project's own
// node_modules. A package that is a link (pnpm's are) leads out of the
// project and is not followed; the version asked for stands in.
func (f files) installedVersion(dir, pkg string) string {
	at := filepath.Join(dir, "node_modules", filepath.FromSlash(pkg))
	if !f.isDir(at) {
		return ""
	}
	b, err := f.readFile(filepath.Join(at, "package.json"), maxProjectFile)
	if err != nil {
		return ""
	}
	var v struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &v) != nil {
		return ""
	}
	return cleanVersion(v.Version)
}

var versionNumber = regexp.MustCompile(`^\d+(\.\d+){0,2}`)

// cleanVersion is a version's number alone, or "".
func cleanVersion(v string) string {
	return versionNumber.FindString(strings.TrimPrefix(strings.TrimSpace(v), "v"))
}

// specVersion reads the version a dependency range asks for: "^16.0.1"
// is "16.0.1". A range that names no number ("latest", "workspace:*") is
// "".
func specVersion(spec string) string {
	spec = strings.TrimSpace(spec)
	spec = strings.TrimPrefix(spec, "npm:")
	return cleanVersion(strings.TrimLeft(spec, "^~>=< v"))
}

func runScript(manager, script string) string {
	switch manager {
	case "pnpm", "yarn":
		if script == "dev" || script == "start" || script == "serve" || script == "develop" {
			return manager + " " + script
		}
		return manager + " run " + script
	case "bun":
		return "bun run " + script
	}
	if script == "start" {
		return "npm start"
	}
	return "npm run " + script
}

var goFrameworks = []struct{ id, name, module string }{
	{"gin", "Gin", "github.com/gin-gonic/gin"},
	{"echo", "Echo", "github.com/labstack/echo"},
	{"chi", "chi", "github.com/go-chi/chi"},
	{"fiber", "Fiber", "github.com/gofiber/fiber"},
	{"gorilla", "gorilla/mux", "github.com/gorilla/mux"},
	{"connect", "Connect", "connectrpc.com/connect"},
}

func (f files) golang(p *Project, dir string, has map[string]fs.DirEntry, read func(string) []byte) (named bool) {
	p.Stack, p.PackageManager = "go", "go"
	goVersion := ""
	for _, line := range strings.Split(string(read("go.mod")), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch {
		case fields[0] == "module":
			named = true
		case fields[0] == "go":
			goVersion = majorMinor(cleanVersion(fields[1]))
		}
		// A requirement: "require x v1" on one line, or "x v1" in a block.
		mod := fields[0]
		version := fields[1]
		if mod == "require" && len(fields) > 2 {
			mod, version = fields[1], fields[2]
		}
		if strings.HasPrefix(mod, "go.opentelemetry.io/otel") {
			p.OpenTelemetry = true
		}
		for _, fw := range goFrameworks {
			if mod == fw.module || strings.HasPrefix(mod, fw.module+"/") {
				p.Frameworks = appendFramework(p.Frameworks, Framework{ID: fw.id, Name: fw.name, Version: cleanVersion(version)})
			}
		}
	}
	// Go itself leads, so the label reads "Go 1.27".
	p.Frameworks = append([]Framework{{ID: "go", Name: "Go", Version: goVersion}}, p.Frameworks...)

	switch {
	case has[".air.toml"] != nil:
		p.DevCommand, p.DevScript = "air", &DevScript{File: ".air.toml"}
	case has["main.go"] != nil:
		p.DevCommand, p.DevScript = "go run .", &DevScript{File: "main.go"}
	case has["cmd"] != nil && has["cmd"].IsDir():
		// One command under cmd/ is the one to run; with several, which
		// is the server is anyone's guess.
		var mains []string
		cmds, _ := f.readDir(filepath.Join(dir, "cmd"))
		for _, c := range cmds {
			if c.IsDir() && f.exists(filepath.Join(dir, "cmd", c.Name(), "main.go")) {
				mains = append(mains, c.Name())
			}
		}
		if len(mains) == 1 {
			p.DevCommand, p.DevScript = "go run ./cmd/"+mains[0], &DevScript{File: "cmd/" + mains[0] + "/main.go"}
		}
	}
	return named
}

func appendFramework(list []Framework, fw Framework) []Framework {
	for _, have := range list {
		if have.ID == fw.ID {
			return list
		}
	}
	return append(list, fw)
}

var pythonFrameworks = []struct{ id, name, pkg string }{
	{"django", "Django", "django"},
	{"fastapi", "FastAPI", "fastapi"},
	{"flask", "Flask", "flask"},
	{"starlette", "Starlette", "starlette"},
}

// pythonDep finds a requirement on pkg in a pyproject.toml or a
// requirements.txt, and the version it pins or sets a floor at, if it
// names one: `django>=5.1`, `"django[argon2]==5.1.2"`, `django = "^5.1"`.
func pythonDep(text, pkg string) (version string, ok bool) {
	re := regexp.MustCompile(`(?im)(?:^|["'\s,\[])` + regexp.QuoteMeta(pkg) + `(?:\[[^\]]*\])?\s*(?:$|["',;\]#]|(?:=\s*["'{]?[^0-9\n]*|[=~<>!^]+\s*)([0-9][0-9A-Za-z.]*)?)`)
	m := re.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return cleanVersion(m[1]), true
}

func (f files) python(p *Project, has map[string]fs.DirEntry, read func(string) []byte) (named bool) {
	p.Stack = "python"
	pyproject := string(read("pyproject.toml"))
	text := pyproject + "\n" + string(read("requirements.txt"))
	switch {
	case has["uv.lock"] != nil:
		p.PackageManager = "uv"
	case has["poetry.lock"] != nil || strings.Contains(pyproject, "[tool.poetry]"):
		p.PackageManager = "poetry"
	case has["Pipfile"] != nil:
		p.PackageManager = "pipenv"
	default:
		p.PackageManager = "pip"
	}
	for _, fw := range pythonFrameworks {
		if version, ok := pythonDep(text, fw.pkg); ok {
			p.Frameworks = append(p.Frameworks, Framework{ID: fw.id, Name: fw.name, Version: version})
		}
	}
	if has["manage.py"] != nil {
		p.Frameworks = appendFramework(p.Frameworks, Framework{ID: "django", Name: "Django"})
	}
	if strings.Contains(strings.ToLower(text), "opentelemetry-") {
		p.OpenTelemetry = true
	}

	prefix := ""
	switch p.PackageManager {
	case "uv":
		prefix = "uv run "
	case "poetry":
		prefix = "poetry run "
	case "pipenv":
		prefix = "pipenv run "
	}
	is := func(id string) bool {
		for _, fw := range p.Frameworks {
			if fw.ID == id {
				return true
			}
		}
		return false
	}
	switch {
	case has["manage.py"] != nil:
		p.DevCommand, p.DevScript = prefix+"python manage.py runserver", &DevScript{File: "manage.py"}
	case is("fastapi") && has["main.py"] != nil:
		p.DevCommand, p.DevScript = prefix+"fastapi dev main.py", &DevScript{File: "main.py"}
	case is("flask") && has["app.py"] != nil:
		p.DevCommand, p.DevScript = prefix+"flask run --debug", &DevScript{File: "app.py"}
	}
	return pyprojectNamed(pyproject)
}

var tomlName = regexp.MustCompile(`^name\s*=\s*["'][^"']+["']`)

// pyprojectNamed reports whether a pyproject.toml names its project where
// runner.pyprojectName looks: `name` in [project] or [tool.poetry]. It
// only says there is a name to ask the runner for.
func pyprojectNamed(text string) bool {
	table := ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			table = strings.Trim(line, "[] \t")
			continue
		}
		if (table == "project" || table == "tool.poetry") && tomlName.MatchString(line) {
			return true
		}
	}
	return false
}

var rubyFrameworks = []struct{ id, name, gem string }{
	{"rails", "Rails", "rails"},
	{"sinatra", "Sinatra", "sinatra"},
	{"hanami", "Hanami", "hanami"},
}

func (f files) ruby(p *Project, read func(string) []byte) {
	p.Stack, p.PackageManager = "ruby", "bundler"
	gemfile, lock := string(read("Gemfile")), string(read("Gemfile.lock"))
	for _, fw := range rubyFrameworks {
		m := regexp.MustCompile(`(?m)^\s*gem\s+["']` + fw.gem + `["'](?:\s*,\s*["']([^"']+)["'])?`).FindStringSubmatch(gemfile)
		if m == nil {
			continue
		}
		version := specVersion(m[1])
		if l := regexp.MustCompile(`(?m)^ {4}` + fw.gem + ` \(([^)]+)\)`).FindStringSubmatch(lock); l != nil {
			version = cleanVersion(l[1])
		}
		p.Frameworks = append(p.Frameworks, Framework{ID: fw.id, Name: fw.name, Version: version})
	}
	if strings.Contains(gemfile, "opentelemetry-") {
		p.OpenTelemetry = true
	}
	if len(p.Frameworks) > 0 && p.Frameworks[0].ID == "rails" {
		p.DevCommand, p.DevScript = "bin/rails server", &DevScript{File: "bin/rails"}
		if f.exists(filepath.Join(p.Path, "bin", "dev")) {
			p.DevCommand, p.DevScript = "bin/dev", &DevScript{File: "bin/dev"}
		}
	}
}

var rustFrameworks = []struct{ id, name, crate string }{
	{"axum", "Axum", "axum"},
	{"actix", "Actix Web", "actix-web"},
	{"rocket", "Rocket", "rocket"},
	{"warp", "warp", "warp"},
}

func (f files) rust(p *Project, read func(string) []byte) {
	p.Stack, p.PackageManager = "rust", "cargo"
	cargo := string(read("Cargo.toml"))
	for _, fw := range rustFrameworks {
		m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(fw.crate) + `\s*=\s*(?:"([^"]*)"|\{[^}\n]*?version\s*=\s*"([^"]*)")?`).FindStringSubmatch(cargo)
		if m == nil {
			continue
		}
		p.Frameworks = append(p.Frameworks, Framework{ID: fw.id, Name: fw.name, Version: specVersion(m[1] + m[2])})
	}
	if regexp.MustCompile(`(?m)^\s*opentelemetry[a-z_-]*\s*=`).MatchString(cargo) {
		p.OpenTelemetry = true
	}
	if strings.Contains(cargo, "[package]") {
		p.DevCommand, p.DevScript = "cargo run", &DevScript{File: "Cargo.toml"}
	}
}

func majorMinor(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, ".")
}

// shortVersion is the part of a version worth showing beside a name:
// "16" of "16.0.1", and "0.8" of "0.8.4", where the minor is the real
// version.
func shortVersion(v string) string {
	major, _, _ := strings.Cut(v, ".")
	if major == "0" {
		return majorMinor(v)
	}
	return major
}

var stackNames = map[string]string{"node": "Node.js", "go": "Go", "python": "Python", "ruby": "Ruby", "rust": "Rust", "unknown": "Folder"}

// label is the stack as a person would say it: "Next.js 16",
// "Vite + Express", "Go 1.27".
func label(p *Project) string {
	fws := p.Frameworks
	if len(fws) == 0 {
		return stackNames[p.Stack]
	}
	with := func(fw Framework, version string) string {
		if version == "" {
			return fw.Name
		}
		return fw.Name + " " + version
	}
	if p.Stack == "go" {
		// Go's own version is the one that matters, in full.
		out := with(fws[0], fws[0].Version)
		if len(fws) > 1 {
			out += " + " + fws[1].Name
		}
		return out
	}
	if p.Stack == "node" {
		// What the app is built as says it all; a bundler and a server
		// are both worth naming.
		for _, fw := range fws {
			if nodeClass(fw.ID) == "app" {
				return with(fw, shortVersion(fw.Version))
			}
		}
	}
	if len(fws) == 1 {
		version := shortVersion(fws[0].Version)
		if p.Stack == "python" || p.Stack == "ruby" {
			version = majorMinor(fws[0].Version)
		}
		return with(fws[0], version)
	}
	return fws[0].Name + " + " + fws[1].Name
}

func nodeClass(id string) string {
	for _, fw := range nodeFrameworks {
		if fw.id == id {
			return fw.class
		}
	}
	return ""
}

func addUnique(list []string, s string) []string {
	for _, have := range list {
		if have == s {
			return list
		}
	}
	list = append(list, s)
	sort.Strings(list)
	return list
}
