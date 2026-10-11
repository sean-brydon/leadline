import { useState } from "react";
import { api, type EndpointKey, type QueryKey, type QueryRow, type Scope } from "@/lib/api";
import { fmtClock, fmtCount, fmtMs, fmtMsShort, fmtPct, fmtPer, fmtSpan } from "@/lib/format";
import { useDebounced, useLive } from "@/lib/live";
import { scopeKey } from "@/lib/scope";
import { Chart } from "@/components/Chart";
import { Heatmap } from "@/components/Heatmap";
import { Histogram } from "@/components/Histogram";
import { TraceList } from "@/components/TraceList";
import { SendButton } from "@/components/SendToShipyard";
import { Bar, DbBadge, dbName, Empty, Figure, Figures, Listing, Loaded, Meter, Panel, PickRow, Rows, Search, Side, Tip, Waiting } from "@/components/ui";
import { Badge } from "@/components/ui/badge";
import { Toggle } from "@/components/ui/toggle";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

// One trace running the same query this many times reads as an N+1.
const N_PLUS_ONE = 5;
const BUCKETS = 48;

type Props = {
  scope: () => Scope;
  rangeMs: number;
  /** The query whose detail is open. Left out, the view keeps track itself. */
  selected?: QueryKey | null;
  onSelect?: (q: QueryKey | null) => void;
  onTrace?: (id: string) => void;
  onEndpoint?: (e: EndpointKey) => void;
};

const same = (a: QueryKey | null, b: QueryKey) => a !== null && a.db_system === b.db_system && a.fingerprint === b.fingerprint;

export function Queries({ scope, rangeMs, selected, onSelect, onTrace, onEndpoint }: Props) {
  const [search, setSearch] = useState("");
  const q = useDebounced(search);
  const [own, setOwn] = useState<QueryKey | null>(null);
  const open = selected === undefined ? own : selected;
  const setOpen = onSelect ?? setOwn;
  // BEGIN, COMMIT, SET and their like are kept out of the queries; this lists them instead.
  const [control, setControl] = useState(false);
  const db = scope().db;
  const { data, error } = useLive(() => api.queries(scope(), q, { control }), [rangeMs, scopeKey(scope()), q, control]);
  const maxTotal = Math.max(1, ...(data ?? []).map((r) => r.total_ms));
  const wide = open === null;
  const listed = open && data?.find((r) => same(open, r));

  return (
    <div className="flex h-full">
      <div className={`flex min-w-0 flex-col ${wide ? "flex-1" : "w-[38%] border-r"}`}>
        <Bar>
          <Search value={search} onChange={setSearch} placeholder="Filter by SQL text" />
          {wide && (
            <span className="truncate text-[11px] text-muted-foreground">
              {control ? "Transaction and session control: in database time, not among the queries" : "Grouped by shape, costliest first"}
            </span>
          )}
          <Tip label="BEGIN, COMMIT, SET and their like: list them in place of the queries">
            <Toggle size="sm" pressed={control} onPressedChange={setControl} className="ml-auto shrink-0 px-2 text-xs font-normal text-muted-foreground data-pressed:text-foreground sm:h-6 sm:text-xs">
              Control
            </Toggle>
          </Tip>
        </Bar>
        {!data ? (
          <Waiting error={error} />
        ) : data.length === 0 ? (
          <Empty>
            {control
              ? `No ${db ? `${dbName(db)} ` : ""}transaction or session control statements in this window.`
              : db
                ? `No ${dbName(db)} calls ${scope().dbServer ? `to ${scope().dbServer} ` : ""}in this window.`
                : "No database calls in this window."}
          </Empty>
        ) : (
          <Rows scroll>
            <TableHeader>
              <TableRow>
                {wide && <TableHead className="w-24">Database</TableHead>}
                <TableHead>Query</TableHead>
                <TableHead className="w-16 text-right">Calls</TableHead>
                {wide && (
                  <>
                    <TableHead className="w-20 text-right">Avg</TableHead>
                    <TableHead className="w-20 text-right">p95</TableHead>
                    <TableHead className="w-20 text-right">Max</TableHead>
                  </>
                )}
                <TableHead className={wide ? "w-40" : "w-[72px] text-right"}>{wide ? "Total time" : "Total"}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.map((r) => (
                <Row
                  key={`${r.db_system}\n${r.fingerprint}`}
                  r={r}
                  wide={wide}
                  on={same(open, r)}
                  maxTotal={maxTotal}
                  onToggle={() => setOpen(same(open, r) ? null : { db_system: r.db_system, fingerprint: r.fingerprint })}
                />
              ))}
            </TableBody>
          </Rows>
        )}
      </div>
      {open && (
        <Detail
          key={`${open.db_system}\n${open.fingerprint}`}
          query={open}
          scope={scope}
          rangeMs={rangeMs}
          listed={listed || undefined}
          onClose={() => setOpen(null)}
          onTrace={onTrace}
          onEndpoint={onEndpoint}
        />
      )}
    </div>
  );
}

function Row({ r, wide, on, maxTotal, onToggle }: { r: QueryRow; wide: boolean; on: boolean; maxTotal: number; onToggle: () => void }) {
  const repeated = r.max_per_trace >= N_PLUS_ONE;
  return (
    <PickRow on={on} onPick={onToggle} className="[&>td]:align-top">
      {wide && (
        <TableCell>
          <DbBadge system={r.db_system} />
        </TableCell>
      )}
      <TableCell className="selectable font-mono text-[11.5px]">
        <div className="truncate">{r.fingerprint}</div>
        {(!wide || repeated || r.errors > 0) && (
          <div className="mt-0.5 flex min-w-0 items-center gap-2 font-sans text-[11px] text-muted-foreground">
            {!wide && <span className="shrink-0">{r.db_system}</span>}
            {repeated &&
              (wide ? (
                <span className="truncate text-warning-foreground">N+1: up to {r.max_per_trace} times in one request</span>
              ) : (
                <Badge variant="warning" className="tabular-nums">
                  N+1 ×{r.max_per_trace}
                </Badge>
              ))}
            {repeated && <SendButton ask={{ kind: "query", db_system: r.db_system, fingerprint: r.fingerprint }} label="Send this N+1 to Shipyard" className="-my-0.5" />}
            {r.errors > 0 && <span className="shrink-0 text-destructive-foreground">{fmtCount(r.errors)} failed</span>}
          </div>
        )}
      </TableCell>
      <TableCell className="text-right tabular-nums">{fmtCount(r.calls)}</TableCell>
      {wide && (
        <>
          <TableCell className="text-right tabular-nums">{fmtMs(r.avg_ms)}</TableCell>
          <TableCell className="text-right tabular-nums">{fmtMs(r.p95_ms)}</TableCell>
          <TableCell className="text-right tabular-nums">{fmtMs(r.max_ms)}</TableCell>
        </>
      )}
      <TableCell>
        {wide ? (
          <div className="flex items-center gap-2">
            <Meter value={r.total_ms} max={maxTotal} />
            <span className="w-14 shrink-0 text-right tabular-nums">{fmtMs(r.total_ms)}</span>
          </div>
        ) : (
          <div className="text-right tabular-nums">{fmtMs(r.total_ms)}</div>
        )}
      </TableCell>
    </PickRow>
  );
}

type DetailProps = {
  query: QueryKey;
  scope: () => Scope;
  rangeMs: number;
  /** The query's row in the list, which is narrowed to the scope's database server; this detail is not. */
  listed?: QueryRow;
  onClose: () => void;
  onTrace?: (id: string) => void;
  onEndpoint?: (e: EndpointKey) => void;
};

function Detail({ query, scope, rangeMs, listed, onClose, onTrace, onEndpoint }: DetailProps) {
  const { data, error } = useLive(() => api.query(scope(), query, BUCKETS), [rangeMs, scope().service, query.db_system, query.fingerprint]);
  const s = data?.stats;
  // Narrowed to one server, the list counts its calls alone; the daemon can't narrow this detail the same way.
  const server = scope().dbServer;
  const elsewhere = server && s && listed && s.calls - listed.calls > Math.max(2, s.calls * 0.02) ? s.calls - listed.calls : 0;
  const per = data ? `per ${fmtSpan(data.series.bucket_ms)}` : "";
  const none = "This query didn't run in this window.";
  const quiet = () => !s;
  const maxEndpoint = Math.max(0.001, ...(data?.endpoints ?? []).map((e) => e.total_ms));

  return (
    <Side
      onClose={onClose}
      label="Close query"
      title={
        <>
          <DbBadge system={query.db_system} />
          <span className="selectable truncate font-mono text-[11.5px]">{query.fingerprint}</span>
          <SendButton words ask={{ kind: "query", db_system: query.db_system, fingerprint: query.fingerprint }} label="Send this query to Shipyard: start a coding agent on it" className="ml-auto" />
        </>
      }
    >
      {!data ? (
        <Waiting error={error} />
      ) : (
        <div className="flex flex-col gap-3">
          {elsewhere > 0 && s && listed && (
            <p role="note" className="rounded-lg bg-muted/50 px-3 py-1.5 text-[11.5px] text-muted-foreground">
              These figures count this statement on every {dbName(query.db_system)} server: {fmtCount(s.calls)} calls, of which {fmtCount(listed.calls)} reached {server}.
            </p>
          )}
          <Figures className="@3xl:grid-cols-8">
            <Figure label="Calls" value={s ? fmtCount(s.calls) : "0"} note={s ? `in ${fmtCount(s.traces)} traces` : undefined} />
            <Figure
              label="Most in a trace"
              value={s ? `×${fmtCount(s.max_per_trace)}` : "–"}
              note={s && s.max_per_trace >= N_PLUS_ONE ? "N+1" : undefined}
              tone={s && s.max_per_trace >= N_PLUS_ONE ? "warn" : undefined}
            />
            <Figure label="Avg" value={s ? fmtMs(s.avg_ms) : "–"} />
            <Figure label="p50" value={s ? fmtMs(s.p50_ms) : "–"} />
            <Figure label="p95" value={s ? fmtMs(s.p95_ms) : "–"} />
            <Figure label="Max" value={s ? fmtMs(s.max_ms) : "–"} />
            <Figure
              label="Failed"
              value={s ? fmtCount(s.errors) : "–"}
              note={s && s.errors > 0 ? fmtPct(s.errors / s.calls) : undefined}
              tone={s && s.errors > 0 ? "err" : undefined}
            />
            <Figure label="Share of DB time" value={s ? fmtPct(s.share) : "–"} note={s ? fmtMs(s.total_ms) : undefined} />
          </Figures>

          <Listing
            title="Statement"
            hint={s ? `the slowest call's, last run at ${fmtClock(s.last_ms)}` : undefined}
            count={s ? 1 : 0}
            none={none}
          >
            <pre className="selectable mx-3 mb-3 max-h-56 overflow-y-auto rounded-md bg-muted p-2 text-[11.5px] break-words whitespace-pre-wrap">
              {s?.example}
            </pre>
          </Listing>

          <div className="grid grid-cols-1 gap-3 @xl:grid-cols-2">
            <Panel title="Calls" hint={per}>
              <Loaded data={data} empty={quiet} none={none}>
                {(d) => (
                  <Chart
                    t={d.series.t}
                    bucketMs={d.series.bucket_ms}
                    format={fmtCount}
                    lines={[
                      { label: "Calls", values: d.series.calls, color: "--blue", kind: "bars" },
                      { label: "Failed", values: d.series.errors, color: "--err", kind: "bars" },
                    ]}
                  />
                )}
              </Loaded>
            </Panel>
            <Panel title="Latency" hint="average and p95">
              <Loaded data={data} empty={quiet} none={none}>
                {(d) => (
                  <Chart
                    t={d.series.t}
                    bucketMs={d.series.bucket_ms}
                    format={fmtMs}
                    axisFormat={fmtMsShort}
                    lines={[
                      { label: "p95", values: d.series.p95_ms, color: "--violet", kind: "area" },
                      { label: "Avg", values: d.series.avg_ms, color: "--blue" },
                    ]}
                  />
                )}
              </Loaded>
            </Panel>
            <Panel title="Latency distribution" hint="calls by how long they took">
              <Loaded data={data} empty={quiet} none={none}>
                {(d) => (
                  <Histogram
                    data={d.histogram}
                    unit="Calls"
                    color="--blue"
                    marks={
                      d.stats
                        ? [
                            { label: "p50", ms: d.stats.p50_ms, color: "--accent" },
                            { label: "p95", ms: d.stats.p95_ms, color: "--violet" },
                          ]
                        : []
                    }
                  />
                )}
              </Loaded>
            </Panel>
            <Panel title="Time in this query" hint={per}>
              <Loaded data={data} empty={quiet} none={none}>
                {(d) => (
                  <Chart
                    t={d.series.t}
                    bucketMs={d.series.bucket_ms}
                    format={fmtMs}
                    axisFormat={fmtMsShort}
                    lines={[{ label: "Total", values: d.series.total_ms, color: "--blue", kind: "area" }]}
                  />
                )}
              </Loaded>
            </Panel>
            <div className="@xl:col-span-2">
              <Panel title="Latency over time" hint="each cell counts calls">
                <Loaded data={data} empty={quiet} none={none}>
                  {(d) => <Heatmap data={d.heatmap} color="--blue" unit="calls" />}
                </Loaded>
              </Panel>
            </div>
          </div>

          <Listing
            title="Endpoints"
            hint="the requests that run it"
            count={data.endpoints.length}
            none={s ? "No request that began in this window ran it." : none}
          >
            <Rows>
              <TableHeader>
                <TableRow>
                  <TableHead>Endpoint</TableHead>
                  <TableHead className="w-[92px] text-right">Per request</TableHead>
                  <TableHead className="w-[84px] text-right">Runs in</TableHead>
                  <TableHead className="w-[132px]">Total time</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.endpoints.map((e) => {
                  const repeated = e.max_per_request >= N_PLUS_ONE;
                  return (
                    <PickRow
                      key={`${e.service}\n${e.name}`}
                      onPick={onEndpoint && (() => onEndpoint({ service: e.service, name: e.name }))}
                      className="[&>td]:align-top"
                    >
                      <TableCell>
                        <div className="truncate">
                          {e.name} <span className="text-[11px] text-muted-foreground">{e.service}</span>
                        </div>
                        {repeated && <div className="mt-0.5 text-[11px] text-warning-foreground">N+1: up to {e.max_per_request} times in one request</div>}
                      </TableCell>
                      <TableCell className={`text-right tabular-nums ${repeated ? "font-medium text-warning-foreground" : ""}`}>×{fmtPer(e.per_request)}</TableCell>
                      <TableCell className="text-right tabular-nums">{fmtPct(e.requests / e.endpoint_requests)}</TableCell>
                      <TableCell>
                        <div className="flex items-center gap-2">
                          <Meter value={e.total_ms} max={maxEndpoint} tone="blue" />
                          <span className="w-14 shrink-0 text-right tabular-nums">{fmtMs(e.total_ms)}</span>
                        </div>
                      </TableCell>
                    </PickRow>
                  );
                })}
              </TableBody>
            </Rows>
          </Listing>

          <Listing title="Slowest traces" hint="by time spent in this query" count={data.traces.length} none={none}>
            <TraceList
              onTrace={onTrace ?? (() => {})}
              rows={data.traces.map((t) => ({
                trace_id: t.trace_id,
                start_ms: t.start_ms,
                ms: t.total_ms,
                failed: t.errors > 0,
                name: t.name,
                note: t.calls > 1 ? `${t.calls} calls, slowest ${fmtMs(t.slowest_ms)}` : undefined,
              }))}
            />
          </Listing>
        </div>
      )}
    </Side>
  );
}
