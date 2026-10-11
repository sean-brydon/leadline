// Looking around a machine, as the onboarding says it: a scan's result
// (internal/discover/API.md) turned into what is ticked, the sentences on
// the screen, and what counts as having heard from a source. Kept apart
// from React so it can be tested.
//
// Looking is two steps, and the second is the person's to take. A scan
// names projects and their env files and opens none of them; the env
// files of a project are read only once the person has ticked it and
// pressed the button that says so. Nothing here ticks a project for
// reading by itself.

import type { Box, Connection, ConnectAdvice, FoundConnection, FoundProject, Listener, Run, Scan, ServiceSource, Sources } from "./api";
import { fmtCount, fmtFor, fmtTick } from "./format.ts";
import { kindName, kindVersion, QUIET_AFTER_MS, signals, versionOf } from "./sources.ts";

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

/** sentence joins parts as a sentence lists them: "a", "a and b", "a, b and c". */
export function sentence(parts: string[]): string {
  if (parts.length <= 1) return parts[0] ?? "";
  return `${parts.slice(0, -1).join(", ")} and ${parts[parts.length - 1]}`;
}

/** shortPath writes a path under someone's home with a ~, as a person would. */
export function shortPath(path: string): string {
  return path.replace(/^\/(?:Users|home)\/[^/]+(?=\/|$)/, "~");
}

/**
 * tailPath is a path short enough for one line: under a home folder with a
 * ~, and when it is still long, its last folders, which say the most.
 */
export function tailPath(path: string, keep = 3, max = 44): string {
  const short = shortPath(path);
  if (short.length <= max) return short;
  const parts = short.split("/").filter(Boolean);
  return parts.length <= keep ? short : `…/${parts.slice(-keep).join("/")}`;
}

/** progressLine says what a running scan is doing now. */
export function progressLine(scan: Pick<Scan, "progress" | "folders">): string {
  const p = scan.progress;
  switch (p.phase) {
    case "listening":
      return "Looking at what is listening…";
    case "projects":
      return "Finding the projects behind what is running…";
    case "folders": {
      const at = scan.folders.find((f) => f.state === "pending");
      const where = at ? tailPath(at.path) : "your folders";
      return `Looking in ${where}: ${plural(p.directories, "folder")} so far, ${plural(p.projects, "project")} found…`;
    }
    default:
      return "Looking around…";
  }
}

/** emptyScan says a finished scan found nothing to connect: no project, so no env file to read either. */
export function emptyScan(scan: Pick<Scan, "projects" | "connections">): boolean {
  return scan.projects.length === 0 && scan.connections.length === 0;
}

// What is ticked.

/**
 * connKey names a found connection across scans. Its id is only good for
 * the scan it came in, and looking in one more folder is a new scan: what
 * the person had ticked is kept by the masked URL, which stays the same.
 */
export function connKey(c: Pick<FoundConnection, "kind" | "url">): string {
  return `${c.kind}\n${c.url}`;
}

export type Picks = {
  /** By connKey: whether to connect it, and whether to capture its traffic. */
  conns: Record<string, { on: boolean; capture: boolean }>;
  /** By project id: whether the person means to connect it now. */
  projects: Record<string, boolean>;
  /** By project id: the person lets Leadline read this project's env files. Only ever set by their own tick. */
  env: Record<string, boolean>;
};

export const NO_PICKS: Picks = { conns: {}, projects: {}, env: {} };

/** canPick says a found connection can be ticked at all: Leadline can connect to it, and hasn't already. */
export function canPick(c: FoundConnection): boolean {
  return c.connectable && !c.saved;
}

// A Postgres is worth capturing from the start; a Redis pays for it (MONITOR
// can halve a busy server's throughput), so that is the person's call.
const captureFirst = (c: FoundConnection) => c.capture_available && c.kind === "postgres";

// A scan of a folder can find dozens of projects: past this many, only the
// ones running now are ticked to begin with.
const TICK_ALL_UP_TO = 4;

/**
 * picksFor is what is ticked for a scan: the person's own choices where
 * they made one (`was`), and for anything new the daemon's suggestion.
 * Remote servers start unticked. A project is ticked for connecting when
 * it is not sending yet and is running now, or when there are only a few.
 *
 * Reading env files has no suggestion: a project is ticked for that only
 * if the person ticked it, and the tick is dropped once its files have
 * been read or it has none.
 */
export function picksFor(scan: Pick<Scan, "connections" | "projects" | "env_projects">, was: Picks = NO_PICKS): Picks {
  const conns: Picks["conns"] = {};
  for (const c of scan.connections) {
    if (!canPick(c)) continue;
    const key = connKey(c);
    const had = was.conns[key];
    conns[key] = { on: had?.on ?? c.suggested, capture: c.capture_available && (had?.capture ?? captureFirst(c)) };
  }
  const todo = scan.projects.filter((p) => !p.sends.already);
  const projects: Picks["projects"] = {};
  for (const p of todo) projects[p.id] = was.projects[p.id] ?? (p.running.length > 0 || todo.length <= TICK_ALL_UP_TO);
  const env: Picks["env"] = {};
  for (const p of scan.projects) if (was.env[p.id] === true && canRead(scan, p)) env[p.id] = true;
  return { conns, projects, env };
}

// Reading env files: the second step, and only for the projects ticked.

/** wasRead says the env step has been asked for this project in this scan. */
export function wasRead(scan: Pick<Scan, "env_projects">, p: Pick<FoundProject, "id">): boolean {
  return (scan.env_projects ?? []).includes(p.id);
}

/** canRead says a project can be ticked for reading: it has env files, and they have not been asked for yet. */
export function canRead(scan: Pick<Scan, "env_projects">, p: Pick<FoundProject, "id" | "env_files">): boolean {
  return p.env_files.length > 0 && !wasRead(scan, p);
}

/** toRead are the projects whose env files the person has ticked to be read, in the scan's order. */
export function toRead(scan: Pick<Scan, "projects" | "env_projects">, picks: Picks): FoundProject[] {
  return scan.projects.filter((p) => picks.env[p.id] === true && canRead(scan, p));
}

/**
 * readLabel is the button that reads env files, saying exactly what a
 * press does: "Read 3 env files in 2 projects for database addresses".
 * Empty when no project is ticked: then there is nothing to press.
 */
export function readLabel(scan: Pick<Scan, "projects" | "env_projects">, picks: Picks): string {
  const projects = toRead(scan, picks);
  if (projects.length === 0) return "";
  const files = projects.reduce((n, p) => n + p.env_files.length, 0);
  return `Read ${plural(files, "env file")} in ${plural(projects.length, "project")} for database addresses`;
}

/** What a project's env files are to the screen that asks about them. */
export type EnvRow = {
  project: FoundProject;
  /** "ask": can be ticked. "read": asked for, and every file was opened. "partial": asked for, and a file could not be opened. */
  state: "ask" | "read" | "partial";
  /** For one that was asked for: what was taken from the files that opened. Empty when none did. */
  line: string;
  /** The files that could not be opened, by name. */
  unopened: string[];
};

/**
 * envRows is the list the person gives permission from: every project
 * that has env files, with what became of them, and apart from those the
 * projects that have none (which have nothing to ask about).
 */
export function envRows(scan: Pick<Scan, "projects" | "env_projects">): { rows: EnvRow[]; without: FoundProject[] } {
  const rows: EnvRow[] = [];
  const without: FoundProject[] = [];
  for (const p of scan.projects) {
    if (p.env_files.length === 0) {
      without.push(p);
      continue;
    }
    if (!wasRead(scan, p)) {
      rows.push({ project: p, state: "ask", line: "", unopened: [] });
      continue;
    }
    const opened = p.env_files.filter((f) => f.read);
    const unopened = p.env_files.filter((f) => !f.read).map((f) => f.file);
    const urls = opened.reduce((n, f) => n + f.urls, 0);
    const others = opened.reduce((n, f) => n + f.other_keys, 0);
    const passed = others > 0 ? `${plural(others, "other value")} passed over` : "";
    const line = opened.length === 0 ? "" : [urls > 0 ? `${plural(urls, "connection URL")} taken` : "No connection URL in " + (opened.length === 1 ? "it" : "them"), passed].filter(Boolean).join(", ");
    rows.push({ project: p, state: unopened.length > 0 ? "partial" : "read", line, unopened });
  }
  return { rows, without };
}

/** withoutLine says which projects have no env file: by name when few, by count when many. */
export function withoutLine(without: Pick<FoundProject, "name">[]): string {
  if (without.length === 0) return "";
  if (without.length > 3) return `${without.length} projects have no env file, so there is nothing of theirs to read.`;
  return `${sentence(without.map((p) => p.name))} ${without.length === 1 ? "has" : "have"} no env file, so there is nothing of ${without.length === 1 ? "its" : "theirs"} to read.`;
}

// The daemon keeps the URLs it read for half an hour after the last env
// step, then forgets them. Near the end the screen says so.
export const HELD_SOON_MS = 10 * 60_000;

export type Held = {
  /** "none": nothing is held that could still be connected. "held": plenty of time. "soon": under ten minutes left. "gone": let go. */
  state: "none" | "held" | "soon" | "gone";
  line: string;
};

/** held says how long the daemon will still hold the URLs it read, for a person who lingers on the list. */
export function held(scan: Pick<Scan, "secrets_until_ms" | "connections">, nowMs: number): Held {
  const until = scan.secrets_until_ms;
  if (!until || !scan.connections.some(canPick)) return { state: "none", line: "" };
  const left = until - nowMs;
  if (left <= 0) return { state: "gone", line: "Leadline has let go of the URLs it read: it holds them for half an hour and then forgets them. Nothing was saved that you didn't connect." };
  if (left <= HELD_SOON_MS) {
    const mins = Math.max(1, Math.ceil(left / 60_000));
    return { state: "soon", line: `Leadline holds these URLs for ${mins === 1 ? "one more minute" : `${mins} more minutes`}, until ${fmtTick(until, false)}, and then forgets them. Connect what you want before then.` };
  }
  return { state: "held", line: "" };
}

/** againLabel is the button on a list whose URLs were let go: it looks again and reads the same projects' env files. */
export function againLabel(scan: Pick<Scan, "env_projects">): string {
  const n = (scan.env_projects ?? []).length;
  return n === 1 ? "Look again and read that project's env files" : `Look again and read those ${n} projects' env files`;
}

/** pickedConns are the connections to save, as the save call wants them. */
export function pickedConns(scan: Pick<Scan, "connections">, picks: Picks): { id: string; capture: boolean }[] {
  return scan.connections.filter((c) => canPick(c) && picks.conns[connKey(c)]?.on).map((c) => ({ id: c.id, capture: picks.conns[connKey(c)].capture }));
}

/** pickedProjects are the projects the person means to connect now. */
export function pickedProjects(scan: Pick<Scan, "projects">, picks: Picks): FoundProject[] {
  return scan.projects.filter((p) => !p.sends.already && picks.projects[p.id]);
}

/** connectLabel is the button that goes on: "Connect 1 database, 1 cache and 2 projects". Empty when nothing is ticked. */
export function connectLabel(scan: Pick<Scan, "connections" | "projects">, picks: Picks): string {
  const ids = new Set(pickedConns(scan, picks).map((c) => c.id));
  const conns = scan.connections.filter((c) => ids.has(c.id));
  const caches = conns.filter((c) => c.kind === "redis").length;
  const parts = [
    conns.length - caches > 0 && plural(conns.length - caches, "database"),
    caches > 0 && plural(caches, "cache"),
    pickedProjects(scan, picks).length > 0 && plural(pickedProjects(scan, picks).length, "project"),
  ].filter(Boolean) as string[];
  return parts.length ? `Connect ${sentence(parts)}` : "";
}

// The sentences on the scan.

/** connTitle is what a found connection is called: its database's name, or "Redis". */
export function connTitle(c: Pick<FoundConnection, "kind" | "database" | "host">): string {
  if (c.kind === "redis") return c.database && c.database !== "0" ? `Redis, database ${c.database}` : "Redis";
  return c.database || c.host;
}

/** foundLine is where a connection URL was found: "DATABASE_URL in acme/.env.local", and how many more places. */
export function foundLine(c: Pick<FoundConnection, "found_in">): string {
  // The one the dev tools would use, when a file that outranks another sets the same variable.
  const at = c.found_in.find((f) => f.effective) ?? c.found_in[0];
  if (!at) return "";
  const more = c.found_in.length - 1;
  return `${at.key} in ${at.project}/${at.file}${more > 0 ? ` and ${plural(more, "more place")}` : ""}`;
}

/** whereLine is where a found server is, and whether something answers there now. */
export function whereLine(c: Pick<FoundConnection, "remote" | "host" | "listening" | "kind">, here: string): string {
  if (c.remote) return `At ${c.host}, on another machine`;
  const port = c.host.split(":").pop();
  const at = `On ${here}${port && /^\d+$/.test(port) ? `, port ${port}` : ""}`;
  return c.listening ? at : `${at}. Nothing is listening there now`;
}

/** whatHappens is what ticking a found connection does, in a few words. */
export function whatHappens(c: FoundConnection, capture: boolean): string {
  if (c.saved) return "Already added";
  if (!c.connectable) return c.reason === "not_supported" ? `${foundKind(c.kind)} can't be connected yet` : "Can't be connected";
  if (capture) return c.kind === "redis" ? "Capture every command" : "Capture every query";
  return c.remote ? "Read its statistics only" : "Read its statistics";
}

const KIND: Record<string, string> = { mysql: "MySQL", mongodb: "MongoDB" };
/** foundKind names every kind of server a scan can find, the ones Leadline can't connect to yet included. */
export function foundKind(kind: string): string {
  return KIND[kind] ?? kindName(kind);
}

/**
 * envLine is the plain account of what was read from env files, from the
 * daemon's own counts. The files are parsed in the daemon's memory; every
 * value that is not a connection URL is passed over and only counted.
 */
export function envLine(env: Scan["env"], here: string): string {
  if (env.files_read === 0) return "No env file has been opened.";
  const files = plural(env.files_read, "env file");
  const others = env.other_keys === 0 ? "" : ` The other ${plural(env.other_keys, "value")} in ${env.files_read === 1 ? "that file" : "those files"} ${env.other_keys === 1 ? "was" : "were"} passed over: counted, never kept.`;
  if (env.urls === 0) return `Leadline looked in ${files} and found no connection URL.${others}`;
  return `Leadline took only the ${plural(env.urls, "connection URL")} from ${files}.${others} Passwords stay in the daemon on ${here}, and are never shown here.`;
}

/** adviceTitle is how a project would be connected, in a few words. */
export function adviceTitle(a: Pick<ConnectAdvice, "kind">): string {
  switch (a.kind) {
    case "already":
      return "Already sending";
    case "next-plugin":
      return "Add the Next.js plugin";
    case "otel-env":
      return "Point its OpenTelemetry at Leadline";
    default:
      return "Put leadline run in front of its dev command";
  }
}

// The advice names the daemon's address itself wherever it is not the
// usual one (`leadline run --url …`, and LEADLINE_URL for the Next.js
// plugin), from the address the scan was asked at: nothing here adds it.

/** adviceCommand is the one line that says how a project would be connected, for the scan's list. */
export function adviceCommand(p: Pick<FoundProject, "connect">): string {
  const a = p.connect;
  if (a.kind === "already") return "";
  if (a.kind === "otel-env") return a.lines?.[0] ?? "";
  return a.command ?? "";
}

/** One thing to copy and where it goes. */
export type Step = { label: string; text: string; copy: string };

/**
 * steps are what to do to connect a project, each with the exact text to
 * copy. The app changes nothing in a project: these are for the person, or
 * their coding agent. `running` says its dev server is up now, which then
 * has to be restarted.
 */
export function steps(p: Pick<FoundProject, "connect" | "running">): Step[] {
  const a = p.connect;
  const up = p.running.length > 0;
  const out: Step[] = [];
  const lines = (a.lines ?? []).join("\n");
  const envLines = (a.env_lines ?? []).join("\n");
  const envFile = a.env_file ?? ".env.local";
  if (a.kind === "next-plugin") {
    if (a.command) out.push({ label: "Install the plugin", text: a.command, copy: "Copy the install command" });
    if (lines) out.push({ label: `Add to ${a.file ?? "next.config.ts"}, around the config it exports`, text: lines, copy: `Copy the lines for ${a.file ?? "next.config.ts"}` });
    // The plugin looks for Leadline at the usual address unless told: the daemon gives the line when it is somewhere else.
    if (envLines) out.push({ label: `Tell it where this Leadline is, in ${envFile}`, text: envLines, copy: `Copy the ${a.env_lines?.length === 1 ? "line" : "lines"} for ${envFile}` });
  } else if (a.kind === "otel-env") {
    if (lines) out.push({ label: `Add to ${a.file ?? ".env"}`, text: lines, copy: `Copy the lines for ${a.file ?? ".env"}` });
    if (a.command) out.push({ label: "Or start it with leadline run, which sets them itself", text: a.command, copy: "Copy the command" });
  } else if (a.kind !== "already") {
    if (a.command) out.push({ label: up ? "Stop it, then start it with" : "Start it with", text: a.command, copy: "Copy the command" });
    if (a.traces?.command) out.push({ label: a.traces.summary, text: a.traces.command, copy: "Copy the install command" });
  }
  return out;
}

/** upFor writes how long something has been up: "up 3 hours". Empty when the daemon didn't say. */
export function upFor(uptimeS: number | undefined): string {
  return uptimeS && uptimeS > 0 ? `up ${fmtFor(uptimeS * 1000)}` : "";
}

/** runningLine is a project's dev servers now: "next dev on port 3000, up 3 hours". Empty when none. */
export function runningLine(p: Pick<FoundProject, "running">): string {
  return sentence(p.running.map((r) => [`${r.what} on port ${r.port}`, upFor(r.uptime_s)].filter(Boolean).join(", ")));
}

/** whereProject is a project's second line: its folder, and its branch when it is in git. */
export function whereProject(p: Pick<FoundProject, "path" | "git">): string {
  const branch = p.git?.branch ? `${p.git.worktree ? "worktree " : ""}${p.git.branch}` : "";
  return [tailPath(p.path), branch].filter(Boolean).join(" · ");
}

export type RunningRow = { key: string; what: string; aside: string; kind: string };

/**
 * runningRows is "Running right now": the dev servers and the databases
 * that are listening, each with whose it is, and how many other ports were
 * left out as nothing Leadline knows what to do with.
 */
export function runningRows(scan: Pick<Scan, "listeners" | "projects">): { rows: RunningRow[]; others: number } {
  const name = (id: string | undefined) => scan.projects.find((p) => p.id === id)?.name ?? "";
  const known = scan.listeners.filter((l) => l.kind !== "other");
  const rows = known.map((l: Listener) => ({
    key: `${l.pid}:${l.port}`,
    kind: l.kind,
    what: `${l.what || l.process || "Something"} on port ${l.port}`,
    aside: [name(l.project_id), upFor(l.uptime_s)].filter(Boolean).join(" · "),
  }));
  return { rows, others: scan.listeners.length - known.length };
}

/** folderLine is what became of a folder the person pointed at. */
export function folderLine(f: Scan["folders"][number]): { text: string; bad: boolean } {
  switch (f.state) {
    case "scanned":
      return { text: `${plural(f.projects, "project")}${f.truncated ? ", and more: the search stopped at its limit" : ""}`, bad: false };
    case "refused":
      return { text: f.reason || "Leadline won't look through a folder that wide. Name one inside it.", bad: true };
    case "missing":
      return { text: f.reason || "It isn't a folder that can be read.", bad: true };
    case "pending":
      return { text: "not looked in yet", bad: false };
    default:
      return { text: "the search was stopped", bad: false };
  }
}

// Shipyard.

/** boxes are the owner's Shipyard boxes other than this computer; none when Shipyard isn't here or didn't answer. */
export function boxesOf(s: { available: boolean; boxes: Box[] } | null | undefined): Box[] {
  return s?.available ? s.boxes.filter((b) => !b.local) : [];
}

/** boxState is a box's state in words: "online, 86 ms away". */
export function boxState(b: Pick<Box, "state" | "latency_ms" | "since_ms">, nowMs: number): string {
  if (b.state === "online") return b.latency_ms ? `online, ${b.latency_ms} ms away` : "online";
  if (b.state === "offline") return b.since_ms ? `offline for ${fmtFor(nowMs - b.since_ms)}` : "offline";
  if (b.state === "untrusted") return "not trusted yet";
  return b.state || "unknown";
}

// The checklist: what counts as having heard from a source.

export type Heard = {
  /** "done": the daemon has heard from it. "wait": not yet. "err": something is wrong. */
  tone: "done" | "wait" | "err";
  line: string;
  hint?: string;
  /** A count for the line's end: "38 queries". */
  aside?: string;
};

/**
 * heardConn says whether Leadline has really heard from a saved
 * connection: the server answered its health check. A capture's own first
 * statement is said beside that, since it waits on an app being pointed at
 * the proxy.
 */
export function heardConn(c: Connection): Heard {
  if (c.health.state === "error") return { tone: "err", line: c.health.message || "It can't be reached.", hint: c.health.hint };
  if (c.health.state !== "ok") return { tone: "wait", line: "Checking that it answers…" };
  const answers = `${kindVersion(c.kind, versionOf(c.health))} answers.`;
  if (c.proxy) {
    if (c.proxy.state === "error") return { tone: "err", line: `${answers} But the proxy on port ${c.proxy.port} couldn't open: ${c.proxy.error ?? "its port is taken"}.`, hint: "Leadline tries the port again every few seconds." };
    if (c.proxy.statements > 0) return { tone: "done", line: `${answers} Capturing through port ${c.proxy.port}.`, aside: `${fmtCount(c.proxy.statements)} ${c.proxy.statements === 1 ? "query" : "queries"}` };
    return { tone: "done", line: `${answers} Capturing through port ${c.proxy.port}: no query has come through it yet.` };
  }
  if (c.monitor) {
    if (c.monitor.state === "error") return { tone: "err", line: `${answers} But its commands aren't being captured: ${c.monitor.error ?? "MONITOR failed"}.` };
    return { tone: "done", line: `${answers} Capturing every command.`, aside: c.monitor.recorded > 0 ? `${fmtCount(c.monitor.recorded)} ${c.monitor.recorded === 1 ? "command" : "commands"}` : undefined };
  }
  return { tone: "done", line: `${answers} Its statistics are being read.` };
}

const inside = (cwd: string, path: string) => cwd === path || cwd.startsWith(`${path}/`);

/**
 * heardProject says whether the daemon has heard from a project: a service
 * by the name it would send under (the one Leadline already holds data
 * for, its own name, or the name of a `leadline run` in its folder) that
 * has sent something lately. A run that has started and said nothing yet
 * is still a wait, and says so.
 */
export function heardProject(p: Pick<FoundProject, "name" | "path" | "sends">, sources: Pick<Sources, "now_ms" | "services"> | undefined, runs: Run[] | null | undefined, sinceMs: number): Heard & { service?: ServiceSource; run?: Run } {
  const mine = (runs ?? []).filter((r) => inside(r.cwd, p.path));
  const names = new Set([p.sends.service, p.name, ...mine.map((r) => r.service)].filter(Boolean) as string[]);
  const live = (s: ServiceSource) => s.last_seen_ms >= sinceMs || (sources ? sources.now_ms - s.last_seen_ms <= QUIET_AFTER_MS : false);
  const service = sources?.services.find((s) => names.has(s.name) && s.last_seen_ms > 0 && live(s));
  if (service) {
    const what = signals(service);
    const only = service.via.length > 0 && !service.via.includes("otlp") && service.spans > 0 && service.logs === 0 ? "queries" : what;
    return { tone: "done", line: `${service.name} is sending${only ? `: ${only}` : ""}.`, aside: service.spans > 0 ? `${fmtCount(service.spans)} ${service.spans === 1 ? "span" : "spans"}` : undefined, service };
  }
  const run = mine.find((r) => r.running);
  if (run) return { tone: "wait", line: `${run.command} is running under leadline run as ${run.service}. Waiting for its first line or request.`, run };
  return { tone: "wait", line: "" };
}
