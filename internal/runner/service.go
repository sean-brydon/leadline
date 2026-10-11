package runner

import (
	"bufio"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// ServiceName settles the name a run shows under in Leadline: the name
// given, else OTEL_SERVICE_NAME, else the name the nearest project file
// declares (package.json, go.mod, pyproject.toml), else the folder's name.
func ServiceName(given string, env func(string) string, dir string) string {
	if s := strings.TrimSpace(given); s != "" {
		return s
	}
	if s := strings.TrimSpace(env("OTEL_SERVICE_NAME")); s != "" {
		return s
	}
	for at := dir; ; at = filepath.Dir(at) {
		if s := projectName(at); s != "" {
			return s
		}
		if filepath.Dir(at) == at {
			break
		}
	}
	if s := filepath.Base(dir); s != "" && s != "." && s != string(filepath.Separator) {
		return s
	}
	return "app"
}

// projectName reads the name a directory's own project file gives it. A
// file that is there but names nothing (a bare {"type":"module"}, a
// pyproject.toml that only configures a linter) is passed over.
func projectName(dir string) string {
	if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		var pkg struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(b, &pkg) == nil && strings.TrimSpace(pkg.Name) != "" {
			return strings.TrimSpace(pkg.Name)
		}
	}
	if s := goModuleName(filepath.Join(dir, "go.mod")); s != "" {
		return s
	}
	return pyprojectName(filepath.Join(dir, "pyproject.toml"))
}

var majorVersion = regexp.MustCompile(`^v\d+$`)

// goModuleName is the last part of the module path that is a name:
// github.com/acme/api/v2 is "api".
func goModuleName(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || fields[0] != "module" {
			continue
		}
		mod := strings.Trim(fields[1], `"`)
		name := path.Base(mod)
		if majorVersion.MatchString(name) {
			name = path.Base(path.Dir(mod))
		}
		if name == "." || name == "/" {
			return ""
		}
		return name
	}
	return ""
}

var tomlName = regexp.MustCompile(`^name\s*=\s*["']([^"']+)["']`)

// pyprojectName reads `name` from [project] or [tool.poetry]. That much of
// TOML is all it understands, which is all those two tables need.
func pyprojectName(file string) string {
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	table, poetry := "", ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			table = strings.Trim(line, "[] \t")
			continue
		}
		m := tomlName.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch table {
		case "project":
			return m[1]
		case "tool.poetry":
			poetry = m[1]
		}
	}
	return poetry
}
