package pgproxy

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

func TestKeyword(t *testing.T) {
	cases := map[string]string{
		"select 1":                          "SELECT",
		"  \n\tINSERT INTO acme VALUES (1)": "INSERT",
		"/* hint */ -- note\n update acme":  "UPDATE",
		"/* a /* nested */ b */ delete":     "DELETE",
		"(select 1) union (select 2)":       "SELECT",
		"WITH x AS (select 1) select *":     "WITH",
		"":                                  "QUERY",
		"   ":                               "QUERY",
		"$1":                                "QUERY",
	}
	for sql, want := range cases {
		if got := keyword(sql); got != want {
			t.Errorf("keyword(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestSplitStatements(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{"select 1", nil},
		{"select 1;", nil},
		{"select 1;  -- done\n", nil},
		{"select 1; select 2", []string{"select 1", "select 2"}},
		{"select 1;;  ; select 2;", []string{"select 1", "select 2"}},
		{"select ';'; select \"a;b\" from acme", []string{"select ';'", `select "a;b" from acme`}},
		{"select 'it''s; fine'; select 2", []string{"select 'it''s; fine'", "select 2"}},
		{`select E'a\'; b'; select 2`, []string{`select E'a\'; b'`, "select 2"}},
		{`select 'a\'; select 2`, []string{`select 'a\'`, "select 2"}},
		{"select $$a;b$$; select $fn$ x; $$ y; $fn$; select $1", []string{"select $$a;b$$", "select $fn$ x; $$ y; $fn$", "select $1"}},
		{"select 1 /* ; */; select 2 -- ; no\n; select 3", []string{"select 1 /* ; */", "select 2 -- ; no", "select 3"}},
		{"begin; insert into acme values (1); commit", []string{"begin", "insert into acme values (1)", "commit"}},
		// A function body with semicolons of its own is left whole.
		{"create function f() returns int begin atomic select 1; select 2; end; select 3", nil},
	}
	for _, c := range cases {
		if got := splitStatements(c.sql); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitStatements(%q)\n got %q\nwant %q", c.sql, got, c.want)
		}
	}
}

func TestTraceparent(t *testing.T) {
	const trace, span = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	good := "select 1 /*action='show',traceparent='00-" + trace + "-" + span + "-01'*/"
	if tr, sp, ok := traceparent(good); !ok || tr != trace || sp != span {
		t.Errorf("traceparent(%q) = %q, %q, %v", good, tr, sp, ok)
	}
	if got := withoutTraceComment(good); got != "select 1" {
		t.Errorf("withoutTraceComment = %q", got)
	}
	for _, bad := range []string{
		"select 1",
		"select 1 /*traceparent='00-" + trace + "'*/",
		"select 1 /*traceparent='00-" + strings.Repeat("0", 32) + "-" + span + "-01'*/",
		"select 1 /*traceparent='00-" + strings.ToUpper(trace) + "-" + span + "-01'*/",
		"select 1 /*traceparent='00-" + trace + "-" + span + "-01",
		"select 1 /*traceparent='",
	} {
		if _, _, ok := traceparent(bad); ok {
			t.Errorf("traceparent(%q) was accepted", bad)
		}
	}
}

func TestRedact(t *testing.T) {
	cases := map[string]string{
		"ALTER USER acme PASSWORD 'p''w'":                    "ALTER USER acme PASSWORD '***'",
		"create role acme login encrypted password E'a\\'b'": "create role acme login encrypted password '***'",
		"update acme_users set password = 'x' where id = 1":  "update acme_users set password = '***' where id = 1",
		"select password from acme_users where id = 1":       "select password from acme_users where id = 1",
		"select 'password' from acme":                        "select 'password' from acme",
	}
	for sql, want := range cases {
		if got := redact(sql); got != want {
			t.Errorf("redact(%q) = %q, want %q", sql, got, want)
		}
	}
}

func TestTagRows(t *testing.T) {
	cases := map[string]int64{
		"SELECT 3\x00": 3, "INSERT 0 12\x00": 12, "UPDATE 0\x00": 0, "COPY 1000000\x00": 1000000,
		"BEGIN\x00": -1, "CREATE TABLE\x00": -1, "\x00": -1, "": -1, "42\x00": -1,
	}
	for tag, want := range cases {
		if got := tagRows([]byte(tag)); got != want {
			t.Errorf("tagRows(%q) = %d, want %d", tag, got, want)
		}
	}
}

// bindBody builds a Bind message from its format codes on.
func bindBody(formats []int16, values [][]byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, uint16(len(formats)))
	for _, f := range formats {
		b = binary.BigEndian.AppendUint16(b, uint16(f))
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(values)))
	for _, v := range values {
		if v == nil {
			b = binary.BigEndian.AppendUint32(b, 0xffffffff)
			continue
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
		b = append(b, v...)
	}
	return binary.BigEndian.AppendUint16(b, 0)
}

func TestParams(t *testing.T) {
	text := bindBody(nil, [][]byte{[]byte("ada"), nil, []byte("42")})
	if got := fmt.Sprint(params(text, nil)); got != "[ada <nil> 42]" {
		t.Errorf("text params: %s", got)
	}

	uuid := []byte{0x12, 0x3e, 0x45, 0x67, 0xe8, 0x9b, 0x12, 0xd3, 0xa4, 0x56, 0x42, 0x66, 0x14, 0x17, 0x40, 0x00}
	mixed := bindBody([]int16{1, 1, 0, 1, 1, 1},
		[][]byte{{0, 0, 0, 7}, {0, 0, 0, 0, 0, 0, 1, 0}, []byte("plain"), {1}, uuid, {1, 2, 3}})
	got := params(mixed, []uint32{23, 20, 25, 16, 2950, 0})
	want := []any{int32(7), int64(256), "plain", true, "123e4567-e89b-12d3-a456-426614174000", "(binary, 3 bytes)"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mixed params:\n got %#v\nwant %#v", got, want)
	}

	// One format code covers every parameter.
	all := bindBody([]int16{1}, [][]byte{{0, 0, 0, 1}, {0, 0, 0, 2}})
	if got := fmt.Sprint(params(all, []uint32{23, 23})); got != "[1 2]" {
		t.Errorf("single format code: %s", got)
	}

	// A long value is clipped, and a message the proxy only kept the
	// start of says so rather than guessing.
	long := bindBody(nil, [][]byte{[]byte(strings.Repeat("é", 500)), []byte("after")})
	clipped := params(long, nil)
	if s := clipped[0].(string); len(s) > maxParam+4 || !strings.HasSuffix(s, "…") || clipped[1] != "after" {
		t.Errorf("long param: %q", clipped)
	}
	if got := fmt.Sprint(params(long[:300], nil)); !strings.HasSuffix(got, "…]") {
		t.Errorf("cut message: %s", got)
	}
	many := make([][]byte, 100)
	for i := range many {
		many[i] = []byte("v")
	}
	if got := params(bindBody(nil, many), nil); len(got) != maxParams+1 || got[maxParams] != "… 68 more" {
		t.Errorf("many params: %v", got)
	}
	// Garbage in must not panic.
	for i := 0; i < 2000; i++ {
		junk := make([]byte, rand.IntN(40))
		for j := range junk {
			junk[j] = byte(rand.IntN(256))
		}
		params(junk, []uint32{23, 20, 2950})
	}
}

func TestParseRoutes(t *testing.T) {
	got, err := ParseRoutes("127.0.0.1:5433=127.0.0.1:5432, 6543=db.acme.test , localhost:7000=/var/run/postgresql/.s.PGSQL.5432,[::1]:7001=5432")
	want := Routes{
		{Listen: "127.0.0.1:5433", Upstream: "127.0.0.1:5432"},
		{Listen: "127.0.0.1:6543", Upstream: "db.acme.test:5432"},
		{Listen: "localhost:7000", Upstream: "/var/run/postgresql/.s.PGSQL.5432"},
		{Listen: "[::1]:7001", Upstream: "127.0.0.1:5432"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ParseRoutes: %v, %v", got, err)
	}
	if got, err := ParseRoutes(""); err != nil || len(got) != 0 {
		t.Errorf("empty: %v, %v", got, err)
	}
	for _, bad := range []string{
		"5433",                  // no upstream
		"5433=",                 //
		"0.0.0.0:5433=5432",     // every interface: the database would be open to the network
		":5433=5432",            // the same
		"192.168.1.5:5433=5432", //
		"nonsense=5432",         //
		"5433=host:port:extra",  //
	} {
		if got, err := ParseRoutes(bad); err == nil {
			t.Errorf("ParseRoutes(%q) = %v, want an error", bad, got)
		}
	}

	// As a repeated flag.
	var flagged Routes
	if err := flagged.Set("5433=5432"); err != nil {
		t.Fatal(err)
	}
	if err := flagged.Set("5434=5435"); err != nil {
		t.Fatal(err)
	}
	if flagged.String() != "127.0.0.1:5433=127.0.0.1:5432,127.0.0.1:5434=127.0.0.1:5435" {
		t.Errorf("flag value %q", flagged.String())
	}
}

// recTap records what a scanner reports.
type recTap struct {
	keepN int
	got   []string
}

func (r *recTap) keep(typ byte) int {
	if typ == '!' {
		return -1
	}
	if typ == 'D' {
		return 0
	}
	return r.keepN
}

func (r *recTap) message(typ byte, body []byte, size int) error {
	r.got = append(r.got, fmt.Sprintf("%c %d %q", typ, size, body))
	return nil
}

func message(typ byte, body string) []byte {
	b := append([]byte{typ}, binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))...)
	return append(b, body...)
}

func TestScannerFindsMessagesHoweverTheyArrive(t *testing.T) {
	var stream []byte
	stream = append(stream, message('Q', "select 1\x00")...)
	stream = append(stream, message('S', "")...)
	stream = append(stream, message('D', strings.Repeat("r", 5000))...)
	stream = append(stream, message('P', strings.Repeat("p", 300))...)
	stream = append(stream, message('X', "")...)
	want := []string{
		`Q 9 "select 1\x00"`, `S 0 ""`, `D 5000 ""`,
		fmt.Sprintf("P 300 %q", strings.Repeat("p", 100)), `X 0 ""`,
	}

	feed := func(sizes func() int) []string {
		tap := &recTap{keepN: 100}
		sc := &scanner{tap: tap}
		for rest := stream; len(rest) > 0; {
			n := min(sizes(), len(rest))
			// A copy, so a scanner that kept a reference past the call
			// would be caught reading freed memory.
			chunk := append([]byte(nil), rest[:n]...)
			if err := sc.feed(chunk); err != nil {
				t.Fatal(err)
			}
			clear(chunk)
			rest = rest[n:]
		}
		return tap.got
	}
	if got := feed(func() int { return len(stream) }); !reflect.DeepEqual(got, want) {
		t.Errorf("in one read:\n got %q\nwant %q", got, want)
	}
	if got := feed(func() int { return 1 }); !reflect.DeepEqual(got, want) {
		t.Errorf("a byte at a time:\n got %q\nwant %q", got, want)
	}
	for i := 0; i < 200; i++ {
		if got := feed(func() int { return 1 + rand.IntN(40) }); !reflect.DeepEqual(got, want) {
			t.Fatalf("in random pieces:\n got %q\nwant %q", got, want)
		}
	}
}

func TestScannerRefusesWhatIsNotPostgres(t *testing.T) {
	for name, stream := range map[string][]byte{
		"unknown type":     message('!', "x"),
		"length too small": {'Q', 0, 0, 0, 3},
		"length too large": {'Q', 0xff, 0xff, 0xff, 0xff},
	} {
		sc := &scanner{tap: &recTap{}}
		if err := sc.feed(stream); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// The hot path of a big result or a COPY: rows go by without the scanner
// allocating or copying anything.
func TestScannerSkipsRowsWithoutAllocating(t *testing.T) {
	tr := &tracker{rec: newRecorder(&memSink{}, nil, 1<<62)}
	defer tr.rec.Close()
	tr.q.push(item{kind: kQuery, st: newStmt("select 1", false)})
	sc := &scanner{tap: backTap{tr}}

	var chunk []byte
	for i := 0; i < 200; i++ {
		chunk = append(chunk, message('D', strings.Repeat("v", 300))...)
	}
	// Feed it in pieces that cut rows in half, as a socket would.
	allocs := testing.AllocsPerRun(50, func() {
		for rest := chunk; len(rest) > 0; {
			n := min(4099, len(rest))
			if err := sc.feed(rest[:n]); err != nil {
				t.Fatal(err)
			}
			rest = rest[n:]
		}
	})
	if allocs != 0 {
		t.Errorf("%v allocations per 200 rows, want 0", allocs)
	}
	if tr.q.front().rows != 200*51 {
		t.Errorf("counted %d rows", tr.q.front().rows)
	}
	if cap(sc.buf) != 0 {
		t.Errorf("the scanner buffered %d bytes of row data", cap(sc.buf))
	}

	// A single value far bigger than any buffer: still nothing kept.
	huge := message('D', "")
	binary.BigEndian.PutUint32(huge[1:], 64<<20+4)
	if err := sc.feed(huge); err != nil {
		t.Fatal(err)
	}
	piece := make([]byte, 64<<10)
	for sent := 0; sent < 64<<20; sent += len(piece) {
		if err := sc.feed(piece); err != nil {
			t.Fatal(err)
		}
	}
	if sc.inBody || cap(sc.buf) != 0 {
		t.Errorf("after a 64 MB row: inBody %v, buffered %d", sc.inBody, cap(sc.buf))
	}
}

// A script's statements are timed apart when the server answers them
// apart, and share their time when it answers them together.
func TestSimpleQueryTiming(t *testing.T) {
	sink := &memSink{}
	rec := newRecorder(sink, nil, 1<<62)
	defer rec.Close()
	tr := &tracker{rec: rec, meta: &meta{}, ready: true}
	front, back := &scanner{tap: frontTap{tr}}, &scanner{tap: backTap{tr}}
	at := func(now int64, sc *scanner, msgs ...[]byte) {
		t.Helper()
		tr.now = now
		for _, m := range msgs {
			if err := sc.feed(m); err != nil {
				t.Fatal(err)
			}
		}
	}
	done := message('C', "SELECT 1\x00")
	at(100, front, message('Q', "select 1; select 2; select 3; select 4\x00"))
	at(150, back, done)
	at(180, back, done, done)
	at(200, back, done, message('Z', "I"))
	rec.Flush()

	type timing struct {
		sql        string
		start, dur int64
		shared     bool
	}
	var got []timing
	for _, sp := range sink.take() {
		got = append(got, timing{sp.DBStatement, sp.StartNs, sp.DurationNs, strings.Contains(sp.Attrs, "shared_timing")})
	}
	want := []timing{
		{"select 1", 100, 50, false},
		{"select 2", 150, 30, true},
		{"select 3", 150, 30, true},
		{"select 4", 180, 20, false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("timings:\n got %+v\nwant %+v", got, want)
	}
}

func TestDeallocateForgetsTheStatement(t *testing.T) {
	rec := newRecorder(&memSink{}, nil, 1<<62)
	defer rec.Close()
	tr := &tracker{rec: rec, meta: &meta{}, ready: true}
	front, back := &scanner{tap: frontTap{tr}}, &scanner{tap: backTap{tr}}
	feed := func(sc *scanner, msgs ...[]byte) {
		t.Helper()
		for _, m := range msgs {
			if err := sc.feed(m); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range []string{"acme_one", "Acme Two", "acme_three"} {
		feed(front, message('P', name+"\x00select 1\x00\x00\x00"), message('S', ""))
		feed(back, message('1', ""), message('Z', "I"))
	}
	if len(tr.stmts) != 3 {
		t.Fatalf("remembers %d statements, want 3", len(tr.stmts))
	}
	for _, c := range []struct {
		sql  string
		left int
	}{
		{"deallocate ACME_ONE;", 2}, // an unquoted name folds to lower case
		{`DEALLOCATE PREPARE "Acme Two"`, 1},
		{"discard all", 0},
	} {
		sql, left := c.sql, c.left
		feed(front, message('Q', sql+"\x00"))
		feed(back, message('C', strings.ToUpper(strings.Fields(sql)[0])+"\x00"), message('Z', "I"))
		if len(tr.stmts) != left {
			t.Errorf("after %q: remembers %d statements, want %d", sql, len(tr.stmts), left)
		}
	}

	// And however many a client prepares, only so many are remembered.
	for i := 0; i < maxStmts+500; i++ {
		tr.remember(fmt.Sprint("s", i), &stmt{})
	}
	if len(tr.stmts) > maxStmts {
		t.Errorf("remembers %d statements", len(tr.stmts))
	}
}

// Whatever bytes arrive, in either direction, the tracker must fail
// quietly rather than panic: the relay calls it on live traffic.
func TestTrackerSurvivesNoise(t *testing.T) {
	rec := newRecorder(&memSink{}, nil, 1<<62)
	defer rec.Close()
	types := []byte("QPBEDCSHXFdcfp123CZTtnIsGHWEAND~")
	for i := 0; i < 3000; i++ {
		tr := &tracker{rec: rec, meta: &meta{}}
		front, back := &scanner{tap: frontTap{tr}}, &scanner{tap: backTap{tr}}
		for j := 0; j < 30; j++ {
			body := make([]byte, rand.IntN(24))
			for k := range body {
				body[k] = byte(rand.IntN(256))
				if rand.IntN(3) == 0 {
					body[k] = 0
				}
			}
			msg := message(types[rand.IntN(len(types))], string(body))
			sc := front
			if rand.IntN(2) == 0 {
				sc = back
			}
			if err := sc.feed(msg); err != nil {
				break
			}
		}
		tr.stop(true)
	}
}

func BenchmarkScanDataRows(b *testing.B) {
	tr := &tracker{rec: newRecorder(&memSink{}, nil, 1<<62)}
	defer tr.rec.Close()
	tr.q.push(item{kind: kQuery, st: newStmt("select 1", false)})
	sc := &scanner{tap: backTap{tr}}
	var chunk []byte
	for len(chunk) < 60<<10 {
		chunk = append(chunk, message('D', strings.Repeat("v", 100))...)
	}
	b.SetBytes(int64(len(chunk)))
	b.ReportAllocs()
	for b.Loop() {
		if err := sc.feed(chunk); err != nil {
			b.Fatal(err)
		}
	}
}
