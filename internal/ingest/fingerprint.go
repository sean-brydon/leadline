package ingest

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	reString = regexp.MustCompile(`'(?:[^']|'')*'`)
	reParam  = regexp.MustCompile(`\$\d+`)
	// A named parameter (:id, @id) starts a token. A colon inside one, as
	// in the Redis key "user:42", is not a parameter.
	reNamed  = regexp.MustCompile(`(^|[\s(,=<>])[:@]\w+`)
	reNumber = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)
	reInList = regexp.MustCompile(`(?i)\bIN\s*\(\s*\?(?:\s*,\s*\?)*\s*\)`)
	reSpace  = regexp.MustCompile(`\s+`)
)

const maxFingerprint = 2000

// Fingerprint reduces a statement to its shape, so the same query with
// different values groups as one: literals and placeholders become "?",
// an IN list of any length becomes "IN (?)", and whitespace collapses. A
// closing semicolon is dropped: psql sends one and a driver doesn't, and
// the two are the same statement.
func Fingerprint(statement string) string {
	s := reString.ReplaceAllString(statement, "?")
	// "::" is a Postgres cast, not a named parameter.
	s = strings.ReplaceAll(s, "::", "\x00")
	s = reParam.ReplaceAllString(s, "?")
	s = reNamed.ReplaceAllString(s, "${1}?")
	s = strings.ReplaceAll(s, "\x00", "::")
	s = reNumber.ReplaceAllString(s, "?")
	s = reInList.ReplaceAllString(s, "IN (?)")
	s = strings.TrimRight(strings.TrimSpace(reSpace.ReplaceAllString(s, " ")), "; ")
	if len(s) > maxFingerprint {
		s = s[:maxFingerprint]
	}
	return s
}

// Commands whose first argument is part of their name: CONFIG GET, not
// CONFIG.
var redisContainers = map[string]bool{
	"ACL": true, "CLIENT": true, "CLUSTER": true, "COMMAND": true, "CONFIG": true, "DEBUG": true,
	"FUNCTION": true, "LATENCY": true, "MEMORY": true, "MODULE": true, "OBJECT": true, "PUBSUB": true,
	"SCRIPT": true, "SLOWLOG": true, "XGROUP": true, "XINFO": true,
}

// RedisCommand is a Redis command's name in capitals, and how many of args
// it took up.
func RedisCommand(args []string) (string, int) {
	if len(args) == 0 {
		return "", 0
	}
	name := strings.ToUpper(clip(args[0], 32))
	if redisContainers[name] && len(args) > 1 {
		return name + " " + strings.ToUpper(clip(args[1], 32)), 2
	}
	return name, 1
}

var (
	reUUID = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	// An id in hex, as in a session key or a content hash.
	reHexID = regexp.MustCompile(`(?i)\b[0-9a-f]{16,}\b`)
)

// RedisFingerprint groups Redis commands by what they do to which kind of
// key: GET user:42 and GET user:43 are both "GET user:?". Only the command
// and its first argument (the key, nearly always) count; what is written is
// a value, and values are what a fingerprint leaves out. A command the app
// reports and the same command captured from the server get the same one.
func RedisFingerprint(args []string) string {
	name, used := RedisCommand(args)
	if name == "AUTH" || len(args) == used {
		return name
	}
	key := clip(oneLine(args[used]), 80)
	key = reUUID.ReplaceAllString(key, "?")
	key = reHexID.ReplaceAllString(key, "?")
	fp := Fingerprint(name + " " + key)
	if len(args) > used+1 {
		fp += " …"
	}
	return fp
}

// oneLine makes an argument safe to show and store: valid UTF-8, on one
// line.
func oneLine(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	if strings.ContainsAny(s, "\r\n\t") {
		s = strings.Join(strings.Fields(s), " ")
	}
	return s
}

// clip cuts s to at most n bytes, on a rune boundary, and says so.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
