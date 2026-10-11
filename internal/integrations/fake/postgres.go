// Package fake has a Postgres and a Redis that live inside a test: each
// speaks its wire protocol well enough for Leadline's own clients, and
// answers with made-up "acme" data. Tests need no Docker and no network.
package fake

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// Postgres is a pretend Postgres server.
type Postgres struct {
	User, Password, Database string
	Version                  string

	ln net.Listener
	mu sync.Mutex
	// See SetStatements and SetPooler.
	statements string
	pooler     bool
	// Databases made after it started; see AddDatabase.
	databases []string
	// What clients sent: every statement, and every startup packet.
	ran      []string
	startups []map[string]string
}

// SetStatements sets the state of pg_stat_statements: "" for neither
// loaded nor created (how it starts), "loaded" for loaded but not created,
// "created" for created but not loaded, "ok" for working.
func (p *Postgres) SetStatements(state string) {
	p.mu.Lock()
	p.statements = state
	p.mu.Unlock()
}

// AddDatabase gives the server another database beside the one it starts
// with, as a worktree's setup script would with createdb.
func (p *Postgres) AddDatabase(name string) {
	p.mu.Lock()
	p.databases = append(p.databases, name)
	p.mu.Unlock()
}

func (p *Postgres) has(database string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, name := range p.databases {
		if name == database {
			return true
		}
	}
	return false
}

// SetPooler makes it refuse startup parameters it doesn't know, the way
// PgBouncer does.
func (p *Postgres) SetPooler(on bool) {
	p.mu.Lock()
	p.pooler = on
	p.mu.Unlock()
}

// NewPostgres starts a pretend Postgres on a loopback port. It takes user
// "acme" with password "acme-pg-secret" into database "acme_dev".
func NewPostgres(t testing.TB) *Postgres {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &Postgres{User: "acme", Password: "acme-pg-secret", Database: "acme_dev", Version: "17.0", ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	return p
}

// Addr is host:port.
func (p *Postgres) Addr() string { return p.ln.Addr().String() }

// URL is a URL that connects.
func (p *Postgres) URL() string {
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", p.User, p.Password, p.Addr(), p.Database)
}

// Close stops the server, as if Postgres were shut down.
func (p *Postgres) Close() { p.ln.Close() }

// Ran is every statement clients have sent, in order.
func (p *Postgres) Ran() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.ran...)
}

// Startups is the parameters of every startup packet received.
func (p *Postgres) Startups() []map[string]string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]map[string]string(nil), p.startups...)
}

// settings are the startup parameters this server takes: what the
// protocol itself has, and the settings Leadline and a driver send.
var settings = map[string]bool{
	"user": true, "database": true, "options": true, "replication": true,
	"application_name": true, "client_encoding": true, "datestyle": true, "timezone": true, "intervalstyle": true,
	"search_path": true, "extra_float_digits": true, "bytea_output": true, "standard_conforming_strings": true,
	"default_transaction_read_only": true, "statement_timeout": true, "lock_timeout": true,
	"idle_in_transaction_session_timeout": true,
}

func pgError(severity, code, message string) *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{Severity: severity, SeverityUnlocalized: severity, Code: code, Message: message}
}

func (p *Postgres) serve(c net.Conn) {
	defer c.Close()
	be := pgproto3.NewBackend(c, c)
	var start *pgproto3.StartupMessage
	for start == nil {
		first, err := be.ReceiveStartupMessage()
		if err != nil {
			return
		}
		switch m := first.(type) {
		case *pgproto3.StartupMessage:
			start = m
		case *pgproto3.SSLRequest, *pgproto3.GSSEncRequest:
			if _, err := c.Write([]byte{'N'}); err != nil {
				return
			}
		default:
			return
		}
	}
	p.mu.Lock()
	p.startups = append(p.startups, start.Parameters)
	pooler := p.pooler
	p.mu.Unlock()

	fatal := func(code, message string) {
		be.Send(pgError("FATAL", code, message))
		be.Flush()
	}
	if pooler {
		for name := range start.Parameters {
			switch name {
			case "user", "database", "application_name", "client_encoding":
			default:
				fatal("08P01", "unsupported startup parameter: "+name)
				return
			}
		}
	}
	user := start.Parameters["user"]
	be.Send(&pgproto3.AuthenticationCleartextPassword{})
	if be.Flush() != nil {
		return
	}
	be.SetAuthType(pgproto3.AuthTypeCleartextPassword)
	msg, err := be.Receive()
	if err != nil {
		return
	}
	if pw, ok := msg.(*pgproto3.PasswordMessage); !ok || user != p.User || pw.Password != p.Password {
		fatal("28P01", fmt.Sprintf("password authentication failed for user %q", user))
		return
	}
	if db := start.Parameters["database"]; db != p.Database && !p.has(db) {
		fatal("3D000", fmt.Sprintf("database %q does not exist", db))
		return
	}
	// A startup parameter is a setting, and one the server does not have
	// ends the connection: what Postgres does with Prisma's `schema`.
	for name := range start.Parameters {
		if !settings[strings.ToLower(name)] {
			fatal("42704", fmt.Sprintf("unrecognized configuration parameter %q", name))
			return
		}
	}

	be.Send(&pgproto3.AuthenticationOk{})
	for k, v := range map[string]string{
		"server_version":              p.Version,
		"client_encoding":             "UTF8",
		"standard_conforming_strings": "on",
		"integer_datetimes":           "on",
		"application_name":            start.Parameters["application_name"],
	} {
		be.Send(&pgproto3.ParameterStatus{Name: k, Value: v})
	}
	be.Send(&pgproto3.BackendKeyData{ProcessID: 4242, SecretKey: []byte{1, 2, 3, 4}})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if be.Flush() != nil {
		return
	}

	status := byte('I')
	for {
		msg, err := be.Receive()
		if err != nil {
			return
		}
		q, ok := msg.(*pgproto3.Query)
		if !ok {
			return // Terminate, or something this server doesn't speak
		}
		for _, sql := range strings.Split(q.String, ";") {
			sql = strings.TrimSpace(sql)
			if sql == "" {
				continue
			}
			p.mu.Lock()
			p.ran = append(p.ran, sql)
			p.mu.Unlock()
			if errResp := p.run(be, sql, &status); errResp != nil {
				be.Send(errResp)
				if status == 'T' {
					status = 'E'
				}
				break // the rest of the message is skipped, as Postgres does
			}
		}
		be.Send(&pgproto3.ReadyForQuery{TxStatus: status})
		if be.Flush() != nil {
			return
		}
	}
}

func tag(be *pgproto3.Backend, s string) {
	be.Send(&pgproto3.CommandComplete{CommandTag: []byte(s)})
}

func (p *Postgres) run(be *pgproto3.Backend, sql string, status *byte) *pgproto3.ErrorResponse {
	upper := strings.ToUpper(sql)
	word := strings.Fields(upper)[0]
	switch {
	case word == "ROLLBACK" || word == "COMMIT":
		*status = 'I'
		tag(be, word)
		return nil
	case *status == 'E':
		return pgError("ERROR", "25P02", "current transaction is aborted, commands ignored until end of transaction block")
	case word == "BEGIN":
		*status = 'T'
		tag(be, "BEGIN")
		return nil
	case word == "SET":
		tag(be, "SET")
		return nil
	case word != "SELECT":
		// Leadline only reads. Anything else is refused the way a
		// read-only transaction would refuse it.
		return pgError("ERROR", "25006", "cannot execute "+word+" in a read-only transaction")
	}
	rows, errResp := p.answer(sql)
	if errResp != nil {
		return errResp
	}
	width := 1
	if len(rows) > 0 {
		width = len(rows[0])
	}
	fields := make([]pgproto3.FieldDescription, width)
	for i := range fields {
		fields[i] = pgproto3.FieldDescription{Name: []byte(fmt.Sprintf("c%d", i)), DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1}
	}
	be.Send(&pgproto3.RowDescription{Fields: fields})
	for _, r := range rows {
		values := make([][]byte, len(r))
		for i, v := range r {
			if v != nil {
				values[i] = []byte(*v)
			}
		}
		be.Send(&pgproto3.DataRow{Values: values})
	}
	tag(be, fmt.Sprintf("SELECT %d", len(rows)))
	return nil
}

type row = []*string

// r makes a row. "\N" stands for NULL, as in COPY.
func r(values ...string) row {
	out := make(row, len(values))
	for i := range values {
		if values[i] != `\N` {
			out[i] = &values[i]
		}
	}
	return out
}

// answer is the acme database: three tables, one of them read end to end
// far too often, and a query waiting on a lock.
func (p *Postgres) answer(sql string) ([]row, *pgproto3.ErrorResponse) {
	has := func(s string) bool { return strings.Contains(sql, s) }
	p.mu.Lock()
	statements := p.statements
	p.mu.Unlock()
	notLoaded := pgError("ERROR", "55000", `pg_stat_statements must be loaded via "shared_preload_libraries"`)
	switch {
	case has("boom"):
		return nil, pgError("ERROR", "42601", `syntax error at or near "boom"`)
	case has("pg_sleep"):
		return nil, pgError("ERROR", "57014", "canceling statement due to statement timeout")

	case has(".pg_stat_statements"):
		switch {
		case statements == "created":
			return nil, notLoaded
		case has("SELECT 1 FROM"):
			return []row{r("1")}, nil
		case !has("total_exec_time"):
			// Version 1.11, as this is, has had no total_time since 1.8.
			return nil, pgError("ERROR", "42703", `column "total_time" does not exist`)
		}
		return []row{
			r("SELECT * FROM orders WHERE user_id = $1", "1204", "9321.5", "7.742", "28896", "900", "100"),
			r("SELECT id, email FROM users WHERE id = $1", "8000", "410.25", "0.051", "8000", "0", "0"),
		}, nil
	case has("pg_available_extensions"):
		ext, schema, preload := `\N`, `\N`, ""
		if statements == "created" || statements == "ok" {
			ext, schema = "1.11", "public"
		}
		if statements == "loaded" || statements == "ok" {
			preload = "pg_stat_statements"
		}
		return []row{r(ext, schema, "1.11", preload)}, nil

	case has("pg_postmaster_start_time"):
		return []row{r(p.Database, p.User, p.Version, "26121907", "1791642889887.376", "f", "100", "3", `\N`)}, nil
	case has("FROM pg_stat_activity WHERE backend_type"):
		return []row{r("7", "2", "4", "1", "1")}, nil
	case has("count(*) FROM pg_stat_user_tables"):
		return []row{r("3")}, nil
	case has("pg_total_relation_size"):
		return []row{
			r("public", "orders", "120000", "10772480", "7266304", "3506176", "27", "0", "12"),
			r("public", "events", "60000", "6881280", "5103616", "1777664", "2", "40", "0"),
			r("public", "users", "5000", "540672", "409600", "131072", "1", "9000", "0"),
		}, nil
	case has("seq_tup_read"):
		return []row{r("public", "orders", "120000", "27", "3240000", "0", "7266304")}, nil
	case has("indexrelname"):
		return []row{
			r("public", "orders", "orders_pkey", "0", "2703360", "t", "t"),
			r("public", "orders", "orders_status_idx", "0", "802816", "f", "f"),
			r("public", "users", "users_pkey", "9000", "131072", "t", "t"),
		}, nil
	case has("FROM pg_stat_user_indexes"):
		return []row{r("5", "2", "1220608")}, nil
	case has("pg_blocking_pids"):
		return []row{r("812", "acme", "acme-web", "transactionid", "ShareLock", "", "5321.4", "UPDATE orders SET status = 'paid' WHERE id = 7", "790")}, nil
	case has("pg_backend_pid"):
		return []row{
			r("790", "acme", "acme-worker", "idle in transaction", "Client: ClientRead", "60211.2", "60250.9", "UPDATE orders SET status = 'refunded' WHERE id = 7", "127.0.0.1"),
			r("812", "acme", "acme-web", "active", "Lock: transactionid", "5321.4", "5330.1", "UPDATE orders SET status = 'paid' WHERE id = 7", "127.0.0.1"),
		}, nil
	case has("current_database(), current_user"):
		return []row{r(p.Database, p.User)}, nil
	case has("FROM orders"):
		return []row{r("24")}, nil
	}
	return []row{r("1")}, nil
}

// Ports finds a stretch of n loopback ports that are free right now, for a
// test that needs ports of its own: the system picks where, so two test
// binaries running at once get different stretches.
func Ports(t testing.TB, n int) (first, last int) {
	t.Helper()
	for range 50 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		first = ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		free := first+n-1 <= 65535
		for port := first; free && port < first+n; port++ {
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err != nil {
				free = false
				break
			}
			l.Close()
		}
		if free {
			return first, first + n - 1
		}
	}
	t.Fatal("no free stretch of ports")
	return 0, 0
}
