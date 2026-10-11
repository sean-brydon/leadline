package brief

import (
	"crypto/rand"
	"regexp"
	"strings"
)

// A log line's or an error message's shape is the text with its values
// taken out, the way a statement's fingerprint is: what is left says what
// kind of thing happened and nothing about whom it happened to. It is also
// how log records that say the same thing are counted together.
var (
	reQuoted = regexp.MustCompile(`"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'`)
	reEmail  = regexp.MustCompile(`[\w.+-]+@[\w-]+(?:\.[\w-]+)+`)
	reUUID   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	// A long run of hex, or of letters and digits mixed: an id, a hash, a token.
	reHex   = regexp.MustCompile(`(?i)\b[0-9a-f]{12,}\b`)
	reToken = regexp.MustCompile(`\b[A-Za-z0-9_-]{24,}\b`)
	// A number, with the unit that may be stuck to it left standing: 1890ms.
	reNumber = regexp.MustCompile(`\b\d+(?:[.,:]\d+)*`)
	// Key (email)=(a@b.test): what Postgres appends to a constraint error.
	reKeyValue = regexp.MustCompile(`\)=\([^)]*\)`)
	reMarks    = regexp.MustCompile(`\?(?:\s*,\s*\?)+`)
)

// Shape reduces a line of text to its shape: quoted strings, numbers, ids,
// addresses and tokens become "?".
func Shape(text string) string {
	s := reKeyValue.ReplaceAllString(text, ")=(?)")
	s = reQuoted.ReplaceAllString(s, "?")
	s = reEmail.ReplaceAllString(s, "?")
	s = reUUID.ReplaceAllString(s, "?")
	s = reHex.ReplaceAllString(s, "?")
	s = reToken.ReplaceAllStringFunc(s, func(m string) string {
		// A long word is a name; a long mix of letters and digits is a value.
		if strings.ContainsAny(m, "0123456789") {
			return "?"
		}
		return m
	})
	s = reNumber.ReplaceAllString(s, "?")
	s = reMarks.ReplaceAllString(s, "?")
	return strings.TrimSpace(s)
}

// firstLine is a text's first line that has words in it.
func firstLine(text string) string {
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// A task's name becomes a branch, a folder and part of a URL in Shipyard:
// lower-case letters, digits and dashes, starting with a letter.
var (
	reNotName = regexp.MustCompile(`[^a-z0-9]+`)
	// TaskName is what a task may be called. It is narrower than what
	// Shipyard takes, on purpose: nothing that passes can be read as a flag,
	// a path or a ref with meaning to git.
	TaskName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
)

const maxTaskName = 40

// slug makes words into the middle of a task name.
func slug(words string, limit int) string {
	s := strings.Trim(reNotName.ReplaceAllString(strings.ToLower(words), "-"), "-")
	if len(s) > limit {
		s = strings.Trim(s[:limit], "-")
		// Cut at a word, when that loses little.
		if i := strings.LastIndexByte(s, '-'); i > limit*2/3 {
			s = s[:i]
		}
	}
	return s
}

// taskName names the work after the problem: "fix-n1-order-items-k3f9".
// The four characters at the end keep two tasks for one problem apart, and
// clear of a branch that is there already.
func taskName(o Options, what string) string {
	end := o.Suffix
	if end == "" {
		end = randomSuffix()
	}
	middle := slug(what, maxTaskName-len("fix-")-len(end)-1)
	if middle == "" {
		middle = "problem"
	}
	return "fix-" + middle + "-" + end
}

func randomSuffix() string {
	const letters = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "new"
	}
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

var (
	reTable = regexp.MustCompile(`(?i)\b(?:from|into|update|join|table)\s+(?:only\s+)?["` + "`" + `\[]?([a-z_][\w$]*(?:\.[a-z_][\w$]*)?)`)
	reWord  = regexp.MustCompile(`[A-Za-z_][\w]{2,}`)
)

// tableOf names what a statement is about: the first table of a SQL
// statement, or the key family of a Redis command ("products" for
// "GET products:category:?"). Empty when it can't tell.
func tableOf(system, shape string) string {
	if m := reTable.FindStringSubmatch(shape); m != nil {
		name := m[1]
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			name = name[i+1:]
		}
		return name
	}
	fields := strings.Fields(shape)
	if len(fields) >= 2 && system != "" {
		// "insert acme.receipts", "GET products:category:?".
		last := fields[len(fields)-1]
		if w := reWord.FindString(strings.NewReplacer(".", " ", ":", " ").Replace(last)); w != "" && len(fields) == 2 {
			if i := strings.LastIndexByte(last, '.'); i >= 0 {
				return last[i+1:]
			}
			return w
		}
	}
	return ""
}

// searchFor is a piece of a statement's shape to hand to list_queries'
// search: the table when there is one, else its words before the first
// value.
func searchFor(system, shape string) string {
	if t := tableOf(system, shape); t != "" {
		return t
	}
	head, _, _ := strings.Cut(shape, "?")
	head = strings.TrimSpace(head)
	if len(head) > 40 {
		head = head[:40]
	}
	return head
}
