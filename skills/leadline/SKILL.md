---
name: leadline
description: See what code really did while it ran in development, through Leadline's MCP tools - which database queries a request ran and how long each took, N+1 queries, where a request failed, and what was logged. Use after changing code that touches a database or an API route, when asked why something is slow or failing locally, to check a fix worked, or before saying a performance change is done.
---

# Checking what code did with Leadline

Leadline captures traces, database queries and logs from apps running on
this machine. Its MCP server is named `leadline`; every tool only reads.

If the `leadline` tools are missing, the daemon is not running or the MCP
server is not added. Say so, and point at the `leadline-setup` skill. Do
not guess what the code did.

## The loop

1. **Run the code.** Leadline only knows what ran. Make the request, run
   the test, or load the page first.
2. **`overview`** for the last few minutes (`minutes: 5`). It gives counts,
   the costliest queries and any N+1 queries.
3. **Go to the one request.** `list_traces` with `search` set to the route
   (`"/api/orders"`), then `get_trace` with its `trace_id`. The spans are in
   order, with the exact SQL of each database call.
4. **Change the code, run it again, look again.** The newest trace is what
   the change did. Compare numbers, not impressions.

## What to look for

- **N+1:** a query with `n_plus_one: true`, or `max_per_trace` well above 1.
  In `get_trace` it shows as the same statement repeated with different
  values. The fix is one query for all the rows (a join, `IN (...)`, or the
  ORM's eager loading).
- **Slow query:** a high `p95_ms` or `max_ms` in `list_queries`. Its
  `example` is a real statement you can run under `EXPLAIN`.
- **Failure:** `list_traces` with `errors_only: true`, then `get_trace`.
  A span's `error` and the trace's `logs` usually name the cause.
- **What counts as a query:** each statement once, however many ways it
  was seen. In `get_trace` a database span with `role: "duplicate"` is the
  app's own report of the statement in the span under it, and
  `role: "control"` is `BEGIN`, `COMMIT`, `SET` and the like: there in the
  trace and in database time, not among the queries or the N+1 counts.
- **A metric:** `get_metrics` lists what apps export (request durations,
  pool sizes, memory, counters) with each one's kind, unit and latest
  value. With `name` it gives that metric over time in twelve buckets;
  `by: "service"` or an attribute key (`keys` lists them) splits it into a
  few lines. Ask for more `buckets` only when the shape matters.
- **Code nothing has run:** `list_routes` gives the routes a Next.js app
  has that no request reached in the last hour, with the file behind each.
  It needs the app to use `@leadline/next`; `all: true` lists every route
  with its request count.
- **Is anything arriving:** `list_sources` says which services Leadline
  holds data for, how much, when each was last heard from and how it
  arrives (`otlp`, `postgres-proxy`, `redis-monitor`), with each proxy's
  statement count and each connected database's health. Use it right after
  setting an app up, and when `overview` is unexpectedly empty.
- **Which worktree:** under Shipyard each worktree is a service of its
  own, named `<project>/<worktree>` (`acme/fix-orders`), and a command run
  in it with a name of its own is `acme/fix-orders:worker`. If
  `BERTH_LOCATION` and `BERTH_WORKTREE_NAME` are in your environment, that
  is your service: pass it as `service` so you read what your worktree
  did, not what another agent's did.
- **A log line:** `search_logs` with `search` or `level: "error"`. A log's
  `trace_id` leads to the request that wrote it. A log with no `trace_id`
  was printed outside any request, or was read off the app's output by
  `leadline run` (its attributes say `leadline.source: run`): find its
  request by the time it was written.

## Reporting

Give the numbers you read: "GET /api/orders ran 24 queries in 58 ms; after
the change, 2 queries in 9 ms." Name the trace you compared. If Leadline
has no data for the code in question, say that plainly: it means the app is
not sending telemetry, not that the code is fine.

Keep results small: use `minutes`, `search` and `limit` to narrow, and ask
`get_trace` for `include_attributes` only when the spans alone don't answer.
