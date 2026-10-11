package integrations

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Reading a Postgres. Leadline connects as the user in the URL and only
// ever asks the statistics views what they hold. Three things keep that
// harmless on a database that matters:
//
//   - the session is opened read-only (default_transaction_read_only), and
//     every query runs in its own BEGIN READ ONLY besides, which holds
//     through a pooler that shares server connections between clients;
//   - every query has a short statement_timeout and lock_timeout, so it
//     gives up rather than wait or make others wait;
//   - nothing here writes: not CREATE EXTENSION, not ANALYZE, not a reset.

const (
	pgConnectTimeout = 5 * time.Second
	pgStatementMs    = 3000
	pgLockMs         = 500
	// One section's whole exchange: the statement's own limit, and room
	// for the network.
	pgSectionTimeout = 5 * time.Second
)

// pgBegin opens every read. SET LOCAL lasts only as long as the transaction,
// which is what makes it safe through a pooler: a plain SET there would
// stay on a server connection that the app gets next.
var pgBegin = fmt.Sprintf("BEGIN READ ONLY; SET LOCAL statement_timeout = %d; SET LOCAL lock_timeout = %d; ", pgStatementMs, pgLockMs)

// How many results pgBegin itself produces.
const pgBeginResults = 3

// pgSession is what the session is opened with. It goes in the startup
// packet, so it belongs to this session alone and nothing has to be SET.
var pgSession = map[string]string{
	"default_transaction_read_only":       "on",
	"statement_timeout":                   strconv.Itoa(pgStatementMs),
	"lock_timeout":                        strconv.Itoa(pgLockMs),
	"idle_in_transaction_session_timeout": "10000",
}

func connectPostgres(ctx context.Context, t *target) (*pgconn.PgConn, error) {
	// Without an ORM's own parameters: see target.dial.
	cfg, err := pgconn.ParseConfig(t.dial)
	if err != nil {
		return nil, errURL("That Postgres URL can't be read: " + lastClause(err) + ".")
	}
	cfg.ConnectTimeout = pgConnectTimeout
	if cfg.RuntimeParams["application_name"] == "" {
		cfg.RuntimeParams["application_name"] = "leadline"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*pgConnectTimeout)
	defer cancel()

	guarded := cfg.Copy()
	for k, v := range pgSession {
		guarded.RuntimeParams[k] = v
	}
	conn, err := pgconn.ConnectConfig(ctx, guarded)
	if err != nil && refusedStartupParams(err) {
		// A pooler (PgBouncer) refuses startup parameters it doesn't know.
		// Every read is still in a read-only transaction with its own
		// limits; see pgBegin.
		conn, err = pgconn.ConnectConfig(ctx, cfg)
	}
	return conn, err
}

func refusedStartupParams(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return false
	}
	msg := strings.ToLower(pg.Message)
	return strings.Contains(msg, "startup parameter") || (pg.Code == "08P01" && strings.Contains(msg, "unsupported"))
}

type pgRow [][]byte

func (r pgRow) s(i int) string {
	if i >= len(r) {
		return ""
	}
	return string(r[i])
}

func (r pgRow) i(i int) int64 {
	s := r.s(i)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	// A bigint that came back as a numeric, or a real.
	f, _ := strconv.ParseFloat(s, 64)
	return int64(f)
}

func (r pgRow) f(i int) float64 {
	f, _ := strconv.ParseFloat(r.s(i), 64)
	return f
}

func (r pgRow) b(i int) bool { s := r.s(i); return s == "t" || s == "true" }

func (r pgRow) null(i int) bool { return i >= len(r) || r[i] == nil }

// pgRead runs one or more SELECTs in a read-only transaction and returns
// each one's rows. Values come back as text, which every server speaks
// whatever its version.
func pgRead(ctx context.Context, conn *pgconn.PgConn, sql string) ([][]pgRow, error) {
	ctx, cancel := context.WithTimeout(ctx, pgSectionTimeout)
	defer cancel()
	results, err := conn.Exec(ctx, pgBegin+sql+"; ROLLBACK").ReadAll()
	if err != nil {
		if !conn.IsClosed() {
			// The failed statement left the transaction open and broken.
			conn.Exec(ctx, "ROLLBACK").ReadAll()
		}
		return nil, err
	}
	if len(results) < pgBeginResults+2 {
		return nil, fmt.Errorf("postgres answered %d of the statements sent", len(results))
	}
	results = results[pgBeginResults : len(results)-1]
	out := make([][]pgRow, len(results))
	for i, res := range results {
		rows := make([]pgRow, len(res.Rows))
		for j, r := range res.Rows {
			rows[j] = pgRow(r)
		}
		out[i] = rows
	}
	return out, nil
}

// PostgresStats is what a Postgres knows about itself.
type PostgresStats struct {
	Kind        string  `json:"kind"`
	CollectedMs float64 `json:"collected_ms"`
	TookMs      float64 `json:"took_ms"`
	Remote      bool    `json:"remote"`
	// ReadOnly is always true: it says how Leadline opened the session.
	ReadOnly bool `json:"read_only"`

	Version   string  `json:"version"`
	Database  string  `json:"database"`
	User      string  `json:"user"`
	SizeBytes int64   `json:"size_bytes"`
	StartedMs float64 `json:"started_ms"`
	// StatsSinceMs is when the counters behind the scan and index numbers
	// were last reset; absent when they never were.
	StatsSinceMs *float64 `json:"stats_since_ms"`
	Replica      bool     `json:"replica"`

	Connections       PgConnections    `json:"connections"`
	TableCount        int64            `json:"table_count"`
	Tables            []PgTable        `json:"tables"`
	MissingIndexHints []PgSeqScanHint  `json:"missing_index_hints"`
	Indexes           PgIndexes        `json:"indexes"`
	Running           []PgActivity     `json:"running"`
	Locks             []PgLock         `json:"locks"`
	Statements        PgStatStatements `json:"pg_stat_statements"`
	Incomplete        []SectionError   `json:"incomplete"`
}

type PgConnections struct {
	Used int64 `json:"used"`
	Max  int64 `json:"max"`
	// Reserved is how many of Max only a superuser can take.
	Reserved          int64 `json:"reserved"`
	Active            int64 `json:"active"`
	Idle              int64 `json:"idle"`
	IdleInTransaction int64 `json:"idle_in_transaction"`
	Waiting           int64 `json:"waiting"`
}

type PgTable struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	// Rows is the planner's estimate, not a count.
	Rows       int64 `json:"rows"`
	TotalBytes int64 `json:"total_bytes"`
	TableBytes int64 `json:"table_bytes"`
	IndexBytes int64 `json:"index_bytes"`
	SeqScans   int64 `json:"seq_scans"`
	IndexScans int64 `json:"index_scans"`
	DeadRows   int64 `json:"dead_rows"`
}

// PgSeqScanHint is a big table that keeps being read from end to end,
// which is what a query with no index to use does.
type PgSeqScanHint struct {
	Schema       string `json:"schema"`
	Table        string `json:"table"`
	Rows         int64  `json:"rows"`
	SeqScans     int64  `json:"seq_scans"`
	RowsRead     int64  `json:"rows_read"`
	RowsPerScan  int64  `json:"rows_per_scan"`
	IndexScans   int64  `json:"index_scans"`
	TableBytes   int64  `json:"table_bytes"`
	Why          string `json:"why"`
	NextStepHint string `json:"next_step"`
}

type PgIndexes struct {
	Count int64 `json:"count"`
	// Unused are indexes never scanned since the counters were reset that
	// don't enforce anything: they cost writes and disk and give nothing.
	UnusedCount int64     `json:"unused_count"`
	UnusedBytes int64     `json:"unused_bytes"`
	Largest     []PgIndex `json:"largest"`
}

type PgIndex struct {
	Schema  string `json:"schema"`
	Table   string `json:"table"`
	Name    string `json:"name"`
	Scans   int64  `json:"scans"`
	Bytes   int64  `json:"bytes"`
	Unique  bool   `json:"unique"`
	Primary bool   `json:"primary"`
	Unused  bool   `json:"unused"`
}

type PgActivity struct {
	PID         int64  `json:"pid"`
	User        string `json:"user"`
	Application string `json:"application"`
	// State is "active", "idle in transaction" and the like.
	State   string `json:"state"`
	Waiting string `json:"waiting,omitempty"`
	// DurationMs is how long the current statement has run, or for a
	// session idle in a transaction, how long since its last one began.
	DurationMs    float64 `json:"duration_ms"`
	TransactionMs float64 `json:"transaction_ms"`
	Query         string  `json:"query"`
	Client        string  `json:"client,omitempty"`
}

type PgLock struct {
	PID         int64   `json:"pid"`
	User        string  `json:"user"`
	Application string  `json:"application"`
	LockType    string  `json:"lock_type"`
	Mode        string  `json:"mode"`
	Relation    string  `json:"relation,omitempty"`
	WaitingMs   float64 `json:"waiting_ms"`
	Query       string  `json:"query"`
	// BlockedBy are the pids holding what this one waits for; they are
	// usually in "running".
	BlockedBy []int64 `json:"blocked_by"`
}

// PgStatStatements is the extension's view, or why there is none.
type PgStatStatements struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	// Reason is "not_installed", "not_loaded", "not_available" or
	// "error" when Available is false.
	Reason     string        `json:"reason,omitempty"`
	Message    string        `json:"message,omitempty"`
	HowTo      []string      `json:"how_to,omitempty"`
	Statements []PgStatement `json:"statements"`
}

type PgStatement struct {
	Query   string  `json:"query"`
	Calls   int64   `json:"calls"`
	TotalMs float64 `json:"total_ms"`
	MeanMs  float64 `json:"mean_ms"`
	Rows    int64   `json:"rows"`
	// CacheHitRate is the share of pages found in Postgres's own cache,
	// 0 to 1; absent when the statement touched none.
	CacheHitRate *float64 `json:"cache_hit_rate"`
}

func postgresStats(ctx context.Context, conn *pgconn.PgConn, t *target) (*PostgresStats, error) {
	began := time.Now()
	out := &PostgresStats{Kind: KindPostgres, Remote: t.remote(), ReadOnly: true,
		Tables: []PgTable{}, MissingIndexHints: []PgSeqScanHint{}, Running: []PgActivity{}, Locks: []PgLock{},
		Incomplete: []SectionError{}}
	out.Indexes.Largest = []PgIndex{}
	out.Statements.Statements = []PgStatement{}

	// The overview is the part nothing else stands without: if it fails
	// the whole call does.
	if err := pgOverview(ctx, conn, out); err != nil {
		return nil, err
	}
	for _, sec := range []struct {
		name string
		read func(context.Context, *pgconn.PgConn, *PostgresStats) error
	}{
		{"tables", pgTables},
		{"missing_index_hints", pgSeqScans},
		{"indexes", pgIndexes},
		{"running", pgRunning},
		{"locks", pgLocks},
	} {
		if conn.IsClosed() || ctx.Err() != nil {
			out.Incomplete = append(out.Incomplete, SectionError{Section: sec.name, Message: "The connection ended before this was read."})
			continue
		}
		if err := sec.read(ctx, conn, out); err != nil {
			out.Incomplete = append(out.Incomplete, SectionError{Section: sec.name, Message: explain(err, t).Message})
		}
	}
	if !conn.IsClosed() && ctx.Err() == nil {
		out.Statements = pgStatStatements(ctx, conn, t, true)
	} else {
		out.Statements = PgStatStatements{Reason: "error", Message: "The connection ended before this was read.", Statements: []PgStatement{}}
	}

	out.CollectedMs = ms(time.Now())
	out.TookMs = float64(time.Since(began).Microseconds()) / 1000
	return out, nil
}

func pgOverview(ctx context.Context, conn *pgconn.PgConn, out *PostgresStats) error {
	res, err := pgRead(ctx, conn, `
SELECT current_database(), current_user, current_setting('server_version'),
       pg_database_size(current_database()),
       extract(epoch FROM pg_postmaster_start_time()) * 1000,
       pg_is_in_recovery(),
       current_setting('max_connections')::int,
       current_setting('superuser_reserved_connections')::int,
       (SELECT extract(epoch FROM stats_reset) * 1000 FROM pg_stat_database WHERE datname = current_database());
SELECT count(*),
       count(*) FILTER (WHERE state = 'active'),
       count(*) FILTER (WHERE state = 'idle'),
       count(*) FILTER (WHERE state LIKE 'idle in transaction%'),
       count(*) FILTER (WHERE wait_event_type = 'Lock')
FROM pg_stat_activity WHERE backend_type = 'client backend'`)
	if err != nil {
		return err
	}
	if len(res) != 2 || len(res[0]) != 1 || len(res[1]) != 1 {
		return fmt.Errorf("postgres gave no overview")
	}
	r := res[0][0]
	out.Database, out.User, out.Version = r.s(0), r.s(1), r.s(2)
	out.SizeBytes, out.StartedMs, out.Replica = r.i(3), r.f(4), r.b(5)
	out.Connections.Max, out.Connections.Reserved = r.i(6), r.i(7)
	if !r.null(8) {
		since := r.f(8)
		out.StatsSinceMs = &since
	}
	c := res[1][0]
	out.Connections.Used, out.Connections.Active, out.Connections.Idle = c.i(0), c.i(1), c.i(2)
	out.Connections.IdleInTransaction, out.Connections.Waiting = c.i(3), c.i(4)
	return nil
}

const (
	pgTopTables  = 15
	pgTopIndexes = 15
	pgTopRunning = 10
	pgTopHints   = 10
	// A table has to be this big before reading all of it is worth a
	// remark, and a scan has to read this much of something.
	pgHintMinRows    = 10_000
	pgHintMinPerScan = 1_000
	pgQueryChars     = 500
)

// pgRows is the planner's row estimate. For a table never analysed it is
// -1 (or 0 before Postgres 14), and the statistics collector's count of
// live rows stands in. That count is not used otherwise: it double-counts
// rows inserted just before an ANALYZE until the next one.
const pgRows = `(CASE WHEN c.reltuples > 0 THEN c.reltuples::bigint ELSE s.n_live_tup END)`

func pgTables(ctx context.Context, conn *pgconn.PgConn, out *PostgresStats) error {
	// Sizes are a file stat per relation, so they are only asked for the
	// tables that are large by the catalog's own page count.
	res, err := pgRead(ctx, conn, fmt.Sprintf(`
SELECT count(*) FROM pg_stat_user_tables;
SELECT schemaname, relname, rows, pg_total_relation_size(relid) AS total, pg_table_size(relid), pg_indexes_size(relid),
       seq_scan, idx_scan, n_dead_tup
FROM (
	SELECT s.schemaname, s.relname, s.relid, %s AS rows,
	       coalesce(s.seq_scan, 0) AS seq_scan, coalesce(s.idx_scan, 0) AS idx_scan, s.n_dead_tup
	FROM pg_stat_user_tables s JOIN pg_class c ON c.oid = s.relid
	ORDER BY c.relpages DESC, s.n_live_tup DESC LIMIT %d
) t ORDER BY total DESC, relname LIMIT %d`, pgRows, 4*pgTopTables, pgTopTables))
	if err != nil {
		return err
	}
	if len(res) != 2 {
		return fmt.Errorf("postgres gave no table list")
	}
	if len(res[0]) == 1 {
		out.TableCount = res[0][0].i(0)
	}
	for _, r := range res[1] {
		out.Tables = append(out.Tables, PgTable{
			Schema: r.s(0), Name: r.s(1), Rows: r.i(2),
			TotalBytes: r.i(3), TableBytes: r.i(4), IndexBytes: r.i(5),
			SeqScans: r.i(6), IndexScans: r.i(7), DeadRows: r.i(8),
		})
	}
	return nil
}

func pgSeqScans(ctx context.Context, conn *pgconn.PgConn, out *PostgresStats) error {
	res, err := pgRead(ctx, conn, fmt.Sprintf(`
SELECT s.schemaname, s.relname, %s, s.seq_scan, s.seq_tup_read, coalesce(s.idx_scan, 0), pg_table_size(s.relid)
FROM pg_stat_user_tables s JOIN pg_class c ON c.oid = s.relid
WHERE s.seq_scan > 0 AND %s >= %d AND s.seq_tup_read / s.seq_scan >= %d
ORDER BY s.seq_tup_read DESC LIMIT %d`, pgRows, pgRows, pgHintMinRows, pgHintMinPerScan, pgTopHints))
	if err != nil {
		return err
	}
	for _, r := range res[0] {
		h := PgSeqScanHint{Schema: r.s(0), Table: r.s(1), Rows: r.i(2), SeqScans: r.i(3), RowsRead: r.i(4),
			IndexScans: r.i(5), TableBytes: r.i(6)}
		h.RowsPerScan = h.RowsRead / max(h.SeqScans, 1)
		h.Why = fmt.Sprintf("%s was read from end to end %s, about %s rows each time, against %s by index.",
			h.Table, plural(h.SeqScans, "time", "times"), count(h.RowsPerScan), plural(h.IndexScans, "lookup", "lookups"))
		h.NextStepHint = "Find the queries that filter or join on " + h.Table + " (EXPLAIN shows \"Seq Scan\") and index the columns they use. " +
			"A report or a migration that really does read everything is fine as it is."
		out.MissingIndexHints = append(out.MissingIndexHints, h)
	}
	return nil
}

func pgIndexes(ctx context.Context, conn *pgconn.PgConn, out *PostgresStats) error {
	const unused = `s.idx_scan = 0 AND NOT i.indisunique AND NOT i.indisprimary`
	res, err := pgRead(ctx, conn, fmt.Sprintf(`
SELECT count(*), count(*) FILTER (WHERE %s),
       coalesce(sum(c.relpages::bigint * current_setting('block_size')::bigint) FILTER (WHERE %s), 0)
FROM pg_stat_user_indexes s JOIN pg_index i ON i.indexrelid = s.indexrelid JOIN pg_class c ON c.oid = s.indexrelid;
SELECT s.schemaname, s.relname, s.indexrelname, coalesce(s.idx_scan, 0), pg_relation_size(s.indexrelid), i.indisunique, i.indisprimary
FROM pg_stat_user_indexes s JOIN pg_index i ON i.indexrelid = s.indexrelid JOIN pg_class c ON c.oid = s.indexrelid
ORDER BY c.relpages DESC, s.indexrelname LIMIT %d`, unused, unused, pgTopIndexes))
	if err != nil {
		return err
	}
	if len(res) != 2 {
		return fmt.Errorf("postgres gave no index list")
	}
	if len(res[0]) == 1 {
		r := res[0][0]
		out.Indexes.Count, out.Indexes.UnusedCount, out.Indexes.UnusedBytes = r.i(0), r.i(1), r.i(2)
	}
	for _, r := range res[1] {
		ix := PgIndex{Schema: r.s(0), Table: r.s(1), Name: r.s(2), Scans: r.i(3), Bytes: r.i(4), Unique: r.b(5), Primary: r.b(6)}
		ix.Unused = ix.Scans == 0 && !ix.Unique && !ix.Primary
		out.Indexes.Largest = append(out.Indexes.Largest, ix)
	}
	return nil
}

func pgRunning(ctx context.Context, conn *pgconn.PgConn, out *PostgresStats) error {
	res, err := pgRead(ctx, conn, fmt.Sprintf(`
SELECT pid, coalesce(usename, ''), coalesce(application_name, ''), state,
       coalesce(wait_event_type || ': ' || wait_event, ''),
       coalesce(extract(epoch FROM (clock_timestamp() - query_start)) * 1000, 0),
       coalesce(extract(epoch FROM (clock_timestamp() - xact_start)) * 1000, 0),
       left(query, %d), coalesce(host(client_addr), '')
FROM pg_stat_activity
WHERE pid <> pg_backend_pid() AND backend_type = 'client backend'
  AND state IS NOT NULL AND state <> 'idle' AND datname = current_database()
ORDER BY query_start LIMIT %d`, pgQueryChars, pgTopRunning))
	if err != nil {
		return err
	}
	for _, r := range res[0] {
		a := PgActivity{PID: r.i(0), User: r.s(1), Application: r.s(2), State: r.s(3), Waiting: r.s(4),
			DurationMs: round1(r.f(5)), TransactionMs: round1(r.f(6)), Query: oneLine(r.s(7)), Client: r.s(8)}
		// "Client: ClientRead" only says the session is waiting for its
		// app, which the state already does.
		if strings.HasPrefix(a.Waiting, "Client:") {
			a.Waiting = ""
		}
		out.Running = append(out.Running, a)
	}
	return nil
}

func pgLocks(ctx context.Context, conn *pgconn.PgConn, out *PostgresStats) error {
	res, err := pgRead(ctx, conn, fmt.Sprintf(`
SELECT a.pid, coalesce(a.usename, ''), coalesce(a.application_name, ''), l.locktype, l.mode,
       coalesce(l.relation::regclass::text, ''),
       coalesce(extract(epoch FROM (clock_timestamp() - a.query_start)) * 1000, 0),
       left(a.query, %d), array_to_string(pg_blocking_pids(a.pid), ',')
FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
WHERE NOT l.granted AND a.datname = current_database()
ORDER BY a.query_start LIMIT %d`, pgQueryChars, pgTopRunning))
	if err != nil {
		return err
	}
	for _, r := range res[0] {
		l := PgLock{PID: r.i(0), User: r.s(1), Application: r.s(2), LockType: r.s(3), Mode: r.s(4), Relation: r.s(5),
			WaitingMs: round1(r.f(6)), Query: oneLine(r.s(7)), BlockedBy: []int64{}}
		for _, pid := range strings.Split(r.s(8), ",") {
			if n, err := strconv.ParseInt(pid, 10, 64); err == nil {
				l.BlockedBy = append(l.BlockedBy, n)
			}
		}
		out.Locks = append(out.Locks, l)
	}
	return nil
}

const pgssName = "pg_stat_statements"

// pgStatStatements reads the extension's view when it is there, and when
// it isn't says which of the two steps is missing. Leadline never takes
// either step itself: both change the database.
func pgStatStatements(ctx context.Context, conn *pgconn.PgConn, t *target, withRows bool) PgStatStatements {
	out := PgStatStatements{Statements: []PgStatement{}}
	fail := func(err error) PgStatStatements {
		out.Reason, out.Message = "error", explain(err, t).Message
		return out
	}
	res, err := pgRead(ctx, conn, `
SELECT (SELECT e.extversion FROM pg_extension e WHERE e.extname = 'pg_stat_statements'),
       (SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'pg_stat_statements'),
       (SELECT a.default_version FROM pg_available_extensions a WHERE a.name = 'pg_stat_statements'),
       coalesce(current_setting('shared_preload_libraries', true), '')`)
	if err != nil {
		return fail(err)
	}
	if len(res) != 1 || len(res[0]) != 1 {
		return fail(fmt.Errorf("postgres gave no answer about its extensions"))
	}
	r := res[0][0]
	created, onDisk := !r.null(0), !r.null(2)
	preloaded := false
	for _, lib := range strings.Split(r.s(3), ",") {
		if strings.TrimSpace(lib) == pgssName {
			preloaded = true
		}
	}
	out.Version = r.s(0)

	const (
		stepPreload = "Load it when Postgres starts: set shared_preload_libraries = 'pg_stat_statements' in postgresql.conf. " +
			"For Postgres in Docker, add this to the end of the run command instead: -c shared_preload_libraries=pg_stat_statements"
		stepRestart = "Restart Postgres."
		stepCreate  = "Run this once in the database: CREATE EXTENSION pg_stat_statements;"
		managed     = "On a managed database (RDS, Cloud SQL, Supabase, Neon) the first step is a setting in the provider's console, and is often on already."
	)
	notLoaded := func() PgStatStatements {
		out.Reason = "not_loaded"
		out.Message = "pg_stat_statements is not loaded, so Postgres is not keeping per-query totals. Leadline only reads, so it won't turn it on for you."
		out.HowTo = []string{stepPreload, stepRestart}
		if !created {
			out.HowTo = append(out.HowTo, stepCreate)
		}
		if t.remote() {
			out.HowTo = append(out.HowTo, managed)
		}
		return out
	}
	switch {
	case !created && !onDisk:
		out.Reason = "not_available"
		out.Message = "This Postgres was installed without pg_stat_statements, so it has no per-query totals to show."
		out.HowTo = []string{"Install Postgres's contrib package (postgresql-contrib on Debian and Ubuntu). The official Docker image already has it.",
			stepPreload, stepRestart, stepCreate}
		return out
	case !created && !preloaded:
		return notLoaded()
	case !created:
		out.Reason = "not_installed"
		out.Message = "pg_stat_statements is loaded but not created in this database. Leadline only reads, so it won't create it for you."
		out.HowTo = []string{stepCreate}
		return out
	}
	if !withRows {
		// Created, but is the library loaded? Only reading the view says
		// for certain: a user who can't see the setting sees it empty.
		if _, err := pgRead(ctx, conn, "SELECT 1 FROM "+quoteIdent(r.s(1))+".pg_stat_statements LIMIT 1"); err != nil {
			if pgCode(err) == "55000" {
				return notLoaded()
			}
			return fail(err)
		}
		out.Available = true
		return out
	}

	// total_time became total_exec_time in version 1.8 (Postgres 13).
	total, mean := "total_exec_time", "mean_exec_time"
	if olderThan(out.Version, 1, 8) {
		total, mean = "total_time", "mean_time"
	}
	rows, err := pgRead(ctx, conn, fmt.Sprintf(`
SELECT left(query, %d), calls, %s, %s, rows, shared_blks_hit, shared_blks_read
FROM %s.pg_stat_statements
WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
ORDER BY %s DESC LIMIT 10`, pgQueryChars, total, mean, quoteIdent(r.s(1)), total))
	if err != nil {
		if pgCode(err) == "55000" {
			return notLoaded()
		}
		return fail(err)
	}
	out.Available = true
	for _, r := range rows[0] {
		st := PgStatement{Query: oneLine(r.s(0)), Calls: r.i(1), TotalMs: round3(r.f(2)), MeanMs: round3(r.f(3)), Rows: r.i(4)}
		if hit, read := r.f(5), r.f(6); hit+read > 0 {
			rate := round3(hit / (hit + read))
			st.CacheHitRate = &rate
		}
		out.Statements = append(out.Statements, st)
	}
	return out
}

// olderThan compares an extension version such as "1.11" with major.minor.
// It is not a decimal: 1.11 comes after 1.8.
func olderThan(version string, major, minor int) bool {
	a, b, _ := strings.Cut(version, ".")
	maj, err := strconv.Atoi(a)
	if err != nil {
		return false
	}
	min, _ := strconv.Atoi(b)
	return maj < major || (maj == major && min < minor)
}

func pgCode(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func pgUsesTLS(conn *pgconn.PgConn) bool {
	_, ok := conn.Conn().(*tls.Conn)
	return ok
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
func round3(f float64) float64 { return float64(int64(f*1000+0.5)) / 1000 }

// count writes a number with thousands separators, for sentences.
func count(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return count(n) + " " + many
}
