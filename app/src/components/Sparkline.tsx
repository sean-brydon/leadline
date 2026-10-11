import { useEffect, useRef } from "react";
import { tokens, useScheme } from "./chartkit";

type Props = {
  values: number[];
  /** The top of the scale; pass one number to every sparkline in a table to compare rows. Defaults to this line's own peak. */
  max?: number;
  width?: number;
  height?: number;
  color?: string;
  label?: string;
};

/**
 * Sparkline is a word-sized chart for a table row: the shape of a series
 * and nothing else. One small canvas each, so a few hundred rows stay cheap.
 */
export function Sparkline({ values, max, width = 88, height = 20, color = "--accent", label }: Props) {
  const ref = useRef<HTMLCanvasElement>(null);
  const dark = useScheme();
  const key = values.join(",");

  useEffect(() => {
    const el = ref.current;
    const ctx = el?.getContext("2d");
    if (!el || !ctx) return;
    const px = devicePixelRatio;
    el.width = Math.round(width * px);
    el.height = Math.round(height * px);
    ctx.clearRect(0, 0, el.width, el.height);
    if (values.length < 2) return;
    const top = Math.max(max ?? 0, ...values, 1);
    const ink = tokens(el)(color);
    const pad = px;
    const x = (i: number) => pad + (i / (values.length - 1)) * (el.width - 2 * pad);
    const y = (v: number) => el.height - pad - (v / top) * (el.height - 2 * pad);

    ctx.beginPath();
    values.forEach((v, i) => (i ? ctx.lineTo(x(i), y(v)) : ctx.moveTo(x(i), y(v))));
    ctx.strokeStyle = ink;
    ctx.lineWidth = 1.25 * px;
    ctx.lineJoin = "round";
    ctx.stroke();
    ctx.lineTo(x(values.length - 1), el.height);
    ctx.lineTo(x(0), el.height);
    ctx.closePath();
    ctx.fillStyle = `${ink}26`;
    ctx.fill();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [key, max, width, height, color, dark]);

  return <canvas ref={ref} role="img" aria-label={label} style={{ width, height }} className="block" />;
}
