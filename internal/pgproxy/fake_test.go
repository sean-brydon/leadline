package pgproxy

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// fakePG is a Postgres that lives in the test: it speaks the wire protocol
// well enough for pgx, and answers a handful of made-up statements. It
// asks for a cleartext password so the tests can check none is recorded.
type fakePG struct {
	t  testing.TB
	ln net.Listener

	// gate, when set, holds a "select gated" result after its first rows
	// until the test lets go.
	gate chan struct{}

	mu       sync.Mutex
	sawTLS   bool
	cancels  []uint32
	copied   []string
	password string
}

const fakePassword = "sekret-acme-pw"

func newFakePG(t testing.TB) *fakePG {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePG{t: t, ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakePG) addr() string { return f.ln.Addr().String() }

type fakePortal struct {
	sql    string
	params []string
	format int16
}

type fakeSession struct {
	f       *fakePG
	be      *pgproto3.Backend
	status  byte
	stmts   map[string]string
	portals map[string]fakePortal
}

func (f *fakePG) serve(c net.Conn) {
	defer c.Close()
	be := pgproto3.NewBackend(c, c)
	first, err := be.ReceiveStartupMessage()
	if err != nil {
		return
	}
	var start *pgproto3.StartupMessage
	switch m := first.(type) {
	case *pgproto3.StartupMessage:
		start = m
	case *pgproto3.CancelRequest:
		f.mu.Lock()
		f.cancels = append(f.cancels, m.ProcessID)
		f.mu.Unlock()
		return
	default:
		// The proxy answers encryption requests itself; one arriving
		// here is a bug.
		f.mu.Lock()
		f.sawTLS = true
		f.mu.Unlock()
		return
	}

	be.Send(&pgproto3.AuthenticationCleartextPassword{})
	if be.Flush() != nil {
		return
	}
	be.SetAuthType(pgproto3.AuthTypeCleartextPassword)
	msg, err := be.Receive()
	if err != nil {
		return
	}
	pw, ok := msg.(*pgproto3.PasswordMessage)
	if !ok || pw.Password != fakePassword {
		be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", SeverityUnlocalized: "FATAL", Code: "28P01", Message: "password authentication failed"})
		be.Flush()
		return
	}
	f.mu.Lock()
	f.password = pw.Password
	f.mu.Unlock()

	be.Send(&pgproto3.AuthenticationOk{})
	for k, v := range map[string]string{
		"server_version":              "17.0 (fake)",
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

	s := &fakeSession{f: f, be: be, status: 'I', stmts: map[string]string{}, portals: map[string]fakePortal{}}
	s.loop()
}

func (s *fakeSession) loop() {
	be := s.be
	discarding := false // after an error, until Sync
	fail := func(err *pgproto3.ErrorResponse) {
		be.Send(err)
		if s.status == 'T' {
			s.status = 'E'
		}
		discarding = true
	}
	for {
		msg, err := be.Receive()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.Terminate:
			return
		case *pgproto3.Flush:
			be.Flush()
			continue
		case *pgproto3.Sync:
			discarding = false
			be.Send(&pgproto3.ReadyForQuery{TxStatus: s.status})
			if be.Flush() != nil {
				return
			}
			continue
		case *pgproto3.Query:
			s.simple(m.String)
			if be.Flush() != nil {
				return
			}
			continue
		}
		if discarding {
			continue
		}
		switch m := msg.(type) {
		case *pgproto3.Parse:
			if strings.Contains(m.Query, "syntax_error") {
				fail(fakeError("42601", `syntax error at or near "syntax_error"`))
				continue
			}
			s.stmts[m.Name] = m.Query
			be.Send(&pgproto3.ParseComplete{})
		case *pgproto3.Bind:
			sql, ok := s.stmts[m.PreparedStatement]
			if !ok {
				fail(fakeError("26000", "prepared statement does not exist"))
				continue
			}
			p := fakePortal{sql: sql}
			for i, v := range m.Parameters {
				binaryFormat := len(m.ParameterFormatCodes) == 1 && m.ParameterFormatCodes[0] == 1 ||
					i < len(m.ParameterFormatCodes) && m.ParameterFormatCodes[i] == 1
				if binaryFormat && len(v) == 4 && strings.Contains(sql, "::int") {
					v = []byte(strconv.Itoa(int(int32(uint32(v[0])<<24 | uint32(v[1])<<16 | uint32(v[2])<<8 | uint32(v[3])))))
				}
				p.params = append(p.params, string(v))
			}
			if len(m.ResultFormatCodes) > 0 {
				p.format = m.ResultFormatCodes[0]
			}
			s.portals[m.DestinationPortal] = p
			be.Send(&pgproto3.BindComplete{})
		case *pgproto3.Describe:
			sql := s.portals[m.Name].sql
			format := s.portals[m.Name].format
			if m.ObjectType == 'S' {
				sql, format = s.stmts[m.Name], 0
				oids := make([]uint32, paramCount(sql))
				for i := range oids {
					oids[i] = 25
					if strings.Contains(sql, "::int") {
						oids[i] = 23
					}
				}
				be.Send(&pgproto3.ParameterDescription{ParameterOIDs: oids})
			}
			if returnsRows(sql) {
				be.Send(rowDescription(format))
			} else {
				be.Send(&pgproto3.NoData{})
			}
		case *pgproto3.Execute:
			p := s.portals[m.Portal]
			if err := s.run(p.sql, p.params, false, p.format); err != nil {
				fail(err)
			}
		case *pgproto3.Close:
			if m.ObjectType == 'S' {
				delete(s.stmts, m.Name)
			} else {
				delete(s.portals, m.Name)
			}
			be.Send(&pgproto3.CloseComplete{})
		default:
			s.f.t.Errorf("fake postgres: unexpected %T", msg)
			return
		}
	}
}

// simple answers a simple query: each statement in turn, stopping at the
// first that fails.
func (s *fakeSession) simple(sql string) {
	// Cut the way the proxy cuts, which is the way Postgres does; the
	// splitter has tests of its own, and the run against a real server
	// checks the two agree.
	statements := splitStatements(sql)
	if statements == nil {
		statements = []string{strings.TrimRight(strings.TrimSpace(sql), ";")}
	}
	ran := false
	for _, one := range statements {
		if strings.TrimSpace(one) == "" {
			continue
		}
		ran = true
		if err := s.run(one, nil, true, 0); err != nil {
			s.be.Send(err)
			if s.status == 'T' {
				s.status = 'E'
			}
			break
		}
	}
	if !ran {
		s.be.Send(&pgproto3.EmptyQueryResponse{})
	}
	s.be.Send(&pgproto3.ReadyForQuery{TxStatus: s.status})
}

var (
	reBig     = regexp.MustCompile(`big (\d+)`)
	reParam   = regexp.MustCompile(`\$(\d+)`)
	reComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
)

// paramCount is the highest $n in the statement.
func paramCount(sql string) int {
	n := 0
	for _, m := range reParam.FindAllStringSubmatch(sql, -1) {
		if i, _ := strconv.Atoi(m[1]); i > n {
			n = i
		}
	}
	return n
}

func returnsRows(sql string) bool {
	word := strings.ToLower(firstWord(sql))
	return word == "select" || word == "with"
}

func firstWord(sql string) string {
	fields := strings.Fields(reComment.ReplaceAllString(sql, " "))
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func rowDescription(format int16) *pgproto3.RowDescription {
	// Every column is text, whose binary form is the same bytes.
	return &pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{
		Name: []byte("v"), DataTypeOID: 25, DataTypeSize: -1, TypeModifier: -1, Format: format,
	}}}
}

func fakeError(code, message string) *pgproto3.ErrorResponse {
	return &pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: code, Message: message}
}

// run executes one statement and sends its result.
func (s *fakeSession) run(sql string, params []string, describe bool, format int16) *pgproto3.ErrorResponse {
	be := s.be
	word := strings.ToLower(firstWord(sql))
	done := func(tag string) *pgproto3.ErrorResponse {
		be.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
		return nil
	}
	switch word {
	case "":
		be.Send(&pgproto3.EmptyQueryResponse{})
		return nil
	case "begin":
		s.status = 'T'
		return done("BEGIN")
	case "commit":
		failed := s.status == 'E'
		s.status = 'I'
		if failed {
			return done("ROLLBACK")
		}
		return done("COMMIT")
	case "rollback":
		s.status = 'I'
		return done("ROLLBACK")
	}
	if s.status == 'E' {
		return fakeError("25P02", "current transaction is aborted, commands ignored until end of transaction block")
	}
	switch word {
	case "select", "with":
		if strings.Contains(sql, "boom") {
			return fakeError("42703", `column "boom" does not exist`)
		}
		if describe {
			be.Send(rowDescription(0))
		}
		rows := 1
		value := "1"
		if m := reBig.FindStringSubmatch(sql); m != nil {
			rows, _ = strconv.Atoi(m[1])
			value = strings.Repeat("x", 1024)
		}
		if len(params) > 0 {
			value = strings.Join(params, ",")
		}
		row := &pgproto3.DataRow{Values: [][]byte{[]byte(value)}}
		for i := 0; i < rows; i++ {
			be.Send(row)
			if i%256 == 255 {
				if be.Flush() != nil {
					return nil
				}
			}
			if i == 99 && s.f.gate != nil && strings.Contains(sql, "gated") {
				be.Flush()
				<-s.f.gate
			}
		}
		return done(fmt.Sprintf("SELECT %d", rows))
	case "insert":
		return done("INSERT 0 1")
	case "update":
		return done("UPDATE 3")
	case "copy":
		if strings.Contains(strings.ToLower(sql), "to stdout") {
			be.Send(&pgproto3.CopyOutResponse{OverallFormat: 0, ColumnFormatCodes: []uint16{0}})
			for i := 0; i < 3; i++ {
				be.Send(&pgproto3.CopyData{Data: []byte(fmt.Sprintf("row %d\n", i))})
			}
			be.Send(&pgproto3.CopyDone{})
			return done("COPY 3")
		}
		be.Send(&pgproto3.CopyInResponse{OverallFormat: 0, ColumnFormatCodes: []uint16{0}})
		if be.Flush() != nil {
			return nil
		}
		n := 0
		for {
			msg, err := be.Receive()
			if err != nil {
				return nil
			}
			switch m := msg.(type) {
			case *pgproto3.CopyData:
				n += strings.Count(string(m.Data), "\n")
				s.f.mu.Lock()
				s.f.copied = append(s.f.copied, string(m.Data))
				s.f.mu.Unlock()
			case *pgproto3.CopyDone:
				return done(fmt.Sprintf("COPY %d", n))
			case *pgproto3.CopyFail:
				return fakeError("57014", "COPY from stdin failed: "+m.Message)
			case *pgproto3.Sync, *pgproto3.Flush:
				// Ignored while copying, as Postgres does.
			}
		}
	}
	return done(strings.ToUpper(word))
}
