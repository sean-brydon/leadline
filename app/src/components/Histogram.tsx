import { useEffect, useRef } from "react";
import uPlot from "uplot";
import type { Histogram as HistogramData } from "@/lib/api";
import { fmtCount, fmtMsShort, fmtRange } from "@/lib/format";
import { axisFont, tokens, useScheme } from "./chartkit";

export type Mark = { label: string; ms: number; color: string };

type Props = {
  data: HistogramData;
  /** Lines drawn at a latency, such as p50 and p95. */
  marks?: Mark[];
  height?: number;
  color?: string;
  /** What a bar counts, for the legend: "Requests", "Calls". */
  unit?: string;
};

/**
 * Histogram draws a latency distribution: log-spaced bins across, how many
 * fell in each up. On a log axis a fast endpoint with a slow tail shows both
 * its bulk and its tail, where a linear one would be a single spike.
 */
export function Histogram({ data, marks = [], height = 150, color = "--accent", unit = "Requests" }: Props) {
  const ref = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const dark = useScheme();
  const bins = data.bins;
  const shape = `${unit}|${color}|${marks.map((m) => `${m.label}/${m.color}`).join("|")}`;

  // Bin i is drawn from i to i+1, so an edge between bins is a whole number.
  const aligned = (): uPlot.AlignedData => [bins.map((_, i) => i + 0.5), bins.map((b) => b.count)];
  const live = useRef({ data, marks, aligned });
  live.current = { data, marks, aligned };

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const token = tokens(el);
    const font = axisFont(token);
    const edgeMs = (j: number) => {
      const b = live.current.data.bins;
      if (b.length === 0) return NaN;
      return j < b.length ? b[Math.max(0, j)].lo_ms : b[b.length - 1].hi_ms;
    };

    const drawMarks = (u: uPlot) => {
      const { data: d, marks } = live.current;
      if (d.bins.length === 0) return;
      const { left, top, width, height: h } = u.bbox;
      const ctx = u.ctx;
      const px = devicePixelRatio;
      ctx.save();
      ctx.font = `${10 * px}px ${token("--font-sans") || "sans-serif"}`;
      ctx.textBaseline = "top";
      ctx.textAlign = "left";
      let lastRight = -Infinity;
      for (const m of marks) {
        if (!(m.ms > 0)) continue;
        // Where in the bins this latency falls: they are even steps of log10.
        const at = (Math.log10(m.ms) - Math.log10(d.bins[0].lo_ms)) * d.per_decade;
        const x = Math.round(u.valToPos(at, "x", true));
        if (x < left || x > left + width) continue;
        ctx.strokeStyle = token(m.color);
        ctx.lineWidth = px;
        ctx.setLineDash([3 * px, 3 * px]);
        ctx.beginPath();
        ctx.moveTo(x + 0.5, top);
        ctx.lineTo(x + 0.5, top + h);
        ctx.stroke();
        // The label sits right of its line, or left when the next would cover it.
        const w = ctx.measureText(m.label).width;
        let tx = x + 4 * px;
        if (tx + w > left + width || tx < lastRight + 4 * px) tx = x - 4 * px - w;
        if (tx < lastRight + 2 * px) continue;
        ctx.fillStyle = token("--fg");
        ctx.fillText(m.label, tx, top + 2 * px);
        lastRight = tx + w;
      }
      ctx.restore();
    };

    const opts: uPlot.Options = {
      width: el.clientWidth || 300,
      height,
      // Room on the right for the last edge's label, which sits under the plot's corner.
      padding: [8, 20, 0, 0],
      cursor: { y: false, points: { show: false }, drag: { x: false, y: false } },
      scales: {
        x: { time: false, range: () => [0, Math.max(1, live.current.data.bins.length)] },
        y: { range: (_u, _min, max) => [0, max > 0 ? max * 1.15 : 1] },
      },
      axes: [
        {
          stroke: token("--muted"),
          font,
          size: 26,
          grid: { show: false },
          ticks: { show: false },
          // Label the edges that fall on round numbers: 1, 2 and 5 of each
          // decade while those fit, else the decades alone.
          splits: (u) => {
            const n = live.current.data.bins.length;
            const round = (lead: number[]) => {
              const out: number[] = [];
              for (let j = 0; j <= n; j++) {
                const ms = edgeMs(j);
                const first = ms / 10 ** Math.floor(Math.log10(ms) + 1e-9);
                if (lead.some((l) => Math.abs(first - l) / l < 0.03)) out.push(j);
              }
              return out;
            };
            const fine = round([1, 2, 5]);
            const room = Math.max(2, Math.floor(u.bbox.width / devicePixelRatio / 56));
            if (fine.length <= room) return fine;
            const decades = round([1]);
            return decades.length >= 2 ? decades : fine.filter((_, i) => i % Math.ceil(fine.length / room) === 0);
          },
          values: (_u, ticks) => ticks.map((j) => fmtMsShort(edgeMs(j))),
        },
        {
          stroke: token("--muted"),
          font,
          size: 52,
          grid: { stroke: token("--line"), width: 1 },
          ticks: { show: false },
          values: (_u, ticks) => ticks.map((v) => (Number.isInteger(v) ? fmtCount(v) : "")),
        },
      ],
      series: [
        {
          label: "Took",
          value: (_u, _v, _s, idx) => {
            const b = idx == null ? undefined : live.current.data.bins[idx];
            return b ? fmtRange(b.lo_ms, b.hi_ms) : "–";
          },
        },
        {
          label: unit,
          stroke: token(color),
          width: 0,
          fill: `${token(color)}cc`,
          paths: uPlot.paths.bars?.({ size: [0.9, 64], gap: 1 }),
          points: { show: false },
          value: (_u, v) => (v == null ? "–" : fmtCount(v)),
        },
      ],
      hooks: { draw: [drawMarks] },
    };
    const p = new uPlot(opts, live.current.aligned(), el);
    plot.current = p;
    const ro = new ResizeObserver(() => p.setSize({ width: el.clientWidth, height }));
    ro.observe(el);
    return () => {
      ro.disconnect();
      p.destroy();
      plot.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [shape, height, dark]);

  useEffect(() => {
    plot.current?.setData(live.current.aligned());
  });

  return <div ref={ref} className="w-full" />;
}
