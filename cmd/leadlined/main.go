// leadlined is the Leadline daemon. It takes OpenTelemetry traces and logs
// over OTLP/HTTP, keeps them in a local DuckDB file, and serves the app.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/sean-brydon/leadline/internal/api"
	"github.com/sean-brydon/leadline/internal/integrations"
	"github.com/sean-brydon/leadline/internal/pgproxy"
	"github.com/sean-brydon/leadline/internal/store"
	"github.com/sean-brydon/leadline/internal/version"
)

// pgRoutes is the -pg flag: the Postgres proxies to run.
var pgRoutes pgproxy.Routes

// keep is the -retain and -max-size flags: what the store holds on to.
var keep retention

// engine is the -memory and -threads flags.
var engine store.Options

type retention struct{ store.Retention }

func (r *retention) parse(age, size string) (err error) {
	if r.MaxAge, err = store.ParseAge(age); err != nil {
		return errors.New("-retain: " + err.Error())
	}
	if r.MaxBytes, err = store.ParseSize(size); err != nil {
		return errors.New("-max-size: " + err.Error())
	}
	return nil
}

func main() {
	addr := flag.String("addr", envOr("LEADLINE_ADDR", "127.0.0.1:4320"), "address for the app's API and OTLP/HTTP")
	otlp := flag.String("otlp", otlpDefault(), "also take OTLP/HTTP on the standard port when it is free; empty or \"off\" turns this off (LEADLINE_OTLP_ADDR)")
	data := flag.String("data", envOr("LEADLINE_DATA", defaultData()), "data directory; \":memory:\" keeps nothing on disk")
	flag.Var(&pgRoutes, "pg", "record a Postgres's queries by proxying it: listen=upstream, as in 127.0.0.1:5433=127.0.0.1:5432; repeatable, or LEADLINE_PG with commas between")
	retain := flag.String("retain", envOr("LEADLINE_RETAIN", "72h"), "how long to keep data, as in 72h or 7d; 0 keeps it forever")
	maxSize := flag.String("max-size", envOr("LEADLINE_MAX_SIZE", "2GB"), "drop the oldest data early once the database is larger than this; 0 is no cap")
	memory := flag.String("memory", envOr("LEADLINE_MEMORY", ""), "cap on the database engine's memory, as in 256MB; past it big queries spill to disk (default 512MB)")
	threads := flag.Int("threads", envInt("LEADLINE_THREADS"), "cores one query may take (default: all but two)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.Line("leadlined"))
		return
	}
	if err := keep.parse(*retain, *maxSize); err != nil {
		slog.Error("leadlined", "err", err)
		os.Exit(2)
	}
	limit, err := store.ParseSize(*memory)
	if err != nil {
		slog.Error("leadlined", "err", "-memory: "+err.Error())
		os.Exit(2)
	}
	engine = store.Options{MemoryLimit: limit, Threads: *threads}
	if *otlp == "off" {
		*otlp = ""
	}

	if err := run(*addr, *otlp, *data); err != nil {
		slog.Error("leadlined stopped", "err", err)
		os.Exit(1)
	}
}

// otlpDefault is the second listener's address when the flag doesn't say.
// LEADLINE_OTLP_ADDR set to nothing, or to "off", means no second listener:
// unlike the other settings, here an empty value is an answer.
func otlpDefault() string {
	if v, ok := os.LookupEnv("LEADLINE_OTLP_ADDR"); ok {
		return v
	}
	return "127.0.0.1:4318"
}

func run(addr, otlpAddr, data string) error {
	// The signals are taken first and given back last, after the database
	// has closed. Until then a second Ctrl-C is only noted: let through, it
	// would kill the daemon in the middle of DuckDB's last write and leave
	// a write-ahead log to replay.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		<-signals
		cancel()
		said := false
		for range signals {
			if !said {
				slog.Info("already stopping: writing what is left and closing the database")
				said = true
			}
		}
	}()

	path := ""
	if data != ":memory:" {
		if err := os.MkdirAll(data, 0o700); err != nil {
			return err
		}
		path = filepath.Join(data, "leadline.duckdb")
	}
	st, err := store.Open(path, engine)
	if err != nil {
		return err
	}
	defer st.Close()
	st.Retain(keep.Retention)

	// What a person connected in the app: databases and Redis, their
	// proxies and live captures. The connections are in a file beside the
	// database, and are started again from it here.
	dir := data
	if data == ":memory:" {
		dir = "" // and no file of connections either
	}
	integ, err := integrations.Open(integrations.Options{Dir: dir, Store: st})
	if err != nil {
		return err
	}
	defer integ.Close()
	srv := api.New(st, integ)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	// Every request's context ends with ctx. An event stream never ends by
	// itself, so this is what lets the servers shut down at once.
	base := func(net.Listener) context.Context { return ctx }
	app := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, BaseContext: base}
	servers := []*http.Server{app}
	errs := make(chan error, 2)
	go func() { errs <- app.Serve(ln) }()
	slog.Info("listening", "addr", ln.Addr().String(), "data", data)
	otlpEndpoints := []string{"http://" + ln.Addr().String()}

	// 4318 is where every OTLP SDK sends by default, so taking it means an
	// app needs no endpoint setting. Something else often has it (a
	// collector in Docker, say); then the app's own port still works.
	if otlpAddr != "" {
		// Listening alone doesn't tell: on macOS a loopback listener is
		// allowed beside another program's listener on every address, and
		// would then take that program's traffic. So ask the port first.
		if answering(otlpAddr) {
			slog.Warn("standard OTLP port is in use by another program; send to the main address instead", "otlp", otlpAddr, "addr", ln.Addr().String())
		} else if oln, err := net.Listen("tcp", otlpAddr); err != nil {
			slog.Warn("standard OTLP port is taken; send to the main address instead", "otlp", otlpAddr, "addr", ln.Addr().String())
		} else {
			std := &http.Server{Handler: srv.OTLPHandler(), ReadHeaderTimeout: 10 * time.Second, BaseContext: base}
			servers = append(servers, std)
			go func() { errs <- std.Serve(oln) }()
			slog.Info("listening for OTLP", "addr", oln.Addr().String())
			otlpEndpoints = append(otlpEndpoints, "http://"+oln.Addr().String())
		}
	}

	integ.SetOTLP(otlpEndpoints...)

	// An app that sends no telemetry still shows its queries if it reaches
	// Postgres through one of these: the proxy records what passes. The
	// flag declares proxies ahead of time; the app adds more while the
	// daemon runs, and they are all in the one place.
	routes, err := pgproxy.RoutesOr(pgRoutes, os.Getenv("LEADLINE_PG"))
	if err != nil {
		return err
	}
	integ.Declare(routes)

	var failed error
	select {
	case <-ctx.Done():
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			failed = err
		}
	}
	cancel()
	// The streams have ended with ctx. What is left is a request in the
	// middle of being answered, which gets a moment and no more.
	shutdown, done := context.WithTimeout(context.Background(), 2*time.Second)
	defer done()
	for _, s := range servers {
		if err := s.Shutdown(shutdown); err != nil {
			s.Close()
		}
	}
	// The deferred closes run now, the database's last, and only then are
	// the signals let go.
	return failed
}

// answering reports whether something already accepts connections at addr.
func answering(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func defaultData() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".leadline"
	}
	return filepath.Join(home, ".leadline")
}

func envInt(key string) int {
	n, _ := strconv.Atoi(os.Getenv(key))
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
