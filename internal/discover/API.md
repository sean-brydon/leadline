# The discovery API

What onboarding calls to look around instead of asking for a URL: what is
listening on this machine, the projects behind it, the database and Redis
URLs in their env files, and the owner's Shipyard boxes. The code is this
package (`internal/discover`); the routes are in `internal/api/discover.go`.
Change them together with this file: the app's screens are written from it.

| | |
| --- | --- |
| `GET /api/discover/listening` | what is listening now (fast, no project file read) |
| `POST /api/discover/scan` | start a scan, optionally of folders. **Opens no env file** |
| `GET /api/discover/scan` | the scan: its progress, then its result |
| `DELETE /api/discover/scan` | stop it and forget what it found |
| `POST /api/discover/env` | read the env files of the projects named, for connection URLs |
| `POST /api/discover/save` | save found connections by id, capture on or off |
| `POST /api/discover/test` | try found connections by id, saving nothing |
| `GET /api/discover/shipyard` | is Shipyard here, and its boxes |
| `GET /api/discover/shipyard/boxes/{box}` | one box's projects and worktrees |

Onboarding in order:

1. `GET /api/discover/listening` for the "this machine" card.
2. `POST /api/discover/scan`, then `GET /api/discover/scan?wait_ms=…` until
   `state` is `done`: the projects, with what each is, how to connect it,
   and **the names of its env files**. Nothing in an env file has been read.
3. The person picks projects. `POST /api/discover/env` with those: their env
   files are read for connection URLs, which come back masked, each with an
   `id`.
4. `POST /api/discover/save` with the ids the person ticked.

Every example below is a real response, from a daemon on a Linux machine
set up for the purpose: five made-up "acme" projects in `/home/acme/code`,
four of them with a dev server running, and a Postgres 17 and a Redis 7 in
containers beside it. That daemon was started on port 4342, not the usual
4320, which is why the advice in the examples names its address. Where a
list is shortened the text says so, and a long `note` is cut with `…`.
Nothing else was changed, with two exceptions, both written in by hand as
the daemon answers now, because they are newer than those responses: a
saved connection's `owner` (its two places in the example under
[Saving](#saving)), and the `url` of the last result under
[Testing](#testing), which has the parameters Prisma wrote.

## Ground rules

The integrations API's rules hold here too (`internal/integrations/API.md`):
the base URL is the daemon's, every route is behind `guard` (localhost
only), bodies are JSON of at most 64 KB, times are unix milliseconds in
fields ending `_ms`, lists are never `null`, and an error is
`{"error":{"code","message","hint"}}`:

```json
{
  "error": {
    "code": "no_scan",
    "message": "There is no scan.",
    "hint": "Start one with POST /api/discover/scan."
  }
}
```

Codes this API adds:

| code | status | when |
| --- | --- | --- |
| `no_scan` | 404 | there is no scan to read, to read env files for, or to save from |
| `no_project` | 404 | `env` named a project the latest scan does not have |
| `scan_not_done` | 409 | `env` was asked while the scan is still running |
| `bad_request` | 400 | the body is not JSON, `env` named no project, or there are too many folders, projects or ids |
| `shipyard_not_found` | 404 | a box's projects were asked for and Shipyard is not here |
| `no_box` | 404 | Shipyard has no box of that name |
| `shipyard_failed` | 502 | the `berth` command failed, timed out, or printed something else than JSON |

Inside a `save` or `test` answer, each id has its own error: `not_found`
(no found connection has that id in the latest scan), `expired` (the daemon
no longer holds the URL: scan again), `not_connectable` (MySQL, MongoDB, a
URL that refers to other variables), `remote` (a test of a remote server
that was not asked for by name), or any code the integrations API gives.

## What is read, and what is not

This is the contract. The tests in this package hold the code to it.

### A scan: no env file

`GET /api/discover/listening` and `POST /api/discover/scan` read this and
nothing more.

**From the operating system**, with no extra privileges and without running
any other program:

- The listening TCP sockets of this user's processes: the port and the
  address it is bound to. On macOS through `libproc` (`proc_pidinfo`,
  `proc_pidfdinfo`); on Linux from `/proc/net/tcp`, `/proc/net/tcp6` and
  `/proc/<pid>/fd`.
- For each process that has one: its name, its command line (arguments
  only, never its environment), its working directory, its parent, and when
  it started.
- Not the environment of any process. Not what it has open besides
  listening sockets. Not the sockets of another user's processes, which the
  system does not show (see [Limits](#limits)).
- No port is connected to, and nothing is sent to any of them.

**From disk**, which folders:

- For a listening dev server, and for a `leadline run` the daemon has seen:
  its working directory and the folders above it, at most six levels up and
  **never the home directory or anything above it**, looking for a project
  file by name. Then, from the project up to its git checkout, for a `.git`
  and a lock file by name.
- For a folder a person names (`scan` only): that folder, to **3 levels
  below it**, not following symbolic links, skipping dot-directories and
  build output (`node_modules`, `vendor`, `dist`, `build`, `out`, `target`,
  `coverage`, `tmp`, …), stopping at 5 000 directories or 200 projects.
  **The home directory, anything above it, and `/` are refused**, not
  walked: name a folder inside.

**From disk**, which files, in a folder that is a project:

- The folder's listing: the names in it. This is how the env files are
  known to be there. **They are not opened, and not stat'ed.**
- Read: `package.json`; `go.mod`; `pyproject.toml` and `requirements.txt`;
  `Gemfile` and `Gemfile.lock`; `Cargo.toml` (whichever the project has);
  `next.config.*`; the `package.json` of an installed framework
  (`node_modules/next/package.json`), when it is a real folder and not a
  link out of the project.
- Looked at by name only: lock files (`pnpm-lock.yaml`, `yarn.lock`,
  `bun.lock`, `package-lock.json`, `uv.lock`, `poetry.lock`), `manage.py`,
  `main.py`, `app.py`, `.air.toml`, `bin/dev`, and under `cmd/` which
  folders have a `main.go`.
- From git: `.git/HEAD`, and for each linked worktree
  `.git/worktrees/<name>/gitdir` and `HEAD`. For a project that is itself a
  linked worktree, its `.git` file and the `HEAD` it points to, which is in
  the main repository's `.git/worktrees/<name>/`: the one file read outside
  the folders above. Git's config, with the remote's URL, is not read.

`GET /api/discover/listening` reads none of those files: it only looks for
a project file by name beside a runtime that is listening.

### The env step: the env files of the projects you name

`POST /api/discover/env` is the only request that opens an env file, and it
opens those of the projects in its `projects` list and of no other. There
is no request that reads env files without a list of projects in it, and no
way to say "all": the caller names each one, from the scan's result.

In each named project, four files are read if they are there:
`.env.development.local`, `.env.local`, `.env.development`, `.env` (the
ones `leadline run`, Next.js and Vite read in development, in that order of
precedence). A file that is a symbolic link is read through it, as those
tools read it.

From them, only connection URLs: a variable whose whole value is a
`postgres://`, `postgresql://`, `redis://`, `rediss://`, `mysql://`,
`mariadb://`, `mongodb://` or `mongodb+srv://` URL (with the `jdbc:` and
`+driver` spellings). For each: the URL, the variable's name, the file.

- **No other value is kept.** The file is read into memory to be parsed;
  every value that is not a connection URL is passed over where it lies,
  never copied out, and only counted (`other_keys`). The buffer is zeroed
  when the parse ends.
- **Nothing is interpolated.** A URL that refers to another variable
  (`${DB_PASSWORD}`) is reported as found and not connectable; the other
  variable is not read to fill it in.
- **The password stays in the daemon.** A response has the URL with `****`
  for the password and an opaque `id`. Saving is by id. The URL as written
  is held in memory only, for 30 minutes after the last `env` request
  (`secrets_until_ms`) or until the next scan or a `DELETE`, whichever is
  first; then it is in the connections file if it was saved, and nowhere
  if it was not.
- No response, log line or error has a password, or any value from an env
  file that is not a connection URL.

### Always

**Nothing is written** to any project. `connect` is advice.

**Nothing is connected to** except by `test`, and by `save` (the
integrations manager checks a connection it has just saved).

**Shipyard's command line is run only when one of the two Shipyard routes
is asked for**, or one of the routes that send a problem to Shipyard
(`internal/brief/API.md`): not when the daemon starts, not by a scan, not by
`env` or `save`. See [Shipyard](#shipyard).

## Listening

```
GET /api/discover/listening
```

Answers in a few milliseconds. For the "this machine" card.

```json
{
  "machine": {
    "os": "linux",
    "hostname": "acme-box"
  },
  "listeners": [
    {
      "port": 3000,
      "address": "127.0.0.1",
      "scope": "loopback",
      "pid": 4415,
      "process": "node",
      "command": "node node_modules/.bin/next dev -p 3000",
      "cwd": "/home/acme/code/acme",
      "started_ms": 1791653792340,
      "uptime_s": 547,
      "kind": "dev-server",
      "what": "next dev",
      "framework": "next",
      "basis": "command"
    },
    {
      "port": 8080,
      "address": "127.0.0.1",
      "scope": "loopback",
      "pid": 6457,
      "process": "billing",
      "command": "/tmp/go-build293428833/b001/exe/billing",
      "cwd": "/home/acme/code/billing-service",
      "started_ms": 1791653799790,
      "uptime_s": 539,
      "kind": "dev-server",
      "what": "go run",
      "framework": "go",
      "basis": "command"
    },
    {
      "port": 5432,
      "address": "0.0.0.0",
      "scope": "all",
      "pid": 0,
      "process": "",
      "command": "",
      "cwd": "",
      "kind": "postgres",
      "what": "Postgres",
      "basis": "port"
    }
  ],
  "limits": {
    "method": "procfs",
    "complete": false,
    "skipped_processes": 6,
    "unowned_ports": 2,
    "note": "Some ports are listed without a process: they belong to another user (root's services, Docker's published ports), and Linux shows which process holds a socket only to its owner."
  }
}
```

(Three of the 6 listeners that machine had; the others were two more dev
servers and Redis.)

A listener:

| field | |
| --- | --- |
| `port`, `address` | what it is bound to. One entry per port: a server on both `127.0.0.1` and `::1` is one entry, and so is one whose workers share the socket |
| `scope` | `loopback`, `all` (every interface) or `address` (one address of this machine) |
| `pid`, `process`, `command`, `cwd` | the process. `command` has any URL's password, and the value of any flag or variable that is called a password, token, secret or key, replaced by `***`, and is cut at 400 characters. `pid` is `0` and the rest empty for a port whose process can't be seen |
| `started_ms`, `uptime_s` | when the process started |
| `kind` | `dev-server`, `postgres`, `redis`, `mysql`, `mongodb` or `other` |
| `what` | that in words: `next dev`, `vite`, `go run`, `Postgres`, `node` |
| `framework` | a dev server's: `next`, `vite`, `nuxt`, `astro`, `remix`, `django`, `rails`, `flask`, `fastapi`, `go`, `rust`, `node`, `python`, … |
| `basis` | how `kind` was decided: `process` (its name), `command` (its command line), `cwd` (a runtime such as `node` or `python` whose working directory is in a project), `env-url` (a found connection URL points at this port; only after `env`), `port` (the port number alone, 5432, 6379, 3306 or 27017: a guess) |
| `project_id` | the project its working directory is in; only in a scan |

Left out: Leadline's own daemon (so also its proxies and its OTLP port),
any `leadlined`, and anything one of them started.

The list is sorted: dev servers, then databases and caches, then the rest;
by port within each.

### Limits

`limits` says what the list could not include:

| | macOS | Linux |
| --- | --- | --- |
| `method` | `libproc` | `procfs` |
| this user's processes | everything | everything |
| another user's processes (root's, a system service's) | **not listed at all**; counted in `skipped_processes` | **the port is listed, its process is not**: `pid` is `0`; counted in `unowned_ports` |
| a server in a container | listed as the port its runtime publishes, under the runtime's own process (so `kind` comes from the port number, or from an env file's URL once `env` has run) | a published port belongs to root's proxy: the port, no process (as Postgres above). A port that is not published is in another network namespace and not seen |
| as root | not tried | everything, given `CAP_SYS_PTRACE`, which root on the machine itself has and root inside a container usually has not |

`complete` is true when nothing was skipped; `note` says the same as a
sentence. Where there is no way built in to ask (Windows, or a macOS build
without cgo) `method` is `none` and the list is empty; a scan still finds
projects in folders and from `leadline run`.

## Scanning

```
POST /api/discover/scan
{ "folders": ["~/code"] }
```

Starts a scan and answers at once with it, `state` `running`. `folders` is
optional (at most 8): without it the scan is of what is listening and of
the `leadline run` commands the daemon has seen. A leading `~` is the home
directory. Starting a scan while one runs cancels the old one, and a new
scan forgets what the last one found: the ids of its connections stop
working.

```json
{
  "id": "s_55fafedac3",
  "state": "running",
  "started_ms": 1791654339681.3005,
  "progress": {
    "phase": "listening",
    "folders_done": 0,
    "folders_total": 1,
    "directories": 0,
    "projects": 0
  },
  "folders": [
    {
      "path": "~/code",
      "state": "pending",
      "projects": 0,
      "directories": 0
    }
  ],
  "machine": {
    "os": "linux",
    "hostname": "acme-box"
  },
  "listeners": [],
  "limits": {
    "method": "",
    "complete": false,
    "skipped_processes": 0,
    "unowned_ports": 0
  },
  "projects": [],
  "connections": [],
  "env_projects": [],
  "env": {
    "files_read": 0,
    "urls": 0,
    "other_keys": 0
  }
}
```

```
GET /api/discover/scan
GET /api/discover/scan?wait_ms=10000
```

The latest scan. Poll it until `state` is not `running`; with `wait_ms` (at
most 30 000) the answer is held until the scan ends or that long has passed,
which saves polling. `404 no_scan` before the first scan and after a
`DELETE`.

`state` is `running`, `done`, `cancelled` (a newer scan replaced it while
this request waited) or `failed`. While it runs, `progress.phase` goes
`listening`, `projects`, `folders`; `listeners` and `limits` are filled
when the first ends and `projects` when the scan is done. That machine's
scan took 7 ms.

```json
{
  "id": "s_55fafedac3",
  "state": "done",
  "started_ms": 1791654339681.3005,
  "finished_ms": 1791654339687.9253,
  "progress": {
    "phase": "done",
    "folders_done": 1,
    "folders_total": 1,
    "directories": 8,
    "projects": 5
  },
  "folders": [
    {
      "path": "~/code",
      "resolved": "/home/acme/code",
      "state": "scanned",
      "projects": 5,
      "directories": 8
    }
  ],
  "machine": {
    "os": "linux",
    "hostname": "acme-box"
  },
  "listeners": [
    {
      "port": 3000,
      "address": "127.0.0.1",
      "scope": "loopback",
      "pid": 4415,
      "process": "node",
      "command": "node node_modules/.bin/next dev -p 3000",
      "cwd": "/home/acme/code/acme",
      "started_ms": 1791653792340,
      "uptime_s": 547,
      "kind": "dev-server",
      "what": "next dev",
      "framework": "next",
      "basis": "command",
      "project_id": "p_a6cec1a720"
    },
    {
      "port": 5432,
      "address": "0.0.0.0",
      "scope": "all",
      "pid": 0,
      "process": "",
      "command": "",
      "cwd": "",
      "kind": "postgres",
      "what": "Postgres",
      "basis": "port"
    }
  ],
  "limits": {
    "method": "procfs",
    "complete": false,
    "skipped_processes": 6,
    "unowned_ports": 2,
    "note": "Some ports are listed without a process: they belong to another user (root's services, Docker's published ports), and Linux shows which process holds a socket only to its owner."
  },
  "projects": [
    {
      "id": "p_a6cec1a720",
      "name": "acme",
      "path": "/home/acme/code/acme",
      "sources": [
        "folder",
        "listening"
      ],
      "stack": "node",
      "frameworks": [
        {
          "id": "next",
          "name": "Next.js",
          "version": "16.0.3"
        }
      ],
      "label": "Next.js 16",
      "package_manager": "pnpm",
      "dev_command": "pnpm dev",
      "dev_script": {
        "file": "package.json",
        "name": "dev",
        "command": "next dev"
      },
      "opentelemetry": false,
      "sends": {
        "already": false,
        "because": []
      },
      "git": {
        "root": "/home/acme/code/acme",
        "branch": "main",
        "worktree": false,
        "worktrees": [
          {
            "path": "/home/acme/code/acme-fix-orders-n1",
            "branch": "fix-orders-n1"
          }
        ]
      },
      "connect": {
        "kind": "next-plugin",
        "summary": "Add the Next.js plugin: one development dependency and one line in next.config.ts.",
        "command": "pnpm add -D @leadline/next",
        "file": "next.config.ts",
        "lines": [
          "import { withLeadline } from \"@leadline/next\";",
          "export default withLeadline(nextConfig);"
        ],
        "env_file": ".env.local",
        "env_lines": [
          "LEADLINE_URL=http://127.0.0.1:4342"
        ]
      },
      "running": [
        {
          "port": 3000,
          "pid": 4415,
          "what": "next dev",
          "uptime_s": 547
        }
      ],
      "env_files": [
        {
          "file": ".env.local",
          "read": false,
          "urls": 0,
          "other_keys": 0
        },
        {
          "file": ".env",
          "read": false,
          "urls": 0,
          "other_keys": 0
        }
      ],
      "connection_ids": []
    },
    {
      "id": "p_a5f0f9c122",
      "name": "acme-admin",
      "path": "/home/acme/code/acme-admin",
      "sources": [
        "folder",
        "listening"
      ],
      "stack": "node",
      "frameworks": [
        {
          "id": "vite",
          "name": "Vite",
          "version": "7.1.4"
        },
        {
          "id": "express",
          "name": "Express",
          "version": "5.1.0"
        }
      ],
      "label": "Vite + Express",
      "package_manager": "npm",
      "dev_command": "npm run dev",
      "dev_script": {
        "file": "package.json",
        "name": "dev",
        "command": "vite"
      },
      "opentelemetry": false,
      "sends": {
        "already": false,
        "because": []
      },
      "connect": {
        "kind": "run-wrap",
        "summary": "Put leadline run in front of the dev command: its output becomes logs and its Postgres queries are recorded.",
        "command": "leadline run --url http://127.0.0.1:4342 -- npm run dev",
        "traces": {
          "summary": "For traces too, add the Node.js provider. leadline run loads it.",
          "command": "npm install --save-dev @leadline/node"
        }
      },
      "running": [
        {
          "port": 5173,
          "pid": 4419,
          "what": "vite",
          "uptime_s": 547
        }
      ],
      "env_files": [
        {
          "file": ".env",
          "read": false,
          "urls": 0,
          "other_keys": 0
        }
      ],
      "connection_ids": []
    },
    {
      "id": "p_4f3c4a7cef",
      "name": "billing-service",
      "path": "/home/acme/code/billing-service",
      "sources": [
        "folder",
        "listening"
      ],
      "stack": "go",
      "frameworks": [
        {
          "id": "go",
          "name": "Go",
          "version": "1.27"
        },
        {
          "id": "chi",
          "name": "chi",
          "version": "5.2.1"
        }
      ],
      "label": "Go 1.27 + chi",
      "package_manager": "go",
      "dev_command": "go run ./cmd/billing",
      "dev_script": {
        "file": "cmd/billing/main.go"
      },
      "opentelemetry": true,
      "sends": {
        "already": false,
        "because": []
      },
      "connect": {
        "kind": "otel-env",
        "summary": "It already has OpenTelemetry: point its exporter at Leadline.",
        "command": "leadline run --url http://127.0.0.1:4342 -- go run ./cmd/billing",
        "file": ".env",
        "lines": [
          "OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4342",
          "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf"
        ]
      },
      "running": [
        {
          "port": 8080,
          "pid": 6457,
          "what": "go run",
          "uptime_s": 539
        }
      ],
      "env_files": [
        {
          "file": ".env",
          "read": false,
          "urls": 0,
          "other_keys": 0
        }
      ],
      "connection_ids": []
    }
  ],
  "connections": [],
  "env_projects": [],
  "env": {
    "files_read": 0,
    "urls": 0,
    "other_keys": 0
  }
}
```

(Two of the 6 listeners and three of the 5 projects. `connections` is
empty and `env` is all zeros because no env file has been read: that is the
next step.)

```
DELETE /api/discover/scan
```

Stops the scan if it is running and forgets it, found URLs included.
Answers `{"ok":true}` whether or not there was one.

### A folder

| `state` | |
| --- | --- |
| `pending` | not reached yet |
| `scanned` | walked. `truncated` is true if it stopped at 5 000 directories or 200 projects |
| `refused` | Leadline won't walk it: the home directory, a folder above it, or `/`. `reason` says so |
| `missing` | it is not a folder that can be read. `reason` says which |

What `{"folders": ["~", "/home/acme/nope"]}` came to:

```json
[
  {
    "path": "~",
    "resolved": "/home/acme",
    "state": "refused",
    "reason": "Leadline won't walk the home directory or a folder above it. Name the folder your projects are in, such as ~/code.",
    "projects": 0,
    "directories": 0
  },
  {
    "path": "/home/acme/nope",
    "state": "missing",
    "reason": "There is no folder there.",
    "projects": 0,
    "directories": 0
  }
]
```

### A project

A project is a folder with `package.json`, `go.mod`, `pyproject.toml`,
`Gemfile` or `Cargo.toml`, or the root of a git checkout. A package inside
a repository is a project of its own only when it has a dev command or a
framework.

| field | |
| --- | --- |
| `id` | the same for the same path in every scan |
| `name` | the name `leadline run` gives it: what its project file declares, else the folder's name. A linked worktree has its repository's name; tell them apart by `git.branch` |
| `sources` | how it was found: `listening`, `run`, `folder` |
| `stack` | `node`, `go`, `python`, `ruby`, `rust`, `unknown` |
| `frameworks` | `id` (`next`, `nuxt`, `remix`, `react-router`, `astro`, `sveltekit`, `gatsby`, `angular`, `expo`, `vite`, `create-react-app`, `nest`, `express`, `fastify`, `hono`, `koa`, `django`, `fastapi`, `flask`, `starlette`, `rails`, `sinatra`, `hanami`, `go`, `gin`, `echo`, `chi`, `fiber`, `gorilla`, `connect`, `axum`, `actix`, `rocket`, `warp`), `name`, and `version`: the installed one when `node_modules` has the package itself or `Gemfile.lock` names it, else the one the project file asks for |
| `label` | for showing: `Next.js 16`, `Vite + Express`, `Go 1.27 + chi`, `Django 5.1` |
| `package_manager` | `pnpm`, `npm`, `yarn`, `bun`, `uv`, `poetry`, `pipenv`, `pip`, `bundler`, `go`, `cargo` |
| `dev_command`, `dev_script` | what starts it in development, and where that came from. Absent when nothing says |
| `opentelemetry` | it already depends on an OpenTelemetry SDK |
| `sends` | `already`, and `because`: `service` (Leadline holds data under its name; `service` names it), `run` (a `leadline run` in its folder), `plugin` (`@leadline/next` is in `next.config`), `provider` (`@leadline/node` is a dependency), `dev-script` (its dev script already uses `leadline run`) |
| `git` | `root`, `branch`, `worktree`; `main_repo` for a linked worktree; `worktrees` (path and branch of each) for a main checkout that has some. Absent for a folder in no repository |
| `connect` | the advice, below |
| `running` | its listeners now |
| `env_files` | the env files it has, by name. `read` is false, and the counts zero, until `env` is asked for this project |
| `connection_ids` | the connections found in them; empty until then |

The `git` of the linked worktree that scan found:

```json
{
  "root": "/home/acme/code/acme-fix-orders-n1",
  "branch": "fix-orders-n1",
  "worktree": true,
  "main_repo": "/home/acme/code/acme"
}
```

### `connect`: how to connect it

Advice as data. Leadline changes nothing in a project.

| `kind` | when | what is in it |
| --- | --- | --- |
| `already` | data is arriving under its name, it runs under `leadline run`, its dev script uses `leadline run`, or the Next.js plugin is wired in | `summary` only |
| `next-plugin` | Next.js without OpenTelemetry | `command`: the install, with the project's package manager (absent when it is installed and only not wired in). `file` and `lines`: what goes in `next.config` |
| `otel-env` | it has OpenTelemetry | `file` and `lines`: `OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_PROTOCOL`, with this daemon's address. `command`: the same done by `leadline run`, which sets them itself |
| `run-wrap` | everything else | `command`: `leadline run -- ` and the dev command. `traces`, for Node.js: a further step, installing `@leadline/node` |

`summary` is one sentence. When no dev command is known, `command` ends in
`<your dev command>`.

**The daemon's address** is in the advice whenever the daemon is not at the
usual `http://127.0.0.1:4320`, taken from the address the scan request
came to, so the app need not patch it: `command` is then
`leadline run --url http://127.0.0.1:4342 -- pnpm dev`, and a `next-plugin`
has `env_file` and `env_lines` (`LEADLINE_URL=…` for `.env.local`), which
is where the plugin looks. At the usual address there is no `--url` and no
`env_file`.

## Env files

```
POST /api/discover/env
{ "projects": ["p_6da2e3d1c4", "/home/acme/code/acme"] }
```

Reads the env files of those projects, and of no other, for connection
URLs. A project is named by its `id` or its `path`, as the latest scan gave
them. The scan must be `done`. At most 200 projects.

- `projects` is required and must name at least one. There is no "all":

  ```json
  {
  "error": {
    "code": "bad_request",
    "message": "Name the projects whose env files to read.",
    "hint": "Send {\"projects\": [...]} with ids or paths from the scan."
  }
}
  ```

- Every name must be a project of the latest scan, or nothing is read:

  ```json
  {
  "error": {
    "code": "no_project",
    "message": "The latest scan has no project with that id or path: p_0000000000.",
    "hint": "Projects are named by the id or path the scan gave them."
  }
}
  ```

- A project whose env files were already read is left as it is, so asking
  again with a longer list reads only what is new, and ids stay.

The answer is the scan, as `GET /api/discover/scan` gives it from then on,
with what was found. After naming one project, `acme-admin`, the scan had:

```json
{
  "env_projects": [
    "p_a5f0f9c122"
  ],
  "env": {
    "files_read": 1,
    "urls": 2,
    "other_keys": 2
  },
  "secrets_until_ms": 1791656139690.8904
}
```

and that project:

```json
{
  "id": "p_a5f0f9c122",
  "name": "acme-admin",
  "env_files": [
    {
      "file": ".env",
      "read": true,
      "urls": 2,
      "other_keys": 2
    }
  ],
  "connection_ids": [
    "u_07da6ffaba",
    "u_db02e07f95"
  ]
}
```

After naming all 5 (4 of the 8 connections, and the one listener that
changed):

```json
{
  "env_projects": [
    "p_4f3c4a7cef",
    "p_6692dbcf87",
    "p_9f6a51a766",
    "p_a5f0f9c122",
    "p_a6cec1a720"
  ],
  "env": {
    "files_read": 6,
    "urls": 11,
    "other_keys": 11
  },
  "secrets_until_ms": 1791656139692.6282,
  "connections": [
    {
      "id": "u_db02e07f95",
      "kind": "postgres",
      "url": "postgres://acme:****@localhost:5432/acme",
      "host": "localhost:5432",
      "database": "acme",
      "user": "acme",
      "remote": false,
      "connectable": true,
      "capture_available": true,
      "suggested": true,
      "found_in": [
        {
          "project_id": "p_a5f0f9c122",
          "project": "acme-admin",
          "key": "DATABASE_URL",
          "file": ".env",
          "effective": true
        },
        {
          "project_id": "p_a6cec1a720",
          "project": "acme",
          "key": "DATABASE_URL",
          "file": ".env.local",
          "effective": true
        },
        {
          "project_id": "p_9f6a51a766",
          "project": "acme",
          "key": "DATABASE_URL",
          "file": ".env.local",
          "effective": true
        }
      ],
      "saved": false,
      "listening": {
        "port": 5432,
        "pid": 0,
        "what": "Postgres"
      }
    },
    {
      "id": "u_d8c0f16a2a",
      "kind": "postgres",
      "url": "postgresql://reader:****@db.acme.test:5432/analytics?sslmode=require",
      "host": "db.acme.test:5432",
      "database": "analytics",
      "user": "reader",
      "remote": true,
      "connectable": true,
      "capture_available": false,
      "suggested": false,
      "found_in": [
        {
          "project_id": "p_4f3c4a7cef",
          "project": "billing-service",
          "key": "ANALYTICS_URL",
          "file": ".env",
          "effective": true
        }
      ],
      "saved": false
    },
    {
      "id": "u_07da6ffaba",
      "kind": "mysql",
      "url": "mysql://admin:****@127.0.0.1:3306/legacy",
      "host": "127.0.0.1:3306",
      "database": "legacy",
      "user": "admin",
      "remote": false,
      "connectable": false,
      "reason": "not_supported",
      "note": "Leadline connects to Postgres and Redis. This one was found, and can't be connected yet.",
      "capture_available": false,
      "suggested": false,
      "found_in": [
        {
          "project_id": "p_a5f0f9c122",
          "project": "acme-admin",
          "key": "LEGACY_MYSQL_URL",
          "file": ".env",
          "effective": true
        }
      ],
      "saved": false
    },
    {
      "id": "u_09a44c59e0",
      "kind": "redis",
      "url": "redis://default:****@cache.acme.test:6379/1",
      "host": "cache.acme.test:6379",
      "database": "",
      "user": "",
      "remote": true,
      "connectable": false,
      "reason": "interpolated",
      "note": "This URL is filled in from other variables, which Leadline does not read. Add it by hand with the finished URL.",
      "capture_available": false,
      "suggested": false,
      "found_in": [
        {
          "project_id": "p_4f3c4a7cef",
          "project": "billing-service",
          "key": "CACHE_URL",
          "file": ".env",
          "effective": true
        }
      ],
      "saved": false
    }
  ],
  "listeners": [
    {
      "port": 5432,
      "address": "0.0.0.0",
      "scope": "all",
      "pid": 0,
      "process": "",
      "command": "",
      "cwd": "",
      "kind": "postgres",
      "what": "Postgres",
      "basis": "env-url"
    }
  ]
}
```

`env` adds up the files that were read: here 11 connection URLs were taken
from 6 files, and the 11 other variables in them were counted and not
kept. The 11 URLs are 8 connections: the same one in several places is
one entry.

Port 5432's listener had `basis` `port` before: a guess from the number.
An env file's URL points at it and says it is Postgres, so it is now
`env-url`.

### A connection

One entry per connection: the same server, database, user and password
found in two files, or in two projects, or spelled two ways (`localhost`
and `127.0.0.1`, with an ORM's `?schema=public` and without) is one entry
with several `found_in`.

When the spellings differ, the one shown, tried and saved is that of the
first `effective` place: the value the dev tools would use. A value that
another file outranks is nobody's working URL, and may lack what the
working one has. So reading a second project's env files can change an
entry's `url` and `host` (never its `id`): a plain URL in an outranked
`.env`, found first, gives way to the same database's URL with `?schema=`
in the file the other project runs on.

| field | |
| --- | --- |
| `id` | opaque, and only good for this scan |
| `kind` | `postgres`, `redis`, `mysql`, `mongodb` |
| `url` | with `****` for the password. A URL that can't be read as a URL at all (a password with an unescaped `/`, `?` or `#`) is shown as `postgres://****` |
| `host`, `database`, `user` | as in a saved connection |
| `remote` | on another machine, by the rule saved connections use (it never asks DNS: a name other than `localhost` is somewhere else). The saved connection's own `remote` is the authority |
| `connectable` | false with `reason` `not_supported` (MySQL and MongoDB: found, not connectable yet), `interpolated` or `bad_url`; `note` is the sentence |
| `capture_available` | capture can be on: it is on this machine |
| `suggested` | whether to tick it to begin with: connectable, on this machine, and not saved already |
| `found_in` | `project_id`, `project`, `key`, `file`, and `effective`: false when a file that outranks this one sets the same variable, so the dev tools would not use this value |
| `saved`, `saved_id` | it is already a saved connection (the same server, database and user) |
| `listening` | the listener the URL points at, when one is there |

They are sorted: connectable and on this machine first, then remote, then
what can't be connected.

## Saving

```
POST /api/discover/save
{ "connections": [
    { "id": "u_…", "capture": true },
    { "id": "u_…", "capture": true },
    { "id": "u_…", "capture": true },
    { "id": "u_…", "capture": false } ] }
```

Saves each through the integrations manager, exactly as `POST
/api/connections` would with the URL, one at a time in the order given:
afterwards they are in `/api/connections` and `/api/sources`. `name` is
optional beside `capture`. At most 50.

One thing is done to a found URL on its way to being saved, and the
password is untouched by it: a `jdbc:` prefix or `+driver` suffix comes
off the scheme. Everything else is saved as the env file has it, the
parameters an ORM adds for itself included (Prisma's `?schema=public`,
`connection_limit` and the like). So the saved connection's `url` has
them, and so does its `proxy.url`, which is the line to put back in that
project's `.env`: a project on a schema of its own keeps its `schema=`.

Postgres refuses those parameters, so Leadline's own connections leave
them out: in `test` here, and in everything the saved connection does. That
is the integrations API's rule, the same for a URL pasted by hand
(`internal/integrations/API.md`, "A URL from an ORM"), and discovery takes
nothing off for it.

**A connection saved here says it was found by a scan**: its `owner` is
`"kind": "scan"`, with the project the URL was found in, that project's
folder and branch, and the variable and env file by name. When the URL was
in several places it names the first `effective` one of `found_in`. Names
only: nothing of a value. The proxy opened for it says the same. One that
was already saved (`already` true) keeps the owner it had. The fields are
in the integrations API, under "Owners".

The answer is `200` whenever the body could be read; each id is in `saved`
or in `failed`. Here the first two were a local Postgres and Redis, the
third a remote Postgres asked for with capture on, the fourth MySQL:

```json
{
  "saved": [
    {
      "id": "u_db02e07f95",
      "already": false,
      "connection": {
        "id": "c_14a55becfb",
        "kind": "postgres",
        "name": "acme",
        "url": "postgres://acme:****@localhost:5432/acme",
        "host": "localhost:5432",
        "database": "acme",
        "user": "acme",
        "remote": false,
        "capture": true,
        "capture_offer": {
          "available": true,
          "how": "proxy",
          "note": "Leadline opens a port on this machine that forwards to the…"
        },
        "proxy": {
          "id": "p_66200561c9",
          "kind": "postgres",
          "listen": "127.0.0.1:15432",
          "port": 15432,
          "upstream": "localhost:5432",
          "label": "acme",
          "origins": [
            "connection"
          ],
          "connection_ids": [
            "c_14a55becfb"
          ],
          "state": "listening",
          "url": "postgres://acme:****@127.0.0.1:15432/acme?sslmode=disable",
          "owner": {
            "kind": "scan",
            "project": "acme-admin",
            "path": "/home/acme/code/acme-admin",
            "env_key": "DATABASE_URL",
            "env_file": ".env",
            "label": "found by a scan, DATABASE_URL in acme-admin"
          },
          "connections_open": 0,
          "connections_total": 0,
          "statements": 0,
          "failed_statements": 0,
          "unread_connections": 0,
          "last_activity_ms": 0,
          "window_statements": 0
        },
        "health": {
          "state": "ok",
          "version": "17.11 (Debian 17.11-1.pgdg13+2)",
          "version_short": "17.11",
          "latency_ms": 2.8,
          "checked_ms": 1791654339716
        },
        "created_ms": 1791654339708,
        "owner": {
          "kind": "scan",
          "project": "acme-admin",
          "path": "/home/acme/code/acme-admin",
          "env_key": "DATABASE_URL",
          "env_file": ".env",
          "label": "found by a scan, DATABASE_URL in acme-admin"
        },
        "env": []
      }
    }
  ],
  "failed": [
    {
      "id": "u_d8c0f16a2a",
      "error": {
        "code": "capture_remote",
        "message": "A remote database can't be captured. Leadline's proxy can read queries only when they are not encrypted, so it doesn't speak TLS; putting it in front of a database on another machine would send the app's queries and results over the network in the clear. Leadline still reads this database's own statistics.",
        "hint": "Add it with capture off to read its stats."
      }
    },
    {
      "id": "u_07da6ffaba",
      "error": {
        "code": "not_connectable",
        "message": "Leadline connects to Postgres and Redis. This one was found, and can't be connected yet."
      }
    }
  ]
}
```

(`saved` has the Redis one too, left out here. `connection` is the saved
connection exactly as `GET /api/connections/{id}` gives it. Its URL was in
three places, as `found_in` above shows; the `owner` names the first of
them, `DATABASE_URL` in `acme-admin`'s `.env`, a folder in no repository
and so with no `branch`.)

The scan then says so, and no longer suggests them:

```json
[
  {
    "id": "u_db02e07f95",
    "url": "postgres://acme:****@localhost:5432/acme",
    "suggested": false,
    "saved": true,
    "saved_id": "c_14a55becfb"
  },
  {
    "id": "u_f63091d837",
    "url": "redis://localhost:6379",
    "suggested": false,
    "saved": true,
    "saved_id": "c_a0372ef725"
  },
  {
    "id": "u_6d2fb4989b",
    "url": "postgresql://acme:****@localhost:5432/postgres?schema=public&connection_limit=5",
    "suggested": true,
    "saved": false
  }
]
```

One that is already saved is not added twice: its capture setting is set to
what was asked, and it comes back with `already` true (`connection` cut to
five of its members here):

```json
{
  "saved": [
    {
      "id": "u_db02e07f95",
      "already": true,
      "connection": {
        "id": "c_14a55becfb",
        "kind": "postgres",
        "name": "acme",
        "url": "postgres://acme:****@localhost:5432/acme",
        "capture": true
      }
    }
  ],
  "failed": []
}
```

An id from a scan that has been replaced:

```json
{
  "saved": [],
  "failed": [
    {
      "id": "u_f63091d837",
      "error": {
        "code": "not_found",
        "message": "The latest scan found no connection with that id.",
        "hint": "Ids are only good for the scan they came from."
      }
    }
  ]
}
```

A body that can't be read is the one `400`:

```json
{
  "error": {
    "code": "bad_request",
    "message": "The request body is not JSON Leadline can read."
  }
}
```

## Testing

```
POST /api/discover/test
{ "ids": ["u_…", "u_…", "u_…", "u_…", "u_…"], "remote": false }
```

Connects to each, asks the server who it is, disconnects, and saves
nothing: `POST /api/connections/test` for a URL the app does not have.
This is how onboarding can show "Postgres 17" before anything is saved. A
remote server is only tried with `"remote": true`; without it, its id
answers with the error `remote`. At most 20 ids.

The same four as above, and a fifth that Prisma wrote with
`?schema=public&connection_limit=5`: it answers, because Leadline connects
without those two, and its `url` is the URL as the env file has it
(`result` is what `POST /api/connections/test` answers, shown here without
its `capture` and `pg_stat_statements` members):

```json
{
  "results": [
    {
      "id": "u_db02e07f95",
      "result": {
        "ok": true,
        "kind": "postgres",
        "url": "postgres://acme:****@localhost:5432/acme",
        "host": "localhost:5432",
        "database": "acme",
        "user": "acme",
        "remote": false,
        "version": "17.11 (Debian 17.11-1.pgdg13+2)",
        "version_short": "17.11",
        "latency_ms": 5.4,
        "tls": false
      }
    },
    {
      "id": "u_f63091d837",
      "result": {
        "ok": true,
        "kind": "redis",
        "url": "redis://localhost:6379",
        "host": "localhost:6379",
        "database": "0",
        "remote": false,
        "version": "7.4.11",
        "version_short": "7.4.11",
        "latency_ms": 2.5,
        "tls": false
      }
    },
    {
      "id": "u_d8c0f16a2a",
      "error": {
        "code": "remote",
        "message": "This server is on another machine, and Leadline connects to those only when asked.",
        "hint": "Ask again with \"remote\": true."
      }
    },
    {
      "id": "u_07da6ffaba",
      "error": {
        "code": "not_connectable",
        "message": "Leadline connects to Postgres and Redis. This one was found, and can't be connected yet."
      }
    },
    {
      "id": "u_6d2fb4989b",
      "result": {
        "ok": true,
        "kind": "postgres",
        "url": "postgresql://acme:****@localhost:5432/postgres?schema=public&connection_limit=5",
        "host": "localhost:5432",
        "database": "postgres",
        "user": "acme",
        "remote": false,
        "version": "17.11 (Debian 17.11-1.pgdg13+2)",
        "version_short": "17.11",
        "latency_ms": 8.5,
        "tls": false
      }
    }
  ]
}
```

## Shipyard

```
GET /api/discover/shipyard
GET /api/discover/shipyard?fresh=1
```

Looks for the `berth` command on the daemon's `PATH` and then in
`~/.local/bin`, `~/bin`, `/usr/local/bin`, `/opt/homebrew/bin` and inside
`Shipyard.app` (a daemon started by a desktop app has a thin `PATH`; in the
example it was found in `~/.local/bin`, which was not on it), and runs
`berth boxes --json`. It is given 4 seconds, and the answer is kept for 10
(`fresh=1` asks again).

`LEADLINE_BERTH` in the daemon's environment says where the command is
instead, for a machine that keeps it somewhere else. `LEADLINE_BERTH=off`
says not to look: Shipyard is then not installed as far as that daemon
goes, and it runs no `berth` at all.

```json
{
  "installed": true,
  "available": true,
  "cli": "/home/acme/.local/bin/berth",
  "boxes": [
    {
      "name": "hetzner-1",
      "state": "online",
      "latency_ms": 86,
      "since_ms": 1791642067000,
      "leadline": {
        "state": "unsupported",
        "installed": false,
        "reachable": false,
        "note": "Leadline can't run on a box yet."
      }
    },
    {
      "name": "build-box",
      "state": "offline",
      "error": "dial tcp 100.64.0.12:1376: i/o timeout",
      "since_ms": 1791567000000,
      "leadline": {
        "state": "unsupported",
        "installed": false,
        "reachable": false,
        "note": "Leadline can't run on a box yet."
      }
    }
  ],
  "checked_ms": 1791654339729.6738
}
```

`state` is Shipyard's own: `online`, `offline`, `connecting`, `untrusted`.
`since_ms` is since when it has been in that state. `local` is true for a
box that is this computer. Only those fields are taken: a box's address and
fingerprint are not passed on.

**`leadline` is a listing only today.** Leadline cannot run on a box or be
reached there yet, so every box says `unsupported`. The fields are there so
that can change without the shape changing: `state` will become `missing`,
`installed` or `reachable`, and `url` will be where its API is reached from
this machine.

Always `200`. When Shipyard is not there, or did not answer, `available` is
false and the app shows "Shipyard not found". Not installed:

```json
{
  "installed": false,
  "available": false,
  "boxes": [],
  "checked_ms": 1791654447920.7102,
  "problem": {
    "code": "shipyard_not_found",
    "message": "Shipyard is not installed on this machine."
  }
}
```

Installed, and its command failed (what it printed is not passed on); the
same with `"message": "Shipyard did not answer in time."` when it hangs:

```json
{
  "installed": true,
  "available": false,
  "cli": "/home/acme/.local/bin/berth",
  "boxes": [],
  "checked_ms": 1791654447929.7974,
  "problem": {
    "code": "shipyard_failed",
    "message": "Shipyard's command line could not list that."
  }
}
```

```
GET /api/discover/shipyard/boxes/hetzner-1
```

Runs `berth locations hetzner-1 --json` (6 seconds at most, kept for 10):
the box's projects and their worktrees. The name must be one of the boxes
in the listing, which is asked for first if it is not at hand.

```json
{
  "box": "hetzner-1",
  "projects": [
    {
      "name": "acme",
      "path": "/home/dev/acme",
      "repo": true,
      "slug": "acme/acme",
      "default_branch": "main",
      "worktrees": [
        {
          "name": "main",
          "path": "/home/dev/acme",
          "branch": "main",
          "main": true,
          "port": 3000
        },
        {
          "name": "fix-orders-n1",
          "path": "/home/dev/.berth/worktrees/acme/fix-orders-n1",
          "branch": "fix-orders-n1",
          "port": 3004
        },
        {
          "name": "checkout-retry",
          "path": "/home/dev/.berth/worktrees/acme/checkout-retry",
          "branch": "checkout-retry",
          "title": "Retry failed checkouts",
          "port": 3008
        }
      ]
    },
    {
      "name": "acme-admin",
      "path": "/home/dev/acme-admin",
      "repo": true,
      "slug": "acme/acme-admin",
      "default_branch": "main",
      "worktrees": [
        {
          "name": "main",
          "path": "/home/dev/acme-admin",
          "branch": "main",
          "main": true,
          "port": 3012
        }
      ]
    },
    {
      "name": "billing-service",
      "path": "/home/dev/billing-service",
      "repo": true,
      "slug": "acme/billing-service",
      "default_branch": "main",
      "worktrees": [
        {
          "name": "main",
          "path": "/home/dev/billing-service",
          "branch": "main",
          "main": true
        }
      ]
    }
  ],
  "checked_ms": 1791654339733.1208
}
```

Only those fields are taken from Shipyard's answer, and one more: a project
whose own config adds agents has them as `"agents": [{"id": "reviewer",
"name": "Acme reviewer"}]`, by id and name (absent when it adds none). A
repository's remote URL, which can carry a token, is not taken, nor is an
agent's command.

```json
{
  "error": {
    "code": "no_box",
    "message": "Shipyard has no box of that name."
  }
}
```

These two routes run those two `berth` commands and no other, and neither
changes anything. They are run when one of the routes is asked for and at no
other time. One thing to know: `berth boxes` starts Shipyard's own laptop
agent if it is installed and not running, as any `berth` command does.

Leadline runs two more `berth` commands, from the routes that send a problem
to Shipyard: `berth agents BOX --json`, which reads, and `berth task new`,
which starts a task and is the only command Leadline runs that changes
anything in Shipyard. `internal/brief/API.md` describes both, and when.
