import { useEffect, useRef } from "react";
import uPlot from "uplot";
import { fmtTick } from "@/lib/format";
import { axisFont, TIME_SYNC, tokens, useScheme } from "./chartkit";

export type Line = {
  label: string;
  /** One value per bucket; null where there is nothing to draw. */
  values: (number | null)[];
  /** A colour token from styles.css, such as "--accent". */
  color: string;
  kind?: "line" | "area" | "bars";
};

type Props = {
  /** Bucket start times, unix milliseconds. */
  t: number[];
  /** How long a bucket is. With it, points sit mid-bucket and the axis spans the whole window. */
  bucketMs?: number;
  lines: Line[];
  height?: number;
  /** Writes a value in the legend. */
  format?: (v: number) => string;
  /** Writes a value on the axis, where there is less room; defaults to format. */
  axisFormat?: (v: number) => string;
  /** Pile the lines on top of each other, first at the bottom, to show parts of a whole. */
  stack?: boolean;
};

/**
 * Chart draws aligned time series on a canvas with uPlot, which stays
 * smooth with far more points than SVG would. The plot is built once per
 * shape (which lines, which colours) and fed new data after that. Charts
 * share a cursor: hover one and the same moment is marked on the rest.
 */
export function Chart({ t, bucketMs = 0, lines, height = 150, format = String, axisFormat, stack = false }: Props) {
  const ref = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const dark = useScheme();
  const shape = lines.map((l) => `${l.label}/${l.color}/${l.kind ?? "line"}`).join("|") + (stack ? "|stack" : "");

  // A stack is drawn back to front: the tallest pile first, each shorter one
  // over it. The legend still reads each line's own value.
  const drawn = stack ? [...lines].reverse() : lines;
  const own = drawn.map((l) => l.values);
  let ys = own;
  if (stack) {
    const pile = new Array<number>(t.length).fill(0);
    ys = lines.map((l) => l.values.map((v, i) => (pile[i] += v ?? 0))).reverse();
  }
  const half = bucketMs / 2;
  const data = useRef<uPlot.AlignedData>([t]);
  data.current = [half ? t.map((v) => v + half) : t, ...ys];
  const live = useRef({ own, half, span: [0, 1] as [number, number], format, axisFormat: axisFormat ?? format });
  live.current = {
    own,
    half,
    span: t.length ? [t[0], t[t.length - 1] + bucketMs] : [0, 1],
    format,
    axisFormat: axisFormat ?? format,
  };

  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const token = tokens(el);
    const font = axisFont(token);
    const opts: uPlot.Options = {
      width: el.clientWidth || 300,
      height,
      ms: 1,
      padding: [8, 8, 0, 0],
      cursor: {
        y: false,
        points: { size: 6 },
        drag: { x: false, y: false },
        sync: { key: TIME_SYNC, setSeries: false },
      },
      scales: {
        x: { range: (_u, min, max) => (live.current.half ? live.current.span : [min, max]) },
        y: { range: (_u, _min, max) => [0, max > 0 ? max * 1.15 : 1] },
      },
      axes: [
        {
          stroke: token("--muted"),
          font,
          size: 26,
          grid: { show: false },
          ticks: { show: false },
          // One line of local time; uPlot's own labels add a date line.
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
          values: (_u, ticks) => ticks.map((v) => live.current.axisFormat(v)),
        },
      ],
      series: [
        { label: "At", value: (_u, v) => (v == null ? "–" : fmtTick(v - live.current.half, true)) },
        ...drawn.map((l, i): uPlot.Series => {
          const color = token(l.color);
          const kind = l.kind ?? "line";
          // Token colours are six-digit hex, so two more digits are alpha.
          // Stacked fills are solid: see-through ones would mix with the pile behind.
          const fill = stack ? color : kind === "bars" ? `${color}cc` : kind === "area" ? `${color}2e` : undefined;
          return {
            label: l.label,
            stroke: color,
            width: kind === "bars" ? 0 : 1.5,
            fill,
            paths: kind === "bars" ? uPlot.paths.bars?.({ size: [0.75, 28] }) : undefined,
            points: { show: false },
            // A bucket where nothing happened has no latency; the line runs on to the next that has.
            spanGaps: true,
            value: (_u, _v, _s, idx) => {
              const v = idx == null ? null : live.current.own[i]?.[idx];
              return v == null ? "–" : live.current.format(v);
            },
          };
        }),
      ],
    };
    const p = new uPlot(opts, data.current, el);
    plot.current = p;
    const ro = new ResizeObserver(() => p.setSize({ width: el.clientWidth, height }));
    ro.observe(el);
    return () => {
      ro.disconnect();
      p.destroy();
      plot.current = null;
    };
    // The lines' values change every refetch; only their shape rebuilds.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [shape, height, dark]);

  useEffect(() => {
    plot.current?.setData(data.current);
  });

  return <div ref={ref} className="w-full" />;
}
