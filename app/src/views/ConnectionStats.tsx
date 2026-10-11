import type { ReactNode } from "react";
import { CheckIcon, RefreshCwIcon } from "lucide-react";
import { api, ApiError, type Connection, type PgStatStatements, type PostgresStats, type RedisStats } from "@/lib/api";
import { masked } from "@/lib/connurl";
import { fmtAgo, fmtBytes, fmtClock, fmtCount, fmtFor, fmtInt, fmtMs, fmtPct } from "@/lib/format";
import { useEvery, useMachine } from "@/lib/live";
import { hereOf } from "@/lib/machines";
import { captureWord, connProblem, kindVersion, versionOf } from "@/lib/sources";
import { useCapture, CaptureSwitch } from "@/components/Capture";
import { CopyLine, Figure, Figures, Listing, Meter, Rows, Side, Surface, Tip, Waiting } from "@/components/ui";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

// How often an open pane asks the server for its statistics again. The
// daemon keeps an answer for 2 seconds (10 for a remote server), so this is
// the pace the database sees.
const EVERY_MS = 5000;

/**
 * ConnectionStats is what a saved database or Redis knows about itself,
 * read by the daemon over its own read-only connection, with the
 * connection's one setting (capture) at the top.
 */
export function ConnectionStats({ conn, onClose, onChanged }: { conn: Connection; onClose: () => void; onChanged: () => void }) {
  const here = hereOf(useMachine());
  const { data, error, loading, again } = useEvery(() => api.connectionStats(conn.id), EVERY_MS, [conn.id]);
  const fresh = () => {
    api.connectionStats(conn.id, true).then(again, again);
  };
  const fault = error ? (error instanceof ApiError ? { message: error.message, hint: error.hint } : { message: error instanceof Error ? error.message : String(error), hint: "" }) : null;

  return (
    <Side
      onClose={onClose}
      label="Close stats"
      title={
        <>
          <span className="selectable truncate font-medium">{conn.name}</span>
          <span className="truncate text-[11px] text-muted-foreground">
            {kindVersion(conn.kind, versionOf(conn.health))} {conn.remote ? `at ${conn.host}` : `on ${here}, ${conn.host}`}
          </span>
          {conn.remote && (
            <Badge variant="secondary" className="font-normal text-muted-foreground">
              remote
            </Badge>
          )}
          <span className="ml-auto shrink-0 text-[11px] text-muted-foreground tabular-nums">{data ? `Read ${fmtAgo(Date.now() - data.collected_ms)}` : ""}</span>
          <Tip label="Read its statistics again">
            <Button variant="ghost" size="icon-xs" aria-label="Read its statistics again" onClick={fresh} className="shrink-0 text-muted-foreground hover:text-foreground">
              <RefreshCwIcon className={loading ? "animate-spin" : undefined} />
            </Button>
          </Tip>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <CaptureCard conn={conn} onChanged={onChanged} />
        {fault && (
          <Surface role="alert" className="px-3 py-2.5">
            <p className="selectable text-destructive-foreground">{fault.message}</p>
            {fault.hint && <p className="selectable mt-0.5 text-[11.5px] text-muted-foreground">{fault.hint}</p>}
            {data && <p className="mt-1.5 text-[11.5px] text-muted-foreground">What is below was read {fmtAgo(Date.now() - data.collected_ms)}.</p>}
          </Surface>
        )}
        {!data ? (
          !fault && (
            <div className="h-40">
              <Waiting />
            </div>
          )
        ) : data.kind === "postgres" ? (
          <Postgres s={data} />
        ) : (
          <Redis s={data} />
        )}
        {data && data.incomplete.length > 0 && (
          <Surface className="px-3 py-2.5 text-[11.5px] text-muted-foreground">
            <p className="text-warning-foreground">Some of this could not be read:</p>
            <ul className="selectable mt-1 list-disc pl-4">
              {data.incomplete.map((i, n) => (
                <li key={n}>
                  {i.section ? `${i.section}: ` : ""}
                  {i.message}
                </li>
              ))}
            </ul>
          </Surface>
        )}
        {data && (
          <p className="px-1 text-[11px] text-muted-foreground">
            {data.kind === "postgres" ? "Read over a read-only session; Leadline never writes to the database. " : "Leadline only reads, and never sends KEYS. "}
            Reading took {fmtMs(data.took_ms)}.
          </p>
        )}
      </div>
    </Side>
  );
}

/** CaptureCard is the capture switch with everything about it: what it does, the proxy's address and URL, and what it has carried. */
export function CaptureCard({ conn, onChanged }: { conn: Connection; onChanged: () => void }) {
  const capture = useCapture(conn, onChanged);
  const word = captureWord(conn.kind);
  // A server that can't be reached is said once, where its statistics would be; here only what is wrong with the capture.
  const problem = conn.health.state === "error" ? null : connProblem(conn);
  const p = conn.proxy;
  const m = conn.monitor;
  return (
    <Surface className="gap-2.5 px-3 py-2.5">
      <div className="flex items-start gap-2.5">
        <span className="flex h-5 items-center">
          <CaptureSwitch conn={conn} capture={capture} />
        </span>
        <div className="min-w-0 flex-1">
          <p>
            {!conn.capture_offer.available
              ? "Reading its statistics"
              : capture.on
                ? `Capturing every ${word}`
                : `Statistics only: its ${conn.kind === "redis" ? "commands" : "queries"} aren't recorded`}
          </p>
          <p className="mt-0.5 text-[11.5px] leading-snug text-muted-foreground">{conn.capture_offer.note}</p>
        </div>
      </div>
      {capture.fault && (
        <div role="alert" className="pl-[42px] text-[11.5px]">
          <p className="selectable text-destructive-foreground">{capture.fault.message}</p>
          {capture.fault.hint && <p className="selectable text-muted-foreground">{capture.fault.hint}</p>}
        </div>
      )}
      {problem && (
        <div className="pl-[42px] text-[11.5px]">
          <p className="selectable text-destructive-foreground">{problem.message}</p>
          {problem.hint && <p className="selectable text-muted-foreground">{problem.hint}</p>}
        </div>
      )}
      {p && p.state === "listening" && (
        <div className="flex flex-col gap-1.5 pl-[42px]">
          <p className="text-[11.5px] text-muted-foreground">
            Point the app at the proxy, <span className="selectable font-mono text-foreground">{p.listen}</span>, which forwards to <span className="selectable font-mono">{p.upstream}</span>:
          </p>
          {p.url && (
            <CopyLine text={`DATABASE_URL=${p.url}`} label="Copy the line for your .env">
              DATABASE_URL={p.url}
            </CopyLine>
          )}
          <p className="text-[11.5px] text-muted-foreground tabular-nums">
            {p.url && masked(p.url) ? "Put your password in place of ****. " : ""}
            {p.statements === 0 && p.connections_total === 0
              ? "Nothing has come through it yet."
              : `${fmtCount(p.statements)} ${p.statements === 1 ? "statement" : "statements"} recorded${p.failed_statements > 0 ? `, ${fmtCount(p.failed_statements)} failed` : ""}, over ${fmtCount(p.connections_total)} ${p.connections_total === 1 ? "connection" : "connections"} (${p.connections_open} open now).`}
            {p.unread_connections > 0 ? ` ${fmtCount(p.unread_connections)} passed through unread, as a replication stream does.` : ""}
          </p>
        </div>
      )}
      {m && m.state !== "error" && (
        <p className="pl-[42px] text-[11.5px] text-muted-foreground tabular-nums">
          {m.state === "connecting" ? "Connecting to Redis to listen. " : ""}
          {m.seen === 0
            ? "No command has been reported yet."
            : `${fmtCount(m.seen)} ${m.seen === 1 ? "command" : "commands"} reported, ${fmtCount(m.recorded)} recorded${m.dropped > 0 ? `, ${fmtCount(m.dropped)} dropped for coming faster than ${fmtCount(m.max_per_sec)} a second` : ""}.`}
        </p>
      )}
    </Surface>
  );
}

const Num = ({ children, className = "" }: { children: ReactNode; className?: string }) => <TableCell className={`text-right tabular-nums ${className}`}>{children}</TableCell>;
const Sql = ({ children }: { children: ReactNode }) => <div className="selectable line-clamp-2 font-mono text-[11.5px] break-words">{children}</div>;
const dash = <span className="text-muted-foreground">–</span>;

function Postgres({ s }: { s: PostgresStats }) {
  const c = s.connections;
  const maxTable = Math.max(1, ...s.tables.map((t) => t.total_bytes));
  const maxIndex = Math.max(1, ...s.indexes.largest.map((i) => i.bytes));
  return (
    <>
      <Figures className="@3xl:grid-cols-6 grid-cols-3">
        <Figure label="Size" value={fmtBytes(s.size_bytes)} note={`database ${s.database}`} />
        <Figure
          label="Connections"
          value={`${c.used} of ${c.max}`}
          note={[`${c.active} active`, `${c.idle} idle`, c.idle_in_transaction > 0 && `${c.idle_in_transaction} idle in a transaction`].filter(Boolean).join(", ")}
          tone={c.used >= c.max - c.reserved ? "err" : c.used >= (c.max - c.reserved) * 0.8 ? "warn" : undefined}
        />
        <Figure label="Waiting on a lock" value={String(c.waiting)} tone={c.waiting > 0 ? "warn" : undefined} note={s.locks.length > 0 ? "see below" : undefined} />
        <Figure label="Tables" value={fmtInt(s.table_count)} />
        <Figure
          label="Indexes"
          value={fmtInt(s.indexes.count)}
          note={s.indexes.unused_count > 0 ? `${s.indexes.unused_count} unused, ${fmtBytes(s.indexes.unused_bytes)}` : "none unused"}
        />
        <Figure label="Up for" value={fmtFor(s.collected_ms - s.started_ms)} note={s.replica ? "a standby" : `as ${s.user}`} />
      </Figures>

      <Listing
        title="Missing-index hints"
        hint="big tables read from end to end"
        count={s.missing_index_hints.length}
        none="No large table is being read from end to end."
      >
        <ul className="flex flex-col">
          {s.missing_index_hints.map((h) => (
            <li key={`${h.schema}.${h.table}`} className="border-t px-3 py-2 first:border-t-0">
              <div className="flex items-baseline gap-2">
                <span className="selectable font-mono text-[11.5px] font-medium">{h.schema === "public" ? h.table : `${h.schema}.${h.table}`}</span>
                <span className="text-[11px] text-muted-foreground tabular-nums">
                  {fmtCount(h.rows)} rows, {fmtBytes(h.table_bytes)}
                </span>
              </div>
              <p className="selectable mt-0.5 text-xs text-warning-foreground">{h.why}</p>
              <p className="selectable mt-0.5 text-[11.5px] text-muted-foreground">{h.next_step}</p>
            </li>
          ))}
        </ul>
        <p className="border-t px-3 py-1.5 text-[11px] text-muted-foreground">
          A hint, not a diagnosis: Postgres doesn't say which query did it. Counted {s.stats_since_ms ? `since ${new Date(s.stats_since_ms).toLocaleString()}` : "since the database was created"}.
        </p>
      </Listing>

      <Listing title="Running now" hint="everything not idle, longest first" count={s.running.length} none="Nothing is running in this database right now.">
        <Rows>
          <TableHeader>
            <TableRow>
              <TableHead className="w-[58px]">Pid</TableHead>
              <TableHead className="w-[22%]">Who</TableHead>
              <TableHead>Statement</TableHead>
              <TableHead className="w-[76px] text-right">For</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {s.running.map((r) => (
              <TableRow key={r.pid} className="[&>td]:align-top">
                <TableCell className="text-muted-foreground tabular-nums">{r.pid}</TableCell>
                <TableCell>
                  <div className="truncate">{r.application || r.user}</div>
                  <div className={`text-[11px] leading-snug ${r.waiting.startsWith("Lock") ? "text-warning-foreground" : "text-muted-foreground"}`}>
                    {r.state}
                    {r.waiting ? `, waiting on ${r.waiting}` : ""}
                  </div>
                </TableCell>
                <TableCell>
                  <Sql>{r.query}</Sql>
                </TableCell>
                <Num>
                  {fmtMs(r.duration_ms)}
                  {r.transaction_ms > r.duration_ms * 1.5 && <div className="text-[11px] text-muted-foreground">in a {fmtMs(r.transaction_ms)} transaction</div>}
                </Num>
              </TableRow>
            ))}
          </TableBody>
        </Rows>
      </Listing>

      <Listing title="Waiting on a lock" hint="and the pids that hold it" count={s.locks.length} none="No statement is waiting for a lock.">
        <Rows>
          <TableHeader>
            <TableRow>
              <TableHead className="w-[58px]">Pid</TableHead>
              <TableHead className="w-[22%]">Who</TableHead>
              <TableHead>Statement</TableHead>
              <TableHead className="w-[104px]">Blocked by</TableHead>
              <TableHead className="w-[76px] text-right">For</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {s.locks.map((l) => (
              <TableRow key={l.pid} className="[&>td]:align-top">
                <TableCell className="text-muted-foreground tabular-nums">{l.pid}</TableCell>
                <TableCell>
                  <div className="truncate">{l.application || l.user}</div>
                  <div className="text-[11px] leading-snug text-muted-foreground">
                    {l.mode} on {l.lock_type}
                  </div>
                </TableCell>
                <TableCell>
                  <Sql>{l.query}</Sql>
                </TableCell>
                <TableCell className="text-warning-foreground tabular-nums">{l.blocked_by.length ? `pid ${l.blocked_by.join(", ")}` : dash}</TableCell>
                <Num>{fmtMs(l.waiting_ms)}</Num>
              </TableRow>
            ))}
          </TableBody>
        </Rows>
      </Listing>

      <Statements s={s.pg_stat_statements} />

      <Listing title="Biggest tables" hint={s.table_count > s.tables.length ? `${s.tables.length} of ${fmtInt(s.table_count)}, by total size` : "by total size"} count={s.tables.length} none="This database has no tables.">
        <Rows>
          <TableHeader>
            <TableRow>
              <TableHead>Table</TableHead>
              <TableHead className="w-[78px] text-right">Rows, est.</TableHead>
              <TableHead className="w-[132px]">Size</TableHead>
              <TableHead className="w-[76px] text-right">Indexes</TableHead>
              <TableHead className="w-[76px] text-right">Full scans</TableHead>
              <TableHead className="w-[84px] text-right">Index scans</TableHead>
              <TableHead className="w-[76px] text-right">Dead rows</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {s.tables.map((t) => (
              <TableRow key={`${t.schema}.${t.name}`}>
                <TableCell className="selectable truncate font-mono text-[11.5px]">{t.schema === "public" ? t.name : `${t.schema}.${t.name}`}</TableCell>
                <Num>{fmtCount(t.rows)}</Num>
                <TableCell>
                  <div className="flex items-center gap-2">
                    <Meter value={t.total_bytes} max={maxTable} tone="blue" />
                    <span className="w-14 shrink-0 text-right tabular-nums">{fmtBytes(t.total_bytes)}</span>
                  </div>
                </TableCell>
                <Num className="text-muted-foreground">{fmtBytes(t.index_bytes)}</Num>
                <Num>{fmtCount(t.seq_scans)}</Num>
                <Num>{fmtCount(t.index_scans)}</Num>
                <Num className={t.dead_rows > t.rows * 0.2 && t.dead_rows > 1000 ? "text-warning-foreground" : "text-muted-foreground"}>{fmtCount(t.dead_rows)}</Num>
              </TableRow>
            ))}
          </TableBody>
        </Rows>
      </Listing>

      <Listing title="Largest indexes" hint="and how often each was used" count={s.indexes.largest.length} none="This database has no indexes.">
        <Rows>
          <TableHeader>
            <TableRow>
              <TableHead>Index</TableHead>
              <TableHead className="w-[132px]">Size</TableHead>
              <TableHead className="w-[76px] text-right">Scans</TableHead>
              <TableHead className="w-[96px]" />
            </TableRow>
          </TableHeader>
          <TableBody>
            {s.indexes.largest.map((i) => (
              <TableRow key={`${i.schema}.${i.name}`}>
                <TableCell>
                  <div className="selectable truncate font-mono text-[11.5px]">{i.name}</div>
                  <div className="truncate text-[11px] text-muted-foreground">on {i.table}</div>
                </TableCell>
                <TableCell>
                  <div className="flex items-center gap-2">
                    <Meter value={i.bytes} max={maxIndex} tone="blue" />
                    <span className="w-14 shrink-0 text-right tabular-nums">{fmtBytes(i.bytes)}</span>
                  </div>
                </TableCell>
                <Num>{fmtCount(i.scans)}</Num>
                <TableCell className="text-right text-[11px]">
                  {i.unused ? <span className="text-warning-foreground">never used</span> : <span className="text-muted-foreground">{i.primary ? "primary key" : i.unique ? "unique" : ""}</span>}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Rows>
      </Listing>
    </>
  );
}

/**
 * Statements is pg_stat_statements in each of its states: there with totals,
 * there with nothing recorded yet, and not there, with which step is missing
 * and how to take it (Leadline takes none of them itself).
 */
function Statements({ s }: { s: PgStatStatements }) {
  const title = "Costliest statements";
  if (!s.available) {
    const reason =
      s.reason === "not_loaded"
        ? "pg_stat_statements isn't loaded"
        : s.reason === "not_installed"
          ? "pg_stat_statements is loaded, but not created in this database"
          : s.reason === "not_available"
            ? "this Postgres has no pg_stat_statements"
            : "pg_stat_statements couldn't be asked";
    return (
      <Surface render={<section />} className="px-3 py-2.5">
        <div className="flex items-baseline gap-2">
          <h2 className="shrink-0 text-xs font-medium">{title}</h2>
          <span className="truncate text-[11px] text-muted-foreground">{reason}</span>
        </div>
        <p className={`selectable mt-1.5 text-xs ${s.reason === "error" ? "text-destructive-foreground" : ""}`}>{s.message}</p>
        {s.how_to && s.how_to.length > 0 && (
          <ol className="selectable mt-2 flex list-decimal flex-col gap-1 pl-4 text-[11.5px] text-muted-foreground marker:text-muted-foreground">
            {s.how_to.map((step, i) => (
              <li key={i} className="pl-0.5">
                <HowTo text={step} />
              </li>
            ))}
          </ol>
        )}
      </Surface>
    );
  }
  const max = Math.max(0.001, ...s.statements.map((q) => q.total_ms));
  return (
    <Listing
      title={title}
      hint={`pg_stat_statements${s.version ? ` ${s.version}` : ""}, every client's, since Postgres started`}
      count={s.statements.length}
      none="pg_stat_statements is on and has recorded nothing yet. Run a query and it shows here."
    >
      <Rows>
        <TableHeader>
          <TableRow>
            <TableHead>Statement</TableHead>
            <TableHead className="w-[64px] text-right">Calls</TableHead>
            <TableHead className="w-[72px] text-right">Mean</TableHead>
            <TableHead className="w-[64px] text-right">Rows</TableHead>
            <TableHead className="w-[76px] text-right">Cache hits</TableHead>
            <TableHead className="w-[132px]">Total time</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {s.statements.map((q, i) => (
            <TableRow key={i} className="[&>td]:align-top">
              <TableCell>
                <Sql>{q.query}</Sql>
              </TableCell>
              <Num>{fmtCount(q.calls)}</Num>
              <Num>{fmtMs(q.mean_ms)}</Num>
              <Num>{fmtCount(q.rows)}</Num>
              <Num className="text-muted-foreground">{q.cache_hit_rate == null ? dash : fmtPct(q.cache_hit_rate)}</Num>
              <TableCell>
                <div className="flex items-center gap-2">
                  <Meter value={q.total_ms} max={max} tone="blue" />
                  <span className="w-14 shrink-0 text-right tabular-nums">{fmtMs(q.total_ms)}</span>
                </div>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Rows>
    </Listing>
  );
}

/** HowTo is a step with what is to be typed set apart: the daemon writes those after a colon. */
function HowTo({ text }: { text: string }) {
  const at = text.lastIndexOf(": ");
  const tail = at > 0 ? text.slice(at + 2) : "";
  // Something to type has no sentence in it: one run of settings or SQL.
  if (!tail || !/^[-\w]/.test(tail) || !/[=;]|^-c /.test(tail)) return <>{text}</>;
  return (
    <>
      {text.slice(0, at + 1)}
      <CopyLine text={tail} label="Copy this" className="mt-1 mb-0.5" />
    </>
  );
}

function Redis({ s }: { s: RedisStats }) {
  const maxCmd = Math.max(0.000001, ...s.commands.map((c) => c.total_ms));
  const maxKey = Math.max(1, ...s.biggest_keys.keys.map((k) => k.bytes));
  const k = s.keyspace;
  return (
    <>
      <Figures className="@3xl:grid-cols-6 grid-cols-3">
        <Figure
          label="Memory"
          value={fmtBytes(s.memory.used_bytes)}
          note={s.memory.max_bytes > 0 ? `of ${fmtBytes(s.memory.max_bytes)}, ${s.memory.policy}` : `no limit, peak ${fmtBytes(s.memory.peak_bytes)}`}
          tone={s.memory.max_bytes > 0 && s.memory.used_bytes > s.memory.max_bytes * 0.9 ? "warn" : undefined}
        />
        <Figure label="Clients" value={`${s.clients.connected} of ${fmtCount(s.clients.max)}`} note={s.clients.blocked > 0 ? `${s.clients.blocked} blocked` : "one is Leadline"} />
        <Figure label="Commands a second" value={fmtCount(s.ops_per_sec)} note={`${fmtCount(s.total_commands)} since it started`} />
        <Figure label="Keys" value={fmtCount(k.keys)} note={k.evicted > 0 ? `${fmtCount(k.evicted)} evicted` : `${fmtCount(k.expired)} expired`} tone={k.evicted > 0 ? "warn" : undefined} />
        <Figure label="Hit rate" value={k.hit_rate == null ? "–" : fmtPct(k.hit_rate)} note={`${fmtCount(k.hits)} hits, ${fmtCount(k.misses)} misses`} />
        <Figure label="Up for" value={fmtFor(s.uptime_s * 1000)} note={s.mode} />
      </Figures>

      <Listing title="Commands" hint="where the server's time went, since it started" count={s.commands.length} none="No command has run yet.">
        <Rows>
          <TableHeader>
            <TableRow>
              <TableHead>Command</TableHead>
              <TableHead className="w-[76px] text-right">Calls</TableHead>
              <TableHead className="w-[76px] text-right">Average</TableHead>
              <TableHead className="w-[148px]">Total time</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {s.commands.map((c) => (
              <TableRow key={c.command}>
                <TableCell className="selectable font-mono text-[11.5px]">{c.command}</TableCell>
                <Num>{fmtCount(c.calls)}</Num>
                <Num>{fmtMs(c.avg_ms)}</Num>
                <TableCell>
                  <div className="flex items-center gap-2">
                    <Meter value={c.total_ms} max={maxCmd} tone="blue" />
                    <span className="w-14 shrink-0 text-right tabular-nums">{fmtMs(c.total_ms)}</span>
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Rows>
      </Listing>

      <Listing
        title="Slow log"
        hint={s.slow_log.threshold_ms == null ? "newest first; the server wouldn't say how slow counts" : `commands that took ${fmtMs(s.slow_log.threshold_ms)} or more, newest first`}
        count={s.slow_log.entries.length}
        none="Nothing in the slow log."
      >
        <Rows>
          <TableHeader>
            <TableRow>
              <TableHead className="w-[104px]">At</TableHead>
              <TableHead>Command</TableHead>
              <TableHead className="w-[132px]">Client</TableHead>
              <TableHead className="w-[76px] text-right">Took</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {s.slow_log.entries.map((e) => (
              <TableRow key={e.id}>
                <TableCell className="font-mono text-[11px] text-muted-foreground tabular-nums">{fmtClock(e.at_ms).slice(0, 8)}</TableCell>
                <TableCell className="selectable truncate font-mono text-[11.5px]">{e.command}</TableCell>
                <TableCell className="truncate font-mono text-[11px] text-muted-foreground">{e.client}</TableCell>
                <Num>{fmtMs(e.duration_ms)}</Num>
              </TableRow>
            ))}
          </TableBody>
        </Rows>
      </Listing>

      <Listing
        title="Biggest keys"
        hint={
          <span className="inline-flex items-center gap-1">
            {s.biggest_keys.complete && <CheckIcon aria-hidden className="size-3" />}
            {s.biggest_keys.note || `in database ${s.biggest_keys.db}`}
          </span>
        }
        count={s.biggest_keys.keys.length}
        none={`Database ${s.biggest_keys.db} has no keys.`}
      >
        <Rows>
          <TableHeader>
            <TableRow>
              <TableHead>Key</TableHead>
              <TableHead className="w-[76px]">Type</TableHead>
              <TableHead className="w-[148px]">Size</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {s.biggest_keys.keys.map((key) => (
              <TableRow key={key.key}>
                <TableCell className="selectable truncate font-mono text-[11.5px]">{key.key}</TableCell>
                <TableCell className="text-muted-foreground">{key.type}</TableCell>
                <TableCell>
                  <div className="flex items-center gap-2">
                    <Meter value={key.bytes} max={maxKey} tone="blue" />
                    <span className="w-14 shrink-0 text-right tabular-nums">{fmtBytes(key.bytes)}</span>
                  </div>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Rows>
      </Listing>

      {s.databases.length > 1 && (
        <Listing title="Databases" hint="those with keys" count={s.databases.length} none="">
          <Rows>
            <TableHeader>
              <TableRow>
                <TableHead>Database</TableHead>
                <TableHead className="w-[96px] text-right">Keys</TableHead>
                <TableHead className="w-[96px] text-right">With a TTL</TableHead>
                <TableHead className="w-[112px] text-right">Average TTL</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {s.databases.map((d) => (
                <TableRow key={d.db}>
                  <TableCell className="tabular-nums">{d.db}</TableCell>
                  <Num>{fmtCount(d.keys)}</Num>
                  <Num>{fmtCount(d.expiring)}</Num>
                  <Num className="text-muted-foreground">{d.avg_ttl_ms > 0 ? fmtFor(d.avg_ttl_ms) : dash}</Num>
                </TableRow>
              ))}
            </TableBody>
          </Rows>
        </Listing>
      )}
    </>
  );
}
