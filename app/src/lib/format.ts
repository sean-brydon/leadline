/** fmtMs writes a duration given in milliseconds: 412µs, 12.3ms, 1.24s. */
export function fmtMs(ms: number): string {
  if (!Number.isFinite(ms)) return "–";
  if (ms < 1) return `${Math.round(ms * 1000)}µs`;
  if (ms < 10) return `${ms.toFixed(2)}ms`;
  if (ms < 100) return `${ms.toFixed(1)}ms`;
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(2)}s`;
  return `${(ms / 60_000).toFixed(1)}m`;
}

/** fmtCount shortens a count: 950, 1.2k, 3.4M. */
export function fmtCount(n: number): string {
  if (n < 1000) return String(Math.round(n));
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0)}k`;
  return `${(n / 1_000_000).toFixed(1)}M`;
}

/** fmtClock writes a unix-millisecond time as local HH:MM:SS.mmm. */
export function fmtClock(ms: number): string {
  const d = new Date(ms);
  const p = (n: number, w = 2) => String(n).padStart(w, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}

/** fmtSpan writes a round length of time, such as a chart's bucket: 10s, 4m, 2h. */
export function fmtSpan(ms: number): string {
  const s = ms / 1000;
  if (s < 1) return `${Math.round(ms)}ms`;
  if (s < 90) return `${Math.round(s)}s`;
  if (s < 90 * 60) return `${Math.round(s / 60)}m`;
  return `${Math.round(s / 3600)}h`;
}

/** fmtTick labels a chart's time axis: seconds only when the ticks need them. */
export function fmtTick(ms: number, withSeconds: boolean): string {
  const d = new Date(ms);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}${withSeconds ? `:${p(d.getSeconds())}` : ""}`;
}

/** fmtMsShort writes a duration for an axis, where 10ms beats 10.0ms: 250µs, 3.2ms, 10ms, 1.5s. */
export function fmtMsShort(ms: number): string {
  if (!Number.isFinite(ms)) return "–";
  if (ms === 0) return "0";
  const trim = (n: number) => String(Number(n.toPrecision(2)));
  if (ms < 1) return `${trim(ms * 1000)}µs`;
  if (ms < 1000) return `${trim(ms)}ms`;
  if (ms < 60_000) return `${trim(ms / 1000)}s`;
  return `${trim(ms / 60_000)}m`;
}

/** fmtPct writes a share between 0 and 1: 0%, <0.1%, 4.2%, 48%. */
export function fmtPct(share: number): string {
  if (!Number.isFinite(share)) return "–";
  const pct = share * 100;
  if (pct === 0) return "0%";
  if (pct < 0.1) return "<0.1%";
  if (pct < 10) return `${pct.toFixed(1)}%`;
  return `${Math.round(pct)}%`;
}

/** fmtPer writes an average count, such as queries per request: 0.04, 1, 13.2, 240. */
export function fmtPer(n: number): string {
  if (!Number.isFinite(n)) return "–";
  if (n >= 100) return fmtCount(n);
  if (n >= 10) return n.toFixed(1).replace(/\.0$/, "");
  return String(Number(n.toFixed(n < 1 ? 2 : 1)));
}

/**
 * bandMs is where a log-spaced latency band starts: band i of perDecade
 * runs from 10^(i/perDecade) to 10^((i+1)/perDecade) milliseconds. The
 * daemon's histograms and heatmaps count in these bands.
 */
export function bandMs(band: number, perDecade: number): number {
  return 10 ** (band / perDecade);
}

/** fmtRange writes a latency band: 100–178ms, 560ms–1s. One unit when both ends share it. */
export function fmtRange(loMs: number, hiMs: number): string {
  const lo = fmtMsShort(loMs);
  const hi = fmtMsShort(hiMs);
  const unit = (s: string) => s.replace(/^[\d.]+/, "");
  return unit(lo) === unit(hi) ? `${lo.slice(0, lo.length - unit(lo).length)}–${hi}` : `${lo}–${hi}`;
}

/** fmtBytes writes a size: 0 B, 812 B, 1.6 MB, 312 GB. */
export function fmtBytes(n: number): string {
  if (!Number.isFinite(n)) return "–";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let v = Math.max(0, n);
  let u = 0;
  while (v >= 1000 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return `${u === 0 || v >= 100 ? Math.round(v) : v.toFixed(1).replace(/\.0$/, "")} ${units[u]}`;
}

/** fmtInt writes a whole number in full, with separators: 112,941. */
export function fmtInt(n: number): string {
  return Number.isFinite(n) ? Math.round(n).toLocaleString("en-US") : "–";
}

const plural = (n: number, unit: string) => `${n} ${unit}${n === 1 ? "" : "s"}`;

/** fmtFor writes a length of time in words, to its largest unit: 40 seconds, 12 minutes, 3 hours, 2 days. */
export function fmtFor(ms: number): string {
  if (!Number.isFinite(ms)) return "–";
  const s = Math.max(0, ms) / 1000;
  if (s < 60) return plural(Math.floor(s), "second");
  if (s < 3600) return plural(Math.floor(s / 60), "minute");
  if (s < 86_400) return plural(Math.floor(s / 3600), "hour");
  return plural(Math.floor(s / 86_400), "day");
}

/** fmtAgo writes how long ago something happened, short enough for a column: now, 8 s ago, 12 m ago, 3 h ago, 2 d ago. */
export function fmtAgo(ms: number): string {
  if (!Number.isFinite(ms)) return "–";
  const s = Math.max(0, ms) / 1000;
  if (s < 2) return "now";
  if (s < 60) return `${Math.floor(s)} s ago`;
  if (s < 3600) return `${Math.floor(s / 60)} m ago`;
  if (s < 86_400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86_400)} d ago`;
}

/** fmtRate writes a count per minute: 42/m, 1.1k/m. Empty for none, so a quiet row shows nothing. */
export function fmtRate(perMinute: number): string {
  if (!Number.isFinite(perMinute) || perMinute <= 0) return "";
  if (perMinute < 1) return "<1/m";
  return `${fmtCount(perMinute)}/m`;
}
