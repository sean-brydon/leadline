package runner

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Dotenv is what a project's .env files set, read the way Next.js, Vite,
// dotenv-flow and Rails' dotenv read them: for mode "development" the files
// are .env.development.local, .env.local, .env.development and .env, and
// the first file to set a variable wins. The process's own environment
// outranks all of them; the caller applies that.
type Dotenv struct {
	Values map[string]string
	// Which file each value came from, for saying so.
	Source map[string]string
	// Files are the files that were there to read, in the order read.
	Files []string
}

// ReadDotenv reads dir's .env files for a mode ("development" when empty).
func ReadDotenv(dir, mode string, env func(string) string) Dotenv {
	if mode == "" {
		mode = "development"
	}
	names := []string{".env." + mode + ".local"}
	// The convention those tools share: tests do not see .env.local, so they
	// run the same for everyone.
	if mode != "test" {
		names = append(names, ".env.local")
	}
	names = append(names, ".env."+mode, ".env")

	d := Dotenv{Values: map[string]string{}, Source: map[string]string{}}
	type raw struct {
		value  string
		expand bool
	}
	found := map[string]raw{}
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		d.Files = append(d.Files, name)
		for _, kv := range parseDotenv(string(b)) {
			if _, ok := found[kv.key]; !ok {
				found[kv.key] = raw{kv.value, kv.expand}
				d.Source[kv.key] = name
			}
		}
	}
	// ${VAR} and $VAR refer to the environment first, then to the files.
	var resolve func(key string, depth int) string
	resolve = func(key string, depth int) string {
		if v := env(key); v != "" {
			return v
		}
		r, ok := found[key]
		if !ok || !r.expand || depth > 8 {
			return r.value
		}
		return expandVars(r.value, func(name string) string { return resolve(name, depth+1) })
	}
	for key, r := range found {
		if r.expand {
			d.Values[key] = expandVars(r.value, func(name string) string { return resolve(name, 1) })
		} else {
			d.Values[key] = r.value
		}
	}
	return d
}

type dotenvPair struct {
	key, value string
	// False for a single-quoted value, which is taken as written.
	expand bool
}

var dotenvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// parseDotenv reads KEY=value lines: an optional `export`, values bare or
// in single, double or back quotes (quoted ones may span lines), `\n` in
// double quotes, and a ` #` comment after a bare value.
func parseDotenv(src string) []dotenvPair {
	var out []dotenvPair
	src = strings.ReplaceAll(src, "\r\n", "\n")
	for len(src) > 0 {
		line, rest, _ := strings.Cut(src, "\n")
		src = rest
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			// YAML-ish "KEY: value" is accepted by dotenv too.
			if key, value, ok = strings.Cut(line, ":"); !ok {
				continue
			}
		}
		key = strings.TrimSpace(key)
		if !dotenvKey.MatchString(key) {
			continue
		}
		value = strings.TrimLeft(value, " \t")
		pair := dotenvPair{key: key, expand: true}
		if value != "" && strings.ContainsRune("\"'`", rune(value[0])) {
			quote := value[0]
			body := value[1:]
			end := closingQuote(body, quote)
			for end < 0 && len(src) > 0 {
				// The value runs on to a later line.
				line, rest, _ = strings.Cut(src, "\n")
				src = rest
				body += "\n" + line
				end = closingQuote(body, quote)
			}
			if end < 0 {
				// Never closed: dotenv takes the first line as it stands.
				pair.value, _, _ = strings.Cut(value, "\n")
			} else {
				pair.value = body[:end]
				switch quote {
				case '"':
					pair.value = doubleQuoted.Replace(pair.value)
				case '\'':
					pair.expand = false
				}
			}
		} else {
			if i := strings.Index(value, " #"); i >= 0 {
				value = value[:i]
			} else if strings.HasPrefix(value, "#") {
				value = ""
			}
			pair.value = strings.TrimSpace(value)
		}
		out = append(out, pair)
	}
	return out
}

var doubleQuoted = strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t", `\"`, `"`, `\\`, `\`)

func closingQuote(s string, quote byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && quote == '"' {
			i++
			continue
		}
		if s[i] == quote {
			return i
		}
	}
	return -1
}

var varRef = regexp.MustCompile(`\\\$|\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expandVars fills in ${VAR}, ${VAR:-default} and $VAR. `\$` is a dollar.
func expandVars(s string, lookup func(string) string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return varRef.ReplaceAllStringFunc(s, func(ref string) string {
		if ref == `\$` {
			return "$"
		}
		m := varRef.FindStringSubmatch(ref)
		name := m[1] + m[3]
		if v := lookup(name); v != "" {
			return v
		}
		return m[2]
	})
}
