// What the daemon keeps, in words: how much is on disk, since when, and
// when it goes. The sidebar's foot and the Sources page both say it, from
// /api/storage and /api/summary. Kept apart from React so it can be tested.

import type { Storage, Summary } from "./api";
import { fmtAgo, fmtBytes, fmtCount } from "./format.ts";

/** fmtKeep writes how long data is kept, in whole days when it is, else in hours: "3 days", "36 hours". */
export function fmtKeep(ms: number): string {
  const hours = Math.round(ms / 3_600_000);
  if (hours >= 24 && hours % 24 === 0) return `${hours / 24} ${hours === 24 ? "day" : "days"}`;
  if (ms >= 3_600_000) return `${hours} ${hours === 1 ? "hour" : "hours"}`;
  const minutes = Math.max(1, Math.round(ms / 60_000));
  return `${minutes} ${minutes === 1 ? "minute" : "minutes"}`;
}

export type Kept = {
  /** Two short sentences, for the foot of the sidebar. */
  short: string;
  /** The whole of it, for the Sources page. */
  long: string;
  /** How full the store is against its size cap, 0..1; null when there is no cap, or nothing on disk to measure. */
  full: number | null;
  /** The meter's words: "8.2 MB of 2 GB". */
  fullText: string;
};

/**
 * kept says what is held and for how long. `here` is the machine whose
 * daemon holds it, as in "this Mac" or "hetzner-1". `away` says that
 * machine is another one, looked at from this computer: the data is kept
 * there, and what this window shows of it does travel here, so the lines
 * say that and don't claim nothing leaves. `storage` is null from a daemon
 * too old to report it, and then the lines count what is held and promise
 * no more.
 */
export function kept(here: string, summary: Summary | undefined, storage: Storage | null | undefined, nowMs: number, away = false): Kept {
  const none: Pick<Kept, "full" | "fullText"> = { full: null, fullText: "" };
  const n = (count: number, one: string) => `${fmtCount(count)} ${count === 1 ? one : `${one}s`}`;
  const counts = summary ? `${n(summary.spans, "span")}, ${n(summary.logs, "log")} and ${n(summary.traces, "trace")}` : "";
  const stays = away ? `Everything is kept on ${here}; this window only reads it.` : `Everything stays on ${here}.`;
  const leaves = away ? `It is all kept on ${here}, and leaves only for this window, through the port forwarded to this Mac.` : `Nothing leaves ${here}.`;
  if (!storage) {
    if (!summary) return { short: stays, long: leaves, ...none };
    return {
      short: `${stays} Holding ${n(summary.spans, "span")} and ${n(summary.logs, "log")}.`,
      long: `${leaves} Leadline holds ${counts} now, and drops the oldest by itself.`,
      ...none,
    };
  }
  const onDisk = storage.size_bytes > 0;
  const size = fmtBytes(storage.size_bytes);
  const until = storage.retain_ms > 0 ? `for up to ${fmtKeep(storage.retain_ms)}` : "until you remove it";
  const short = `${stays} ${onDisk ? `${size} kept` : "Kept in memory"}, ${until}.`;

  const holds = onDisk ? `Leadline holds ${size} on disk${counts ? `: ${counts}` : ""}` : `Leadline holds ${counts || "its data"} in memory, and writes none of it to disk`;
  const oldest = storage.oldest_ms > 0 ? ` The oldest is from ${fmtAgo(nowMs - storage.oldest_ms)}.` : "";
  const cap = storage.max_bytes > 0 ? fmtBytes(storage.max_bytes) : "";
  const rule =
    storage.retain_ms > 0
      ? `Data is kept for ${fmtKeep(storage.retain_ms)}${cap ? `, or until it passes ${cap}` : ""}; the oldest goes first.`
      : cap
        ? `Data is kept until it passes ${cap}; then the oldest goes first.`
        : "Nothing is dropped by itself.";
  return {
    short,
    long: `${leaves} ${holds}.${oldest} ${rule}`,
    full: onDisk && storage.max_bytes > 0 ? Math.min(1, storage.size_bytes / storage.max_bytes) : null,
    fullText: onDisk && cap ? `${size} of ${cap}` : "",
  };
}
