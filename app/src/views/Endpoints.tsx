import { useEffect, useMemo, useRef, useState, type RefObject } from "react";
import { api, type EndpointKey, type EndpointRow, type EndpointSort, type QueryKey, type QuietRoute, type Scope } from "@/lib/api";
import { fmtCount, fmtMs, fmtMsShort, fmtPct, fmtPer, fmtSpan } from "@/lib/format";
import { useDebounced, useLive } from "@/lib/live";
import { quietRoutes } from "@/lib/routes";
import { scopeKey } from "@/lib/scope";
import { Chart } from "@/components/Chart";
import { Heatmap } from "@/components/Heatmap";
import { Histogram } from "@/components/Histogram";
import { Sparkline } from "@/components/Sparkline";
import { TraceList } from "@/components/TraceList";
import { SendButton } from "@/components/SendToShipyard";
import { Bar, DbBadge, Empty, Figure, Figures, Listing, Loaded, Meter, Panel, PickRow, Rows, Search, Side, SortTh, Waiting } from "@/components/ui";
import { Button } from "@/components/ui/button";
import { TableBody, TableCaption, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

type Props = {
  scope: () => Scope;
  rangeMs: number;
  selected: EndpointKey | null;
  onSelect: (e: EndpointKey | null) => void;
  onTrace: (id: string) => void;
  onQuery?: (q: QueryKey) => void;
};

const SPARK = 24;
const BUCKETS = 48;
// One request running the same query this many times reads as an N+1.
const N_PLUS_ONE = 5;

// The wide list has nine columns of numbers. In a window near its narrowest
// they would leave the endpoint's name no room at all, so below this width
// the three that say least (the sparkline, p50, the maximum) step aside.
const ROOMY_FROM = 1000;

/** useRoomy says whether an element is at least `from` pixels wide, and keeps saying as it is resized. */
function useRoomy(from: number): [RefObject<HTMLDivElement | null>, boolean] {
  const ref = useRef<HTMLDivElement>(null);
  const [roomy, setRoomy] = useState(true);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const see = () => setRoomy(el.clientWidth >= from);
    see();
    const ro = new ResizeObserver(see);
    ro.observe(el);
    return () => ro.disconnect();
  }, [from]);
  return [ref, roomy];
}

// How many never-hit routes are listed before the rest fold behind a button.
const QUIET_FIRST = 12;

const same = (a: EndpointKey | null, b: EndpointKey) => a !== null && a.service === b.service && a.name === b.name;

/**
 * Endpoints lists what the services were asked to do: requests grouped by
 * service and name, with where the time went. A row opens the endpoint's
 * own charts, queries and traces beside the list.
 */
export function Endpoints({ scope, rangeMs, selected, onSelect, onTrace, onQuery }: Props) {
  const [search, setSearch] = useState("");
  const q = useDebounced(search);
  const [sort, setSort] = useState<EndpointSort>("time");
  const [asc, setAsc] = useState(false);
  const { data, error } = useLive(
    () => api.endpoints(scope(), { q, sort, asc, spark: SPARK }),
    [rangeMs, scopeKey(scope()), q, sort, asc],
  );
  const wide = selected === null;
  const [listRef, roomy] = useRoomy(ROOMY_FROM);
  // Every column, or without the three a narrow window has no room for.
  const full = wide && roomy;
  const db = scope().db !== "";
  // The routes the app has that nobody asked for. Narrowed to a database they say nothing: no request is what they are.
  const quiet = useMemo(() => (db ? [] : quietRoutes(data?.never_hit ?? [], q)), [data?.never_hit, q, db]);
  const [allQuiet, setAllQuiet] = useState(false);
  const cols = full ? 10 : wide ? 7 : 4;

  const onSort = (id: EndpointSort) => {
    if (id === sort) setAsc(!asc);
    else {
      setSort(id);
      // Names read best A to Z; every number, largest first.
      setAsc(id === "name");
    }
  };
  const th = { sort, asc, onSort };

  return (
    <div className="flex h-full">
      <div ref={listRef} className={`flex min-w-0 flex-col ${wide ? "flex-1" : "w-[38%] border-r"}`}>
        <Bar>
          <Search value={search} onChange={setSearch} placeholder="Filter by name" />
          <span className={`text-[11px] text-muted-foreground ${wide ? "truncate" : "shrink-0"}`}>
            {data ? `${data.rows.length}${data.total > data.rows.length ? ` of ${fmtCount(data.total)}` : ""} ${data.rows.length === 1 && data.total <= 1 ? "endpoint" : "endpoints"}` : ""}
            {wide && data && data.rows.length > 0 ? (db ? " · only the requests that sent this database a call" : " · requests by service and name") : ""}
            {wide && data && quiet.length > 0 ? ` · ${quiet.length} never hit` : ""}
          </span>
        </Bar>
        {!data ? (
          <Waiting error={error} />
        ) : data.rows.length === 0 && quiet.length === 0 ? (
          <Empty>{q ? "No endpoint matches in this window." : db ? "No request in this window sent this database a call." : "No requests in this window."}</Empty>
        ) : (
          <Rows scroll>
            {/* A caption comes first in a table; it is drawn under the rows. */}
            {wide && (
              <TableCaption className="mt-0 border-t px-3 py-2 text-left text-[11px]">
                Errors are requests in which any span failed. Queries and In DB are averages per request.
              </TableCaption>
            )}
            <TableHeader>
              <TableRow>
                <SortTh id="name" {...th}>
                  Endpoint
                </SortTh>
                {full && <TableHead className="w-[112px]">Rate, per {fmtSpan(data.bucket_ms)}</TableHead>}
                <SortTh id="requests" {...th} className={`text-right ${wide ? "w-[84px]" : "w-[76px]"}`}>
                  Requests
                </SortTh>
                {wide && (
                  <>
                    <SortTh id="errors" {...th} className="w-[72px] text-right">
                      Errors
                    </SortTh>
                    {full && (
                      <SortTh id="p50" {...th} className="w-[76px] text-right">
                        p50
                      </SortTh>
                    )}
                  </>
                )}
                <SortTh id="p95" {...th} className={`text-right ${wide ? "w-[76px]" : "w-[70px]"}`}>
                  p95
                </SortTh>
                {wide && (
                  <>
                    {full && (
                      <SortTh id="max" {...th} className="w-[76px] text-right">
                        Max
                      </SortTh>
                    )}
                    <SortTh id="queries" {...th} className="w-[84px] text-right">
                      Queries
                    </SortTh>
                    <SortTh id="db" {...th} className="w-[84px] text-right">
                      In DB
                    </SortTh>
                  </>
                )}
                <SortTh id="time" {...th} className={wide ? "w-[168px]" : "w-[88px]"}>
                  Time
                </SortTh>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.rows.map((r) => (
                <Row key={`${r.service}\n${r.name}`} r={r} wide={wide} full={full} on={same(selected, r)} onSelect={onSelect} />
              ))}
              {data.rows.length === 0 && (
                <TableRow className="hover:bg-transparent">
                  <TableCell colSpan={cols} className="py-2.5! text-muted-foreground">
                    {q ? "No endpoint with requests matches." : "No requests in this window."}
                  </TableCell>
                </TableRow>
              )}
              {quiet.length > 0 && (
                <>
                  <TableRow className="hover:bg-transparent">
                    <TableHead colSpan={cols} scope="colgroup" className="h-7! bg-muted/50 text-[11px] tracking-wide">
                      Never hit in the last {fmtSpan(rangeMs)}
                      <span className="pl-2 tracking-normal">{wide ? "routes the app has that no request asked for" : ""}</span>
                    </TableHead>
                  </TableRow>
                  {(allQuiet ? quiet : quiet.slice(0, QUIET_FIRST)).map((r) => (
                    <QuietRow key={`${r.service}\n${r.kind}\n${r.route}`} r={r} cols={cols} wide={wide} full={full} />
                  ))}
                  {quiet.length > QUIET_FIRST && (
                    <TableRow className="hover:bg-transparent">
                      <TableCell colSpan={cols} className="py-1!">
                        <Button variant="ghost" size="xs" onClick={() => setAllQuiet(!allQuiet)} aria-expanded={allQuiet} className="-ml-2 font-normal text-muted-foreground">
                          {allQuiet ? "Show fewer" : `Show all ${quiet.length}`}
                        </Button>
                      </TableCell>
                    </TableRow>
                  )}
                </>
              )}
            </TableBody>
          </Rows>
        )}
      </div>
      {selected && (
        <Detail
          key={`${selected.service}\n${selected.name}`}
          endpoint={selected}
          scope={scope}
          rangeMs={rangeMs}
          onClose={() => onSelect(null)}
          onTrace={onTrace}
          onQuery={onQuery}
        />
      )}
    </div>
  );
}

function Row({ r, wide, full, on, onSelect }: { r: EndpointRow; wide: boolean; /** With the columns only a roomy window has. */ full: boolean; on: boolean; onSelect: (e: EndpointKey | null) => void }) {
  const toggle = () => onSelect(on ? null : { service: r.service, name: r.name });
  return (
    <PickRow on={on} onPick={toggle}>
      <TableCell>
        <div className="flex min-w-0 items-center gap-1.5">
          <span className="truncate">{r.name}</span>
          {r.kind && <KindTag kind={r.kind} />}
        </div>
        <div className="truncate text-[11px] text-muted-foreground">{r.service}</div>
      </TableCell>
      {full && (
        <TableCell>
          <Sparkline values={r.spark ?? []} label={`Request rate of ${r.name}`} />
        </TableCell>
      )}
      <TableCell className="text-right tabular-nums">{fmtCount(r.requests)}</TableCell>
      {wide && (
        <>
          <TableCell className={`text-right tabular-nums ${r.errors > 0 ? "text-destructive-foreground" : "text-muted-foreground"}`}>
            {fmtPct(r.errors / r.requests)}
          </TableCell>
          {full && <TableCell className="text-right tabular-nums">{fmtMs(r.p50_ms)}</TableCell>}
        </>
      )}
      <TableCell className="text-right tabular-nums">{fmtMs(r.p95_ms)}</TableCell>
      {wide && (
        <>
          {full && <TableCell className="text-right tabular-nums">{fmtMs(r.max_ms)}</TableCell>}
          <TableCell className="text-right tabular-nums">{fmtPer(r.queries)}</TableCell>
          <TableCell className="text-right tabular-nums">{r.db_ms > 0 ? fmtMs(r.db_ms) : <span className="text-muted-foreground">–</span>}</TableCell>
        </>
      )}
      <TableCell>
        <div className="flex items-center gap-2">
          <Meter value={r.share} />
          <span className="w-9 shrink-0 text-right tabular-nums">{fmtPct(r.share)}</span>
          {wide && <span className="w-14 shrink-0 text-right text-muted-foreground tabular-nums">{fmtMs(r.total_ms)}</span>}
        </div>
      </TableCell>
    </PickRow>
  );
}

/** KindTag says what a framework calls an endpoint or a route: page, action, route, api. */
function KindTag({ kind }: { kind: string }) {
  return <span className="shrink-0 rounded-sm border px-1 text-[10px] leading-[14px] text-muted-foreground">{kind}</span>;
}

/** QuietRow is a route nobody asked for: its name, what it is, and the file it lives in. It has no numbers and opens nothing. */
function QuietRow({ r, cols, wide, full }: { r: QuietRoute; cols: number; wide: boolean; full: boolean }) {
  // The name's cell: with the sparkline's beside it when that column is there.
  const name = full ? 2 : 1;
  return (
    <TableRow className="hover:bg-transparent">
      <TableCell colSpan={wide ? name : cols}>
        <div className="flex min-w-0 items-center gap-1.5 text-muted-foreground">
          <span className="selectable truncate text-foreground/80">
            {r.methods?.length ? `${r.methods.join(", ")} ` : ""}
            {r.route}
          </span>
          <KindTag kind={r.kind} />
        </div>
        <div className="truncate text-[11px] text-muted-foreground">{wide ? r.service : [r.service, r.file].filter(Boolean).join(" · ")}</div>
      </TableCell>
      {wide && (
        <TableCell colSpan={cols - name} className="text-muted-foreground">
          <span className="selectable font-mono text-[11px]">{r.file}</span>
          {r.router && <span className="pl-2 text-[11px]">{r.router === "app" ? "App Router" : r.router === "pages" ? "Pages Router" : r.router}</span>}
        </TableCell>
      )}
    </TableRow>
  );
}

type DetailProps = {
  endpoint: EndpointKey;
  scope: () => Scope;
  rangeMs: number;
  onClose: () => void;
  onTrace: (id: string) => void;
  onQuery?: (q: QueryKey) => void;
};

function Detail({ endpoint, scope, rangeMs, onClose, onTrace, onQuery }: DetailProps) {
  const { data, error } = useLive(() => api.endpoint(scope(), endpoint, BUCKETS), [rangeMs, scopeKey(scope()), endpoint.service, endpoint.name]);
  const s = data?.stats;
  // Narrowed to a database, the figures and the two charts over time count only the requests that called it. The daemon's distributions can't be cut that way, and say so.
  const db = scope().db !== "";
  const every = "all requests, whichever database";
  const series = data?.series;
  const per = series ? `per ${fmtSpan(series.bucket_ms)}` : "";
  const none = db ? "No request to this endpoint sent this database a call in this window." : "No requests in this window.";
  const quiet = () => !s;
  const maxQuery = Math.max(0.001, ...(data?.queries ?? []).map((q) => q.total_ms));

  return (
    <Side
      onClose={onClose}
      label="Close endpoint"
      title={
        <>
          <span className="selectable truncate font-medium">{endpoint.name}</span>
          <span className="shrink-0 text-[11px] text-muted-foreground">{endpoint.service}</span>
          <SendButton words ask={{ kind: "endpoint", service: endpoint.service, name: endpoint.name }} label="Send this endpoint to Shipyard: start a coding agent on it" className="ml-auto" />
        </>
      }
    >
      {!data ? (
        <Waiting error={error} />
      ) : (
        <div className="flex flex-col gap-3">
          <Figures className="@3xl:grid-cols-8">
            <Figure label="Requests" value={s ? fmtCount(s.requests) : "0"} />
            <Figure
              label="Errors"
              value={s ? fmtPct(s.errors / s.requests) : "–"}
              note={s && s.errors > 0 ? `${fmtCount(s.errors)} failed` : undefined}
              tone={s && s.errors > 0 ? "err" : undefined}
            />
            <Figure label="p50" value={s ? fmtMs(s.p50_ms) : "–"} />
            <Figure label="p95" value={s ? fmtMs(s.p95_ms) : "–"} />
            <Figure label="Max" value={s ? fmtMs(s.max_ms) : "–"} />
            <Figure label="Queries" value={s ? fmtPer(s.queries) : "–"} note="per request" />
            <Figure label="In databases" value={s ? fmtMs(s.db_ms) : "–"} note={s && s.avg_ms > 0 ? `${fmtPct(s.db_ms / s.avg_ms)} of request` : undefined} />
            <Figure label="Share of time" value={s ? fmtPct(s.share) : "–"} note={s ? fmtMs(s.total_ms) : undefined} />
          </Figures>

          <div className="grid grid-cols-1 gap-3 @xl:grid-cols-2">
            <Panel title="Requests" hint={per}>
              <Loaded data={data} empty={quiet} none={none}>
                {(d) => (
                  <Chart
                    t={d.series.t}
                    bucketMs={d.series.bucket_ms}
                    format={fmtCount}
                    lines={[
                      { label: "Requests", values: d.series.requests, color: "--accent", kind: "bars" },
                      { label: "With errors", values: d.series.errors, color: "--err", kind: "bars" },
                    ]}
                  />
                )}
              </Loaded>
            </Panel>
            <Panel title="Latency" hint="p50 and p95">
              <Loaded data={data} empty={quiet} none={none}>
                {(d) => (
                  <Chart
                    t={d.series.t}
                    bucketMs={d.series.bucket_ms}
                    format={fmtMs}
                    axisFormat={fmtMsShort}
                    lines={[
                      { label: "p95", values: d.series.p95_ms, color: "--violet", kind: "area" },
                      { label: "p50", values: d.series.p50_ms, color: "--blue" },
                    ]}
                  />
                )}
              </Loaded>
            </Panel>
            <Panel title="Latency distribution" hint={db ? every : "requests by how long they took"}>
              <Loaded data={data} empty={quiet} none={none}>
                {(d) => (
                  <Histogram
                    data={d.histogram}
                    marks={
                      d.stats
                        ? [
                            { label: "p50", ms: d.stats.p50_ms, color: "--blue" },
                            { label: "p95", ms: d.stats.p95_ms, color: "--violet" },
                          ]
                        : []
                    }
                  />
                )}
              </Loaded>
            </Panel>
            <Panel title="Latency over time" hint={db ? every : "each cell counts requests"}>
              <Loaded data={data} empty={quiet} none={none}>
                {(d) => <Heatmap data={d.heatmap} />}
              </Loaded>
            </Panel>
            <Panel title="Where the time goes" hint="average per request">
              <Loaded data={data} empty={quiet} none={none}>
                {(d) => (
                  <Chart
                    t={d.series.t}
                    bucketMs={d.series.bucket_ms}
                    format={fmtMs}
                    axisFormat={fmtMsShort}
                    lines={[
                      { label: "Whole request", values: d.series.avg_ms, color: "--muted" },
                      { label: "In databases", values: d.series.db_ms, color: "--blue", kind: "area" },
                    ]}
                  />
                )}
              </Loaded>
            </Panel>
            <Panel title="Queries per request" hint="average">
              <Loaded data={data} empty={quiet} none={none}>
                {(d) => (
                  <Chart
                    t={d.series.t}
                    bucketMs={d.series.bucket_ms}
                    format={fmtPer}
                    lines={[{ label: "Queries", values: d.series.queries, color: "--blue", kind: "area" }]}
                  />
                )}
              </Loaded>
            </Panel>
          </div>

          <Listing
            title="Queries"
            hint="what a request runs, costliest first"
            count={data.queries.length}
            none="This endpoint ran no database calls in this window."
          >
            <Rows>
              <TableHeader>
                <TableRow>
                  <TableHead>Query</TableHead>
                  <TableHead className="w-[92px] text-right">Per request</TableHead>
                  <TableHead className="w-[68px] text-right">Avg</TableHead>
                  <TableHead className="w-[132px]">Total time</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.queries.map((r) => {
                  const repeated = r.max_per_request >= N_PLUS_ONE;
                  return (
                    <PickRow
                      key={`${r.db_system}\n${r.fingerprint}`}
                      onPick={onQuery && (() => onQuery({ db_system: r.db_system, fingerprint: r.fingerprint }))}
                      className="[&>td]:align-top"
                    >
                      <TableCell>
                        <div className="flex min-w-0 items-center gap-2">
                          <DbBadge system={r.db_system} />
                          <span className="selectable truncate font-mono text-[11.5px]">{r.fingerprint}</span>
                        </div>
                        {repeated && (
                          <div className="mt-0.5 flex items-center gap-2 text-[11px] text-warning-foreground">
                            <span className="truncate">N+1: up to {r.max_per_request} times in one request</span>
                            <SendButton ask={{ kind: "query", db_system: r.db_system, fingerprint: r.fingerprint }} label="Send this N+1 to Shipyard" className="-my-0.5" />
                          </div>
                        )}
                      </TableCell>
                      <TableCell className={`text-right tabular-nums ${repeated ? "font-medium text-warning-foreground" : ""}`}>×{fmtPer(r.per_request)}</TableCell>
                      <TableCell className="text-right tabular-nums">{fmtMs(r.avg_ms)}</TableCell>
                      <TableCell>
                        <div className="flex items-center gap-2">
                          <Meter value={r.total_ms} max={maxQuery} tone="blue" />
                          <span className="w-14 shrink-0 text-right tabular-nums">{fmtMs(r.total_ms)}</span>
                        </div>
                      </TableCell>
                    </PickRow>
                  );
                })}
              </TableBody>
            </Rows>
          </Listing>

          <div className="grid grid-cols-1 gap-3 @xl:grid-cols-2">
            <Listing title="Slowest requests" count={data.slowest.length} none={none}>
              <TraceList
                onTrace={onTrace}
                rows={data.slowest.map((r) => ({
                  trace_id: r.trace_id,
                  start_ms: r.start_ms,
                  ms: r.duration_ms,
                  failed: r.failed,
                  note: r.queries > 0 ? `${r.queries} queries` : undefined,
                }))}
              />
            </Listing>
            <Listing title="Failing requests" hint="newest first" count={data.failing.length} none="No request to this endpoint failed in this window.">
              <TraceList
                onTrace={onTrace}
                rows={data.failing.map((r) => ({
                  trace_id: r.trace_id,
                  start_ms: r.start_ms,
                  ms: r.duration_ms,
                  failed: r.failed,
                  note: r.queries > 0 ? `${r.queries} queries` : undefined,
                }))}
              />
            </Listing>
          </div>
        </div>
      )}
    </Side>
  );
}
