package integrations

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sean-brydon/leadline/internal/integrations/fake"
	"github.com/sean-brydon/leadline/internal/store"
)

// A worktree's own database on the shared server, the way a setup script
// names one: postgres://…/acme_fix_orders.
func worktreeURL(pg *fake.Postgres, database string) string {
	pg.AddDatabase(database)
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable", pg.User, pg.Password, pg.Addr(), database)
}

func attachInput(worktree string, urls ...AttachURL) AttachInput {
	return AttachInput{Service: "acme/" + worktree, Via: "shipyard", Project: "acme", Worktree: worktree,
		Branch: worktree, Box: "devl", Path: "/home/dev/work/acme-" + worktree, Connections: urls}
}

func attach(t *testing.T, m *Manager, in AttachInput) AttachResult {
	t.Helper()
	out, err := m.Attach(context.Background(), in)
	if err != nil {
		t.Fatalf("attach %s: %v", in.Service, err)
	}
	return out
}

// queryDB runs one statement through addr against a database, as an app
// that names itself would.
func queryDB(t *testing.T, addr, password, database, app, sql string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgconn.Connect(ctx, "postgres://acme:"+password+"@"+addr+"/"+database+"?sslmode=disable&application_name="+app)
	if err != nil {
		t.Fatalf("connect through %s: %v", addr, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, sql).ReadAll(); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func spansOf(t *testing.T, st *store.Store, service string) int64 {
	t.Helper()
	var n int64
	if err := st.DB().QueryRow(`SELECT count(*) FROM spans WHERE service = ?`, service).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// captureLogs sends everything the package logs to a buffer for the length
// of a test, so the test can look for a password in it.
func captureLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Attaching a worktree twice changes nothing: not what it owns, not what is
// running for it, not a byte of the file.
func TestAttachIsIdempotent(t *testing.T) {
	pg, rd := fake.NewPostgres(t), fake.NewRedis(t)
	dir := t.TempDir()
	m := open(t, dir, openStore(t))
	in := attachInput("fix-orders",
		AttachURL{Env: []string{"DATABASE_URL", "DIRECT_URL"}, URL: worktreeURL(pg, "acme_fix_orders")},
		// The same database under a third name, with an option of its own.
		AttachURL{Env: []string{"SHADOW_URL"}, URL: worktreeURL(pg, "acme_fix_orders") + "&connect_timeout=5"},
		AttachURL{Env: []string{"REDIS_URL"}, URL: rd.URL()},
		AttachURL{Env: []string{"SEARCH_URL"}, URL: "https://search.acme.example"})

	first := attach(t, m, in)
	if !first.Created || !first.Changed || first.Added != 2 || len(first.Connections) != 2 {
		t.Fatalf("first attach: %+v", first)
	}
	if len(first.Skipped) != 1 || first.Skipped[0].Env[0] != "SEARCH_URL" {
		t.Errorf("skipped = %+v; want the URL that is neither Postgres nor Redis", first.Skipped)
	}
	pgc, rdc := first.Connections[0], first.Connections[1]
	if pgc.Kind != KindPostgres || !pgc.Capture || pgc.Proxy == nil || pgc.Proxy.State != "listening" ||
		strings.Join(pgc.Env, ",") != "DATABASE_URL,DIRECT_URL,SHADOW_URL" || pgc.Name != "acme/fix-orders · acme_fix_orders" {
		t.Errorf("the Postgres connection: %+v", pgc)
	}
	if rdc.Kind != KindRedis || !rdc.Capture || rdc.Monitor == nil {
		t.Errorf("the Redis connection: %+v", rdc)
	}
	for _, c := range first.Connections {
		o := c.Owner
		if o.Kind != OwnerWorktree || o.Service != "acme/fix-orders" || o.Project != "acme" || o.Worktree != "fix-orders" ||
			o.Branch != "fix-orders" || o.Box != "devl" || o.Via != "shipyard" || o.Label != "from Shipyard, worktree fix-orders" {
			t.Errorf("owner of %s: %+v", c.Name, o)
		}
	}
	if first.Source.Owner.Label != "from Shipyard, worktree fix-orders" || len(first.Source.ConnectionIDs) != 2 {
		t.Errorf("source: %+v", first.Source)
	}
	path := filepath.Join(dir, fileName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stamp, _ := os.Stat(path)

	again := attach(t, m, in)
	if again.Created || again.Changed || again.Added+again.Updated+again.Removed != 0 || again.Unchanged != 2 {
		t.Errorf("second attach: created %v, changed %v, +%d ~%d -%d =%d", again.Created, again.Changed, again.Added, again.Updated, again.Removed, again.Unchanged)
	}
	if len(again.Connections) != 2 || again.Connections[0].ID != pgc.ID || again.Connections[1].ID != rdc.ID ||
		again.Connections[0].Proxy.ID != pgc.Proxy.ID || again.Connections[0].Proxy.Port != pgc.Proxy.Port {
		t.Errorf("second attach made something new: %+v", again.Connections)
	}
	after, _ := os.ReadFile(path)
	now, _ := os.Stat(path)
	if !bytes.Equal(before, after) || !now.ModTime().Equal(stamp.ModTime()) {
		t.Error("the connections file was written again by an attach that changed nothing")
	}
	if n := len(m.Connections()); n != 2 {
		t.Errorf("%d connections after attaching twice", n)
	}
	// A hook whose event says less (a finished setup names no branch) does
	// not unsay what an earlier one said.
	less := in
	less.Branch, less.Box, less.Path = "", "", ""
	if got := attach(t, m, less); got.Changed || got.Source.Owner.Branch != "fix-orders" || got.Source.Owner.Box != "devl" || got.Connections[0].Owner.Path == "" {
		t.Errorf("an attach that names no branch: changed %v, owner %+v", got.Changed, got.Source.Owner)
	}
	if n := len(m.Proxies()); n != 1 {
		t.Errorf("%d proxies after attaching twice", n)
	}
}

// The heart of it. Two worktrees with a database each on one Postgres, and
// a connection a person added to that same server: each is its own, each
// worktree's queries are filed under the worktree, and detaching one takes
// its connection, its proxy and its data, and nothing of the others.
func TestDetachTakesExactlyWhatTheWorktreeOwns(t *testing.T) {
	pg := fake.NewPostgres(t)
	st := openStore(t)
	dir := t.TempDir()
	m := open(t, dir, st)
	ctx := context.Background()

	mine := add(t, m, ConnectionInput{URL: ptr(pg.URL()), Name: ptr("Acme dev"), Capture: ptr(true)})
	a := attach(t, m, attachInput("fix-orders", AttachURL{Env: []string{"DATABASE_URL"}, URL: worktreeURL(pg, "acme_fix_orders")}))
	b := attach(t, m, attachInput("add-refunds", AttachURL{Env: []string{"DATABASE_URL"}, URL: worktreeURL(pg, "acme_add_refunds")}))
	ac, bc := a.Connections[0], b.Connections[0]

	// Three connections to one server, and a proxy each: none is shared.
	ports := map[int]bool{mine.Proxy.Port: true, ac.Proxy.Port: true, bc.Proxy.Port: true}
	if len(ports) != 3 || len(m.Proxies()) != 3 {
		t.Fatalf("proxies: mine %d, fix-orders %d, add-refunds %d", mine.Proxy.Port, ac.Proxy.Port, bc.Proxy.Port)
	}
	if mine.Owner.Kind != OwnerUser || mine.Proxy.Owner.Kind != OwnerUser || ac.Proxy.Owner.Kind != OwnerWorktree || ac.Proxy.Owner.Service != "acme/fix-orders" {
		t.Errorf("owners: mine %+v / %+v, fix-orders' proxy %+v", mine.Owner, mine.Proxy.Owner, ac.Proxy.Owner)
	}

	// Each worktree's app talks to its own database through its own proxy.
	// It calls itself "acme-web" in both, as the one codebase would.
	queryDB(t, ac.Proxy.Listen, pg.Password, "acme_fix_orders", "acme-web", "SELECT id FROM orders")
	queryDB(t, ac.Proxy.Listen, pg.Password, "acme_fix_orders", "acme-web", "SELECT id FROM customers")
	queryDB(t, bc.Proxy.Listen, pg.Password, "acme_add_refunds", "acme-web", "SELECT id FROM refunds")
	query(t, mine.Proxy.Listen, pg.Password, "SELECT 1")
	eventually(t, "every statement to be stored", func() bool {
		return spansOf(t, st, "acme/fix-orders") == 2 && spansOf(t, st, "acme/add-refunds") == 1 && spansOf(t, st, "acme-web") == 1
	})
	// And a command in the worktree that sends telemetry of its own.
	if err := st.AddSpans([]store.Span{{TraceID: strings.Repeat("a", 32), SpanID: strings.Repeat("b", 16), Service: "acme/fix-orders:worker",
		Name: "job send-receipt", Kind: "internal", StartNs: time.Now().UnixNano(), DurationNs: 1e6, Status: "unset", Attrs: "{}"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddLogs([]store.Log{
		{TsNs: time.Now().UnixNano(), Service: "acme/fix-orders", Severity: "INFO", SeverityNum: 9, Body: "ready", Attrs: "{}"},
		{TsNs: time.Now().UnixNano(), Service: "acme/add-refunds", Severity: "INFO", SeverityNum: 9, Body: "ready", Attrs: "{}"},
	}); err != nil {
		t.Fatal(err)
	}

	src, err := m.Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owners := map[string]string{}
	for _, s := range src.Services {
		if s.Owner != nil {
			owners[s.Name] = s.Owner.Worktree
		}
	}
	if len(src.Attached) != 2 || owners["acme/fix-orders"] != "fix-orders" || owners["acme/fix-orders:worker"] != "fix-orders" ||
		owners["acme/add-refunds"] != "add-refunds" || owners["acme-web"] != "" {
		t.Errorf("attached %d, owners %v", len(src.Attached), owners)
	}

	got, err := m.Detach(ctx, "acme/fix-orders", false)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.Connections != 1 || got.Proxies != 1 || got.KeptData || got.Deleted.Spans != 3 || got.Deleted.Logs != 1 {
		t.Errorf("detach: %+v", got)
	}

	// Its connection, its proxy and its port are gone...
	conns := m.Connections()
	if len(conns) != 2 || conns[0].ID != mine.ID || conns[1].ID != bc.ID {
		t.Fatalf("connections after detach: %+v", conns)
	}
	if answering(ac.Proxy.Listen) {
		t.Errorf("fix-orders' proxy still listens on %s", ac.Proxy.Listen)
	}
	// ...and the other two are as they were: same proxy, same port, still
	// carrying queries to the same server.
	if conns[0].Proxy == nil || conns[0].Proxy.ID != mine.Proxy.ID || conns[0].Proxy.Port != mine.Proxy.Port || conns[0].Proxy.State != "listening" ||
		conns[1].Proxy == nil || conns[1].Proxy.ID != bc.Proxy.ID || conns[1].Proxy.Port != bc.Proxy.Port || conns[1].Proxy.State != "listening" {
		t.Errorf("the others' proxies changed: %+v, %+v", conns[0].Proxy, conns[1].Proxy)
	}
	queryDB(t, bc.Proxy.Listen, pg.Password, "acme_add_refunds", "acme-web", "SELECT id FROM refunds")
	query(t, mine.Proxy.Listen, pg.Password, "SELECT 1")
	eventually(t, "the others' queries to keep arriving", func() bool {
		return spansOf(t, st, "acme/add-refunds") == 2 && spansOf(t, st, "acme-web") == 2
	})
	// Its data is gone, and its commands' with it; nobody else's is.
	if n := spansOf(t, st, "acme/fix-orders") + spansOf(t, st, "acme/fix-orders:worker"); n != 0 {
		t.Errorf("%d spans of the detached worktree are left", n)
	}
	var logs int64
	st.DB().QueryRow(`SELECT count(*) FROM logs WHERE service = 'acme/add-refunds'`).Scan(&logs)
	if logs != 1 {
		t.Errorf("the other worktree has %d logs, want its 1", logs)
	}
	// It has left the list of sources at once.
	src, err = m.Sources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range src.Services {
		if strings.HasPrefix(s.Name, "acme/fix-orders") {
			t.Errorf("%s is still a source", s.Name)
		}
	}
	if len(src.Attached) != 1 || src.Attached[0].Service != "acme/add-refunds" {
		t.Errorf("attached after detach: %+v", src.Attached)
	}

	// Detaching again, or a worktree Leadline never saw, is nothing at all.
	for _, name := range []string{"acme/fix-orders", "acme/never-seen"} {
		got, err := m.Detach(ctx, name, false)
		if err != nil || got.Found || got.Connections != 0 || got.Proxies != 0 || got.Deleted != (store.Forgotten{}) {
			t.Errorf("detach %s again: %+v, %v", name, got, err)
		}
	}
	if n := len(m.Connections()); n != 2 {
		t.Errorf("%d connections after detaching nothing", n)
	}

	// A restart finds what was left, and not what was detached.
	m.Close()
	m2 := open(t, dir, st)
	conns = m2.Connections()
	if len(conns) != 2 || conns[1].ID != bc.ID || conns[1].Owner.Service != "acme/add-refunds" || conns[1].Proxy == nil || conns[1].Proxy.Port != bc.Proxy.Port {
		t.Errorf("after a restart: %+v", conns)
	}
	if got := m2.Attached(); len(got) != 1 || got[0].Service != "acme/add-refunds" || got[0].Owner.Branch != "add-refunds" {
		t.Errorf("attached after a restart: %+v", got)
	}
	// The worktree's proxy still files under the worktree after it.
	queryDB(t, conns[1].Proxy.Listen, pg.Password, "acme_add_refunds", "acme-web", "SELECT id FROM refunds")
	eventually(t, "the restarted proxy to file under the worktree", func() bool { return spansOf(t, st, "acme/add-refunds") == 3 })
}

// --keep-data: the connections go and the recorded data stays.
func TestDetachCanKeepTheData(t *testing.T) {
	pg := fake.NewPostgres(t)
	st := openStore(t)
	m := open(t, t.TempDir(), st)
	a := attach(t, m, attachInput("fix-orders", AttachURL{Env: []string{"DATABASE_URL"}, URL: worktreeURL(pg, "acme_fix_orders")}))
	queryDB(t, a.Connections[0].Proxy.Listen, pg.Password, "acme_fix_orders", "acme-web", "SELECT id FROM orders")
	eventually(t, "the statement to be stored", func() bool { return spansOf(t, st, "acme/fix-orders") == 1 })

	got, err := m.Detach(context.Background(), "acme/fix-orders", true)
	if err != nil || !got.Found || !got.KeptData || got.Connections != 1 || got.Deleted != (store.Forgotten{}) {
		t.Fatalf("detach keeping data: %+v, %v", got, err)
	}
	if n := spansOf(t, st, "acme/fix-orders"); n != 1 {
		t.Errorf("%d spans left, want the 1 that was asked to be kept", n)
	}
	if n := len(m.Connections()) + len(m.Proxies()) + len(m.Attached()); n != 0 {
		t.Errorf("%d things of the worktree are left", n)
	}
}

// Attaching again after the worktree's .env changed updates what it owns:
// a new password is the same connection, a new database is a new one in
// the old one's place, and either way the proxy and its port stay.
func TestAttachFollowsTheEnvFile(t *testing.T) {
	pg := fake.NewPostgres(t)
	rd := fake.NewRedis(t)
	dir := t.TempDir()
	m := open(t, dir, openStore(t))
	first := attach(t, m, attachInput("fix-orders",
		AttachURL{Env: []string{"DATABASE_URL"}, URL: worktreeURL(pg, "acme_fix_orders")},
		AttachURL{Env: []string{"REDIS_URL"}, URL: rd.URL()}))
	pgc := first.Connections[0]
	port := pgc.Proxy.Port

	// A new password, and the variable renamed.
	const rotated = "acme-rotated-secret"
	in := attachInput("fix-orders",
		AttachURL{Env: []string{"POSTGRES_URL"}, URL: strings.Replace(worktreeURL(pg, "acme_fix_orders"), pg.Password, rotated, 1)},
		AttachURL{Env: []string{"REDIS_URL"}, URL: rd.URL()})
	got := attach(t, m, in)
	if got.Created || !got.Changed || got.Updated != 1 || got.Unchanged != 1 || got.Added+got.Removed != 0 {
		t.Errorf("after a new password: +%d ~%d -%d =%d", got.Added, got.Updated, got.Removed, got.Unchanged)
	}
	if c := got.Connections[0]; c.ID != pgc.ID || c.Proxy == nil || c.Proxy.ID != pgc.Proxy.ID || c.Proxy.Port != port || strings.Join(c.Env, ",") != "POSTGRES_URL" {
		t.Errorf("the connection after a new password: %+v", c)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, fileName))
	if !strings.Contains(string(raw), rotated) || strings.Contains(string(raw), pg.Password) {
		t.Error("the file does not hold the new password in place of the old")
	}

	// The database renamed, and Redis gone from the file.
	got = attach(t, m, attachInput("fix-orders", AttachURL{Env: []string{"POSTGRES_URL"}, URL: worktreeURL(pg, "acme_fix_orders_v2")}))
	if got.Added != 1 || got.Removed != 2 || len(got.Connections) != 1 {
		t.Fatalf("after a new database: +%d ~%d -%d: %+v", got.Added, got.Updated, got.Removed, got.Connections)
	}
	if c := got.Connections[0]; c.ID == pgc.ID || c.Database != "acme_fix_orders_v2" || c.Proxy == nil || c.Proxy.ID != pgc.Proxy.ID || c.Proxy.Port != port {
		t.Errorf("the new database's connection: %+v (the proxy was %s on %d)", c, pgc.Proxy.ID, port)
	}
	if n := len(m.Connections()); n != 1 {
		t.Errorf("%d connections, want the worktree's one", n)
	}

	// An attach that found nowhere to read URLs from removes nothing.
	partial := attachInput("fix-orders")
	partial.Partial = true
	if got = attach(t, m, partial); got.Changed || len(got.Connections) != 1 {
		t.Errorf("a partial attach changed things: %+v", got)
	}
	// One that read the files and found none removes what is left.
	if got = attach(t, m, attachInput("fix-orders")); got.Removed != 1 || len(got.Connections) != 0 {
		t.Errorf("an attach with no URLs: %+v", got)
	}
	if n := len(m.Proxies()); n != 0 {
		t.Errorf("%d proxies left for a worktree with no database", n)
	}
	// The source itself is still attached, with nothing held yet.
	src, err := m.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Services) != 1 || src.Services[0].Name != "acme/fix-orders" || src.Services[0].Spans != 0 || src.Services[0].LastSeenMs != 0 ||
		src.Services[0].Owner == nil || src.Services[0].Via == nil || src.Services[0].Through == nil {
		t.Errorf("an attached source with no data: %+v", src.Services)
	}
}

// A worktree's Redis is recorded under the worktree, and only the database
// its URL names: the server is one every worktree shares.
func TestAttachedRedisIsFiledUnderTheWorktree(t *testing.T) {
	rd := fake.NewRedis(t)
	st := openStore(t)
	m := open(t, t.TempDir(), st)
	url := strings.TrimSuffix(rd.URL(), "/0") + "/3"
	a := attach(t, m, attachInput("fix-orders", AttachURL{Env: []string{"REDIS_URL"}, URL: url}))
	if c := a.Connections[0]; !c.Capture || c.Database != "3" || c.Name != "acme/fix-orders · redis 3" {
		t.Fatalf("the Redis connection: %+v", c)
	}
	rd.WaitMonitor(t)
	eventually(t, "the capture to be streaming", func() bool {
		v, _ := m.Connection(a.Connections[0].ID)
		return v.Monitor != nil && v.Monitor.State == "streaming"
	})
	at := time.Now()
	line := func(db int, d time.Duration, args ...string) string {
		return strings.Replace(fake.MonitorLine(at.Add(d), "127.0.0.1:50001", args...), "[0 ", fmt.Sprintf("[%d ", db), 1)
	}
	// A connection of Leadline's own, as the server reports one that has
	// just opened: it names itself before it selects the database, and
	// nothing it sends after is the app's.
	own := func(db int, d time.Duration, args ...string) string {
		return strings.Replace(line(db, d, args...), "127.0.0.1:50001", "127.0.0.1:50009", 1)
	}
	rd.Emit(
		line(3, 0, "get", "cart:42"),
		line(0, time.Millisecond, "get", "another-worktree:1"),
		own(0, time.Millisecond, "CLIENT", "SETNAME", "leadline-0a1b2c3d4e5f"),
		own(0, time.Millisecond, "SELECT", "3"),
		own(3, time.Millisecond, "PING"),
		own(3, time.Millisecond, "INFO", "server"),
		line(3, 2*time.Millisecond, "set", "cart:42", "1"),
	)
	eventually(t, "its two commands to be stored", func() bool { return spansOf(t, st, "acme/fix-orders") == 2 })
	var others int64
	st.DB().QueryRow(`SELECT count(*) FROM spans WHERE service <> 'acme/fix-orders'`).Scan(&others)
	if others != 0 {
		t.Errorf("%d commands of another database were recorded", others)
	}
	// The live capture's last commands are in the store before it is asked
	// to forget them, so nothing of the worktree is left behind.
	got, err := m.Detach(context.Background(), "acme/fix-orders", false)
	if err != nil || got.Deleted.Spans != 2 {
		t.Errorf("detach: %+v, %v", got, err)
	}
	if n := spansOf(t, st, "acme/fix-orders"); n != 0 {
		t.Errorf("%d spans left", n)
	}
}

// No password leaves the package: not in an answer, not in a log line, not
// in an error. The file is the one place it is kept.
func TestAttachKeepsPasswordsIn(t *testing.T) {
	logs := captureLogs(t)
	pg, rd := fake.NewPostgres(t), fake.NewRedis(t)
	st := openStore(t)
	dir := t.TempDir()
	m := open(t, dir, st)
	const remotePw, badPw = "acme-remote-secret", "acme-bad-secret"
	in := attachInput("fix-orders",
		AttachURL{Env: []string{"DATABASE_URL"}, URL: worktreeURL(pg, "acme_fix_orders")},
		AttachURL{Env: []string{"REDIS_URL"}, URL: rd.URL()},
		// A database on another machine: saved for its statistics, never proxied.
		AttachURL{Env: []string{"REPLICA_URL"}, URL: "postgres://acme:" + remotePw + "@db.acme.example:5432/acme"},
		// One that can't be read at all.
		AttachURL{Env: []string{"BROKEN_URL"}, URL: "postgres://acme:" + badPw + "@localhost:notaport/acme"})
	secrets := []string{pg.Password, rd.Password, remotePw, badPw}

	got := attach(t, m, in)
	if got.Added != 3 || len(got.Skipped) != 1 {
		t.Fatalf("attach: +%d, skipped %+v", got.Added, got.Skipped)
	}
	for _, c := range got.Connections {
		if c.Remote && (c.Capture || c.Proxy != nil) {
			t.Errorf("a remote database is captured: %+v", c)
		}
	}
	mustNotHold(t, "the attach's answer", got, secrets...)
	mustNotHold(t, "a second attach's answer", attach(t, m, in), secrets...)
	src, err := m.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	mustNotHold(t, "sources", src, secrets...)
	mustNotHold(t, "attached", m.Attached(), secrets...)
	mustNotHold(t, "connections", m.Connections(), secrets...)
	mustNotHold(t, "proxies", m.Proxies(), secrets...)
	source, conns, err := m.AttachedSource("acme/fix-orders")
	if err != nil {
		t.Fatal(err)
	}
	mustNotHold(t, "one attached source", []any{source, conns}, secrets...)

	// Errors too: a name that can't be one, and too many URLs.
	for _, bad := range []AttachInput{{Service: ""}, {Service: "acme/fix:orders"}, {Service: "acme/x", Connections: make([]AttachURL, maxAttachURLs+1)}} {
		_, err := m.Attach(context.Background(), bad)
		if AsProblem(err).Code != "bad_request" {
			t.Errorf("attach %q: %v", bad.Service, err)
		}
		mustNotHold(t, "an error", AsProblem(err), secrets...)
	}

	det, err := m.Detach(context.Background(), "acme/fix-orders", false)
	if err != nil {
		t.Fatal(err)
	}
	mustNotHold(t, "the detach's answer", det, secrets...)
	m.Close()
	for _, s := range secrets {
		if strings.Contains(logs.String(), s) {
			t.Errorf("a password is in the log:\n%s", logs.String())
		}
	}
	// And once detached, the file no longer holds the worktree's passwords.
	raw, _ := os.ReadFile(filepath.Join(dir, fileName))
	for _, s := range secrets {
		if strings.Contains(string(raw), s) {
			t.Errorf("the file still holds a detached worktree's password: %s", raw)
		}
	}
}

// A person's own connections don't count against what worktrees may attach,
// and a person may edit or delete a worktree's connection like any other.
func TestAPersonCanEditAnAttachedConnection(t *testing.T) {
	pg := fake.NewPostgres(t)
	m := open(t, t.TempDir(), openStore(t))
	in := attachInput("fix-orders", AttachURL{Env: []string{"DATABASE_URL"}, URL: worktreeURL(pg, "acme_fix_orders")})
	a := attach(t, m, in)
	id := a.Connections[0].ID

	// Capture turned off in the app stays off when the hook runs again.
	v, err := m.Update(context.Background(), id, ConnectionInput{Capture: ptr(false), Name: ptr("Fix orders")})
	if err != nil || v.Capture || v.Owner.Kind != OwnerWorktree {
		t.Fatalf("edit: %+v, %v", v, err)
	}
	again := attach(t, m, in)
	if again.Changed || again.Connections[0].Capture || again.Connections[0].Name != "Fix orders" {
		t.Errorf("attaching again undid a person's edit: %+v", again.Connections[0])
	}
	// Deleted by hand, it comes back with the next attach: the worktree
	// still names it.
	if err := m.Delete(id); err != nil {
		t.Fatal(err)
	}
	if again = attach(t, m, in); again.Added != 1 || again.Created {
		t.Errorf("after a delete by hand: %+v", again)
	}
}
