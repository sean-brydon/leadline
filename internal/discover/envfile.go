package discover

import (
	"bytes"
	"path/filepath"
	"regexp"
)

// envFileNames are the env files read, in the order the dev tools give
// them: the first to set a variable wins. They are the files
// runner.ReadDotenv reads for development, which are the ones Next.js,
// Vite, dotenv-flow and Rails' dotenv read.
var envFileNames = []string{".env.development.local", ".env.local", ".env.development", ".env"}

// envURL is a connection URL as an env file had it. raw is the secret:
// it goes to describeURL and to the scan's private table, nowhere else.
type envURL struct {
	key  string
	file string
	kind string
	raw  string
	// refs: the value refers to other variables ($VAR, ${VAR}), which
	// are not read, so the URL is not whole.
	refs bool
	// effective is false when a file that outranks this one sets the key.
	effective bool
}

// projectEnv is what a project's env files gave: counts, and the URLs.
type projectEnv struct {
	files []EnvFile
	urls  []envURL
}

// readEnv reads a project's env files for connection URLs.
//
// The parser is runner.parseDotenv's, which is not exported, redone over
// bytes for one reason: a value that is not a connection URL is never
// made into a string. It is a span of the file's buffer that is looked at
// and passed over, and the buffer is zeroed before this returns. Unlike
// the runner's, nothing is interpolated: that would mean reading the
// other variables.
func (f files) readEnv(dir string) projectEnv {
	var out projectEnv
	// The names of the variables set so far, to tell which file's value
	// of a variable the dev tools would use. Names only.
	set := map[string]bool{}
	for _, name := range envFileNames {
		buf, err := f.readFile(filepath.Join(dir, name), maxEnvFile)
		if err != nil {
			continue
		}
		file := EnvFile{File: name, Read: true}
		here := map[string]bool{}
		parseEnv(buf, func(key, value []byte, expand bool) {
			k := string(key)
			kind := urlKind(value)
			if kind == "" {
				file.OtherKeys++
			} else {
				raw, refs := finishValue(value, expand)
				file.URLs++
				out.urls = append(out.urls, envURL{key: k, file: name, kind: kind, raw: raw, refs: refs, effective: !set[k]})
			}
			here[k] = true
		})
		clear(buf)
		for k := range here {
			set[k] = true
		}
		out.files = append(out.files, file)
	}
	return out
}

// parseEnv reads KEY=value lines and calls each with spans of src: an
// optional `export`, values bare or in single, double or back quotes
// (quoted ones may span lines), and a ` #` comment after a bare value.
// A double-quoted value is given as written, escapes and all (see
// finishValue). expand is false for a single-quoted value, which the dev
// tools take as written.
func parseEnv(src []byte, each func(key, value []byte, expand bool)) {
	for pos := 0; pos < len(src); {
		end := pos + lineEnd(src[pos:])
		next := end + 1
		line := trimSpaceSpan(src, pos, end)
		pos = next
		if line.empty() || src[line.from] == '#' {
			continue
		}
		rest := src[line.from:line.to]
		if bytes.HasPrefix(rest, []byte("export ")) || bytes.HasPrefix(rest, []byte("export\t")) {
			line = trimSpaceSpan(src, line.from+7, line.to)
			rest = src[line.from:line.to]
		}
		sep := bytes.IndexByte(rest, '=')
		if sep < 0 {
			// YAML-ish "KEY: value" is accepted by dotenv too.
			if sep = bytes.IndexByte(rest, ':'); sep < 0 {
				continue
			}
		}
		key := bytes.TrimSpace(rest[:sep])
		if !envKey.Match(key) {
			continue
		}
		// The value starts after the separator and any blanks.
		from := line.from + sep + 1
		for from < line.to && (src[from] == ' ' || src[from] == '\t') {
			from++
		}
		if from < line.to && (src[from] == '"' || src[from] == '\'' || src[from] == '`') {
			quote := src[from]
			// The closing quote may be on a later line.
			if n := closingQuote(src[from+1:], quote); n >= 0 {
				closeAt := from + 1 + n
				each(key, src[from+1:closeAt], quote != '\'')
				if closeAt >= end {
					// It ran on: carry on after the line it closed on.
					pos = closeAt + lineEnd(src[closeAt:]) + 1
				}
				continue
			}
			// Never closed: dotenv takes the first line as it stands.
			each(key, src[from:line.to], true)
			continue
		}
		value := src[from:line.to]
		if i := bytes.Index(value, []byte(" #")); i >= 0 {
			value = value[:i]
		} else if len(value) > 0 && value[0] == '#' {
			value = nil
		}
		each(key, bytes.TrimSpace(value), true)
	}
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

type span struct{ from, to int }

func (s span) empty() bool { return s.from >= s.to }

// lineEnd is the offset of the newline that ends the line src starts in,
// or len(src).
func lineEnd(src []byte) int {
	if i := bytes.IndexByte(src, '\n'); i >= 0 {
		return i
	}
	return len(src)
}

func trimSpaceSpan(src []byte, from, to int) span {
	for from < to && isSpace(src[from]) {
		from++
	}
	for to > from && isSpace(src[to-1]) {
		to--
	}
	return span{from, to}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func closingQuote(s []byte, quote byte) int {
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

// connection URL schemes, and the kind each is. A `+driver` suffix
// (SQLAlchemy's postgresql+psycopg) and a `jdbc:` prefix are allowed for.
var urlSchemes = map[string]string{
	"postgres": "postgres", "postgresql": "postgres",
	"redis": "redis", "rediss": "redis",
	"mysql": "mysql", "mysql2": "mysql", "mariadb": "mysql",
	"mongodb": "mongodb",
}

// urlKind says which kind of connection URL a value is, or "" for
// anything else. It reads only as far as the scheme, and then checks the
// value is one word: a URL does not have a space or a line break in it.
func urlKind(value []byte) string {
	if len(value) < 8 || len(value) > 4096 {
		return ""
	}
	head := value
	if len(head) > 48 {
		head = head[:48]
	}
	head = bytes.ToLower(head)
	head = bytes.TrimPrefix(head, []byte("jdbc:"))
	i := bytes.Index(head, []byte("://"))
	if i <= 0 {
		return ""
	}
	scheme, driver, plus := bytes.Cut(head[:i], []byte("+"))
	kind := urlSchemes[string(scheme)]
	if kind == "" {
		return ""
	}
	// redis+sentinel and prisma+postgres are other things; mongodb+srv is
	// MongoDB.
	if plus && kind == "redis" {
		return ""
	}
	if plus && kind == "mongodb" && string(driver) != "srv" {
		return ""
	}
	for _, c := range value {
		if c <= ' ' || c == 0x7f {
			return ""
		}
	}
	return kind
}

var envRef = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*(?::?-[^}]*)?\}|\$[A-Za-z_][A-Za-z0-9_]*`)

// finishValue makes the string of a value that is a connection URL, and
// says whether it refers to other variables. `\$` is a dollar, as the dev
// tools have it; a single-quoted value is as written.
func finishValue(value []byte, expand bool) (raw string, refs bool) {
	if !expand {
		return string(value), false
	}
	// With the escaped dollars out of the way, any $NAME left is a
	// reference.
	bare := bytes.ReplaceAll(value, []byte(`\$`), nil)
	refs = envRef.Match(bare)
	clear(bare)
	if !bytes.ContainsRune(value, '\\') {
		return string(value), refs
	}
	// A URL has no use for the other escapes (\n, \"), and can't hold
	// what they stand for, so only the dollar is unescaped.
	out := bytes.ReplaceAll(value, []byte(`\$`), []byte("$"))
	raw = string(out)
	clear(out)
	return raw, refs
}
