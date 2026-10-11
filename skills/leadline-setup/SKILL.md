---
name: leadline-setup
description: Connect a project's development environment to Leadline so its logs, traces and database queries show up - wrap the dev command in `leadline run`, add the language's provider or OpenTelemetry setup, add Leadline's MCP server and skills to the coding agent, and prove that data arrives. Use when asked to set up or install Leadline in a codebase, when Leadline shows no data for a project, or when the leadline MCP tools are missing.
---

# Connecting a project to Leadline

Leadline shows what an app did while it ran on this machine: what it
logged, each request as a trace, and every database query. Its daemon
listens at `http://127.0.0.1:4320` and takes OpenTelemetry over OTLP/HTTP.

Work through the steps in order, from the project's root. Take the
lightest path that works: most projects need one changed line and, for
traces, one development dependency. Everything here is for development
only. Nothing you add may run in production, in CI or in a build.

## 1. Check Leadline is there

```sh
curl -s http://127.0.0.1:4320/api/health
```

`{"name":"leadline","ok":true}` means the daemon is up. Anything else: ask
the person to start Leadline (the app, or `leadlined` in a terminal) and
stop until they have.

```sh
command -v leadline && leadline doctor
```

`leadline doctor` says what is already wired in this project and what is
missing; trust it over your own guess, and run it again at the end. If
`leadline` is not on the PATH, try `~/.local/bin/leadline`, which is where
it is installed, and use that path from here on. If it is not there either,
do not build or download it: tell the person how to get it. On a Linux
machine Leadline's installer (`install.sh`, attached to every release and in
`scripts/` of its repository) puts `leadline` and the daemon in
`~/.local/bin` and runs the daemon; on a Mac the app runs the daemon but
brings no command line, and `make install` in a checkout of the repository
puts one in `~/.local/bin`. Until it is there, skip step 3 and use the
"without `leadline run`" parts of step 4.

## 2. Find out what the project is

Read, don't assume:

| Look for | It tells you |
| --- | --- |
| `package.json` | Node. The framework is in its dependencies (`next`, `vite`, `express`, `fastify`, `@nestjs/core`, `hono`, `@remix-run/*`), and the dev command in `scripts.dev`. |
| `pnpm-lock.yaml`, `yarn.lock`, `bun.lock`, `package-lock.json` | The package manager. Use that one, never a second. |
| `pnpm-workspace.yaml`, `turbo.json`, `nx.json`, `workspaces` in `package.json` | A monorepo. Set up the app the person works on, not the root, unless they say otherwise. |
| `pyproject.toml`, `requirements.txt`, `manage.py` | Python, and how it is run (`uv`, `poetry`, `pip`; Django, Flask, FastAPI). |
| `go.mod` | Go. |
| `Gemfile` | Ruby. |
| `Procfile.dev`, `Makefile`, `justfile`, `docker-compose.yml`, the README | How the app is really started in development. |
| `.env`, `.env.local`, `.env.development` | Whether `OTEL_*` or a database URL is already set, and where development settings live. Never print their values. |

Then check whether it already has OpenTelemetry or a Leadline provider:
search the dependencies for `@leadline/`, `@opentelemetry/`,
`opentelemetry`, `go.opentelemetry.io`. **If it has one, do not add
another.** Two in one process fight, and the second one's data goes
nowhere. Go to step 3; `leadline run` points the one that is there at
Leadline.

## 3. Put `leadline run` in front of the dev command

```sh
leadline run -- <the dev command, exactly as it is now>
```

For example `leadline run -- next dev`, `leadline run -- python manage.py
runserver`, `leadline run -- go run ./cmd/api`. The command runs as it did
before, in the same terminal, with the same colours, keys and exit code.
Beside that, with no change to the project's code:

- Every line it prints becomes a log in Leadline, under the project's name.
- It is given `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_PROTOCOL`,
  `OTEL_SERVICE_NAME` and `LEADLINE_URL`, unless the project sets them
  itself. An app that already has OpenTelemetry starts sending at once.
- If `@leadline/node` is installed, it is loaded into every Node process
  (step 4).
- If the environment or a `.env` file has a URL for a Postgres on this
  machine (`DATABASE_URL` and the like), the app is handed that database
  through a recording proxy, so every query shows in Leadline even from
  an app with no instrumentation. A remote database is never touched.
  The proxy is there beside instrumentation too: a query both report is
  one query in Leadline, in its request's trace, with the proxy's exact
  timing, row count and parameters.

It prints one dim line saying what it set up. `--name <service>` picks the
name, `--no-proxy` leaves database URLs alone, `--no-inject` sets nothing
in the environment.
With Leadline not running, the command runs just the same.

Where the line goes:

- **`package.json`:** `"dev": "leadline run -- next dev"`.
- **`Procfile.dev`, `Makefile`, `justfile`:** the same, in front of the
  command that starts the app.
- **Docker:** `leadline run -- docker compose up` captures the logs. An app
  inside a container cannot send traces: the daemon only answers on
  localhost. Say so; do not open the daemon up to make it work.

If other people work in this repository, do not change `dev` itself, since
it would stop working for anyone without Leadline. Add a script beside it
(`"dev:leadline": "leadline run -- <the dev command>"`) and tell the person
to start the app with that. Change `dev` only in a project that is the
person's alone, or when they ask for it.

## 4. Add traces for the language

`leadline run` alone gives logs, and Postgres queries through the proxy.
Requests as traces, with the queries and logs of each one tied to it, need
instrumentation inside the app.

### Node.js

Add the provider as a development dependency, with the project's package
manager:

```sh
npm install --save-dev @leadline/node     # or: pnpm add -D, yarn add -D, bun add -d
```

That is all: `leadline run` finds it and loads it before the app, in every
Node process the dev command starts. It needs Node.js 20.6 or newer, and it
does nothing when `NODE_ENV=production`. If the registry has no such
package, it has not been published for this Leadline: do not look for a
substitute. Keep step 3 and say so in your report.

It captures HTTP servers and clients, the frameworks and database drivers in
OpenTelemetry's automatic set (Express, Koa, NestJS, `pg`, `mysql2`,
`mongodb`, `ioredis`, `knex` and more), and `console.*` with the trace of
the request that logged. Read its README (`node_modules/@leadline/node/README.md`)
for what an ORM needs: most need nothing, Prisma 6 and older need
`@prisma/instrumentation`.

- **Next.js:** Next bundles server code, and OpenTelemetry cannot be
  bundled. Add `serverExternalPackages: ["@leadline/node"]` to `next.config`,
  and an `instrumentation.ts` at the root (or in `src/`):

  ```ts
  export async function register() {
    if (process.env.NEXT_RUNTIME === "nodejs") {
      const { register } = await import("@leadline/node");
      register();
    }
  }
  ```

- **Without `leadline run`:** load the provider with a flag in the dev
  script, `node --import @leadline/node/register server.js`, or for a tool
  that starts Node itself, `NODE_OPTIONS="--import @leadline/node/register"`.

### Python

There is no Leadline provider for Python; OpenTelemetry's own automatic
instrumentation does the work. Install it into the project's development
dependencies, then let it add the instrumentation for the libraries the
project uses:

```sh
pip install opentelemetry-distro opentelemetry-exporter-otlp-proto-http
opentelemetry-bootstrap -a install
```

and put `opentelemetry-instrument` in front of the app, inside
`leadline run`:

```sh
leadline run -- opentelemetry-instrument python manage.py runserver --noreload
```

`leadline run` supplies the endpoint, the protocol and the service name.
Under this launcher it also turns OpenTelemetry's own log export off
(`OTEL_LOGS_EXPORTER=none`), because the same lines are printed and
captured from there, and would otherwise be listed twice. Python's logs
are therefore in Leadline once, without a trace id.
Django's and Flask's reloaders start the app in a second process the
instrumentation does not follow, which is why the example turns the
reloader off (`--noreload`; Flask: `--no-reload`). Use `uv add --dev` or
`poetry add --group dev` where the project uses them, so the packages stay
out of production.

### Go, Ruby and others

There is no automatic instrumentation to switch on. `leadline run` already
gives logs and, through the proxy, Postgres queries; for many projects that
is the right place to stop. For traces the app needs the OpenTelemetry SDK
with its OTLP/HTTP exporter and the instrumentation for its HTTP framework
and database driver. That is a code change: read OpenTelemetry's current
documentation for the language, propose it to the person, and make it only
if they agree, behind a check that keeps it out of production. The exporter
reads `OTEL_EXPORTER_OTLP_ENDPOINT`, which `leadline run` sets. Leadline
takes `http/protobuf` and `http/json`, not gRPC.

A span shows as a database query when it carries `db.system.name` (or the
older `db.system`) and the statement in `db.query.text` (or `db.statement`).

## 5. Add the MCP server

So the agent can ask Leadline what the code did. From the project's root:

- **Claude Code:**

  ```sh
  claude mcp add --transport http leadline http://127.0.0.1:4320/mcp
  ```

- **Codex** (it keeps the server for every project, not this one alone):

  ```sh
  codex mcp add leadline --url http://127.0.0.1:4320/mcp
  ```

- **Cursor:** add the server to `.cursor/mcp.json`, keeping what is there.
  Cursor asks the person to approve a new server before it loads it:

  ```json
  { "mcpServers": { "leadline": { "url": "http://127.0.0.1:4320/mcp" } } }
  ```

- **Any other agent:** it is a streamable HTTP MCP server at
  `http://127.0.0.1:4320/mcp`, with no authentication. Use the agent's own
  way of adding one; do not guess at a config file you cannot check.

The tools only read: `overview`, `list_queries`, `list_traces`, `get_trace`,
`search_logs`. A server added now is usually loaded when the agent next
starts, so you may not have the tools in this session.

## 6. Install the skills

```sh
leadline skills
```

writes Leadline's skills into `.claude/skills`. For an agent that reads
skills from somewhere else, `leadline skills --dir <that folder>`.

## 7. Prove it

A setup that was not seen working is not done.

1. Start the app with the new command and wait until it is listening. Run
   it in the background or a second terminal; stop it when you are done.
2. Make one request to it (`curl` a page or an API route that reads from the
   database, if there is one).
3. Ask Leadline what arrived. Use the service name from the line
   `leadline run` printed:

   ```sh
   curl -s http://127.0.0.1:4320/api/summary
   curl -s "http://127.0.0.1:4320/api/logs?service=<name>&limit=5"
   curl -s "http://127.0.0.1:4320/api/traces?service=<name>&limit=5"
   curl -s "http://127.0.0.1:4320/api/queries?service=<name>"
   curl -s http://127.0.0.1:4320/api/runs
   ```

   The service must be in `summary`'s `services`, and `logs` must hold lines
   the app just printed. With instrumentation, `traces` holds the request
   you made; with a database, `queries` holds its SQL. The MCP `overview`
   tool answers the same, if you have it.
4. Run `leadline doctor` again and read what it still asks for.

If something is missing, look at the app's own output for errors from an
exporter before changing anything else, and say what you found. Do not
report success for a part you did not see.

## 8. Shipyard: a source for each worktree

Only when the project is run through Shipyard (a `.berth/` folder in the
repository, or `BERTH_*` variables in your environment); skip this
otherwise. Shipyard runs each task in a worktree of its own, often with a
database of its own that the setup script creates. Leadline can show each
worktree as a source of its own, attach its database when the worktree
appears, and clean both up when Shipyard removes it.

Three things do it, and they agree on one name, `<project>/<worktree>`
(`acme/fix-orders`; the project's name alone for its main checkout):

- **`leadline attach`**, run by a hook when a worktree is created and again
  when its setup has finished, registers the worktree and saves the
  Postgres and Redis URLs in its environment and `.env` files as
  connections that belong to it, with their queries recorded. It reads
  nothing else from those files and prints no password. Running it again
  changes nothing unless a URL changed.
- **`leadline run`** inside the worktree files under the same name, tags
  what it sends with the project, worktree, branch and box, and points the
  app at the proxy the worktree already has. Nothing to configure: it
  reads Shipyard's variables. `--name worker` there means
  `acme/fix-orders:worker`, still the worktree's.
- **`leadline detach`**, run by a hook when the worktree is removed, takes
  away the connections and proxies that worktree owns and deletes what was
  recorded under its name. A connection a person added, or another
  worktree's, is not touched. `--keep-data` leaves the data.

Print the hooks, in the format Shipyard's config takes:

```sh
leadline shipyard hooks
```

They go in one of two places, with the same entries in both:

- **This repository, on every box:** the `hooks` list of
  `.berth/config.json`. `leadline shipyard hooks --write .berth/config.json`
  adds them and leaves every other byte of the file as it was; hooks that
  are there already are not added twice. Shipyard runs a repository's
  config only once the box trusts it, and asks again after any change to
  the file, so tell the person to trust it (the app's Project settings, or
  `berth location config BOX/PROJECT --trust HASH`). This file is shared:
  change it only if the person asked for the hooks in the repository.
- **Every repository on one box:** that box's `~/.berth/hooks.json`.
  Leadline does not write there, and neither should you: `~/.berth` is
  Shipyard's and the person's. Give them the entries to paste, or point
  them at Shipyard's app (Automations, Advanced: shell hooks).

What to know before promising it works:

- The hooks run **on the box**, so Leadline has to be running on the box
  the worktree is on. On a box without it, or with it stopped, each hook
  says one line and exits 0; nothing in Shipyard fails. `leadline attach`
  in the worktree later catches it up.
- `leadline` has to be on the PATH of the box's hooks. If it is not,
  `leadline shipyard hooks --bin /full/path/to/leadline` prints the hooks
  with that path.
- A monorepo that keeps its `.env` beside an app needs the folder named:
  put `leadline attach --env-dir apps/web` in the hook's command.
- Worktrees that share one Redis are told apart by database number. Two
  that use the same number each record every command on it; give
  `leadline attach --no-capture` in the hook if that is the project's way.
- Pausing a worktree needs nothing: its source reads as quiet, and its
  connections stay for when it resumes.

To see it without waiting for Shipyard, run the hook's command by hand with
the variables Shipyard would set, then look and undo it:

```sh
BERTH_EVENT=worktree.created BERTH_LOCATION=<project> BERTH_NAME=<worktree> BERTH_PATH="$PWD" leadline attach
curl -s http://127.0.0.1:4320/api/attach
BERTH_EVENT=worktree.removed BERTH_LOCATION=<project> BERTH_NAME=<worktree> leadline detach --keep-data
```

## Do not

- **Touch production.** No change to production or shared configuration,
  deploy files, Dockerfiles used for release, or CI. No dependency outside
  the development group.
- **Commit or print secrets.** Do not echo the contents of `.env` files or
  database URLs, and do not write a URL with a password into any file.
  `leadline run` reads them itself.
- **Add a second provider.** If the project has OpenTelemetry or
  `@leadline/node`, use it.
- **Point the app at another collector,** or change an `OTEL_*` value the
  project already sets.
- **Edit application code** when a dev script and a dependency will do.
- **Leave it running.** Stop the processes you started.
- **Commit.** Leave the changes for the person to read, unless they asked
  you to commit.

## When you are done

Tell the person, briefly: the files you changed and what each change does;
the command that now starts the app; what you saw arrive in Leadline (the
service name, and a count of logs, traces and queries); and anything you
could not do, and why.
