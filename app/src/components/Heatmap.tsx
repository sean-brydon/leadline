import { useEffect, useRef } from "react";
import uPlot from "uplot";
import type { Heatmap as HeatmapData } from "@/lib/api";
import { bandMs, fmtCount, fmtMsShort, fmtRange, fmtTick } from "@/lib/format";
import { alpha, axisFont, heat, TIME_SYNC, tokens, useScheme } from "./chartkit";

type Props = {
  data: HeatmapData;
  height?: number;
  /** A colour token; a cell is this colour, stronger the more it holds. */
  color?: string;
  /** What a cell counts, for the readout: "requests", "calls". */
  unit?: string;
};

// Always show at least this many decades of latency, so one band of data
// isn't stretched into a wall.
const MIN_DECADES = 1;
const KEY_STEPS = [0.05, 0.25, 0.5, 0.75, 1];

/**
 * Heatmap draws latency over time: time across, log-spaced latency bands
 * up, and each cell stronger the more requests fell in it. A line chart of
 * p95 hides whether slow requests are a steady band or a few spikes; this
 * shows both. It is a uPlot plot with the cells painted over it, so it gets
 * the same axes and shares the other charts' cursor.
 */
export function Heatmap({ data, height = 150, color = "--accent", unit = "requests" }: Props) {
  const ref = useRef<HTMLDivElement>(null);
  const readout = useRef<HTMLSpanElement>(null);
  const plot = useRef<uPlot | null>(null);
  const dark = useScheme();

  const k = data.per_decade;
  const half = data.bucket_ms / 2;
  // Whole decades around the bands in use, so the axis starts and ends on a round label.
  let lo = Math.floor(data.y_min / k) * k;
  let hi = Math.ceil((data.y_max + 1) / k) * k;
  if (data.n.length === 0) [lo, hi] = [0, MIN_DECADES * k];
  while (hi - lo < MIN_DECADES * k) hi += k;

  const cells = new Map<number, number>();
  for (let i = 0; i < data.n.length; i++) cells.set(data.x[i] * 4096 + data.y[i] + 2048, data.n[i]);
  const live = useRef({ data, cells, lo, hi, unit });
  live.current = { data, cells, lo, hi, unit };
  // The cells are painted by hand; uPlot only needs the times, for its axis and cursor.
  const aligned = useRef<() => uPlot.AlignedData>(() => [[], []]);
  aligned.current = () => [data.t.map((v) => v + half), data.t.map(() => null)];

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const token = tokens(el);
    const font = axisFont(token);
    const ink = token(color);

    const paint = (u: uPlot) => {
      const { data: d } = live.current;
      const { left, top, width, height: h } = u.bbox;
      const ctx = u.ctx;
      ctx.save();
      ctx.beginPath();
      ctx.rect(left, top, width, h);
      ctx.clip();
      // A hairline between cells, while the cells are big enough to spare it.
      const row = Math.abs(u.valToPos(0, "y", true) - u.valToPos(1, "y", true));
      const col = d.t.length ? width / d.t.length : width;
      const gap = Math.min(row, col) >= 4 * devicePixelRatio ? Math.round(devicePixelRatio) : 0;
      for (let i = 0; i < d.n.length; i++) {
        const x0 = Math.round(u.valToPos(d.t[d.x[i]], "x", true));
        const x1 = Math.round(u.valToPos(d.t[d.x[i]] + d.bucket_ms, "x", true));
        const y0 = Math.round(u.valToPos(d.y[i] + 1, "y", true));
        const y1 = Math.round(u.valToPos(d.y[i], "y", true));
        ctx.fillStyle = ink + alpha(heat(d.n[i], d.max));
        ctx.fillRect(x0, y0, Math.max(1, x1 - x0 - gap), Math.max(1, y1 - y0 - gap));
      }
      ctx.restore();
    };

    // The cell under the pointer, in words. A cursor that comes from another
    // chart has no height, so it names the time alone.
    const describe = (u: uPlot) => {
      const out = readout.current;
      if (!out) return;
      const { data: d, cells, unit } = live.current;
      const { left, top } = u.cursor;
      if (left == null || left < 0 || d.t.length === 0) {
        out.textContent = "";
        return;
      }
      const col = Math.floor((u.posToVal(left, "x") - d.t[0]) / d.bucket_ms);
      if (col < 0 || col >= d.t.length) {
        out.textContent = "";
        return;
      }
      const at = fmtTick(d.t[col], true);
      if (top == null || top < 0) {
        out.textContent = at;
        return;
      }
      const band = Math.floor(u.posToVal(top, "y"));
      const n = cells.get(col * 4096 + band + 2048) ?? 0;
      const what = n === 1 ? unit.replace(/s$/, "") : unit;
      out.textContent = `${at} · ${fmtRange(bandMs(band, d.per_decade), bandMs(band + 1, d.per_decade))} · ${fmtCount(n)} ${what}`;
    };

    const opts: uPlot.Options = {
      width: el.clientWidth || 300,
      height,
      ms: 1,
      padding: [8, 8, 0, 0],
      legend: { show: false },
      cursor: {
        points: { show: false },
        drag: { x: false, y: false },
        sync: { key: TIME_SYNC, setSeries: false },
      },
      scales: {
        x: {
          range: () => {
            const d = live.current.data;
            return d.t.length ? [d.t[0], d.t[d.t.length - 1] + d.bucket_ms] : [0, 1];
          },
        },
        y: { range: () => [live.current.lo, live.current.hi] },
      },
      axes: [
        {
          stroke: token("--muted"),
          font,
          size: 26,
          grid: { show: false },
          ticks: { show: false },
          values: (_u, ticks) => {
            const secs = ticks.length > 1 && ticks[1] - ticks[0] < 60_000;
            return ticks.map((v) => fmtTick(v, secs));
          },
        },
        {
          stroke: token("--muted"),
          font,
          size: 52,
          grid: { stroke: token("--line"), width: 1 },
          ticks: { show: false },
          // A label at every decade: 1ms, 10ms, 100ms, 1s.
          splits: () => {
            const { lo, hi } = live.current;
            const per = live.current.data.per_decade;
            const out: number[] = [];
            for (let y = lo; y <= hi; y += per) out.push(y);
            return out;
          },
          values: (_u, ticks) => ticks.map((y) => fmtMsShort(bandMs(y, live.current.data.per_decade))),
        },
      ],
      series: [{}, { stroke: "transparent", points: { show: false } }],
      hooks: { draw: [paint], setCursor: [describe] },
    };
    const p = new uPlot(opts, aligned.current(), el);
    plot.current = p;
    const ro = new ResizeObserver(() => p.setSize({ width: el.clientWidth, height }));
    ro.observe(el);
    return () => {
      ro.disconnect();
      p.destroy();
      plot.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [height, dark, color]);

  useEffect(() => {
    plot.current?.setData(aligned.current());
  });

  return (
    <div>
      <div ref={ref} className="heatmap w-full" />
      <div className="flex h-[22px] items-center gap-2 pl-1 text-[11px] text-muted">
        <span ref={readout} className="min-w-0 flex-1 truncate tabular-nums text-fg" />
        {data.max > 0 && (
          <span className="flex shrink-0 items-center gap-1 tabular-nums" aria-label={`Colour runs from 1 to ${data.max} ${unit} in a cell`}>
            1
            <span className="flex gap-px">
              {KEY_STEPS.map((s) => (
                <span
                  key={s}
                  className="size-2 rounded-[1px]"
                  style={{ background: `var(${color})`, opacity: heat(Math.max(1, s * data.max), data.max) }}
                />
              ))}
            </span>
            {fmtCount(data.max)} {unit}
          </span>
        )}
      </div>
    </div>
  );
}
