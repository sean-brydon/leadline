package integrations

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

// Problem is an error written for the person at the screen: what is wrong
// in a sentence, and what to try. Code is for the app to branch on.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (p *Problem) Error() string { return p.Message }

func problem(code, message, hint string) *Problem {
	return &Problem{Code: code, Message: message, Hint: hint}
}

func notFound(what string) *Problem {
	return problem("not_found", "There is no "+what+" with that id.", "")
}

// AsProblem is err as a Problem, wrapping anything that isn't one.
func AsProblem(err error) *Problem {
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	return problem("failed", oneLine(err.Error()), "")
}

// explain turns what went wrong while talking to t into a Problem. The raw
// error is a driver's: it names functions and wraps three layers deep. A
// person needs the one clause that says what to fix.
func explain(err error, t *target) *Problem {
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	what, where := "Postgres", t.addr()
	if t.kind == KindRedis {
		what = "Redis"
	}

	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		msg := sentence(scrub(pg.Message, t))
		switch pg.Code {
		case "28P01", "28000":
			return problem("auth_failed", msg, "Check the user and password in the URL.")
		case "3D000":
			return problem("no_database", msg, "Check the database name at the end of the URL.")
		case "53300":
			return problem("too_many_connections", msg, "Postgres has no connection slot free. Close some clients, or raise max_connections.")
		case "57P03":
			return problem("starting", msg, "Postgres is still starting. Try again in a moment.")
		case "42501":
			return problem("permission", msg, "This user may not read that. Leadline shows what the user in the URL can see.")
		case "57014":
			return problem("timeout", "Postgres cancelled the query: it ran past Leadline's time limit.", "Leadline keeps its own queries short so it can't hold a database up.")
		case "25006":
			return problem("read_only", msg, "Leadline only reads. This is a bug in Leadline, not in your database.")
		}
		return problem("postgres_error", msg, "")
	}

	var re respError
	if errors.As(err, &re) {
		msg := string(re)
		switch {
		case strings.HasPrefix(msg, "NOAUTH"):
			return problem("auth_failed", "This Redis needs a password and the URL has none.", "Put it in the URL, as in redis://:password@"+where+".")
		case strings.HasPrefix(msg, "WRONGPASS"), strings.Contains(msg, "invalid password"), strings.Contains(msg, "invalid username-password"):
			return problem("auth_failed", "Redis refused the user or password.", "Check them in the URL: redis://user:password@"+where+", or redis://:password@"+where+" when there is no user.")
		case strings.HasPrefix(msg, "NOPERM"):
			return problem("permission", sentence(scrub(msg, t)), "This Redis user may not run that command. Leadline needs INFO, and SCAN, TYPE, MEMORY and SLOWLOG for the rest.")
		case strings.HasPrefix(msg, "DENIED"):
			return problem("protected", "Redis is in protected mode and only answers on its own machine.", "Give it a password, or connect through localhost.")
		case strings.HasPrefix(msg, "LOADING"):
			return problem("starting", "Redis is still loading its data.", "Try again in a moment.")
		}
		return problem("redis_error", sentence(scrub(msg, t)), "")
	}

	var dns *net.DNSError
	if errors.As(err, &dns) {
		return problem("no_host", "There is no server called "+dns.Name+".", "Check the host name in the URL. A name that only works on a VPN needs the VPN up.")
	}
	var (
		certErr    *tls.CertificateVerificationError
		unknownCA  x509.UnknownAuthorityError
		hostErr    x509.HostnameError
		recordErr  tls.RecordHeaderError
		isCertFail = errors.As(err, &certErr) || errors.As(err, &unknownCA) || errors.As(err, &hostErr)
	)
	text := err.Error()
	switch {
	case isCertFail:
		return problem("tls", what+" at "+where+" has a certificate this machine doesn't trust.", "Use the host name the certificate was issued for, or sslmode=require to encrypt without checking it.")
	case errors.As(err, &recordErr), strings.Contains(text, "server refused TLS"), strings.Contains(text, "first record does not look like a TLS handshake"):
		if t.kind == KindRedis {
			return problem("tls", "Redis at "+where+" does not speak TLS, and the URL asks for it.", "Use redis:// instead of rediss://.")
		}
		return problem("tls", "Postgres at "+where+" does not accept TLS, and the URL asks for it.", "Add sslmode=disable to the URL. That is normal for a database on this machine.")
	case errors.Is(err, syscall.ECONNREFUSED):
		return problem("unreachable", "Nothing is listening at "+where+".", "Is "+what+" running? Check the port: a database in Docker is reached on the port it publishes.")
	case errors.Is(err, os.ErrNotExist), errors.Is(err, syscall.ENOENT):
		return problem("unreachable", "There is no socket at "+where+".", "Is "+what+" running? Check the socket directory in the URL.")
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return problem("timeout", what+" at "+where+" did not answer in time.", "A firewall or a VPN that is down looks like this. So does a wrong port on a remote host.")
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return problem("unreachable", "This machine has no route to "+where+".", "Check the address, and the network or VPN it needs.")
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return problem("closed", "The server at "+where+" closed the connection without answering.", "Is that port really "+what+"?")
	case errors.Is(err, errNotRedis):
		return problem("not_redis", "Whatever answers at "+where+" does not speak the Redis protocol.", "Check the port.")
	}
	return problem("connection_failed", sentence(scrub(lastClause(err), t)), "")
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// sentence makes a server's message read like one: a capital, a full stop.
func sentence(s string) string {
	s = oneLine(s)
	if s == "" {
		return s
	}
	if c := s[0]; c >= 'a' && c <= 'z' {
		s = string(c-32) + s[1:]
	}
	if last := s[len(s)-1]; last != '.' && last != '?' && last != '!' {
		s += "."
	}
	return s
}
