# Leadline

See what your code really did while you were developing it: every database query, trace, log line and metric from the apps running on your machine, in one fast desktop app. Local first; nothing leaves your computer.

- **Queries on any database.** Grouped by shape, with call counts, timings and N+1 detection.
- **Traces.** A waterfall of each request, with the exact SQL and the logs written during it.
- **Endpoints.** Each route's requests, latency, queries and database time.
- **Logs and metrics.** Searchable, filterable, linked to the request that wrote them.
- **Databases.** Connect a Postgres or a Redis and see its own statistics beside what your app sends it.
- **For coding agents.** A local MCP server and skills, so an agent can check what its change did.
- **Send to Shipyard.** From an N+1, a slow query, a failing endpoint or an error in the logs, start a coding agent on it with what Leadline recorded as its brief.

## Try it

```sh
make dev                        # the desktop app, with the daemon inside it
go run ./cmd/leadline demo      # made-up data to look at
```

`make dev` needs Go, Rust and pnpm. The daemon alone is `go run ./cmd/leadlined` (on `127.0.0.1:4320`), and the app also runs in a browser tab: `cd app && pnpm install && pnpm dev`, then http://localhost:1520.

## Show it your app

There are three ways in, and they add up.

**1. Start your app with `leadline run`.** Nothing in the project changes:

```sh
leadline run -- pnpm dev
leadline run -- python manage.py runserver
```

The command runs exactly as it did. Beside that, every line it prints becomes a log; it is handed the settings an OpenTelemetry SDK reads, so an app that has one starts sending; and if it uses a Postgres on this machine, it reaches it through a proxy that records every query. So even an app with no instrumentation shows its logs and its queries.

**2. Add the provider, for requests as traces.** In a Node.js project:

```sh
pnpm add -D @leadline/node      # Next.js: @leadline/next
```

`leadline run` loads it by itself. Each request then becomes a trace with its queries and its logs inside it. See `packages/node` and `packages/next`. In any other language, the app's own OpenTelemetry SDK does the same; Leadline takes OTLP/HTTP:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4320
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
```

**3. Connect a database** in the app (a Postgres or Redis URL) for its own statistics: sizes, slow statements, what is running now. A Postgres on this machine can also be captured through the proxy without `leadline run`.

A query that both your app and the proxy report is counted once: it sits in its request's trace, with the proxy's exact timing, row count and parameters.

`leadline doctor` says what is and isn't wired up in the project you are in.

## Run it on a server

When the dev server and the coding agent run on a Linux machine (a VPS), Leadline runs there too: it records next to the app. As your own user, no root:

```sh
curl -fsSL https://github.com/sean-brydon/leadline/releases/latest/download/install.sh | sh
```

That checks the download against the release's checksums, puts `leadline` and `leadlined` in `~/.local/bin`, and runs the daemon as a systemd service of your user, which restarts it if it fails. Run it again to upgrade. `--version 0.2.0` picks a release; `--from <tarball or folder>` installs one you copied over, for a machine that can't reach GitHub; `--uninstall` removes the service and the programs, and your data too only with `--purge`. It needs x86-64 or arm64 and glibc 2.28 or newer: Debian 10, Ubuntu 20.04, RHEL 8 and everything since, but not Alpine. Where there is no systemd (a container) it installs the programs and says how to start the daemon by hand.

- **At boot.** systemd runs a user's services only while that user is logged in, unless the user "lingers". The installer turns lingering on where it may, and otherwise names the one command an administrator runs once: `sudo loginctl enable-linger <you>`.
- **Data** is in `~/.leadline` on the server, one DuckDB file that only you can read.
- **Nothing listens beyond localhost.** The service binds `127.0.0.1:4320`, and `127.0.0.1:4318` for OpenTelemetry's default port; the proxies it opens for your databases are on `127.0.0.1` as well, and the daemon refuses a request addressed to any other name. It has no password because there is no way to it from the network, so don't publish it through a reverse proxy.
- **The service:** `leadline service status`, and `start`, `stop`, `restart`, `logs`.

On the server everything is as on a Mac: `leadline run -- pnpm dev`, and an agent there adds `http://127.0.0.1:4320/mcp`. To reach the daemon from your laptop, forward a port over SSH and leave that running:

```sh
ssh -N -L 4336:127.0.0.1:4320 you@server
```

The server's daemon is then `http://127.0.0.1:4336` on the laptop (4320 there is the laptop's own Leadline), for anything that takes an address: `LEADLINE_URL=http://127.0.0.1:4336 leadline doctor`, `curl http://127.0.0.1:4336/api/summary`, or `claude mcp add --transport http leadline-server http://127.0.0.1:4336/mcp`. The desktop app shows the Mac's own daemon; switching it to a forwarded one is on its way.

On a Mac the app runs the daemon. For the command line, `make install` in a checkout puts `leadline` and `leadlined` in `~/.local/bin`.

## Use it from a coding agent

```sh
claude mcp add --transport http leadline http://127.0.0.1:4320/mcp
leadline skills                 # installs the skills into .claude/skills
leadline prompt                 # one line that has an agent set Leadline up here
```

The tools only read: `overview`, `list_queries`, `list_traces`, `get_trace`, `search_logs`, `get_metrics`, `list_routes`, `list_sources`.

## Send a problem to an agent

Shipyard runs coding agents on your own machines, each task in a git worktree of its own. Where Leadline shows a problem (a query and its N+1 badge, an endpoint, a trace, an error in the logs) there is **Send to Shipyard**. It writes the problem up as a prompt: what is wrong with its numbers, the evidence (the statement's shape, the requests that run it and how often, the failing trace ids, the stack trace with your own frames picked out), what done looks like, and how to check a fix with the MCP tools above by running the route again and comparing. It guesses no cause.

The dialog shows **exactly the text that will be sent**, and you can edit it. Statements and log lines are reduced to their shape, with `?` where a value was; a switch puts one real statement and the log lines as written back in, and the lines copied from your app are marked either way. It also shows where the task will start, which you can change: if the service was attached by a Shipyard worktree, that worktree's box and project, branching from the worktree's own branch (the code the problem was recorded on); otherwise the Shipyard project its folder belongs to; otherwise you pick. Send starts the task with `berth task new`, and the worktree shows up in Leadline's sidebar when its app starts sending. Nothing is asked of Shipyard until the dialog opens, and nothing is sent until you press Send.

Without Shipyard the same dialog offers **Copy prompt**, and the command line prints it for any agent:

```sh
leadline brief query order_items | claude
leadline brief endpoint "POST /api/checkout"
leadline brief log "checkout failed"      # --example puts real values in
```

When the app is showing a daemon on another machine, that daemon writes the brief and this computer's own daemon sends it, since Shipyard's command line is here. `internal/brief/API.md` has the routes.

## What it keeps

Everything is in one DuckDB file in `~/.leadline`. Data older than 72 hours is dropped, and the oldest goes early once the file passes 2 GB (`leadlined -retain 7d -max-size 5GB`). The daemon answers only on localhost.

## Working on it

`AGENTS.md` has the layout, the checks (`make check`) and the rules.
