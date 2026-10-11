// Writing a metric's values by its unit. OpenTelemetry units follow UCUM:
// "s", "ms", "By" (bytes), "1" (a plain number or a ratio), and a braced
// word for things counted, such as "{request}".

import { fmtCount, fmtMs, fmtMsShort } from "./format.ts";

/** fmtNum writes a plain number in about three figures: 0.042, 7.25, 13.2, 240, 1.2k. */
export function fmtNum(v: number): string {
  if (!Number.isFinite(v)) return "–";
  if (v < 0) return `-${fmtNum(-v)}`;
  if (v >= 1000) return fmtCount(v);
  if (v >= 100) return String(Math.round(v));
  if (v >= 10) return String(Number(v.toFixed(1)));
  if (v >= 1) return String(Number(v.toFixed(2)));
  return String(Number(v.toPrecision(2)));
}

/** fmtBytes writes a size in binary units: 512B, 1.5KiB, 182MiB, 2.25GiB. */
export function fmtBytes(v: number): string {
  if (!Number.isFinite(v)) return "–";
  if (v < 0) return `-${fmtBytes(-v)}`;
  let i = 0;
  while (v >= 1024 && i < BYTE_UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  return `${fmtNum(v)}${BYTE_UNITS[i]}`;
}

const TO_MS: Record<string, number> = { s: 1000, ms: 1, us: 0.001, "µs": 0.001, ns: 0.000001 };
const TO_BYTES: Record<string, number> = { By: 1, KiBy: 1024, MiBy: 1024 ** 2, GiBy: 1024 ** 3, kBy: 1000, MBy: 1000 ** 2, GBy: 1000 ** 3 };

/** A unit of "1" is a share of a whole when the name says so, and a bare count otherwise. */
const isShare = (name: string) => /(utilization|ratio)$/.test(name);

/**
 * metricFormat picks how to write a metric's values. `short` is for a
 * chart's axis, where there is less room.
 */
export function metricFormat(unit: string, name = "", short = false): (v: number) => string {
  if (unit in TO_MS) {
    const k = TO_MS[unit];
    const f = short ? fmtMsShort : fmtMs;
    return (v) => f(v * k);
  }
  if (unit in TO_BYTES) {
    const k = TO_BYTES[unit];
    return (v) => fmtBytes(v * k);
  }
  if (unit === "%") return (v) => `${fmtNum(v)}%`;
  if (unit === "1" && isShare(name)) return (v) => `${fmtNum(v * 100)}%`;
  // A rate the app worked out itself, such as By/s.
  const per = /^(.+)\/(s|min|h)$/.exec(unit);
  if (per) {
    const f = metricFormat(per[1], name, short);
    return (v) => `${f(v)}/${per[2]}`;
  }
  return fmtNum;
}

const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB"];

/**
 * chartScale is for a chart of sizes. An axis picks round numbers, and a
 * round number of bytes is an odd number of mebibytes (95.4MiB), so the
 * chart's values are first multiplied into the unit their peak calls for.
 * Null for a unit that needs none of this.
 */
export function chartScale(unit: string, peak: number): { times: number; format: (v: number) => string } | null {
  if (!(unit in TO_BYTES)) return null;
  let i = 0;
  for (let v = Math.abs(peak) * TO_BYTES[unit]; v >= 1024 && i < BYTE_UNITS.length - 1; v /= 1024) i++;
  return { times: TO_BYTES[unit] / 1024 ** i, format: (v) => `${fmtNum(v)}${BYTE_UNITS[i]}` };
}

/** unitLabel names what a metric counts, and is empty when its values already say (12ms, 4MiB, 30%): "{connection}" is "connection". */
export function unitLabel(unit: string): string {
  if (unit in TO_MS || unit in TO_BYTES || unit === "%" || unit === "1") return "";
  return unit.replace(/[{}]/g, "");
}

/** kindLabel names a metric's instrument: a sum that only rises is a counter. */
export function kindLabel(m: { type: string; monotonic: boolean }): string {
  if (m.type === "sum") return m.monotonic ? "counter" : "up-down counter";
  return m.type;
}
