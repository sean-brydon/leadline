# The integrations API

What the app calls to connect databases and Redis, see their statistics, and
know what is feeding Leadline. The code is this package (`internal/integrations`);
the routes are in `internal/api/integrations.go`. Change them together with
this file: the app's screens are written from it.

Every example below is a real response from a daemon with a Postgres 17 and a
Redis 7 in Docker behind it, with made-up "acme" data. Two things were done to
them for length, and nothing else: a long `note` string is cut with `…`
(each is given in full once, under [Capture](#capture)), and a few long lists
are shortened where the text says so.

A few fields are newer than the examples and are missing from them. Each is
described where it belongs:

| field | on | what |
| --- | --- | --- |
| `since_ms` | `/api/sources` | where the windowed counts start |
| `through` | each of `services` | which proxy or connection its database calls came through |
| `window_statements` | a proxy | statements since `since_ms` |
| `window_recorded` | a connection's `monitor` | Redis commands recorded since `since_ms` |
| `version_short` | `health`, and a test's answer | the version's number alone: `17.11` of `17.11 (Debian 17.11-1.pgdg13+2)` |
| `owner` | a connection, a proxy, and a service that belongs to a worktree | who made it and for what; see [Owners](#owners). Its `env_key` and `env_file`, and the kind `"scan"`, are newer still |
| `env` | a connection | the variables its URL was found in, for one attached from a worktree; `[]` for any other |
| `attached` | `/api/sources` | every source `leadline attach` registered; see [Attached sources](#attached-sources-worktrees) |

## Ground rules

- **Base URL** is the daemon's address, `http://127.0.0.1:4320` unless it was
  started with `-addr`.
- **Localhost only.** Every route is behind `guard`: a request must name a
  loopback host, and only loopback and `tauri://` origins get CORS headers.
  Anything else is a `403`.
- **Methods.** `guard` allows `GET`, `POST`, `PATCH` and `DELETE` across
  origins (the app's dev server and the desktop webview are both another
  origin than the daemon). Edit and delete each also have a `POST` form,
  from when only `GET` and `POST` were allowed; either works.
- **Bodies are JSON**, at most 64 KB.
- **Times** are unix milliseconds, as floats or integers, in fields ending
  `_ms`. Durations are milliseconds too. Sizes are bytes.
- **No password ever comes back.** A URL in a response has `****` where its
  password is, in the user part and in a `password=` parameter alike. The
  connections file on disk (`connections.json` in the data directory, mode
  `0600`) is the only place the password is.
- **Sending a masked URL back means "the password you have".** When editing
  or testing a saved connection, a URL whose password is `****` is filled
  with the saved one. So an edit form can be prefilled with `url` as it was
  shown and posted back unchanged.
- **An ORM's own parameters are not sent to Postgres.** Prisma writes
  `?schema=public` into every `DATABASE_URL` it sets up, and its docs add
  more; Postgres takes each as a setting it does not have and refuses the
  connection. Leadline connects without them, and keeps them everywhere
  else. See [A URL from an ORM](#a-url-from-an-orm).
- **Lists are never `null`.** An empty list is `[]`.

### Errors

A failed request answers with a status that fits and this body:

```json
{
  "error": {
    "code": "unreachable",
    "message": "Nothing is listening at localhost:6399.",
    "hint": "Is Redis running? Check the port: a database in Docker is reached on the port it publishes."
  }
}
```

`message` is a sentence for the person at the screen. `hint`, when there is
one, is what to try. `code` is for the app to branch on:

| code | status | when |
| --- | --- | --- |
| `bad_request` | 400 | the body is not JSON, or a field is out of range |
| `bad_url` | 400 | the URL can't be read, or is for something Leadline doesn't connect to |
| `capture_remote` | 400 | capture was asked for on a server that is not on this machine |
| `too_many` | 400 | there are already 50 connections |
| `not_found` | 404 | no connection or proxy has that id, or no source of that name is attached |
| `port_in_use`, `no_port` | 409 | the proxy's port can't be had |
| `unreachable` | 502 | nothing is listening there, or there is no route |
| `no_host` | 502 | the host name does not resolve |
| `timeout` | 502 | no answer in time, or a query ran past Leadline's limit |
| `auth_failed` | 502 | wrong user or password, or a password is needed |
| `no_database` | 502 | Postgres has no database of that name |
| `tls` | 502 | the URL asks for TLS and the server has none, or its certificate is not trusted |
| `too_many_connections`, `starting`, `permission`, `protected`, `closed`, `not_redis` | 502 | as the message says |
| `postgres_error`, `redis_error`, `connection_failed` | 502 | another error from the server or the network, in its words |
| `shutting_down` | 503 | the daemon is stopping |
| `not_saved`, `failed` | 500 | Leadline's own fault |

The 502s are the database's or Redis's doing, not the daemon's: the daemon
answered, the server behind it did not. Two more real ones:

```json
{
  "error": {
    "code": "bad_url",
    "message": "Leadline connects to Postgres (postgres://) and Redis (redis://). This URL is for neither.",
    "hint": "It should look like postgres://user:password@localhost:5432/dbname or redis://localhost:6379."
  }
}
```

```json
{
  "error": {
    "code": "not_found",
    "message": "There is no connection with that id."
  }
}
```

## Sources: what is feeding Leadline

```
GET /api/sources
GET /api/sources?since_ms=1791644000000
```

`since_ms` is optional: it is where the windowed counts of proxies and live
captures start (`window_statements`, `window_recorded`). Without it they
cover the last 15 minutes. The answer says which in its own `since_ms`.

One call for the onboarding screen: who is sending telemetry, which proxies
are carrying queries, and whether each saved connection answers. It is made
to be polled. With nothing new in the store it costs three row counts (0.3 ms
on a store of three million spans); when data has arrived it re-aggregates,
at most twice a second (9 ms on that store). New data shows on the next call
after it arrives, so polling once a second, or refetching on the
`/api/events` stream's `ingest` event as the rest of the app does, shows a
first span within a second or two.

A daemon nothing has been connected to:

```json
{
  "now_ms": 1791644612639.004,
  "otlp": {
    "endpoints": [
      "http://127.0.0.1:4331"
    ]
  },
  "services": [],
  "proxies": [],
  "connections": []
}
```

After an app exported OTLP, another ran queries through a proxy, and a Redis
was being captured:

```json
{
  "now_ms": 1791644887756.474,
  "otlp": {
    "endpoints": [
      "http://127.0.0.1:4331"
    ]
  },
  "services": [
    {
      "name": "acme-api",
      "spans": 1,
      "logs": 1,
      "metric_points": 0,
      "last_seen_ms": 1791644717832.193,
      "via": [
        "otlp"
      ]
    },
    {
      "name": "acme-cache",
      "spans": 1010,
      "logs": 0,
      "metric_points": 0,
      "last_seen_ms": 1791644884657.877,
      "via": [
        "redis-monitor"
      ]
    },
    {
      "name": "acme-run",
      "spans": 1,
      "logs": 0,
      "metric_points": 0,
      "last_seen_ms": 1791644741164.703,
      "via": [
        "postgres-proxy"
      ]
    },
    {
      "name": "acme-web",
      "spans": 39,
      "logs": 0,
      "metric_points": 0,
      "last_seen_ms": 1791644884524.606,
      "via": [
        "postgres-proxy"
      ]
    }
  ],
  "proxies": [
    {
      "id": "p_55f949c3e2",
      "kind": "postgres",
      "listen": "127.0.0.1:15432",
      "port": 15432,
      "upstream": "127.0.0.1:54341",
      "label": "Acme dev",
      "origins": [
        "connection"
      ],
      "connection_ids": [
        "c_a3228af549"
      ],
      "state": "listening",
      "connections_open": 0,
      "connections_total": 1,
      "statements": 4,
      "failed_statements": 0,
      "unread_connections": 0,
      "last_activity_ms": 1791644884631.648
    }
  ],
  "connections": [
    {
      "id": "c_a3228af549",
      "kind": "postgres",
      "name": "Acme dev",
      "url": "postgres://acme:****@127.0.0.1:54341/acme_dev",
      "host": "127.0.0.1:54341",
      "database": "acme_dev",
      "user": "acme",
      "remote": false,
      "capture": true,
      "capture_offer": {
        "available": true,
        "how": "proxy",
        "note": "Leadline opens a port on this machine that forwards to th…"
      },
      "proxy": {
        "id": "p_55f949c3e2",
        "kind": "postgres",
        "listen": "127.0.0.1:15432",
        "port": 15432,
        "upstream": "127.0.0.1:54341",
        "label": "Acme dev",
        "origins": [
          "connection"
        ],
        "connection_ids": [
          "c_a3228af549"
        ],
        "state": "listening",
        "url": "postgres://acme:****@127.0.0.1:15432/acme_dev?sslmode=disable",
        "connections_open": 0,
        "connections_total": 1,
        "statements": 4,
        "failed_statements": 0,
        "unread_connections": 0,
        "last_activity_ms": 1791644884631.648
      },
      "health": {
        "state": "ok",
        "version": "17.10",
        "checked_ms": 1791644887714
      },
      "created_ms": 1791644628675
    },
    {
      "id": "c_c99429c002",
      "kind": "redis",
      "name": "acme-cache",
      "url": "redis://:****@127.0.0.1:56341/0",
      "host": "127.0.0.1:56341",
      "database": "0",
      "user": "",
      "remote": false,
      "capture": true,
      "capture_offer": {
        "available": true,
        "how": "monitor",
        "note": "Live capture uses Redis's MONITOR, which makes the server…"
      },
      "monitor": {
        "state": "streaming",
        "seen": 6,
        "recorded": 6,
        "dropped": 0,
        "max_per_sec": 1000,
        "last_activity_ms": 1791644887725.2322,
        "note": "Live capture uses Redis's MONITOR, which makes the server…"
      },
      "health": {
        "state": "ok",
        "version": "7.4.11",
        "checked_ms": 1791644887727
      },
      "created_ms": 1791644628695
    }
  ]
}
```

- `otlp.endpoints`: where an app should send OpenTelemetry (OTLP/HTTP:
  `POST /v1/traces`, `/v1/logs`, `/v1/metrics` under each). The second entry,
  when present, is the standard port 4318.
- `services`: every service Leadline holds data for, by name, at most 50
  (the most recently heard from), and every
  [attached source](#attached-sources-worktrees) besides. An attached
  worktree is listed from the moment it is attached: until its first span or
  log arrives its counts and `last_seen_ms` are 0 and `via` is `[]`.
  - `owner` is there when the service belongs to an attached worktree: the
    source itself (`acme/fix-orders`) or a command run in it
    (`acme/fix-orders:worker`). See [Owners](#owners). A service nobody
    attached has no `owner`.
  - `spans`, `logs`, `metric_points` count what is **stored now**. They fall
    when old data is dropped by retention; they are not "since the daemon
    started".
  - `last_seen_ms` is the time of its newest span, log or metric point.
  - `via` says how its data arrives: `"otlp"` (the app exports it),
    `"postgres-proxy"` or `"redis-monitor"` (Leadline captured it). It can
    be more than one: an app that exports traces and also connects through
    the proxy under the same name is both. A service with only
    `"postgres-proxy"` has **not** set up OpenTelemetry, which is the thing
    an onboarding screen wants to say.
  - `through` names the captures behind a `"postgres-proxy"` or
    `"redis-monitor"`: `[{"via": "postgres-proxy", "proxy_id": "p_…",
    "connection_ids": ["c_…"]}]`. `proxy_id` is an entry of `proxies`;
    `connection_ids` are the saved connections that proxy is for (none, for
    a proxy `leadline run` opened), or the Redis connection being watched.
    It is what ties an app to its database. It is known since the daemon
    started and for captures still running: `[]` after a restart until the
    capture records again, and a run's proxy leaves it when the run ends.
  - A statement the app reports itself **and** a proxy records is stored
    once, under the app's own service name, whatever name the proxy would
    have used. So an app seen both ways is one service with both in `via`,
    not two services.
- `proxies`: every Postgres proxy; see [Proxies](#proxies). `statements` is
  since the proxy opened and starts again at 0 when the daemon restarts;
  `window_statements` is since `since_ms`, to the minute.
- `connections`: every saved connection, as in [Connections](#connections),
  with its `health`.
- `attached`: every attached source, as in
  [Attached sources](#attached-sources-worktrees); `[]` when there is none.

Health is checked when someone looks, not on a timer: a call to
`/api/sources` or `/api/connections` starts a check of any connection whose
last one is older than 10 seconds (30 for a remote server) and answers with
what it has. So `health` is at most one poll behind, and a daemon nobody is
looking at leaves the databases alone.

## Connections

A connection is a saved Postgres or Redis.

```
GET    /api/connections              list
POST   /api/connections              add
POST   /api/connections/test         try a URL without saving it
GET    /api/connections/{id}         one
POST   /api/connections/{id}         edit        (also PATCH)
POST   /api/connections/{id}/delete  delete      (also DELETE /api/connections/{id})
POST   /api/connections/{id}/test    try a saved one
GET    /api/connections/{id}/stats   what the server knows about itself
```

### The connection object

```json
{
  "id": "c_a3228af549",
  "kind": "postgres",
  "name": "Acme dev",
  "url": "postgres://acme:****@127.0.0.1:54341/acme_dev",
  "host": "127.0.0.1:54341",
  "database": "acme_dev",
  "user": "acme",
  "remote": false,
  "capture": true,
  "capture_offer": {
    "available": true,
    "how": "proxy",
    "note": "Leadline opens a port on this machine that forwards to th…"
  },
  "proxy": {
    "id": "p_55f949c3e2",
    "kind": "postgres",
    "listen": "127.0.0.1:15432",
    "port": 15432,
    "upstream": "127.0.0.1:54341",
    "label": "Acme dev",
    "origins": [
      "connection"
    ],
    "connection_ids": [
      "c_a3228af549"
    ],
    "state": "listening",
    "url": "postgres://acme:****@127.0.0.1:15432/acme_dev?sslmode=disable",
    "connections_open": 0,
    "connections_total": 1,
    "statements": 4,
    "failed_statements": 0,
    "unread_connections": 0,
    "last_activity_ms": 1791644884631.648
  },
  "health": {
    "state": "ok",
    "version": "17.10",
    "checked_ms": 1791644887714
  },
  "created_ms": 1791644628675
}
```

| field | |
| --- | --- |
| `id` | `c_` and ten hex digits. Stable for the life of the connection. |
| `kind` | `"postgres"` or `"redis"`, from the URL's scheme (`postgres://`, `postgresql://`, `redis://`, `rediss://`). |
| `name` | What the person called it. Left out when adding, it is the database's name for Postgres and `redis` (or `redis-<port>`) for Redis. For a captured Redis it is also the service name its commands are recorded under. |
| `url` | The URL as it was given, password masked. An ORM's own parameters are still in it; see [A URL from an ORM](#a-url-from-an-orm). |
| `host`, `database`, `user` | Read out of the URL, for display. `host` is `host:port`, or a socket path. For Redis `database` is its number. |
| `remote` | `true` when the server is not on this machine. Show it: Leadline reads a remote server's statistics and does nothing else to it. "This machine" is `localhost`, a loopback address, a unix socket, or one of the machine's own addresses. A host name other than `localhost` counts as remote (DNS is never asked), which includes container names such as `db.orb.local`. |
| `capture` | Whether its traffic is being recorded. Off unless asked for. |
| `capture_offer` | Whether capture can be turned on, how (`"proxy"` or `"monitor"`), and a `note` to show beside the switch: what it does and costs, or why it is not offered. |
| `proxy` | Present for a Postgres with `capture` on. See below. |
| `monitor` | Present for a Redis with `capture` on. See below. |
| `health` | `state` is `"unknown"` before the first check, then `"ok"` or `"error"`. With `"error"` come `code`, `message` and `hint` as in [Errors](#errors). `version` is the server's. `latency_ms` is how long the last check took (the first includes connecting) and may be absent. `checked_ms` is when. |
| `created_ms` | When it was added. |
| `owner` | Who made it: `{"kind": "user", "label": "added by hand"}` for one a person added, the worktree it was attached for, or the scan that found it and where. See [Owners](#owners). |
| `env` | The names of the variables its URL was found in (`["DATABASE_URL", "DIRECT_URL"]`), for a connection attached from a worktree's environment. `[]` for any other. Names only, never a value. |

A connection a worktree owns can be edited and deleted like any other: a
rename or a capture switched off stays that way when the worktree is
attached again, and one deleted by hand comes back with the next attach
while the worktree's environment still names it.

Every connection can be read for stats. `capture` is the one choice per
connection, and what it means depends on the kind.

### A URL from an ORM

A Postgres URL copied out of a project's `.env` often carries parameters
that are the ORM's and not Postgres's. Prisma's are the common ones:

```
postgresql://acme:…@localhost:54356/acme?schema=public&connection_limit=5
```

A Postgres driver sends a parameter it does not know to the server as a
setting, and the server ends the connection:
`Unrecognized configuration parameter "schema"`. So wherever Leadline
itself connects (a test, the health check, the stats, with a URL typed in,
edited, attached from a worktree or found by a scan alike) these are left
out of what it connects with:

`schema`, `connection_limit`, `pool_timeout`, `pgbouncer`,
`socket_timeout`, `statement_cache_size`, `sslaccept`, `sslidentity`

Only those, by name, whatever their case. Every other byte of the URL is
used as written, the password's among them; a parameter that is nobody's
(`?acme_setting=1`) is still sent, and Postgres's refusal of it is what the
test answers. A Redis URL is never changed. The list is one table in the
code (`internal/integrations/connurl`).

They are kept everywhere else, because the URL is the person's and their
app still needs them:

- `url`, in a connection and in a test's answer, is the URL as given
  (password masked), so it reads as what was pasted and can be posted back
  unchanged in an edit.
- The connections file keeps the URL as given.
- **`proxy.url` keeps them.** It is the line for the app's `.env`, and
  Prisma reads its schema and pool size from there: a line without
  `schema=` would point the app at another schema. So the proxy's URL is
  the person's own with the host and port changed and `sslmode=disable`,
  and nothing taken out. (Its parameters come back in alphabetical order.)

That URL, tested (`pg_stat_statements` left out of the answer here):

```json
{
  "ok": true,
  "kind": "postgres",
  "url": "postgresql://acme:****@localhost:54356/acme?schema=public&connection_limit=5",
  "host": "localhost:54356",
  "database": "acme",
  "user": "acme",
  "remote": false,
  "version": "17.10",
  "version_short": "17.10",
  "latency_ms": 6.8,
  "tls": false,
  "capture": {
    "available": true,
    "how": "proxy",
    "note": "Leadline opens a port on this machine that forwards to th…"
  }
}
```

And added with `"capture": true` (five of the connection's members, and
its proxy's `url`):

```json
{
  "id": "c_6612897ccd",
  "kind": "postgres",
  "name": "acme",
  "url": "postgresql://acme:****@localhost:54356/acme?schema=public&connection_limit=5",
  "capture": true,
  "proxy": {
    "url": "postgresql://acme:****@127.0.0.1:15432/acme?connection_limit=5&schema=public&sslmode=disable"
  },
  "health": {
    "state": "ok",
    "version": "17.10",
    "version_short": "17.10",
    "latency_ms": 5.1,
    "checked_ms": 1791656839913
  }
}
```

A connection **saved from a scan** (`POST /api/discover/save`) is the
same: the URL is handed to this API as the env file has it, so its `url`
and its `proxy.url` keep the ORM's parameters and its own connections go
without them. See the discovery API's [Saving](../discover/API.md#saving).

### Capture

**Postgres: a proxy.** `capture: true` opens a port on this machine that
forwards to the database and records every statement. `proxy.url` is the
person's own URL pointed at that port, with `sslmode=disable`, and is what
goes in the app's `.env`. Its password is masked like every other URL's: the
person keeps their own password and changes the host and port, or, straight
after adding, the app can put the password the person just typed in place of
`****` so the line can be copied whole. `proxy.port` is remembered in the
connections file, so it is the same after the daemon restarts, and after
capture is turned off and on again. Leadline picks it from 15432 upwards;
`proxy_port` in an add or edit picks another (1024 to 65535), and
`"proxy_port": 0` in an edit has Leadline pick again. Two connections to one
server share one proxy.

`proxy.state` is `"listening"`, or `"error"` with `proxy.error` saying why
(the port was taken when the daemon started, most likely; it is tried again
every few seconds while someone is looking). The counts are the same as in
[Proxies](#proxies).

The note for the switch:

> Leadline opens a port on this machine that forwards to the database and records every statement on the way. Point the app at the proxy's URL instead of the database's and its queries appear with no other change.

**A remote Postgres can't be captured**, and asking is a `400`:

```json
{
  "error": {
    "code": "capture_remote",
    "message": "A remote database can't be captured. Leadline's proxy can read queries only when they are not encrypted, so it doesn't speak TLS; putting it in front of a database on another machine would send the app's queries and results over the network in the clear. Leadline still reads this database's own statistics.",
    "hint": "Add it with capture off to read its stats."
  }
}
```

**Redis: MONITOR.** `capture: true` opens a second connection and sends
`MONITOR`. Each command any client runs becomes a span with `db_system`
`"redis"`, a statement such as `GET user:42` and a fingerprint such as
`GET user:?`, and shows in `/api/queries` with everything else. Only for a
Redis on this machine, and off unless asked for. The note says what it costs
and what the numbers mean, and should be shown wherever the switch is:

> Live capture uses Redis's MONITOR, which makes the server send Leadline every command it runs. That can cost a busy server up to half its throughput, so it is only offered for a Redis on this machine, and is off until you turn it on. MONITOR reports when a command arrived, not how long it took: captured Redis commands show how often and when, and their durations are always 0. For timings see this connection's stats (commands and slow log). At most 1000 commands a second are recorded; the rest are counted as dropped.

The one thing to make plain in the UI: **a captured Redis command has no
duration.** Its `avg_ms` in `/api/queries` is 0 because MONITOR does not say
how long anything took, not because it was instant. Call counts and times of
arrival are real. For timings use the stats below (`commands`, `slow_log`).

`monitor` on a captured Redis:

```json
{
  "state": "streaming",
  "seen": 6,
  "recorded": 6,
  "dropped": 0,
  "max_per_sec": 1000,
  "last_activity_ms": 1791644887725.2322,
  "note": "Live capture uses Redis's MONITOR, which makes the server…"
}
```

`state` is `"connecting"`, `"streaming"`, or `"error"` with `error` saying
why; it reconnects by itself when Redis comes back. `seen` is every command
Redis reported, `recorded` those kept, `dropped` those over the cap of
`max_per_sec` in one second. Leadline's own commands (it reads stats on
another connection) and clients logging in (`AUTH`, `HELLO`) are not counted
at all. Arguments are cut at 64 bytes and credentials are replaced by `?`.

### List

```
GET /api/connections
```

```json
{
  "connections": [
    {
      "id": "c_a3228af549",
      "kind": "postgres",
      "name": "Acme dev",
      "url": "postgres://acme:****@127.0.0.1:54341/acme_dev",
      "host": "127.0.0.1:54341",
      "database": "acme_dev",
      "user": "acme",
      "remote": false,
      "capture": true,
      "capture_offer": {
        "available": true,
        "how": "proxy",
        "note": "Leadline opens a port on this machine that forwards to th…"
      },
      "proxy": {
        "id": "p_55f949c3e2",
        "kind": "postgres",
        "listen": "127.0.0.1:15432",
        "port": 15432,
        "upstream": "127.0.0.1:54341",
        "label": "Acme dev",
        "origins": [
          "connection"
        ],
        "connection_ids": [
          "c_a3228af549"
        ],
        "state": "listening",
        "url": "postgres://acme:****@127.0.0.1:15432/acme_dev?sslmode=disable",
        "connections_open": 0,
        "connections_total": 1,
        "statements": 4,
        "failed_statements": 0,
        "unread_connections": 0,
        "last_activity_ms": 1791644884631.648
      },
      "health": {
        "state": "ok",
        "version": "17.10",
        "checked_ms": 1791644887714
      },
      "created_ms": 1791644628675
    },
    {
      "id": "c_c99429c002",
      "kind": "redis",
      "name": "acme-cache",
      "url": "redis://:****@127.0.0.1:56341/0",
      "host": "127.0.0.1:56341",
      "database": "0",
      "user": "",
      "remote": false,
      "capture": true,
      "capture_offer": {
        "available": true,
        "how": "monitor",
        "note": "Live capture uses Redis's MONITOR, which makes the server…"
      },
      "monitor": {
        "state": "streaming",
        "seen": 6,
        "recorded": 6,
        "dropped": 0,
        "max_per_sec": 1000,
        "last_activity_ms": 1791644887725.2322,
        "note": "Live capture uses Redis's MONITOR, which makes the server…"
      },
      "health": {
        "state": "ok",
        "version": "7.4.11",
        "checked_ms": 1791644887727
      },
      "created_ms": 1791644628695
    }
  ]
}
```

In the order they were added.

### Add

```
POST /api/connections
{"url": "postgres://acme:…@127.0.0.1:54341/acme_dev", "name": "Acme dev", "capture": true}
```

| field | |
| --- | --- |
| `url` | Required. |
| `name` | Optional. |
| `capture` | Optional, default `false`. |
| `proxy_port` | Optional, Postgres only. |
| `kind` | Optional. When given it must agree with the URL. |

Answers with the connection. Adding does not insist that the server is
reachable: a database that is down today is still worth having in the list.
But the daemon waits up to 2.5 seconds for a first health check, so `health`
in the answer is nearly always `"ok"` or `"error"` already; test first
(below) to tell the person before they save.

```json
{
  "id": "c_a3228af549",
  "kind": "postgres",
  "name": "Acme dev",
  "url": "postgres://acme:****@127.0.0.1:54341/acme_dev",
  "host": "127.0.0.1:54341",
  "database": "acme_dev",
  "user": "acme",
  "remote": false,
  "capture": true,
  "capture_offer": {
    "available": true,
    "how": "proxy",
    "note": "Leadline opens a port on this machine that forwards to th…"
  },
  "proxy": {
    "id": "p_5530864191",
    "kind": "postgres",
    "listen": "127.0.0.1:15432",
    "port": 15432,
    "upstream": "127.0.0.1:54341",
    "label": "Acme dev",
    "origins": [
      "connection"
    ],
    "connection_ids": [
      "c_a3228af549"
    ],
    "state": "listening",
    "url": "postgres://acme:****@127.0.0.1:15432/acme_dev?sslmode=disable",
    "connections_open": 0,
    "connections_total": 0,
    "statements": 0,
    "failed_statements": 0,
    "unread_connections": 0,
    "last_activity_ms": 0
  },
  "health": {
    "state": "ok",
    "version": "17.10",
    "latency_ms": 4.9,
    "checked_ms": 1791644628687
  },
  "created_ms": 1791644628675
}
```

A Redis, capture not asked for:

```json
{
  "id": "c_c99429c002",
  "kind": "redis",
  "name": "acme-cache",
  "url": "redis://:****@127.0.0.1:56341/0",
  "host": "127.0.0.1:56341",
  "database": "0",
  "user": "",
  "remote": false,
  "capture": false,
  "capture_offer": {
    "available": true,
    "how": "monitor",
    "note": "Live capture uses Redis's MONITOR, which makes the server…"
  },
  "health": {
    "state": "ok",
    "version": "7.4.11",
    "latency_ms": 1.8,
    "checked_ms": 1791644628701
  },
  "created_ms": 1791644628695
}
```

### Edit

```
POST /api/connections/{id}
{"name": "Acme (dev)", "proxy_port": 15440}
```

Any of `name`, `url`, `capture`, `proxy_port`; what is left out stays as it
is. `kind` can't change. Answers with the connection as it now is:

```json
{
  "id": "c_a3228af549",
  "kind": "postgres",
  "name": "Acme (dev)",
  "url": "postgres://acme:****@127.0.0.1:54341/acme_dev",
  "host": "127.0.0.1:54341",
  "database": "acme_dev",
  "user": "acme",
  "remote": false,
  "capture": true,
  "capture_offer": {
    "available": true,
    "how": "proxy",
    "note": "Leadline opens a port on this machine that forwards to th…"
  },
  "proxy": {
    "id": "p_13b0623788",
    "kind": "postgres",
    "listen": "127.0.0.1:15440",
    "port": 15440,
    "upstream": "127.0.0.1:54341",
    "label": "Acme (dev)",
    "origins": [
      "connection"
    ],
    "connection_ids": [
      "c_a3228af549"
    ],
    "state": "listening",
    "url": "postgres://acme:****@127.0.0.1:15440/acme_dev?sslmode=disable",
    "connections_open": 0,
    "connections_total": 0,
    "statements": 0,
    "failed_statements": 0,
    "unread_connections": 0,
    "last_activity_ms": 0
  },
  "health": {
    "state": "unknown"
  },
  "created_ms": 1791644628675
}
```

A proxy carries the app's open connections, so it is only restarted when it
has to be (its port or the database's address changed, capture went off):
renaming, or changing the password or the database name, leaves it running.
An edit that can't be done changes nothing:

```json
{
  "error": {
    "code": "port_in_use",
    "message": "Port 4331 can't be used for the proxy: another program is listening on it.",
    "hint": "Pick another port, or leave the port out and Leadline picks one."
  }
}
```

### Delete

```
POST /api/connections/{id}/delete
```

```json
{
  "ok": true
}
```

Its proxy closes (unless another connection shares it) and its capture stops.
What was recorded stays in the store.

### Test

```
POST /api/connections/test
{"url": "postgres://acme:…@127.0.0.1:54341/acme_dev"}

POST /api/connections/{id}/test        the saved one, as it is (no body)
POST /api/connections/{id}/test        the saved one with changes: {"url": "postgres://acme:****@otherhost/acme_dev"}
```

Connects, asks the server who it is, disconnects. Nothing is saved or
changed. It always answers `200` (unless the id is unknown): a server that
refuses is the result of the test, not a failed request. Look at `ok`. It
gives up after 12 seconds.

```json
{
  "ok": true,
  "kind": "postgres",
  "url": "postgres://acme:****@127.0.0.1:54341/acme_dev",
  "host": "127.0.0.1:54341",
  "database": "acme_dev",
  "user": "acme",
  "remote": false,
  "version": "17.10",
  "latency_ms": 3,
  "tls": false,
  "capture": {
    "available": true,
    "how": "proxy",
    "note": "Leadline opens a port on this machine that forwards to th…"
  },
  "pg_stat_statements": {
    "available": true,
    "version": "1.11",
    "statements": []
  }
}
```

- `remote`, `capture`: as `remote` and `capture_offer` on a connection, so
  the add form can show the capture switch, or why there is none, before
  anything is saved.
- `latency_ms`: how long connecting and logging in took.
- `tls`: whether the connection was encrypted.
- `pg_stat_statements` (Postgres only): whether per-query totals are there,
  and if not, what to do; see [Postgres stats](#postgres-stats).

When it fails, `ok` is `false` and `error` says why:

```json
{
  "ok": false,
  "kind": "postgres",
  "url": "postgres://acme:****@127.0.0.1:54341/acme_dev",
  "host": "127.0.0.1:54341",
  "database": "acme_dev",
  "user": "acme",
  "remote": false,
  "tls": false,
  "capture": {
    "available": true,
    "how": "proxy",
    "note": "Leadline opens a port on this machine that forwards to th…"
  },
  "error": {
    "code": "auth_failed",
    "message": "Password authentication failed for user \"acme\".",
    "hint": "Check the user and password in the URL."
  }
}
```

```json
{
  "ok": false,
  "kind": "postgres",
  "host": "127.0.0.1:54399",
  "remote": false,
  "error": {
    "code": "unreachable",
    "message": "Nothing is listening at 127.0.0.1:54399.",
    "hint": "Is Postgres running? Check the port: a database in Docker is reached on the port it publishes."
  }
}
```

A Redis:

```json
{
  "ok": true,
  "kind": "redis",
  "url": "redis://:****@127.0.0.1:56341",
  "host": "127.0.0.1:56341",
  "database": "0",
  "remote": false,
  "version": "7.4.11",
  "latency_ms": 1.4,
  "tls": false,
  "capture": {
    "available": true,
    "how": "monitor",
    "note": "Live capture uses Redis's MONITOR, which makes the server…"
  }
}
```

## Stats

```
GET /api/connections/{id}/stats
GET /api/connections/{id}/stats?fresh=1
```

What the server knows about itself, read by Leadline over its own
connection. The answer's shape depends on `kind`. An answer is kept for 2
seconds (10 for a remote server) so a screen that polls asks the server once,
not once per poll; `?fresh=1` asks again regardless. Concurrent calls share
one read. If the server can't be reached the answer is a `502` with the
[error](#errors), and the connection's `health` becomes `"error"`.

Leadline's connection is one per saved connection, opened when first needed
and closed after a minute unused.

`incomplete` lists the sections that could not be read, each with a
`message`, when the rest could (a user without permission for one view, a
query that ran past the time limit). The fields of a section that failed are
empty, not missing.

### Postgres stats

How Leadline treats the database, which matters most for a remote one:

- The session is opened read-only (`default_transaction_read_only = on`),
  and every query also runs in its own `BEGIN READ ONLY`.
- Every query has a `statement_timeout` of 3 seconds and a `lock_timeout` of
  half a second. They are set with `SET LOCAL`, which is safe through a
  pooler such as PgBouncer.
- It never writes: no `CREATE EXTENSION`, no `ANALYZE`, no statistics reset.
- It sees what the user in the URL may see. Other users' query text in
  `running` shows as `<insufficient privilege>` unless the user has
  `pg_read_all_stats`.

`read_only` in the answer is always `true`: it is there to be shown.

```json
{
  "kind": "postgres",
  "collected_ms": 1791644887714.019,
  "took_ms": 16.474,
  "remote": false,
  "read_only": true,
  "version": "17.10",
  "database": "acme_dev",
  "user": "acme",
  "size_bytes": 26228403,
  "started_ms": 1791644609093.315,
  "stats_since_ms": null,
  "replica": false,
  "connections": {
    "used": 3,
    "max": 100,
    "reserved": 3,
    "active": 3,
    "idle": 0,
    "idle_in_transaction": 0,
    "waiting": 1
  },
  "table_count": 3,
  "tables": [
    {
      "schema": "public",
      "name": "orders",
      "rows": 120000,
      "total_bytes": 10780672,
      "table_bytes": 7274496,
      "index_bytes": 3506176,
      "seq_scans": 34,
      "index_scans": 3,
      "dead_rows": 3
    },
    {
      "schema": "public",
      "name": "events",
      "rows": 60000,
      "total_bytes": 6889472,
      "table_bytes": 5111808,
      "index_bytes": 1777664,
      "seq_scans": 2,
      "index_scans": 0,
      "dead_rows": 0
    }
  ],
  "missing_index_hints": [
    {
      "schema": "public",
      "table": "orders",
      "rows": 120000,
      "seq_scans": 34,
      "rows_read": 3840000,
      "rows_per_scan": 112941,
      "index_scans": 3,
      "table_bytes": 7274496,
      "why": "orders was read from end to end 34 times, about 112,941 rows each time, against 3 lookups by index.",
      "next_step": "Find the queries that filter or join on orders (EXPLAIN shows \"Seq Scan\") and index the columns they use. A report or a migration that really does read everything is fine as it is."
    }
  ],
  "indexes": {
    "count": 5,
    "unused_count": 2,
    "unused_bytes": 1220608,
    "largest": [
      {
        "schema": "public",
        "table": "orders",
        "name": "orders_pkey",
        "scans": 3,
        "bytes": 2703360,
        "unique": true,
        "primary": true,
        "unused": false
      },
      {
        "schema": "public",
        "table": "events",
        "name": "events_pkey",
        "scans": 0,
        "bytes": 1359872,
        "unique": true,
        "primary": true,
        "unused": false
      }
    ]
  },
  "running": [
    {
      "pid": 197,
      "user": "acme",
      "application": "psql",
      "state": "active",
      "waiting": "Timeout: PgSleep",
      "duration_ms": 2203.9,
      "transaction_ms": 2204.6,
      "query": "SELECT pg_sleep(4)"
    },
    {
      "pid": 204,
      "user": "acme",
      "application": "acme-worker",
      "state": "active",
      "waiting": "Lock: transactionid",
      "duration_ms": 1494.8,
      "transaction_ms": 1494.8,
      "query": "UPDATE orders SET status='paid' WHERE id=7"
    }
  ],
  "locks": [
    {
      "pid": 204,
      "user": "acme",
      "application": "acme-worker",
      "lock_type": "transactionid",
      "mode": "ShareLock",
      "waiting_ms": 1495.7,
      "query": "UPDATE orders SET status='paid' WHERE id=7",
      "blocked_by": [
        197
      ]
    }
  ],
  "pg_stat_statements": {
    "available": true,
    "version": "1.11",
    "statements": [
      {
        "query": "SELECT pg_sleep($1)",
        "calls": 1,
        "total_ms": 6006.712,
        "mean_ms": 6006.712,
        "rows": 1,
        "cache_hit_rate": null
      },
      {
        "query": "UPDATE orders SET status = $1 WHERE id = $2",
        "calls": 4,
        "total_ms": 5012.85,
        "mean_ms": 1253.213,
        "rows": 4,
        "cache_hit_rate": 1
      }
    ]
  },
  "incomplete": []
}
```

(`tables` has up to 15 entries, `indexes.largest` 15, and
`pg_stat_statements.statements` 10; two of each are shown.)

| field | |
| --- | --- |
| `size_bytes` | The size of this database. |
| `started_ms` | When Postgres started. |
| `stats_since_ms` | When the scan and index counters were last reset; `null` if never. Everything under `seq_scans`, `index_scans` and `scans` counts from then (or from when the database was created). |
| `replica` | `true` on a standby. |
| `connections` | `used` of `max` client connections, of which `reserved` are for superusers. `active`, `idle`, `idle_in_transaction` and `waiting` (on a lock) are how the used ones are spent. |
| `table_count`, `tables` | The biggest tables by total size. `rows` is the planner's estimate, not a count. `total_bytes` = `table_bytes` + `index_bytes`. |
| `missing_index_hints` | Tables of 10,000 rows or more that are read from end to end, 1,000 rows or more at a time: what a query with no index to use does. `why` and `next_step` are sentences to show. It is a hint, not a diagnosis: Postgres does not say which query did it. Up to 10, most rows read first. |
| `indexes` | `count` of all, and `largest` by size with how often each was scanned. `unused` is `true` for an index never scanned that does not enforce anything (not unique, not a primary key); `unused_count` and `unused_bytes` total those over all indexes. |
| `running` | What is running now in this database, longest first, up to 10: everything not idle, so a session `idle in transaction` is here too. `duration_ms` is the current statement's; `transaction_ms` the transaction's. `waiting` is what it waits for, as `Lock: transactionid`. `query` is cut at 500 characters. |
| `locks` | Statements waiting for a lock now, with `blocked_by`: the pids holding it, which are usually in `running`. |
| `pg_stat_statements` | Per-query totals since Postgres started, costliest first, when the extension is there. |

**pg_stat_statements when it isn't there.** `available` is `false`, `reason`
says which step is missing, `message` says it in a sentence, and `how_to` is
the steps to take, in order. Leadline takes none of them itself. From the
same Postgres before it was configured:

```json
{
  "available": false,
  "reason": "not_loaded",
  "message": "pg_stat_statements is not loaded, so Postgres is not keeping per-query totals. Leadline only reads, so it won't turn it on for you.",
  "how_to": [
    "Load it when Postgres starts: set shared_preload_libraries = 'pg_stat_statements' in postgresql.conf. For Postgres in Docker, add this to the end of the run command instead: -c shared_preload_libraries=pg_stat_statements",
    "Restart Postgres.",
    "Run this once in the database: CREATE EXTENSION pg_stat_statements;"
  ],
  "statements": []
}
```

| `reason` | |
| --- | --- |
| `not_loaded` | The library is not in `shared_preload_libraries`. Needs a restart of Postgres. |
| `not_installed` | Loaded, but `CREATE EXTENSION` has not been run in this database. One statement, no restart. |
| `not_available` | This Postgres was installed without the extension's files. |
| `error` | It could not be asked; `message` says why. |

### Redis stats

```json
{
  "kind": "redis",
  "collected_ms": 1791644887727.9329,
  "took_ms": 2.544,
  "remote": false,
  "version": "7.4.11",
  "mode": "standalone",
  "uptime_s": 54,
  "memory": {
    "used_bytes": 1626416,
    "rss_bytes": 10297344,
    "peak_bytes": 1645112,
    "max_bytes": 0,
    "policy": "noeviction",
    "fragmentation": 6.49
  },
  "clients": {
    "connected": 2,
    "blocked": 0,
    "max": 10000
  },
  "ops_per_sec": 0,
  "total_commands": 640,
  "keyspace": {
    "keys": 303,
    "hits": 305,
    "misses": 1,
    "hit_rate": 0.997,
    "expired": 0,
    "evicted": 0
  },
  "databases": [
    {
      "db": 0,
      "keys": 303,
      "expiring": 0,
      "avg_ttl_ms": 0
    }
  ],
  "commands": [
    {
      "command": "INFO",
      "calls": 4,
      "total_ms": 0.206,
      "avg_ms": 0.0515
    },
    {
      "command": "MEMORY USAGE",
      "calls": 303,
      "total_ms": 0.051,
      "avg_ms": 0.00017
    },
    {
      "command": "GET",
      "calls": 3,
      "total_ms": 0.046,
      "avg_ms": 0.01533
    }
  ],
  "slow_log": {
    "threshold_ms": 10,
    "entries": [
      {
        "id": 3,
        "at_ms": 1791644884000,
        "duration_ms": 0.003,
        "command": "AUTH ?",
        "client": "127.0.0.1:52402"
      },
      {
        "id": 2,
        "at_ms": 1791644884000,
        "duration_ms": 0.024,
        "command": "GET user:5",
        "client": "127.0.0.1:52398"
      }
    ]
  },
  "biggest_keys": {
    "db": 0,
    "sampled": 303,
    "total": 303,
    "complete": true,
    "note": "Every key in this database was measured.",
    "keys": [
      {
        "key": "big:blob",
        "type": "string",
        "bytes": 229432
      },
      {
        "key": "cart:1",
        "type": "hash",
        "bytes": 80
      },
      {
        "key": "queue:emails",
        "type": "list",
        "bytes": 80
      }
    ]
  },
  "incomplete": []
}
```

(`commands` has up to 10 entries, `slow_log.entries` 10 and
`biggest_keys.keys` 10; fewer are shown.)

| field | |
| --- | --- |
| `memory` | `used_bytes` is Redis's own count; `rss_bytes` what the system gave it. `max_bytes` is the `maxmemory` setting, 0 for no limit. |
| `clients` | `connected` of `max`. Leadline's own connection is one of them. |
| `ops_per_sec` | Redis's own instantaneous figure. |
| `keyspace` | `keys` over all databases. `hit_rate` is `hits / (hits + misses)`, 0 to 1, `null` before any lookup. |
| `databases` | Key counts per database; only databases with keys are listed. |
| `commands` | Where the server's time went, by command, since it started: `calls`, `total_ms`, `avg_ms`. These are real durations, measured by Redis. |
| `slow_log` | Redis's slow log, newest first. `threshold_ms` is how slow a command must be to be logged; `null` when the server would not say (a managed Redis often refuses `CONFIG`). |
| `biggest_keys` | The largest keys in the URL's database among a sample: up to about 1,000 keys or one second of `SCAN`, whichever comes first. `complete` is `true` when that was every key. Otherwise it is a sample, `note` says so, and a larger key may exist. Leadline never sends `KEYS`. |

## Proxies

A proxy is one port in front of one Postgres. There are three ways to get
one, and they all show here and in `/api/sources`:

- a saved connection with `capture` on (`origins` has `"connection"`). A
  person's connections to one server share a proxy. A worktree's
  connection gets a proxy of that worktree's own, on its own port: its
  `label` is the worktree's service name, and everything through it is
  filed under that name, whatever `application_name` a connection gives,
- `leadline run`, for as long as the command runs (`"run"`),
- the daemon's `-pg listen=upstream` flag (`"flag"`).

```
GET /api/proxies
```

```json
{
  "proxies": [
    {
      "id": "p_55f949c3e2",
      "kind": "postgres",
      "listen": "127.0.0.1:15432",
      "port": 15432,
      "upstream": "127.0.0.1:54341",
      "label": "Acme dev",
      "origins": [
        "connection"
      ],
      "connection_ids": [
        "c_a3228af549"
      ],
      "state": "listening",
      "connections_open": 0,
      "connections_total": 1,
      "statements": 4,
      "failed_statements": 0,
      "unread_connections": 0,
      "last_activity_ms": 1791644884631.648
    }
  ]
}
```

`connections_open` and `connections_total` are client connections now and
since the proxy opened. `statements` counts statements recorded,
`failed_statements` those that ended in an error. `unread_connections` are
connections the proxy passed along without understanding (a replication
stream, say), whose statements were not recorded. `last_activity_ms` is when
a connection last arrived or a statement was last recorded; 0 if never.
`label` is what `leadline run` called it, or the names of the connections it
is for. `window_statements` is the statements recorded since the `since_ms`
of the `/api/sources` call (the last 15 minutes here and anywhere else a
proxy is shown), counted by the minute and kept for a day. A live Redis
capture has the same as `monitor.window_recorded`. `owner` is who the proxy
is for: the worktree (`"worktree"`), a person's saved connections
(`"user"`), a `leadline run` (`"run"`, with the run's `service`) or the
`-pg` flag (`"flag"`); see [Owners](#owners).

### For `leadline run`

```
POST /api/proxies
{"kind": "postgres", "upstream": "127.0.0.1:54342", "label": "acme-web"}
```

```json
{"id":"p_f326283b5e","kind":"postgres","listen":"127.0.0.1:63105","upstream":"127.0.0.1:54342"}
```

Exactly those four fields. `label` is the run's service name, and the proxy
files what it records under it, unless a connection names itself with
`application_name`. So a proxy is shared only by runs of the same name:
asking again for the same upstream (however the address is spelled:
`localhost:5432` and `127.0.0.1:5432` are one) with the same label returns
the same proxy, and another label gets a proxy of its own, even when a saved
connection already captures that database. With no label there is no name
to file under: the statements go under the database's name, and an upstream
that already has an unnamed proxy, a saved connection's included, is given
that one. Nothing is saved: these proxies end with the daemon.

```
DELETE /api/proxies/{id}
```

```json
{"closed":true,"ok":true}
```

Gives the proxy back. Every `POST` counts as one use and every `DELETE`
gives one back; the proxy closes when the last use has (`closed` says
whether this call was the one). That is so two `leadline run`s of one
service on one database share a proxy and the first to exit does not cut
the other off.
A proxy that a saved connection or the `-pg` flag also wants stays open.
An unknown id is a `404`.

`kind` must be `"postgres"`, and the upstream must be on this machine: a
remote one is a `400` with code `capture_remote`, for the reason given under
[Capture](#capture).

## Owners

Connections, proxies and sources each say who made them, so that a row can
read "from Shipyard, worktree fix-orders" or "found by a scan, DATABASE_URL
in acme-web", and so that detaching a worktree takes away exactly what it
brought.

```json
{
  "kind": "worktree",
  "via": "shipyard",
  "service": "acme/fix-orders",
  "project": "acme",
  "worktree": "fix-orders",
  "branch": "fix-orders",
  "box": "devl",
  "path": "/home/dev/work/acme-fix-orders",
  "label": "from Shipyard, worktree fix-orders"
}
```

| field | |
| --- | --- |
| `kind` | `"user"` (a person added it by its URL), `"worktree"` (`leadline attach` made it for a worktree), `"run"` (a `leadline run` asked for it, for as long as it runs), `"flag"` (the daemon's `-pg`) or `"scan"` (a person ticked it in a scan, which had found its URL in a project's env file). Always present. An app should show the `label` of a kind it does not know. |
| `label` | The same in a few words, to show as it is: `"added by hand"`, `"from Shipyard, worktree fix-orders"`, `"attached, worktree fix-orders"`, `"leadline run"`, `"the daemon's -pg flag"`, `"found by a scan, DATABASE_URL in acme-web"`. Always present. |
| `service` | The source it belongs to: the worktree's service name, or the run's. |
| `via` | For a worktree, the tool that attached it: `"shipyard"` when Shipyard's hooks or variables did, `"cli"` when a person ran `leadline attach` outside it. |
| `project`, `worktree`, `branch`, `box`, `path` | For a worktree: the project it is a checkout of, its own name (absent for the project's main checkout), its branch, the machine it is on and its folder. Any may be absent: they are what the caller knew. For a scan: `project` is the project whose env file had the URL, `path` that project's folder and `branch` its git branch (absent for a folder in no repository). |
| `env_key`, `env_file` | For a scan: the variable the URL was found in, and the env file it was in, by name (`DATABASE_URL`, `.env.local`). Names only, never a value. |

Everything but `kind` and `label` is left out when it does not apply:

```json
{"kind": "user", "label": "added by hand"}
```

**A connection saved from a scan** (`POST /api/discover/save`, in
`internal/discover/API.md`) says where its URL was found. This is the
`owner` of one found in a made-up project's `.env.local` and saved (the
folder is written here as a person would have it):

```json
{
  "kind": "scan",
  "project": "acme-web",
  "branch": "main",
  "path": "/Users/acme/code/acme-web",
  "env_key": "DATABASE_URL",
  "env_file": ".env.local",
  "label": "found by a scan, DATABASE_URL in acme-web"
}
```

- When the URL was in several places, the owner names the one the dev
  tools would use: the first that no other file outranks (the first
  `effective` entry of the found connection's `found_in`).
- It is the person's connection in every other way. It is edited, deleted
  and counted like one added by hand, no `leadline detach` removes it, and
  `env` stays `[]`: that list is a worktree's.
- The owner is set when the scan saves it and at no other time. It can't
  be sent in the body of `POST /api/connections`, where an `owner` member
  is ignored, and a connection that was already saved when a scan found it
  again keeps the owner it had.
- The label is made from what is there: `"found by a scan, in acme-web"`
  without a variable, `"found by a scan"` alone.

Where it appears:

- on every **connection**, as `owner`: `"user"`, `"worktree"` or `"scan"`;
- on every **proxy**, as `owner`. A proxy opened for saved connections is
  `"user"`, or `"scan"` with the same members when every connection it is
  for was found by a scan in the same place; two that came different ways
  share a proxy that is `"user"`;
- on a **service** in `/api/sources`, as `owner`, only when the service
  belongs to an attached worktree;
- on every entry of `attached`.

## Attached sources: worktrees

A tool that makes a checkout for each task (Shipyard, with a database for
each) can make each one a source of its own. `leadline attach` registers the
worktree under one service name and saves the databases its environment
names as connections the worktree owns; `leadline detach` takes away exactly
those, and what was recorded under the name. `leadline shipyard hooks`
prints the Shipyard hooks that call the two.

```
GET  /api/attach                  every attached source
GET  /api/attach?service=NAME     one, with its connections
POST /api/attach                  attach a worktree, or bring it up to date
POST /api/detach                  forget one
```

**The name.** A source is its service name, and `leadline attach` makes it
`<project>/<worktree>` (`acme/fix-orders`), or the project's name alone for
its main checkout. `leadline run` in the worktree files under the same name,
and `leadline run --name worker` there under `acme/fix-orders:worker`. A
service named `<source>:<anything>` belongs to the source: it carries the
source's `owner` in `/api/sources` and is deleted with it. That is why a
source's own name can't contain a colon.

### Attach

```
POST /api/attach
{
  "service": "acme/fix-orders",
  "via": "shipyard",
  "project": "acme", "worktree": "fix-orders", "branch": "fix-orders",
  "box": "devl", "path": "/home/dev/work/acme-fix-orders",
  "connections": [
    {"env": ["DATABASE_URL", "DIRECT_URL"], "url": "postgres://acme:…@localhost:54351/acme_fix_orders"},
    {"env": ["REDIS_URL"], "url": "redis://localhost:56351/1"},
    {"env": ["SEARCH_URL"], "url": "https://search.acme.example"}
  ]
}
```

| field | |
| --- | --- |
| `service` | Required. At most 80 characters, no colon. Attaching a name that is attached already is an update. |
| `via`, `project`, `worktree`, `branch`, `box`, `path` | Optional, kept as the source's `owner`. One left out stays as an earlier attach said it: not every hook knows the branch. `via` defaults to `"cli"`. |
| `connections` | The worktree's database and cache URLs, each with the names of the variables it was found in. At most 16. They are **all** of the worktree's: a connection it owns whose URL is not among them is removed. |
| `partial` | Optional. `true` says the URLs may not be all of them (the caller found no file to read them from): what is given is added or updated and nothing is removed. |
| `capture` | Optional. `false` saves new connections for their statistics only. Left out, a new connection to a server on this machine is captured, and one to a remote server never is. |

What it does with the URLs:

- Two URLs for the same server, database and user are one connection, with
  both lists of `env` names.
- A URL not seen before becomes a connection named `<service> · <database>`
  (`· redis`, or `· redis 3` for a database number other than 0).
- A URL for a connection it already owns, changed (a new password, a new
  option), updates that connection in place: same `id`, same proxy, same
  port.
- A Postgres connection gets the worktree's own proxy, one port for each
  worktree and server. The port is kept in the connections file, so it is
  the same after a restart.
- A captured Redis is recorded under the worktree's service name (not the
  connection's name, as a person's is), and only the commands on the
  database number its URL names: the server is often one every worktree
  shares.
- A URL that is neither Postgres nor Redis, or can't be read, is in
  `skipped` with the reason. No URL is ever quoted back.

It does not wait for any server: `health` in the answer is `"unknown"` and
the first check runs behind it. The answer, for the request above:

```json
{
  "service": "acme/fix-orders",
  "created": true,
  "changed": true,
  "source": {
    "service": "acme/fix-orders",
    "owner": {
      "kind": "worktree",
      "via": "shipyard",
      "service": "acme/fix-orders",
      "project": "acme",
      "worktree": "fix-orders",
      "branch": "fix-orders",
      "box": "devl",
      "path": "/home/dev/work/acme-fix-orders",
      "label": "from Shipyard, worktree fix-orders"
    },
    "connection_ids": [
      "c_30072de5c0",
      "c_5eaffe3509"
    ],
    "created_ms": 1791653090706,
    "updated_ms": 1791653090706
  },
  "connections": [
    {
      "id": "c_30072de5c0",
      "kind": "postgres",
      "name": "acme/fix-orders · acme_fix_orders",
      "url": "postgres://acme:****@localhost:54351/acme_fix_orders",
      "host": "localhost:54351",
      "database": "acme_fix_orders",
      "user": "acme",
      "remote": false,
      "capture": true,
      "capture_offer": {
        "available": true,
        "how": "proxy",
        "note": "Leadline opens a port on this machine that forwards to th…"
      },
      "proxy": {
        "id": "p_8f52081ce4",
        "kind": "postgres",
        "listen": "127.0.0.1:15434",
        "port": 15434,
        "upstream": "localhost:54351",
        "label": "acme/fix-orders",
        "origins": [
          "connection"
        ],
        "connection_ids": [
          "c_30072de5c0"
        ],
        "state": "listening",
        "url": "postgres://acme:****@127.0.0.1:15434/acme_fix_orders?sslmode=disable",
        "owner": {
          "kind": "worktree",
          "via": "shipyard",
          "service": "acme/fix-orders",
          "project": "acme",
          "worktree": "fix-orders",
          "branch": "fix-orders",
          "box": "devl",
          "path": "/home/dev/work/acme-fix-orders",
          "label": "from Shipyard, worktree fix-orders"
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
        "state": "unknown"
      },
      "created_ms": 1791653090706,
      "owner": {
        "kind": "worktree",
        "via": "shipyard",
        "service": "acme/fix-orders",
        "project": "acme",
        "worktree": "fix-orders",
        "branch": "fix-orders",
        "box": "devl",
        "path": "/home/dev/work/acme-fix-orders",
        "label": "from Shipyard, worktree fix-orders"
      },
      "env": [
        "DATABASE_URL",
        "DIRECT_URL"
      ]
    },
    {
      "id": "c_5eaffe3509",
      "kind": "redis",
      "name": "acme/fix-orders · redis 1",
      "url": "redis://localhost:56351/1",
      "host": "localhost:56351",
      "database": "1",
      "user": "",
      "remote": false,
      "capture": true,
      "capture_offer": {
        "available": true,
        "how": "monitor",
        "note": "Live capture uses Redis's MONITOR, which makes the server…"
      },
      "monitor": {
        "state": "connecting",
        "seen": 0,
        "recorded": 0,
        "dropped": 0,
        "max_per_sec": 1000,
        "last_activity_ms": 0,
        "note": "Live capture uses Redis's MONITOR, which makes the server…",
        "window_recorded": 0
      },
      "health": {
        "state": "unknown"
      },
      "created_ms": 1791653090706,
      "owner": {
        "kind": "worktree",
        "via": "shipyard",
        "service": "acme/fix-orders",
        "project": "acme",
        "worktree": "fix-orders",
        "branch": "fix-orders",
        "box": "devl",
        "path": "/home/dev/work/acme-fix-orders",
        "label": "from Shipyard, worktree fix-orders"
      },
      "env": [
        "REDIS_URL"
      ]
    }
  ],
  "added": 2,
  "updated": 0,
  "removed": 0,
  "unchanged": 0,
  "skipped": [
    {
      "env": [
        "SEARCH_URL"
      ],
      "reason": "Leadline connects to Postgres (postgres://) and Redis (redis://). This URL is for neither."
    }
  ]
}
```

| field | |
| --- | --- |
| `created` | The source is new. |
| `changed` | Anything at all was added, updated or removed, the source's `owner` included. |
| `source` | The source, as in the list below. |
| `connections` | The connections it owns now, as [connection objects](#the-connection-object). |
| `added`, `updated`, `removed`, `unchanged` | How many of its connections each happened to. |
| `skipped` | The URLs that were not saved, by the names of their variables, and why. `[]` when none. |

**Attaching twice changes nothing.** The same request again answers with
`created` and `changed` both `false`, the same ids and the same proxy, and
the connections file is not written:

```json
{"service": "acme/fix-orders", "created": false, "changed": false, "added": 0, "updated": 0, "removed": 0, "unchanged": 2}
```

(shown without `source`, `connections` and `skipped`, which are as above).

A name that can't be one is a `400`:

```json
{"error":{"code":"bad_request","message":"A source's name can't contain \":\".","hint":"The colon is what joins a source's name to a command's, as in \"acme/fix-orders:worker\"."}}
```

### List

```
GET /api/attach
```

```json
{
  "attached": [
    {
      "service": "acme/fix-orders",
      "owner": {
        "kind": "worktree",
        "via": "shipyard",
        "service": "acme/fix-orders",
        "project": "acme",
        "worktree": "fix-orders",
        "branch": "fix-orders",
        "box": "devl",
        "path": "/home/dev/work/acme-fix-orders",
        "label": "from Shipyard, worktree fix-orders"
      },
      "connection_ids": [
        "c_30072de5c0",
        "c_5eaffe3509"
      ],
      "created_ms": 1791653090706,
      "updated_ms": 1791653090706
    }
  ]
}
```

In the order they were attached. `connection_ids` are the connections the
source owns; `updated_ms` is when an attach last changed anything. The same
list is `attached` in `/api/sources`, where the source is also one of
`services`:

```json
{
  "name": "acme/fix-orders",
  "spans": 15,
  "logs": 5,
  "metric_points": 0,
  "last_seen_ms": 1791653141151.007,
  "owner": {
    "kind": "worktree",
    "via": "shipyard",
    "service": "acme/fix-orders",
    "project": "acme",
    "worktree": "fix-orders",
    "branch": "fix-orders",
    "box": "devl",
    "path": "/home/dev/work/acme-fix-orders",
    "label": "from Shipyard, worktree fix-orders"
  },
  "via": [
    "otlp",
    "postgres-proxy",
    "redis-monitor"
  ],
  "through": [
    {
      "via": "redis-monitor",
      "connection_ids": [
        "c_5eaffe3509"
      ]
    },
    {
      "via": "postgres-proxy",
      "proxy_id": "p_447ec6ea03",
      "connection_ids": [
        "c_30072de5c0"
      ]
    }
  ]
}
```

One source, with its connections in full (what `leadline run` asks, to
point a command at the worktree's own proxy):

```
GET /api/attach?service=acme/fix-orders
```

answers `{"source": {…}, "connections": [{…}, {…}]}`, the same two shapes
as above. A name that is not attached is a `404`:

```json
{"error":{"code":"not_found","message":"No source of that name is attached.","hint":"\"leadline attach\" in the worktree attaches it."}}
```

### Detach

```
POST /api/detach
{"service": "acme/fix-orders"}
```

```json
{
  "service": "acme/fix-orders",
  "found": true,
  "connections": 2,
  "proxies": 1,
  "kept_data": false,
  "deleted": {
    "spans": 15,
    "logs": 5,
    "metric_points": 0
  }
}
```

It stops and removes the connections the source owns and the proxies opened
for them, forgets the source, and deletes what was recorded under its name
and under every `<service>:<name>`: spans, logs, metric points and the
route list. **Nothing else is touched.** A connection a person added, or
another worktree owns, stays as it is even when it is to the same server:
ownership is the `owner` on each connection, never where it points.

| field | |
| --- | --- |
| `service` | Required: the source's name. |
| `keep_data` | Optional. `true` leaves the recorded data, which then ages out like any other. |

| in the answer | |
| --- | --- |
| `found` | The source was attached. |
| `connections`, `proxies` | How many were removed and closed. A proxy a `leadline run` is still using stays open until that run ends, and is not counted. |
| `kept_data` | The `keep_data` asked for. |
| `deleted` | The rows deleted. All 0 with `keep_data`. |

The delete reads the store's service column from end to end and holds
nothing ingest or a read needs while it does (taking 300,000 of three
million spans took under a second, with exports and reads going on beside
it at their usual speed). The rows stop being seen at once; their space is
given back as retention frees the blocks they were in. A service's rows
that arrive after the detach are kept: detach is for a worktree that has
stopped.

**Detaching twice, or a worktree Leadline never saw, is not an error:**

```json
{"service":"acme/fix-orders","found":false,"connections":0,"proxies":0,"kept_data":false,"deleted":{"spans":0,"logs":0,"metric_points":0}}
```

Data filed under the name is deleted even when the source was never
attached (a `leadline run` in the worktree files under the worktree's name
whether or not a hook attached it): the caller is saying the worktree is
gone.

## The MCP tool

`list_sources`, with no arguments, is the same answer as `/api/sources`
made small for a coding agent that wants to check its setup worked: no
URLs, at most 20 services, times in ISO 8601.

```json
{
  "connections": [
    {
      "capture": true,
      "health": "ok",
      "host": "127.0.0.1:54341",
      "kind": "postgres",
      "name": "Acme dev"
    },
    {
      "capture": true,
      "health": "ok",
      "host": "127.0.0.1:56341",
      "kind": "redis",
      "name": "acme-cache"
    }
  ],
  "otlp_endpoints": [
    "http://127.0.0.1:4331"
  ],
  "proxies": [
    {
      "connections": 1,
      "label": "Acme dev",
      "last_activity": "2026-10-10T16:08:04.631+01:00",
      "listen": "127.0.0.1:15432",
      "statements": 4,
      "upstream": "127.0.0.1:54341"
    }
  ],
  "services": [
    {
      "last_seen": "2026-10-10T16:08:04.657+01:00",
      "logs": 0,
      "metric_points": 0,
      "name": "acme-cache",
      "spans": 1010,
      "via": [
        "redis-monitor"
      ]
    },
    {
      "last_seen": "2026-10-10T16:08:04.524+01:00",
      "logs": 0,
      "metric_points": 0,
      "name": "acme-web",
      "spans": 39,
      "via": [
        "postgres-proxy"
      ]
    }
  ]
}
```

With nothing arriving it adds a `hint` saying what to do. An attached
worktree that has sent nothing yet is among `services` with zero counts.
