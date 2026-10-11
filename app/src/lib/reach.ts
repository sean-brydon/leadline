// Reaching another machine's Leadline, as the Machines view says it: what a
// daemon's health answer means, how its version compares with this
// computer's, and the commands a person is given to copy. Kept apart from
// React so it can be tested.

import { fmtAgo, fmtBytes, fmtCount } from "./format.ts";

/** What `GET /api/health` answers from a Leadline daemon. */
export type Health = { ok?: boolean; name?: string; version?: string };

/**
 * What asking an address for its health came to. "none": nothing answered
 * (no forward, no daemon). "other": something answered, and it isn't a
 * Leadline. "leadline": one did, with its version.
 */
export type Probe = { state: "leadline"; version: string } | { state: "other" } | { state: "none" };

/** probeOf reads a health answer: a Leadline says its name and a version. */
export function probeOf(body: unknown): Probe {
  const h = body as Health | null;
  if (!h || typeof h !== "object" || h.name !== "leadline" || typeof h.version !== "string" || h.version === "") return { state: "other" };
  return { state: "leadline", version: h.version.slice(0, 64) };
}

/** Where a box's Leadline listens: what scripts/install.sh sets up, and what a forward is opened to. */
export const LEADLINE_PORT = 4320;

/** The one line that installs Leadline on a Linux machine (README). */
export const INSTALL_LINE = "curl -fsSL https://github.com/sean-brydon/leadline/releases/latest/download/install.sh | sh";

/** sshLine is the forward a person runs themselves: the remote machine's Leadline, on a port of this computer. */
export function sshLine(localPort: number | string = 4336): string {
  return `ssh -N -L ${localPort}:127.0.0.1:${LEADLINE_PORT} user@host`;
}

// A released version is three numbers; anything else ("dev", a commit) can't be ordered.
function numbers(version: string): number[] | null {
  const m = /^v?(\d+)\.(\d+)\.(\d+)/.exec(version.trim());
  return m ? [Number(m[1]), Number(m[2]), Number(m[3])] : null;
}

/** compared orders two versions: -1 when `a` is the older, 1 when the newer, 0 the same, null when they can't be ordered. */
export function compared(a: string, b: string): -1 | 0 | 1 | null {
  if (a === b) return 0;
  const x = numbers(a);
  const y = numbers(b);
  if (!x || !y) return null;
  for (let i = 0; i < 3; i++) {
    if (x[i] !== y[i]) return x[i] < y[i] ? -1 : 1;
  }
  return 0;
}

/**
 * versionNote says how a machine's Leadline compares with this computer's,
 * when they differ. A different version is shown and never refused: the
 * views already leave out what an older daemon can't answer. Empty when
 * they are the same, or this computer's isn't known.
 */
export function versionNote(theirs: string, ours: string | undefined): string {
  if (!ours || theirs === ours) return "";
  const order = compared(theirs, ours);
  if (order === 0) return "";
  if (order === -1) return `Older than this Mac's ${ours}: a screen may show less there.`;
  if (order === 1) return `Newer than this Mac's ${ours}: update Leadline here to see everything it has.`;
  return `Not the version on this Mac (${ours}).`;
}

/** answerLine is a machine's Leadline in a few words, for its row. */
export function answerLine(probe: Probe | undefined): string {
  if (!probe) return "Checking…";
  if (probe.state === "leadline") return `Leadline ${probe.version}`;
  if (probe.state === "other") return "Not a Leadline";
  return "No answer";
}

/** noAnswer says why nothing useful came back from an address, in a sentence. `where` is the machine or the address, `through` the port on this computer. */
export function noAnswer(probe: Probe, where: string, through: string): string {
  if (probe.state === "other") return `Something answers on port ${through}, and it isn't Leadline.`;
  return `Nothing answers from ${where} on port ${through}.`;
}

/** What a daemon holds, from its summary and storage answers; null when it gave neither. */
export type Holding = {
  summary?: { spans: number; logs: number; traces: number; services: string[] } | null;
  storage?: { size_bytes: number; retain_ms: number } | null;
};

/** holdsLine is how much a machine's Leadline holds: "8.2 MB · 20k spans, 2.6k logs". */
export function holdsLine(h: Holding): string {
  const size = h.storage ? (h.storage.size_bytes > 0 ? fmtBytes(h.storage.size_bytes) : "In memory") : "";
  const n = (count: number, one: string) => `${fmtCount(count)} ${count === 1 ? one : `${one}s`}`;
  const counts = h.summary ? `${n(h.summary.spans, "span")}, ${n(h.summary.logs, "log")}` : "";
  return [size, counts].filter(Boolean).join(" · ");
}

/** sourcesLine is how many sources have sent to it: "3 sources". */
export function sourcesLine(h: Holding): string {
  if (!h.summary) return "";
  const n = h.summary.services.length;
  return n === 0 ? "Nothing has reached it yet" : `${n} ${n === 1 ? "source" : "sources"}`;
}

/** seenLine is when a machine last answered: "now" while it does, else how long ago, or never. */
export function seenLine(answers: boolean, seenMs: number | undefined, nowMs: number): string {
  if (answers) return "now";
  if (!seenMs) return "never";
  return fmtAgo(Math.max(0, nowMs - seenMs));
}

// A daemon looked at through a forwarded port is reached at one address
// and listens at another: 127.0.0.1:14321 on this computer, its own 4320
// on its machine. What a person runs on that machine has to name the
// second.

/**
 * ownAddress is the address a daemon listens at on its own machine, given
 * what it says of itself (the first of its sources' OTLP endpoints is its
 * main listener) and the address this window reaches it at. Until it has
 * said, and for a daemon that names none, that is the address it is reached
 * at, which is right for this computer's own.
 */
export function ownAddress(endpoints: string[] | undefined, reachedAt: string): string {
  const own = endpoints?.[0];
  return own && /^http:\/\/[^/]+$/.test(own) ? own : reachedAt;
}

/**
 * homed rewrites text a daemon wrote for a person so that it names the
 * daemon's own address. A current daemon already does; one from before
 * that built addresses from the request's Host, so through a forward its
 * agent prompt and its scan's advice name the forwarded port. Nothing is
 * changed when the two addresses are the same, and the forwarded address
 * is only replaced where it stands whole (":14321" is not found inside
 * ":143210").
 */
export function homed(text: string, reachedAt: string, own: string): string {
  if (!text || reachedAt === own || !text.includes(reachedAt)) return text;
  const escaped = reachedAt.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return text.replace(new RegExp(`${escaped}(?!\\d)`, "g"), own);
}
