import { useEffect, useRef, useState, type RefObject } from "react";
import { api, type MetricGroup, type MetricInfo, type MetricSeries, type Scope } from "@/lib/api";
import { fmtCount, fmtSpan } from "@/lib/format";
import { useDebounced, useLive } from "@/lib/live";
import { chartScale, fmtNum, kindLabel, metricFormat, unitLabel } from "@/lib/metricFormat";
import { Chart, type Line } from "@/components/Chart";
import { Bar, Empty, Figure, Figures as FigureCard, Hold, Listing, Loading, Meter, Panel, PickOne, Rows, Search, Side, Surface, Waiting } from "@/components/ui";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

type Props = { scope: () => Scope; rangeMs: number };

const TILE_BUCKETS = 60;
const TILE_CHART = 96;
// A tile's chart with its legend, so a tile is as tall before its data arrives.
const TILE_SLOT = TILE_CHART + 26;
const BUCKETS = 90;
// One colour per line of a split chart, largest group first.
const COLORS = ["--accent", "--blue", "--violet", "--warn", "--ok", "--muted"];

/**
 * Metrics shows what the apps measure about themselves: every metric as a
 * small chart, and one of them large beside the grid, split by service or
 * by one of its attributes.
 */
export function Metrics({ scope, rangeMs }: Props) {
  const [search, setSearch] = useState("");
  const q = useDebounced(search);
  const [open, setOpen] = useState<string | null>(null);
  const { data, error } = useLive(() => api.metrics(scope(), q), [rangeMs, scope().service, q]);

  return (
    <div className="flex h-full">
      <div className={`flex min-w-0 flex-col ${open === null ? "flex-1" : "w-[38%] border-r"}`}>
        <Bar>
          <Search value={search} onChange={setSearch} placeholder="Filter by name" />
          <span className="truncate text-[11px] text-muted-foreground">{data ? `${data.length} ${data.length === 1 ? "metric" : "metrics"}` : ""}</span>
        </Bar>
        {!data ? (
          <Waiting error={error} />
        ) : data.length === 0 ? (
          <Empty>
            {q ? (
              "No metric by that name in this window."
            ) : (
              "No metrics in this window. An app sends them when its OpenTelemetry SDK has a metrics exporter, usually once a minute."
            )}
          </Empty>
        ) : (
          <div className="min-h-0 flex-1 overflow-y-auto p-3">
            <div className="grid grid-cols-[repeat(auto-fill,minmax(300px,1fr))] gap-3">
              {data.map((m) => (
                <Tile
                  key={m.name}
                  metric={m}
                  scope={scope}
                  rangeMs={rangeMs}
                  open={m.name === open}
                  onOpen={() => setOpen(m.name === open ? null : m.name)}
                />
              ))}
            </div>
          </div>
        )}
      </div>
      {open !== null && (
        <Detail
          key={open}
          name={open}
          info={data?.find((m) => m.name === open)}
          scope={scope}
          rangeMs={rangeMs}
          onClose={() => setOpen(null)}
        />
      )}
    </div>
  );
}

/** useOnScreen says whether an element is in view, or near enough to be soon. */
function useOnScreen(ref: RefObject<Element | null>): boolean {
  const [on, setOn] = useState(false);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const io = new IntersectionObserver(([e]) => setOn(e.isIntersecting), { rootMargin: "200px" });
    io.observe(el);
    return () => io.disconnect();
  }, [ref]);
  return on;
}

/** describe is the small print under a metric's name: what kind it is and what it counts. */
function describe(m: { type: string; monotonic: boolean; unit: string }): string {
  const unit = unitLabel(m.unit);
  return unit ? `${kindLabel(m)} · ${unit}` : kindLabel(m);
}

/**
 * together says how a level's series were made one line, when there are
 * several: a gauge's are averaged, an up-down counter's add up. Counters
 * and histograms always add up, which needs no saying.
 */
function together(m: MetricInfo | undefined): string | undefined {
  if (!m || m.kind !== "gauge" || m.series < 2) return undefined;
  return `${m.type === "sum" ? "total" : "average"} of ${m.series} series`;
}

const present = (vs: (number | null)[]) => vs.filter((v): v is number => v !== null);

/** plot readies a metric's lines for a chart: how to write the values, with sizes all in one unit. */
function plot(s: { unit: string; name: string }, lines: Line[]): { lines: Line[]; format: (v: number) => string; axisFormat: (v: number) => string } {
  const scale = chartScale(s.unit, Math.max(0, ...lines.flatMap((l) => present(l.values))));
  if (!scale) return { lines, format: metricFormat(s.unit, s.name), axisFormat: metricFormat(s.unit, s.name, true) };
  const scaled = lines.map((l) => ({ ...l, values: l.values.map((v) => (v === null ? null : v * scale.times)) }));
  return { lines: scaled, format: scale.format, axisFormat: scale.format };
}

/** headline is the one number a metric comes down to, and what that number is. */
function headline(s: MetricSeries, rangeMs: number): { value: string; note: string } {
  const g = s.groups[0];
  const f = metricFormat(s.unit, s.name);
  if (!g) return { value: "–", note: "" };
  if (s.kind === "gauge") return { value: g.stats.last === null ? "–" : f(g.stats.last), note: "now" };
  if (s.kind === "rate") return { value: f(g.stats.total), note: `in ${fmtSpan(rangeMs)}` };
  if (g.stats.p95 !== null) return { value: f(g.stats.p95), note: "p95" };
  return { value: g.stats.avg === null ? "–" : f(g.stats.avg), note: "mean" };
}

/** whole draws a metric as one: a level, an increase per bucket, or a histogram's percentiles. */
function whole(s: MetricSeries): Line[] {
  const g = s.groups[0];
  if (!g) return [];
  if (s.kind === "gauge") return [{ label: "Value", values: g.value, color: "--blue", kind: "area" }];
  if (s.kind === "rate") return [{ label: "Increase", values: g.value, color: "--accent", kind: "bars" }];
  // A summary, or a histogram sent without buckets, has only its mean.
  if (!g.p95 || !g.p50) return [{ label: "Mean", values: g.avg ?? [], color: "--blue" }];
  return [
    { label: "p95", values: g.p95, color: "--violet", kind: "area" },
    { label: "p50", values: g.p50, color: "--blue" },
  ];
}

function Tile({ metric: m, scope, rangeMs, open, onOpen }: Props & { metric: MetricInfo; open: boolean; onOpen: () => void }) {
  const ref = useRef<HTMLDivElement>(null);
  const seen = useOnScreen(ref);
  // A long list of metrics only loads the charts on screen; one scrolled
  // away keeps what it last drew.
  const last = useRef<MetricSeries | undefined>(undefined);
  const { data } = useLive(
    () => (seen ? api.metricSeries(scope(), Date.now(), TILE_BUCKETS, m.name).then((d) => (last.current = d)) : Promise.resolve(last.current)),
    [seen, rangeMs, scope().service, m.name],
  );
  const top = data ? headline(data, rangeMs) : null;

  return (
    <Surface
      ref={ref}
      onClick={onOpen}
      data-state={open ? "selected" : undefined}
      className="cursor-default transition-colors hover:border-ring/60 data-[state=selected]:border-ring"
    >
      <header className="flex items-start gap-3 px-3 pt-2.5">
        <div className="min-w-0 flex-1">
          <button
            type="button"
            // WebKit's Tab passes over a button without one (see RowButton).
            tabIndex={0}
            onClick={(e) => {
              e.stopPropagation();
              onOpen();
            }}
            aria-expanded={open}
            className="selectable block max-w-full truncate rounded-sm text-left font-mono text-xs font-medium outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-card"
          >
            {m.name}
          </button>
          <div className="truncate text-[11px] text-muted-foreground">
            {describe(m)}
            {together(m) && ` · ${together(m)}`}
          </div>
        </div>
        <div className="shrink-0 text-right">
          <div className="text-[15px] leading-[18px] font-medium tabular-nums">{top?.value ?? "–"}</div>
          <div className="text-[11px] text-muted-foreground">{top?.note ?? ""}</div>
        </div>
      </header>
      <div className="p-2">
        {!data ? (
          <Loading height={TILE_SLOT} />
        ) : data.groups.length === 0 ? (
          <Hold height={TILE_SLOT}>No points in this window.</Hold>
        ) : (
          <Chart t={data.t} bucketMs={data.bucket_ms} height={TILE_CHART} {...plot(data, whole(data))} />
        )}
      </div>
    </Surface>
  );
}

function Detail({ name, info, scope, rangeMs, onClose }: Props & { name: string; info: MetricInfo | undefined; onClose: () => void }) {
  const [by, setBy] = useState("");
  const deps = [rangeMs, scope().service, name];
  const { data, error } = useLive(() => api.metricSeries(scope(), Date.now(), BUCKETS, name), deps);
  const split = useLive(
    () => (by ? api.metricSeries(scope(), Date.now(), BUCKETS, name, by) : Promise.resolve(undefined)),
    [...deps, by],
  );

  const title = (
    <>
      <span className="selectable truncate font-mono text-xs font-medium">{name}</span>
      {data && data.kind !== "" && (
        <span className="shrink-0 text-[11px] text-muted-foreground">
          {describe(data)}
          {data.temporality && ` · ${data.temporality}`}
        </span>
      )}
    </>
  );
  if (!data) {
    return (
      <Side title={title} onClose={onClose} label="Close metric">
        {error ? <Hold tone="err">{error}</Hold> : <Loading />}
      </Side>
    );
  }
  const g = data.groups[0];
  if (!g) {
    return (
      <Side title={title} onClose={onClose} label="Close metric">
        <Hold>No points of this metric in this window.</Hold>
      </Side>
    );
  }

  const per = `per ${fmtSpan(data.bucket_ms)}`;
  // Splitting by service says nothing when one is picked, or only one reports.
  const keys = [...(scope().service === "" && (info?.services.length ?? 0) > 1 ? ["service"] : []), ...data.keys];

  return (
    <Side title={title} onClose={onClose} label="Close metric">
      <div className="flex flex-col gap-3">
        {data.description && <p className="selectable text-xs text-muted-foreground">{data.description}</p>}
        <FigureCard>
          <Figures series={data} group={g} info={info} rangeMs={rangeMs} />
        </FigureCard>

        <Panel
          title={data.kind === "histogram" ? (g.p95 ? "p50 and p95" : "Mean") : data.kind === "rate" ? "Increase" : "Value"}
          hint={data.kind === "rate" ? per : together(info)}
        >
          <Chart t={data.t} bucketMs={data.bucket_ms} height={220} {...plot(data, whole(data))} />
        </Panel>
        {data.kind === "histogram" && (
          <Panel title="Observations" hint={per}>
            <Chart
              t={data.t}
              bucketMs={data.bucket_ms}
              height={120}
              format={fmtNum}
              lines={[{ label: "Observations", values: g.value, color: "--accent", kind: "bars" }]}
            />
          </Panel>
        )}

        {keys.length > 0 && (
          <div className="flex flex-wrap items-center gap-2">
            <span className="text-[11px] text-muted-foreground">Split by</span>
            <PickOne
              label="Split by"
              value={by}
              onChange={setBy}
              options={["", ...keys].map((k) => ({
                value: k,
                label: k === "" ? "Nothing" : k,
                className: k === "" || k === "service" ? undefined : "font-mono text-[11.5px] sm:text-[11.5px]",
              }))}
            />
          </div>
        )}
        {by !== "" &&
          (split.data && split.data.by === by ? (
            <Split series={split.data} />
          ) : (
            <Panel title={`By ${by}`}>
              {split.error ? <Hold tone="err">{split.error}</Hold> : <Loading />}
            </Panel>
          ))}
      </div>
    </Side>
  );
}

/** Figures are a metric's numbers over the window, by what kind of metric it is. */
function Figures({ series: s, group: g, info, rangeMs }: { series: MetricSeries; group: MetricGroup; info: MetricInfo | undefined; rangeMs: number }) {
  const f = metricFormat(s.unit, s.name);
  const or = (v: number | null) => (v === null ? "–" : f(v));
  const vs = present(g.value);
  if (s.kind === "gauge") {
    return (
      <>
        <Figure label="Now" value={or(g.stats.last)} />
        <Figure label="Average" value={vs.length ? f(vs.reduce((a, b) => a + b, 0) / vs.length) : "–"} />
        <Figure label="Lowest" value={vs.length ? f(Math.min(...vs)) : "–"} />
        <Figure label="Highest" value={vs.length ? f(Math.max(...vs)) : "–"} />
      </>
    );
  }
  // The time the points cover, which is less than the window when the app
  // started (or Leadline did) partway through it.
  const seconds = (vs.length * s.bucket_ms) / 1000;
  const perSecond = seconds > 0 ? g.stats.total / seconds : 0;
  const rate = (w: (v: number) => string) => (perSecond >= 1 || perSecond === 0 ? `${w(perSecond)}/s` : `${w(perSecond * 60)}/min`);
  if (s.kind === "rate") {
    return (
      <>
        <Figure label="Increase" value={f(g.stats.total)} note={`in ${fmtSpan(rangeMs)}`} />
        <Figure label="Rate" value={rate(f)} note="on average" />
        <Figure label="Peak" value={vs.length ? f(Math.max(...vs)) : "–"} note={`per ${fmtSpan(s.bucket_ms)}`} />
        <Figure label="Series" value={info ? fmtCount(info.series) : "–"} />
      </>
    );
  }
  return (
    <>
      <Figure label="Observations" value={fmtCount(g.stats.total)} note={rate(fmtNum)} />
      <Figure label="Mean" value={or(g.stats.avg)} />
      <Figure label="p50" value={or(g.stats.p50)} />
      <Figure label="p95" value={or(g.stats.p95)} />
    </>
  );
}

/** Split draws a metric's groups against each other, and lists them. */
function Split({ series: s }: { series: MetricSeries }) {
  const f = metricFormat(s.unit, s.name);
  const name = (g: MetricGroup) => g.label || "(not set)";
  const hist = s.kind === "histogram";
  const lines: Line[] = s.groups.map((g, i) => ({
    label: name(g),
    values: hist ? (g.p95 ?? g.avg ?? []) : g.value,
    color: COLORS[i % COLORS.length],
    kind: s.kind === "rate" ? "bars" : "line",
  }));
  const most = Math.max(0, ...s.groups.map((g) => g.stats.total));
  const what = hist ? (s.groups.some((g) => g.p95) ? "p95" : "Mean") : s.kind === "rate" ? "Increase" : "Value";

  return (
    <Listing
      title={
        <>
          {what} by <span className="font-mono">{s.by}</span>
        </>
      }
      hint={s.kind === "rate" ? `per ${fmtSpan(s.bucket_ms)}` : undefined}
      count={s.groups.length}
      none="No points in this window."
    >
      <div className="px-2 pb-2">
        <Chart t={s.t} bucketMs={s.bucket_ms} height={200} stack={s.kind === "rate"} {...plot(s, lines)} />
      </div>
      <Rows className="table-auto border-t">
        <TableHeader>
          <TableRow>
            <TableHead>{s.by}</TableHead>
            {s.kind === "gauge" && <TableHead className="w-24 text-right">Now</TableHead>}
            {s.kind === "rate" && <TableHead className="w-48">Increase</TableHead>}
            {hist && (
              <>
                <TableHead className="w-24 text-right">Observations</TableHead>
                <TableHead className="w-20 text-right">Mean</TableHead>
                <TableHead className="w-20 text-right">p50</TableHead>
                <TableHead className="w-20 text-right">p95</TableHead>
              </>
            )}
          </TableRow>
        </TableHeader>
        <TableBody>
          {s.groups.map((g, i) => (
            <TableRow key={g.label}>
              <TableCell>
                <span className="flex min-w-0 items-center gap-2">
                  <span className="size-2 shrink-0 rounded-[2px]" style={{ background: `var(${COLORS[i % COLORS.length]})` }} />
                  <span className={`selectable truncate ${g.label ? "font-mono text-[11.5px]" : "text-muted-foreground"}`}>{name(g)}</span>
                </span>
              </TableCell>
              {s.kind === "gauge" && <TableCell className="text-right tabular-nums">{g.stats.last === null ? "–" : f(g.stats.last)}</TableCell>}
              {s.kind === "rate" && (
                <TableCell>
                  <span className="flex items-center gap-2">
                    <Meter value={g.stats.total} max={most} />
                    <span className="w-14 shrink-0 text-right tabular-nums">{f(g.stats.total)}</span>
                  </span>
                </TableCell>
              )}
              {hist && (
                <>
                  <TableCell className="text-right tabular-nums">{fmtCount(g.stats.total)}</TableCell>
                  <TableCell className="text-right tabular-nums">{g.stats.avg === null ? "–" : f(g.stats.avg)}</TableCell>
                  <TableCell className="text-right tabular-nums">{g.stats.p50 === null ? "–" : f(g.stats.p50)}</TableCell>
                  <TableCell className="text-right tabular-nums">{g.stats.p95 === null ? "–" : f(g.stats.p95)}</TableCell>
                </>
              )}
            </TableRow>
          ))}
        </TableBody>
      </Rows>
      {s.more > 0 && (
        <div className="border-t px-3 py-1.5 text-[11px] text-muted-foreground">
          {s.more} smaller {s.more === 1 ? "one is" : "ones are"} not shown.
        </div>
      )}
    </Listing>
  );
}
