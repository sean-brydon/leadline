import { useEffect, useRef, useState, type ReactNode, type RefObject } from "react";
import { api, type Connection, type EndpointKey, type GroupedSeries, type QueryKey, type QueryRow, type Scope } from "@/lib/api";
import { fmtClock, fmtCount, fmtMs, fmtMsShort, fmtPct, fmtPer, fmtRate, fmtSpan } from "@/lib/format";
import { useLive } from "@/lib/live";
import { chartNote, groupsInDb } from "@/lib/scope";
import { captureWord, kindVersion, versionOf } from "@/lib/sources";
import { Chart, type Line } from "@/components/Chart";
import { Heatmap } from "@/components/Heatmap";
import { CHART_SLOT, DB_INK, DbBadge, dbName, Loaded, Meter, Panel, RowButton, Stat, Surface, Waiting } from "@/components/ui";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";

const BUCKETS = 90;
const TOP = 5;
// The big chart, so that with its legend it is as tall as the figures beside it.
const BIG = 196;
// One trace running the same query this many times reads as an N+1.
const N_PLUS_ONE = 5;
const WARN = 13; // OTLP severity: warn and up
const sum = (xs: number[]) => xs.reduce((a, b) => a + b, 0);

type Props = {
  scope: () => Scope;
  rangeMs: number;
  /** The saved connection the window is narrowed to, when it is narrowed to one. */
  conn?: Connection;
  /** Open an endpoint, a query or a trace in its own view. */
  onEndpoint?: (e: EndpointKey) => void;
  onQuery?: (q: QueryKey) => void;
  onTrace?: (id: string) => void;
  onLogs?: () => void;
  /** Open a connection's stats. */
  onStats?: (id: string) => void;
};

// A colour for each line of a split chart. Status classes say how a request
// went, so they take the status colours. A database system or a service is
// neither good nor bad: it takes a series colour, and keeps it (a system's
// is DB_INK, its badge's too).
const SERIES = ["--series-1", "--series-2", "--series-3", "--series-4", "--series-5"];
const STATUS_COLOR: Record<string, string> = {
  "1xx": "--muted",
  "2xx": "--accent",
  "3xx": "--blue",
  "4xx": "--warn",
  "5xx": "--err",
  other: "--muted",
};
const STATUS_ORDER = ["2xx", "3xx", "1xx", "other", "4xx", "5xx"];

// split turns groups into a chart's lines. A group with no colour of its own
// takes the first series colour nobody has; past the last, the rest are grey.
function split(g: GroupedSeries, colors: Record<string, string>, kind: Line["kind"]): Line[] {
  const taken = new Set(g.groups.map((grp) => colors[grp.key]));
  const free = SERIES.filter((c) => !taken.has(c));
  return g.groups.map((grp) => ({ label: grp.key, values: grp.values, color: colors[grp.key] ?? free.shift() ?? "--muted", kind }));
}

const LOG_TONE: Record<string, string> = { warn: "text-warning-foreground", error: "text-destructive-foreground", fatal: "text-destructive-foreground" };

/**
 * Overview is the window at a glance: how much is being asked and by which
 * app, how it is going, where the time goes, and what was just logged.
 * Narrowed to a database it is that database's calls instead (DbOverview).
 */
export function Overview(props: Props) {
  return props.scope().db ? <DbOverview {...props} /> : <AppOverview {...props} />;
}

function AppOverview({ scope, rangeMs, onEndpoint, onQuery, onTrace, onLogs }: Props) {
  const service = scope().service;
  const deps = [rangeMs, service];
  // Requests are counted where a call arrives at a service, as Endpoints counts them.
  const byApp = useLive(() => api.breakdown(scope(), "service", BUCKETS, 8), deps);
  const before = useLive(() => {
    const s = scope();
    return api.breakdown({ ...s, sinceMs: s.sinceMs - rangeMs }, "service", 1, 20, s.sinceMs);
  }, deps);
  const whole = useLive(() => api.series(scope(), Date.now(), 1), deps);
  const endpoints = useLive(() => api.endpoints(scope(), { limit: 1000 }), deps);
  const queries = useLive(() => api.queries(scope(), ""), deps);
  const dbTime = useLive(() => api.breakdown(scope(), "db_time", 1), deps);
  const logs = useLive(() => api.logs(scope(), "", WARN, undefined, 6), deps);
  if (!byApp.data) return <Waiting error={byApp.error} />;

  const requests = sum(byApp.data.groups.map((g) => g.total));
  const was = before.data ? sum(before.data.groups.map((g) => g.total)) : undefined;
  const perBucket = `per ${fmtSpan(byApp.data.bucket_ms)}`;
  const w = whole.data;
  const rootRequests = w?.requests[0] ?? 0;
  const queryCount = w?.queries[0] ?? 0;
  // Every endpoint is in hand unless there are more than were asked for.
  const e = endpoints.data;
  const allEndpoints = e !== undefined && e.total <= e.rows.length;
  const failed = e ? sum(e.rows.map((r) => r.errors)) : 0;
  const counted = e ? sum(e.rows.map((r) => r.requests)) : 0;
  const slowest = e?.rows.reduce<(typeof e.rows)[number] | undefined>((a, r) => (!a || r.p95_ms > a.p95_ms ? r : a), undefined);
  const repeats = (queries.data ?? []).filter((q) => q.max_per_trace >= N_PLUS_ONE).length;
  const top = (queries.data ?? []).slice(0, TOP);
  const maxQuery = Math.max(0.001, ...top.map((q) => q.total_ms));
  const noRequests = "No requests in this window.";
  const noQueries = "No database calls in this window.";

  return (
    <div className="@container h-full overflow-y-auto">
      <div className="grid grid-cols-12 gap-3 p-3">
        <div className="col-span-12 grid @3xl:col-span-8">
          <Panel title="Requests" hint={service ? perBucket : `${perBucket}, by app`}>
            <Loaded data={byApp.data} empty={(d) => d.groups.length === 0} none={noRequests} height={BIG + 26}>
              {(d) => <Chart t={d.t} bucketMs={d.bucket_ms} height={BIG} format={fmtCount} lines={split(d, {}, d.groups.length === 1 ? "area" : "line")} />}
            </Loaded>
          </Panel>
        </div>

        <div className="col-span-12 grid grid-cols-4 gap-3 @3xl:col-span-4 @3xl:grid-cols-2">
          <Stat label="Requests" value={fmtCount(requests)} note={trend(requests, was) ?? (requests > 0 ? `${fmtRate(requests / (rangeMs / 60_000)).replace("/m", "")} a minute` : "")} />
          {rootRequests > 0 || !slowest ? (
            <Stat label="p95 latency" value={rootRequests > 0 && w ? fmtMs(w.p95_ms[0]) : "–"} note={rootRequests > 0 && w ? `p50 ${fmtMs(w.p50_ms[0])}` : ""} />
          ) : (
            // Latency over everything is measured on the requests that start a trace. A service that is only ever called by another has none, so its slowest endpoint speaks for it.
            <Stat label="p95, slowest endpoint" value={fmtMs(slowest.p95_ms)} note={slowest.name} />
          )}
          {allEndpoints || !e ? (
            <Stat label="Failed" value={counted > 0 ? fmtPct(failed / counted) : "–"} tone={failed > 0 ? "err" : undefined} note={counted > 0 ? `${fmtCount(failed)} ${failed === 1 ? "request" : "requests"}` : ""} />
          ) : (
            <Stat label="Failed spans" value={w ? fmtCount(w.errors[0]) : "–"} tone={w && w.errors[0] > 0 ? "err" : undefined} note="" />
          )}
          <Stat
            label="Queries a request"
            value={requests > 0 && w ? fmtPer(queryCount / requests) : "–"}
            note={repeats > 0 ? <span className="text-warning-foreground">{`${repeats} N+1 ${repeats === 1 ? "query" : "queries"}`}</span> : queryCount > 0 ? `${fmtCount(queryCount)} queries` : ""}
          />
          <DbSplit data={dbTime.data} error={dbTime.error} className="col-span-4 @3xl:col-span-2" />
        </div>

        <div className="col-span-12 @3xl:col-span-6">
          <Panel title="Slowest endpoints" hint="by share of all request time">
            <Loaded data={e} error={endpoints.error} empty={(d) => d.rows.length === 0} none={noRequests} height={TOP * 28}>
              {(d) => (
                <Ranked>
                  {d.rows.slice(0, TOP).map((r) => (
                    <RankRow key={`${r.service}\n${r.name}`} onOpen={onEndpoint && (() => onEndpoint({ service: r.service, name: r.name }))}>
                      <span className="min-w-0 flex-1 truncate">
                        {r.name} {!service && <span className="text-[11px] text-muted-foreground">{r.service}</span>}
                      </span>
                      {r.errors > 0 && <span className="shrink-0 text-[11px] text-destructive-foreground tabular-nums">{fmtPct(r.errors / r.requests)} failed</span>}
                      <span className="w-16 shrink-0 text-right text-[11px] text-muted-foreground tabular-nums">p95 {fmtMsShort(r.p95_ms)}</span>
                      <span className="flex w-20 shrink-0 items-center">
                        <Meter value={r.share} />
                      </span>
                      <span className="w-9 shrink-0 text-right tabular-nums">{fmtPct(r.share)}</span>
                    </RankRow>
                  ))}
                </Ranked>
              )}
            </Loaded>
          </Panel>
        </div>
        <div className="col-span-12 @3xl:col-span-6">
          <Panel title="Costliest queries" hint="by total time in the database">
            <Loaded data={queries.data} error={queries.error} empty={(d) => d.length === 0} none={noQueries} height={TOP * 28}>
              {() => (
                <Ranked>
                  {top.map((q) => (
                    <QueryLine key={`${q.db_system}\n${q.fingerprint}`} q={q} max={maxQuery} badge onQuery={onQuery} />
                  ))}
                </Ranked>
              )}
            </Loaded>
          </Panel>
        </div>

        <div className="col-span-12">
          <Surface render={<section />}>
            <header className="flex items-baseline gap-2 px-3 pt-2.5">
              <h2 className="shrink-0 text-xs font-medium">Just logged</h2>
              <span className="truncate text-[11px] text-muted-foreground">warnings and errors, newest first</span>
              {onLogs && (
                <Button variant="link" size="xs" onClick={onLogs} className="ml-auto h-auto px-0 text-[11px] font-normal text-muted-foreground hover:text-foreground sm:h-auto sm:text-[11px]">
                  All logs
                </Button>
              )}
            </header>
            <div className="p-2">
              <Loaded data={logs.data} error={logs.error} empty={(d) => d.length === 0} none="No warnings or errors in this window." height={48}>
                {(d) => (
                  <div className="flex flex-col font-mono text-[11.5px]">
                    {d.map((l) => {
                      const cells = (
                        <>
                          <span className="w-[88px] shrink-0 text-muted-foreground tabular-nums">{fmtClock(l.ts_ms)}</span>
                          <span className={`w-10 shrink-0 ${LOG_TONE[l.severity] ?? "text-muted-foreground"}`}>{l.severity}</span>
                          <span className="w-28 shrink-0 truncate text-muted-foreground">{l.service}</span>
                          <span className="min-w-0 flex-1 truncate">{l.body}</span>
                        </>
                      );
                      const cls = "h-6 gap-3 rounded-md px-1.5 text-[11.5px]";
                      return l.trace_id && onTrace ? (
                        <RowButton key={l.cursor} onClick={() => onTrace(l.trace_id)} className={cls}>
                          {cells}
                        </RowButton>
                      ) : (
                        <div key={l.cursor} className={`selectable flex items-center ${cls}`}>
                          {cells}
                        </div>
                      );
                    })}
                  </div>
                )}
              </Loaded>
            </div>
          </Surface>
        </div>

        <More scope={scope} deps={deps} />
      </div>
    </div>
  );
}

/** trend says how a count moved against the window before: "up 12%". Nothing when there is no before to compare with. */
function trend(now: number, was: number | undefined): string | undefined {
  // A handful of requests before is no baseline: the first minutes of a session would read "up 26,000%".
  if (was === undefined || was < 20 || now <= 0) return undefined;
  const by = (now - was) / was;
  if (Math.abs(by) < 0.005) return "unchanged";
  if (by > 9.99) return "up many times over";
  return `${by > 0 ? "up" : "down"} ${fmtPct(Math.abs(by))}`;
}

/** DbSplit is the time spent in databases as one bar, cut by system. */
function DbSplit({ data, error, className }: { data: GroupedSeries | undefined; error: string | null; className?: string }) {
  const groups = data?.groups ?? [];
  const total = sum(groups.map((g) => g.total));
  return (
    <Surface className={`justify-center px-3 py-2.5 ${className ?? ""}`}>
      <div className="flex items-baseline justify-between gap-2 text-[11px] text-muted-foreground">
        <span>Time in databases</span>
        {total > 0 && <span className="text-foreground tabular-nums">{fmtMs(total)}</span>}
      </div>
      {!data ? (
        <div className={`mt-2 text-[11px] ${error ? "text-destructive-foreground" : "text-muted-foreground"}`}>{error ?? "Loading…"}</div>
      ) : total <= 0 ? (
        <div className="mt-2 text-[11px] text-muted-foreground">No database calls in this window.</div>
      ) : (
        <>
          <div role="img" aria-label={groups.map((g) => `${dbName(g.key)} ${fmtPct(g.total / total)}`).join(", ")} className="mt-2 flex h-2 gap-0.5 overflow-hidden rounded-full">
            {groups.map((g) => (
              <span key={g.key} className="h-full min-w-0.5 rounded-[1px]" style={{ flexGrow: g.total, flexBasis: 0, background: `var(${DB_INK[g.key] ?? "--muted"})` }} />
            ))}
          </div>
          <div className="mt-2 flex flex-wrap gap-x-3 gap-y-0.5 text-[11px] text-muted-foreground tabular-nums">
            {groups.map((g) => (
              <span key={g.key} className="flex items-center gap-1.5 whitespace-nowrap">
                <span className="size-2 rounded-[2px]" style={{ background: `var(${DB_INK[g.key] ?? "--muted"})` }} />
                {dbName(g.key)} {fmtMs(g.total)}
              </span>
            ))}
          </div>
        </>
      )}
    </Surface>
  );
}

type QueryLineProps = {
  q: QueryRow;
  max: number;
  /** Say which system it ran on. */
  badge?: boolean;
  /** Rank by calls: the bar and the number are the count. */
  calls?: number;
  onQuery?: (q: QueryKey) => void;
};

function QueryLine({ q, max, badge, calls, onQuery }: QueryLineProps) {
  return (
    <RankRow onOpen={onQuery && (() => onQuery({ db_system: q.db_system, fingerprint: q.fingerprint }))}>
      {badge && <DbBadge system={q.db_system} />}
      <span className="min-w-0 flex-1 truncate font-mono text-[11.5px]">{q.fingerprint}</span>
      {q.max_per_trace >= N_PLUS_ONE ? (
        <Badge variant="warning" className="tabular-nums">
          N+1 ×{q.max_per_trace}
        </Badge>
      ) : (
        <span className="shrink-0 text-right text-[11px] text-muted-foreground tabular-nums">{calls === undefined ? `${fmtCount(q.calls)}×` : fmtMs(q.avg_ms)}</span>
      )}
      <span className="flex w-20 shrink-0 items-center">
        <Meter value={calls ?? q.total_ms} max={max} tone="blue" />
      </span>
      <span className="w-14 shrink-0 text-right tabular-nums">{calls === undefined ? fmtMs(q.total_ms) : `${fmtCount(calls)}×`}</span>
    </RankRow>
  );
}

/** useSeen says whether an element has come into view; once it has, it stays so. */
function useSeen(): [RefObject<HTMLDivElement | null>, boolean] {
  const ref = useRef<HTMLDivElement>(null);
  const [seen, setSeen] = useState(false);
  useEffect(() => {
    const el = ref.current;
    if (!el || seen) return;
    const io = new IntersectionObserver(([e]) => e.isIntersecting && setSeen(true), { rootMargin: "120px" });
    io.observe(el);
    return () => io.disconnect();
  }, [seen]);
  return [ref, seen];
}

/**
 * More is the charts under the fold: latency and responses over time, and
 * the time in each database. They are asked for only once scrolled to, so a
 * glance at the overview doesn't pay for them.
 */
function More({ scope, deps }: { scope: () => Scope; deps: unknown[] }) {
  const [ref, seen] = useSeen();
  return (
    <div ref={ref} className="col-span-12 grid grid-cols-1 gap-3 @3xl:grid-cols-2" style={seen ? undefined : { minHeight: 2 * (CHART_SLOT + 46) }}>
      {seen && <MoreCharts scope={scope} deps={deps} />}
    </div>
  );
}

function MoreCharts({ scope, deps }: { scope: () => Scope; deps: unknown[] }) {
  const s = useLive(() => api.series(scope(), Date.now(), BUCKETS), deps);
  const heat = useLive(() => api.heatmap(scope(), BUCKETS), deps);
  const status = useLive(() => api.breakdown(scope(), "status", BUCKETS), deps);
  const dbTime = useLive(() => api.breakdown(scope(), "db_time", BUCKETS), deps);
  const noRequests = "No requests in this window.";
  return (
    <>
      <Panel title="Request latency" hint="root spans">
        <Loaded data={s.data} error={s.error} empty={(d) => sum(d.requests) === 0} none={noRequests}>
          {(d) => (
            <Chart
              t={d.t}
              bucketMs={d.bucket_ms}
              format={fmtMs}
              axisFormat={fmtMsShort}
              lines={[
                { label: "p95", values: d.p95_ms, color: "--violet", kind: "area" },
                { label: "p50", values: d.p50_ms, color: "--blue" },
              ]}
            />
          )}
        </Loaded>
      </Panel>
      <Panel title="Latency over time" hint="each cell counts the requests that took that long">
        <Loaded data={heat.data} error={heat.error} empty={(d) => d.n.length === 0} none={noRequests}>
          {(d) => <Heatmap data={d} />}
        </Loaded>
      </Panel>
      <Panel title="Responses" hint="by status">
        <Loaded data={status.data} error={status.error} empty={(d) => d.groups.length === 0} none={noRequests}>
          {(d) => {
            // Stacked from the healthy classes up, so trouble sits on top.
            const groups = [...d.groups].sort((a, b) => STATUS_ORDER.indexOf(a.key) - STATUS_ORDER.indexOf(b.key));
            return <Chart t={d.t} bucketMs={d.bucket_ms} format={fmtCount} stack lines={split({ ...d, groups }, STATUS_COLOR, "bars")} />;
          }}
        </Loaded>
      </Panel>
      <Panel title="Time in databases" hint="by system">
        <Loaded data={dbTime.data} error={dbTime.error} empty={(d) => d.groups.length === 0} none="No database calls in this window.">
          {(d) => <Chart t={d.t} bucketMs={d.bucket_ms} format={fmtMs} axisFormat={fmtMsShort} lines={split(d, DB_INK, "line")} />}
        </Loaded>
      </Panel>
    </>
  );
}

/**
 * DbOverview is the overview narrowed to a database: the calls that reached
 * that server. The figures and the two lists are asked of the daemon for it
 * alone. The chart is cut by system, which is all the daemon's grouped
 * series can do, so it says so when another server of that system is in it.
 */
function DbOverview({ scope, rangeMs, conn, onQuery, onStats }: Props) {
  const { db, dbServer } = scope();
  const deps = [rangeMs, db, dbServer];
  const calls = useLive(() => api.breakdown(scope(), "db_calls", BUCKETS, 20), deps);
  const queries = useLive(() => api.queries(scope(), "", { limit: 2000 }), deps);
  // BEGIN, COMMIT and their like are time in the database too, though not queries.
  const control = useLive(() => api.queries(scope(), "", { control: true, limit: 2000 }), deps);
  if (!calls.data) return <Waiting error={calls.error} />;

  const name = dbName(db);
  const mine = groupsInDb(calls.data.groups, db);
  const charted = sum(mine.map((g) => g.total));
  const rows = queries.data;
  const count = rows ? sum(rows.map((r) => r.calls)) : undefined;
  const spent = rows ? sum(rows.map((r) => r.total_ms)) + sum((control.data ?? []).map((r) => r.total_ms)) : undefined;
  const failed = rows ? sum(rows.map((r) => r.errors)) : 0;
  const repeats = (rows ?? []).filter((q) => q.max_per_trace >= N_PLUS_ONE).length;
  const costly = (rows ?? []).slice(0, TOP);
  const busy = [...(rows ?? [])].sort((a, b) => b.calls - a.calls).slice(0, TOP);
  const none = `No ${name} calls ${conn ? `to ${conn.name} ` : ""}in this window.`;
  const perBucket = `per ${fmtSpan(calls.data.bucket_ms)}`;
  // MONITOR says when a Redis command ran, never how long it took.
  const untimed = db === "redis" && conn?.capture;
  const others = conn && count !== undefined ? chartNote(conn.name, name, charted, count) : null;

  return (
    <div className="@container h-full overflow-y-auto">
      <div className="grid grid-cols-12 gap-3 p-3">
        <div className="col-span-12 grid @3xl:col-span-8">
          <Panel title={`Calls to ${name}`} hint={others ? `${perBucket}, every ${name} server` : perBucket}>
            <Loaded
              data={calls.data}
              empty={() => charted === 0}
              none={conn && !conn.capture && conn.capture_offer.available ? `No ${name} calls in this window. Turn on capture under Sources to record every ${captureWord(conn.kind)}.` : `No ${name} calls in this window.`}
              height={BIG + 26}
            >
              {(d) => <Chart t={d.t} bucketMs={d.bucket_ms} height={BIG} format={fmtCount} lines={split({ ...d, groups: mine }, DB_INK, "bars")} />}
            </Loaded>
          </Panel>
        </div>
        <div className="col-span-12 grid grid-cols-4 gap-3 @3xl:col-span-4 @3xl:grid-cols-2">
          <Stat label="Calls" value={count === undefined ? "–" : fmtCount(count)} note={count ? `${fmtRate(count / (rangeMs / 60_000)).replace("/m", "")} a minute` : ""} />
          <Stat
            label={`Time in ${name}`}
            value={spent === undefined ? "–" : fmtMs(spent)}
            note={untimed ? "captured commands have no duration" : count && spent !== undefined ? `${fmtMs(spent / count)} a call` : ""}
          />
          <Stat label="Failed" value={rows ? fmtCount(failed) : "–"} tone={failed > 0 ? "err" : undefined} note={rows && count ? fmtPct(failed / count) : ""} />
          <Stat
            label="Shapes"
            value={rows ? fmtCount(rows.length) : "–"}
            note={repeats > 0 ? <span className="text-warning-foreground">{`${repeats} N+1 ${repeats === 1 ? "query" : "queries"}`}</span> : "distinct statements"}
          />
          <Surface className="col-span-4 justify-center gap-1 px-3 py-2.5 @3xl:col-span-2">
            {conn ? (
              <>
                <div className="flex items-center gap-2">
                  <span className="min-w-0 flex-1 truncate">
                    {kindVersion(conn.kind, versionOf(conn.health))} <span className="text-muted-foreground">{conn.remote ? `at ${conn.host}` : "on this Mac"}</span>
                  </span>
                  {onStats && (
                    <Button variant="outline" size="xs" onClick={() => onStats(conn.id)} className="shrink-0 font-normal">
                      Its stats
                    </Button>
                  )}
                </div>
                <div role={others ? "note" : undefined} className="text-[11px] leading-snug text-pretty text-muted-foreground">
                  {others ??
                    (conn.capture
                      ? `Every ${captureWord(conn.kind)} of ${conn.name} is being captured, at ${conn.host}.`
                      : `${conn.name} isn't captured, so its calls are here only when an app reports them, and says they went to ${conn.host}.`)}
                </div>
              </>
            ) : (
              <div className="text-[11px] text-muted-foreground">Every {name} call Leadline holds is counted here.</div>
            )}
          </Surface>
        </div>

        <div className="col-span-12 @3xl:col-span-6">
          <Panel title="Costliest queries" hint="by total time">
            <Loaded data={rows} error={queries.error} empty={(d) => d.length === 0} none={none} height={TOP * 28}>
              {() => {
                const max = Math.max(0.001, ...costly.map((q) => q.total_ms));
                return (
                  <Ranked>
                    {costly.map((q) => (
                      <QueryLine key={q.fingerprint} q={q} max={max} onQuery={onQuery} />
                    ))}
                  </Ranked>
                );
              }}
            </Loaded>
          </Panel>
        </div>
        <div className="col-span-12 @3xl:col-span-6">
          <Panel title="Most called" hint="with the average time of a call">
            <Loaded data={rows} error={queries.error} empty={(d) => d.length === 0} none={none} height={TOP * 28}>
              {() => {
                const max = Math.max(1, ...busy.map((q) => q.calls));
                return (
                  <Ranked>
                    {busy.map((q) => (
                      <QueryLine key={q.fingerprint} q={q} max={max} calls={q.calls} onQuery={onQuery} />
                    ))}
                  </Ranked>
                );
              }}
            </Loaded>
          </Panel>
        </div>
      </div>
    </div>
  );
}

/** Ranked is a panel's worth of rows, as tall as five whether or not there are five. */
function Ranked({ children }: { children: ReactNode }) {
  return (
    <div className="flex flex-col" style={{ minHeight: TOP * 28 }}>
      {children}
    </div>
  );
}

function RankRow({ onOpen, children }: { onOpen?: () => void; children: ReactNode }) {
  const cls = "h-7 gap-2.5 rounded-md px-1.5";
  return onOpen ? (
    <RowButton onClick={onOpen} className={cls}>
      {children}
    </RowButton>
  ) : (
    <div className={`flex w-full items-center text-left text-xs ${cls}`}>{children}</div>
  );
}
