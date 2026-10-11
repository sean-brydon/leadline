// Package store keeps spans and logs in one DuckDB file and answers the
// app's questions about them. DuckDB is columnar, so the aggregates behind
// the charts stay fast over millions of rows.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
)

// Times are stored as unix nanoseconds, as OTLP sends them. The API reports
// milliseconds as floats: a nanosecond timestamp doesn't fit a JS number.
//
// A column added after the first version goes at the end, here and in
// columnsAdded: a database file made before it gets the column by ALTER,
// which can only add at the end, and the appenders write columns by place.
const coreSchema = `
CREATE TABLE IF NOT EXISTS spans (
	trace_id VARCHAR, span_id VARCHAR, parent_id VARCHAR,
	service VARCHAR, name VARCHAR, kind VARCHAR,
	start_ns BIGINT, duration_ns BIGINT,
	status VARCHAR, status_message VARCHAR,
	db_system VARCHAR, db_statement VARCHAR, db_fingerprint VARCHAR,
	http_method VARCHAR, http_route VARCHAR, http_status INTEGER,
	attrs VARCHAR,
	db_role TINYINT DEFAULT 0, entry BOOLEAN DEFAULT false, user_id VARCHAR DEFAULT '',
	db_server VARCHAR DEFAULT ''
);
CREATE TABLE IF NOT EXISTS logs (
	ts_ns BIGINT, service VARCHAR,
	severity VARCHAR, severity_num INTEGER,
	body VARCHAR, trace_id VARCHAR, span_id VARCHAR,
	attrs VARCHAR,
	kind VARCHAR DEFAULT '', user_id VARCHAR DEFAULT ''
);
`

// schemas is every table the store needs. A feature with its own table adds
// its CREATE statements here; they must be safe to run on every start.
var schemas = []string{coreSchema, metricsSchema, routesSchema}

// Span is one row of spans. Empty strings stand for "not set": a root span
// has ParentID "", a span that isn't a database call has DBSystem "".
type Span struct {
	TraceID, SpanID, ParentID string
	Service, Name, Kind       string
	StartNs, DurationNs       int64
	Status, StatusMessage     string
	DBSystem, DBStatement     string
	DBFingerprint             string
	HTTPMethod, HTTPRoute     string
	HTTPStatus                int32
	Attrs                     string // a JSON object
	// UserID is who the span ran for (the "user.id" attribute), when the
	// app says.
	UserID string
	// DBServer is where a database call went, as host:port spelled one way
	// (see DBServer), when the span says: it tells two databases of one
	// system apart.
	DBServer string

	// Source and DBName are not stored. They tell the store where the span
	// came from, which decides what it counts as; see link.go.
	//
	// Source is "" for a span the app exported, or one of the Source
	// constants. DBName is the database a database call went to, when known.
	Source string
	DBName string
	// Capture is which capture recorded the span: the id the daemon gave
	// the proxy or the connection. Only its bookkeeping reads it.
	Capture string
	// Linked says a capture's span carries the app's own trace id and the
	// id of the span that sent the statement, read from a sqlcommenter
	// comment on it, and not ids the capture made up.
	Linked bool
}

// Where a span came from, when not from the app's own telemetry.
const (
	SourceBrowser      = "browser"       // the page, not the server: never a request of its own
	SourcePostgresWire = "pgproxy"       // read off the wire by the Postgres proxy
	SourceRedisMonitor = "redis-monitor" // reported by Redis's MONITOR, which gives no duration
)

type Log struct {
	TsNs            int64
	Service         string
	Severity        string
	SeverityNum     int32
	Body            string
	TraceID, SpanID string
	Attrs           string // a JSON object
	// Kind is the "leadline.kind" attribute: what sort of record a provider
	// says this is ("browser.error", say). UserID is "user.id".
	Kind, UserID string
}

type Store struct {
	db *sql.DB

	// Appenders are DuckDB's bulk-insert path. They share one connection
	// and aren't safe for concurrent use. This connection and its lock are
	// for the features that open an appender of their own (metrics).
	mu   sync.Mutex
	conn driver.Conn

	// Spans and logs wait in memory and one goroutine, the flusher, writes
	// them together a moment later. So an export costs the app that sent
	// it an append to a slice and nothing else: no commit, and no
	// checkpoint when one falls due.
	//
	// pmu guards what is waiting. added counts every row ever taken in and
	// written is that count as of the last finished write, so "written has
	// caught up with added as I saw it" means a read misses nothing.
	pmu       sync.Mutex
	waitSpans []Span
	waitLogs  []Log
	closed    bool
	added     atomic.Int64
	written   atomic.Int64
	flushed   chan struct{} // closed, and replaced, after every write
	wake      chan struct{} // rows are waiting
	urgent    chan struct{} // someone is waiting on the flusher: don't linger

	// wmu guards the span and log appenders and the connection they share,
	// and with them everything else the flusher uses to write: the linker
	// and the connection it corrects stored rows on. It is never taken
	// while holding mu.
	wmu    sync.Mutex
	wconn  driver.Conn
	wspans *duckdb.Appender
	wlogs  *duckdb.Appender
	uconn  driver.Conn
	link   *linker

	// DuckDB stops every commit while it checkpoints (folds its write-ahead
	// log into the file), but not reads. So the store checkpoints by its
	// own hand, holding wmu, and says so here: rows keep collecting in
	// memory meanwhile, and a read goes ahead without them rather than
	// wait out the checkpoint.
	checkpointing atomic.Bool

	flushEvery time.Duration
	stop       chan struct{} // closed by Close
	bg         sync.WaitGroup
	closeOnce  sync.Once
	closeErr   error

	// gen goes up whenever stored rows change, for answers worth caching.
	gen     atomic.Int64
	path    string
	summary summaryCache

	// lastUse is when a statement last ran, and tidied the lastUse the
	// store last gave memory back after; see tidyLoop.
	lastUse atomic.Int64
	tidied  int64
	memory  int64 // DuckDB's cap in bytes, 0 for none

	// metrics remembers each metric series' newest point; guarded by mu.
	metrics *metricState
	// routeMu makes replacing a service's route list one step.
	routeMu sync.Mutex
	// keep is what Retain was last given, for Storage to report.
	keep atomic.Pointer[Retention]
}

// Options tune a store. The zero value is what the daemon runs with.
type Options struct {
	// FlushEvery is the longest a row waits in memory before it is written
	// to the database. A read never sees that wait, because every statement
	// first writes what is waiting. Zero means 200ms.
	FlushEvery time.Duration
	// MemoryLimit caps DuckDB's memory, in bytes. Zero means 512 MB for a
	// database file, and no cap in memory, where the rows themselves live
	// in that memory. Negative means no cap. Past the cap DuckDB spills big
	// aggregates to disk.
	MemoryLimit int64
	// Threads caps the cores one query may take. Zero means all but two,
	// which are left for the app the daemon is watching, and never fewer
	// than two.
	Threads int
}

// walLimit is how large the write-ahead log grows before a checkpoint, the
// size DuckDB itself would pick. A checkpoint's length follows the log's:
// about 50ms at this size, once a minute on a busy machine.
const walLimit = 16 << 20

const (
	defaultFlushEvery  = 200 * time.Millisecond
	defaultMemoryLimit = 512 << 20
	// maxWaiting is how many rows may wait in memory. Whoever brings a
	// burst past it waits for the flusher, which is what slows a sender
	// that outruns the disk.
	maxWaiting = 20_000
	// syncWait is the longest a read waits for rows on their way in. A
	// write takes milliseconds; this only matters when the disk has
	// stalled, and then a read a moment behind beats no read.
	syncWait = time.Second
)

func (o Options) withDefaults(path string) Options {
	if o.FlushEvery <= 0 {
		o.FlushEvery = defaultFlushEvery
	}
	if o.MemoryLimit == 0 && path != "" {
		o.MemoryLimit = defaultMemoryLimit
	}
	if o.Threads <= 0 {
		o.Threads = max(2, runtime.NumCPU()-2)
	}
	return o
}

// Open opens the database at path, creating it if needed. An empty path
// keeps everything in memory.
func Open(path string, opts ...Options) (*Store, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	o = o.withDefaults(path)
	connector, err := duckdb.NewConnector(path, nil)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	s := &Store{
		path:       path,
		flushEvery: o.FlushEvery,
		flushed:    make(chan struct{}),
		wake:       make(chan struct{}, 1),
		urgent:     make(chan struct{}, 1),
		stop:       make(chan struct{}),
		link:       newLinker(),
		metrics:    newMetricState(),
	}
	s.db = sql.OpenDB(&syncConnector{duck: connector, store: s})
	settings := []string{fmt.Sprintf(`SET threads = %d`, o.Threads)}
	if o.MemoryLimit > 0 {
		settings = append(settings, fmt.Sprintf(`SET memory_limit = '%dMiB'`, max(64, o.MemoryLimit>>20)))
	}
	if path != "" {
		// Never checkpoint inside a commit; see Store.checkpointing.
		settings = append(settings, `SET checkpoint_threshold = '1TB'`)
	}
	for _, q := range append(settings, schemas...) {
		if _, err := s.db.Exec(q); err != nil {
			s.Close()
			return nil, fmt.Errorf("set up the database: %w", err)
		}
	}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, fmt.Errorf("bring the database up to date: %w", err)
	}
	if s.conn, err = connector.Connect(context.Background()); err != nil {
		s.Close()
		return nil, err
	}
	if s.wconn, err = connector.Connect(context.Background()); err != nil {
		s.Close()
		return nil, err
	}
	if s.wspans, err = duckdb.NewAppenderFromConn(s.wconn, "", "spans"); err != nil {
		s.Close()
		return nil, err
	}
	if s.wlogs, err = duckdb.NewAppenderFromConn(s.wconn, "", "logs"); err != nil {
		s.Close()
		return nil, err
	}
	if s.uconn, err = connector.Connect(context.Background()); err != nil {
		s.Close()
		return nil, err
	}
	s.memory = o.MemoryLimit
	s.bg.Add(1)
	go s.flushLoop()
	if path != "" {
		s.bg.Add(1)
		go s.tidyLoop()
	}
	return s, nil
}

// DB is the database itself, for the queries a feature keeps in its own file.
// A statement on it sees every row added before it, like the store's own.
func (s *Store) DB() *sql.DB { return s.db }

// Close writes what is waiting and closes the database. Rows added before
// Close are kept; adding after it is an error.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		s.bg.Wait()
		s.pmu.Lock()
		s.closed = true
		s.pmu.Unlock()
		err := s.write()

		s.wmu.Lock()
		if s.wspans != nil {
			err = errors.Join(err, s.wspans.Close())
		}
		if s.wlogs != nil {
			err = errors.Join(err, s.wlogs.Close())
		}
		if s.wconn != nil {
			s.wconn.Close()
		}
		if s.uconn != nil {
			s.uconn.Close()
		}
		s.wspans, s.wlogs, s.uconn = nil, nil, nil
		s.wmu.Unlock()

		s.mu.Lock()
		if s.conn != nil {
			s.conn.Close()
		}
		s.mu.Unlock()
		s.closeErr = errors.Join(err, s.db.Close())
	})
	return s.closeErr
}

var errClosed = errors.New("store is closed")

// AddSpans takes spans in. They are written within Options.FlushEvery, and
// before the next statement on the database, so the next query sees them.
func (s *Store) AddSpans(spans []Span) error {
	if len(spans) == 0 {
		return nil
	}
	s.pmu.Lock()
	if s.closed {
		s.pmu.Unlock()
		return errClosed
	}
	s.waitSpans = append(s.waitSpans, spans...)
	return s.took(len(spans))
}

// AddLogs takes log records in, as AddSpans does spans.
func (s *Store) AddLogs(logs []Log) error {
	if len(logs) == 0 {
		return nil
	}
	s.pmu.Lock()
	if s.closed {
		s.pmu.Unlock()
		return errClosed
	}
	s.waitLogs = append(s.waitLogs, logs...)
	return s.took(len(logs))
}

// took is the end of an Add of n rows, entered holding pmu.
func (s *Store) took(n int) error {
	waiting := len(s.waitSpans) + len(s.waitLogs)
	s.added.Add(int64(n))
	s.pmu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	if waiting >= maxWaiting {
		s.await(0)
	}
	return nil
}

// Flush returns once every row added before the call is in the database.
func (s *Store) Flush() { s.await(0) }

// sync is Flush for a statement about to run. It costs two atomic loads
// when nothing is waiting, which is nearly always.
func (s *Store) sync() { s.await(syncWait) }

// await waits for the flusher to write every row added so far, and tells it
// not to linger. With a limit it gives up after that long, and at once
// during a checkpoint, when no write can land.
func (s *Store) await(limit time.Duration) {
	want := s.added.Load()
	if s.written.Load() >= want {
		return
	}
	var late <-chan time.Time
	if limit > 0 {
		t := time.NewTimer(limit)
		defer t.Stop()
		late = t.C
	}
	for {
		s.pmu.Lock()
		flushed := s.flushed
		s.pmu.Unlock()
		if s.written.Load() >= want || (limit > 0 && s.checkpointing.Load()) {
			return
		}
		select {
		case s.urgent <- struct{}{}:
		default:
		}
		select {
		case <-flushed:
		case <-late:
			return
		case <-s.stop:
			return // Close writes what is left
		}
	}
}

// flushLoop is the flusher. It writes waiting rows a short while after the
// first of them arrives, so a burst of exports becomes one commit, and at
// once when a read or a full buffer is waiting on it. It sleeps while
// nothing arrives, but for a look at the write-ahead log twice a minute:
// other features commit to it too, and none of them checkpoints.
func (s *Store) flushLoop() {
	defer s.bg.Done()
	look := time.NewTicker(30 * time.Second)
	defer look.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-look.C:
		case <-s.wake:
			linger := time.NewTimer(s.flushEvery)
			select {
			case <-s.stop:
				linger.Stop()
				return
			case <-s.urgent:
				linger.Stop()
			case <-linger.C:
			}
			if err := s.write(); err != nil {
				slog.Error("writing rows", "err", err)
			}
		}
		if s.walSize() >= walLimit {
			if err := s.checkpoint(); err != nil {
				slog.Error("checkpoint", "err", err)
			}
		}
	}
}

// write moves every waiting row into the database and wakes whoever was
// waiting for that.
func (s *Store) write() error {
	s.wmu.Lock()
	s.pmu.Lock()
	spans, logs, upTo := s.waitSpans, s.waitLogs, s.added.Load()
	s.waitSpans, s.waitLogs = nil, nil
	s.pmu.Unlock()

	var err error
	if s.wspans != nil {
		// What each span counts as, and which stored rows that changes.
		marks, fixes := s.link.place(spans, time.Now().UnixNano())
		for i := range spans {
			sp := &spans[i]
			err = s.wspans.AppendRow(
				sp.TraceID, sp.SpanID, sp.ParentID,
				sp.Service, sp.Name, sp.Kind,
				sp.StartNs, sp.DurationNs,
				sp.Status, sp.StatusMessage,
				sp.DBSystem, sp.DBStatement, sp.DBFingerprint,
				sp.HTTPMethod, sp.HTTPRoute, sp.HTTPStatus,
				sp.Attrs,
				marks[i].role, marks[i].entry, sp.UserID, sp.DBServer,
			)
			if err != nil {
				err = fmt.Errorf("append span: %w", err)
				break
			}
		}
		for i := range logs {
			if err != nil {
				break
			}
			l := &logs[i]
			err = s.wlogs.AppendRow(
				l.TsNs, l.Service,
				l.Severity, l.SeverityNum,
				l.Body, l.TraceID, l.SpanID,
				l.Attrs,
				l.Kind, l.UserID,
			)
			if err != nil {
				err = fmt.Errorf("append log: %w", err)
			}
		}
		// Whatever was appended is committed even when a row was refused.
		if len(spans) > 0 {
			err = errors.Join(err, s.wspans.Flush())
		}
		if len(logs) > 0 {
			err = errors.Join(err, s.wlogs.Flush())
		}
		// After the commit: a correction may be to a row of this batch's
		// neighbours, never to one of its own.
		err = errors.Join(err, s.correct(fixes))
	}
	s.wmu.Unlock()

	if len(spans)+len(logs) > 0 {
		s.gen.Add(1)
	}
	s.written.Store(upTo)
	s.announce()
	return err
}

// announce wakes everyone in await to look again.
func (s *Store) announce() {
	s.pmu.Lock()
	close(s.flushed)
	s.flushed = make(chan struct{})
	s.pmu.Unlock()
}

func (s *Store) walSize() int64 {
	if s.path == "" {
		return 0
	}
	fi, err := os.Stat(s.path + ".wal")
	if err != nil {
		return 0
	}
	return fi.Size()
}

// checkpoint folds the write-ahead log into the database file, which is
// also when the space of deleted rows goes back to the file's free list.
func (s *Store) checkpoint() error {
	s.checkpointing.Store(true)
	defer s.checkpointing.Store(false)
	s.announce() // reads waiting on a write: go ahead without it
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.db.Exec(`CHECKPOINT`)
	return err
}

// syncConnector gives database/sql connections that write waiting rows
// before every statement. That is what lets ingest answer before its rows
// are stored while every query, the store's and any feature's, still reads
// its own writes. The appenders use connections of their own, not these.
type syncConnector struct {
	duck  *duckdb.Connector
	store *Store
}

func (c *syncConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.duck.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &syncConn{duckConn: conn.(duckConn), store: c.store}, nil
}

func (c *syncConnector) Driver() driver.Driver { return c.duck.Driver() }
func (c *syncConnector) Close() error          { return c.duck.Close() }

// duckConn is what the DuckDB driver's connection offers database/sql.
type duckConn interface {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	driver.NamedValueChecker
}

type syncConn struct {
	duckConn
	store *Store
}

func (c *syncConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.store.used()
	c.store.sync()
	return c.duckConn.ExecContext(ctx, query, args)
}

func (c *syncConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.store.used()
	c.store.sync()
	return c.duckConn.QueryContext(ctx, query, args)
}

func (c *syncConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	stmt, err := c.duckConn.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &syncStmt{duckStmt: stmt.(duckStmt), store: c.store}, nil
}

func (c *syncConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

type duckStmt interface {
	driver.Stmt
	driver.StmtExecContext
	driver.StmtQueryContext
}

type syncStmt struct {
	duckStmt
	store *Store
}

func (st *syncStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	st.store.used()
	st.store.sync()
	return st.duckStmt.ExecContext(ctx, args)
}

func (st *syncStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	st.store.used()
	st.store.sync()
	return st.duckStmt.QueryContext(ctx, args)
}
