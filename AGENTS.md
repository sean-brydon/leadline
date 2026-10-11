# Working on Leadline

Leadline is a local-first dev tool: it captures the traces, database queries, logs and metrics of apps running on your machine and shows them in a fast desktop app. Nothing leaves the machine.

## What is where

- `cmd/leadlined`: the daemon. One HTTP listener on `127.0.0.1:4320` takes OTLP/HTTP (`/v1/traces`, `/v1/logs`, `/v1/metrics`), serves the app's JSON API (`/api/*`), a server-sent "new data" stream (`/api/events`) and the MCP endpoint (`/mcp`). It also takes OTLP on the standard `4318` when that port is free (`-otlp ""` or `LEADLINE_OTLP_ADDR=off` turns that off).
- `cmd/leadline`: the command line: `run`, `doctor`, `prompt`, `brief`, `demo`, `skills`, `bench`, `service` (the daemon as a systemd user service on a Linux machine), `version`.
- `internal/store`: DuckDB (columnar, one file in `~/.leadline`). Every question the app asks is a SQL aggregate here. `link.go` decides what each span counts as; `retention.go` drops old data.
- `internal/ingest`: OTLP to rows, and `Fingerprint`, which groups queries by shape.
- `internal/api`: the HTTP surface, and `guard`, which keeps the daemon to localhost pages.
- `internal/pgproxy`: a Postgres proxy that records every statement passing through as a span, for apps with no instrumentation.
- `internal/integrations`: what a person connects (databases and Redis by URL), the proxies and Redis captures run for them, and `/api/sources`. Its HTTP API is written down in `internal/integrations/API.md`; the app is built from that file, so change them together.
- `internal/runner`: `leadline run`. It runs a dev command as if you had run it yourself, turns what it prints into logs, and sets it up to send telemetry.
- `internal/mcpserver`: read-only MCP tools for coding agents. `skills/`: the agent skills, embedded in the CLI.
- `internal/brief`: a problem written up as a prompt for a coding agent (a query, an endpoint, a trace, a log line), behind the app's "Send to Shipyard" and `leadline brief`. `internal/discover` finds Shipyard's command line, lists its boxes and, in `task.go`, starts a task: the one thing Leadline does outside itself. `internal/brief/API.md` describes the routes; the app's dialog is built from it.
- `internal/demo`: made-up "acme" telemetry, for trying the app and as the test fixture.
- `packages/node`: `@leadline/node`, the provider a Node.js app loads to send traces and logs. `packages/next`: `@leadline/next`, the same for Next.js, with the browser's side. `examples/`: small apps wired to them.
- `app/`: Tauri 2 + React 19 + Tailwind 4. Charts are uPlot on canvas (`components/Chart.tsx`). The page talks to the daemon over HTTP, so it also runs in a browser tab. The desktop app carries the daemon and starts it (`scripts/sidecar.sh` builds that copy).
- `site/`: the landing page, in Astro (`cd site && pnpm dev`, port 4390). Its one picture, `public/hero.png`, is made by `design/hero/dither.py` from `design/hero/source/`; `src/scripts/engraving.ts` makes its dots move.
- `Makefile`: `make dev` (the app from source), `make build` (both binaries into `bin/`), `make install` (both into `~/.local/bin`), `make check` (every check below), `make app` (the bundled app), `make demo`, `make linux` and `make release` (below).
- `scripts/`: `install.sh`, the installer for a Linux machine, and `install_test.sh`, its test; `build-linux.sh` and `linux-builder.Dockerfile`, the Linux binaries; `release.sh`, a release's files; `sidecar.sh`, the daemon the app carries.

## How data flows

An app exports OTLP and `ingest` flattens it to `store.Span`, `store.Log` and `store.MetricPoint`. The Postgres proxy and the Redis capture make spans of their own. All of them go to the store with `AddSpans` and friends, which only puts the rows in memory: one goroutine, the flusher, writes what is waiting a moment later (within 200 ms, and before the next query runs, so a read always sees what was added). The API's hub then wakes open event streams and the app refetches what it shows (`lib/live.ts`, at most once a second).

As it writes, the store settles what each span counts as, in two columns every aggregate filters on (`internal/store/link.go`):

- `entry`: the span is a request. A server or consumer span, or a span with no parent; not a span the browser made, not a server span inside a server span of the same service, not a statement a capture recorded. The overview and the endpoints count the same spans.
- `db_role`: `1` a query, `2` transaction and session control (`BEGIN`, `COMMIT`, `SET`, Redis `PING`: in the trace and in database time, not among the queries), `3` a second sighting of a call another span already counts.

An app can be seen two ways at once: its own OpenTelemetry spans, and the proxy recording the same statements off the wire. The store pairs the two (same system, same fingerprint, the wire's timing inside the app's span) so each statement is one query: the proxy's span moves into the app's trace, under the app's span, and is the one counted.

A database call is a span with `db_system` set; "queries" are the spans with `db_role = 1`, grouped by `db_fingerprint`.

Times are unix nanoseconds in the store and milliseconds (floats) in the API: a nanosecond timestamp doesn't fit a JS number.

## Checks

`make check` runs all of these, and CI runs the same:

```sh
go vet ./... && go test -race ./...
scripts/install_test.sh                              # the installer, on a made-up release in a temporary home
scripts/sidecar.sh                                   # the daemon the app carries; cargo needs it to exist
(cd app/src-tauri && cargo check && cargo test)
(cd app && CI=true pnpm install --frozen-lockfile && pnpm test && pnpm build)
(cd packages/node && CI=true pnpm install --frozen-lockfile && pnpm test)
```

To see it: `make dev`, then `go run ./cmd/leadline demo`. Or by hand: `go run ./cmd/leadlined`, and `cd app && pnpm dev` (http://localhost:1520).

After a change to a query, measure it: `go run ./cmd/leadline bench -db /tmp/bench.duckdb` fills a file with five million spans and times every question the app asks. Run it before and after, back to back; the file is large, so delete it when done.

CI also runs `shellcheck scripts/*.sh`; run it when you change a script.

## Versions and releases

The project has one version, the `VERSION` file (`0.2.0`, no `v`). The Makefile and the scripts stamp it into both programs as they build (`internal/version`, by `-ldflags -X`), with the commit: `leadline version`, `leadlined --version` and `"version"` in `GET /api/health` say it. A plain `go build` or `go run` says `dev`. The desktop app has a version of its own in `app/package.json`, `app/src-tauri/tauri.conf.json` and `app/src-tauri/Cargo.toml`; keep the four the same.

- **`make install`** puts `leadline` and `leadlined` in `~/.local/bin` (`PREFIX=` changes where). Two files and nothing else: no service, no data.
- **`make linux`** builds the Linux programs, amd64 and arm64, into `dist/linux_<arch>/`, in a container (`scripts/build-linux.sh`). The container is AlmaLinux 8, where DuckDB builds the static library the daemon links, so the programs need glibc 2.28 or newer and nothing else of the machine: the C++ runtime is linked in. The script fails if either stops being true. Each architecture builds on its own kind of processor; the other one runs under emulation, which Docker has to have turned on (the script says how). An amd64 program does not *run* under qemu without `--ulimit stack=-1` (DuckDB's allocator and qemu's addresses), which is only about testing on a Mac.
- **`make release`** makes `dist/leadline_<version>_<os>_<arch>.tar.gz` for Linux and macOS, both architectures of each, and `dist/checksums.txt` (`scripts/release.sh`; name targets to build fewer). Each tarball holds the two programs.
- **`scripts/install.sh`** installs a release on a Linux machine: both programs into `~/.local/bin` and the daemon as a systemd user service (the unit is written by the script, so it is the place to change it). It installs nothing it could not check against `checksums.txt` or `--sha256`. Test a change to it with `scripts/install_test.sh`, and for real with `sh scripts/install.sh --from dist/leadline_<version>_linux_<arch>.tar.gz` in a container that runs systemd.

To release:

1. Set `VERSION` (and the app's three) and commit: `release: v0.2.0`.
2. `make check`, then `make release` and try a tarball: `sh scripts/install.sh --from dist/…` on a Linux machine or in a container.
3. Tag that commit `v0.2.0` and push the tag. `.github/workflows/release.yml` builds the four tarballs on runners of each kind, installs the Linux ones and asks the daemon, and attaches the tarballs, `checksums.txt` and `install.sh` to a GitHub release. It refuses a tag that isn't `VERSION`'s. Run the workflow by hand from the Actions tab first to rehearse: it builds everything and releases nothing.

## Rules

- **Ports:** the app's dev server is 1520 and the daemon 4320. Never use or kill 1420 or 1377-1379 (another app's). Something else often holds 4318; the daemon copes, tests must not need it. For a second daemon or dev server pick a free port (`leadlined -addr 127.0.0.1:43xx -data :memory: -otlp ""`, `vite --port 15xx`) and check it is free first. Kill only PIDs you started.
- **Launching the app starts a real daemon.** `pnpm tauri dev`, the bundled app and the Rust tests run `leadlined` on the user's own data unless told otherwise. Anything that starts one sets `LEADLINE_DATA` to a temporary folder (and `LEADLINE_ADDR` to a free port), so a test never writes to `~/.leadline`.
- **Localhost only:** the daemon holds SQL and logs. It binds loopback, and `guard` refuses foreign Host and Origin headers. Keep new endpoints behind it.
- **The proxy must never break the app.** It relays bytes whatever happens: if it meets something it doesn't understand it stops reading and carries on relaying, and nothing it does to record a statement may delay or change one. The same goes for `leadline run`: the command first, Leadline second.
- **Tests need no network and no Docker.** Use `store.Open("")` (in memory), `internal/demo`, and the fake servers in `internal/integrations/fake`. Always `-race`. Measurements over millions of rows belong to `leadline bench`, not `go test`.
- **Fixtures are synthetic** ("acme"). Nothing from a real company or database goes in the repository.
- **No passwords out.** A database URL has one. Nothing that leaves `internal/integrations` or `internal/runner` carries it: not a response, not a log line, not a span.
- **API responses only grow.** The app is written against the JSON as it is. Add fields; don't rename or remove them, and don't change what one means.
- **New columns go last.** A database file from an older version gets a new column by `ALTER`, which adds at the end, and the appenders write by position. Add the column at the end of the `CREATE TABLE` and to `columnsAdded` in `internal/store/link.go`.
- **DuckDB types:** `sum` of a BIGINT is a HUGEINT, which the Go driver scans as `*big.Int`. Divide to a DOUBLE or cast before scanning. Appender values must match the column type exactly (`int64` for BIGINT, `int32` for INTEGER, `int8` for TINYINT).
- **New unit test files in the app** go into `app/package.json`'s `"test"` script list.
- **Shipyard is only read, except when a person sends.** Leadline runs four `berth` commands: three that list (`boxes`, `locations`, `agents`) and `task new`, which only `POST /api/shipyard/tasks` runs. No test runs a real `berth`: use `internal/discover/fakeberth`, and start a daemon you drive by hand with `LEADLINE_BERTH` set to that fake, or to `off`.
- **A brief holds no values unless asked.** Statements, log lines and error messages go in as shapes; `example=1` is the only way a value gets into a prompt, and `recorded` says which lines hold one.
- **MCP results stay small:** an agent's context is scarce. Default to short lists and a few buckets, and put anything large behind an argument.
- **Commit messages** are lowercase conventional (`feat(app): …`, `fix(daemon): …`). The subject says what the user gets.
- Comments say why, in plain words. Match the density of the file you are in.
