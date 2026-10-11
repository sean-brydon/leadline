import assert from "node:assert/strict";
import test from "node:test";
import type { Connection, FoundConnection, FoundProject, Run, Scan, ServiceSource } from "./api.ts";
import { fmtTick } from "./format.ts";
import {
  adviceCommand,
  adviceTitle,
  againLabel,
  boxesOf,
  boxState,
  canPick,
  canRead,
  connectLabel,
  connKey,
  connTitle,
  emptyScan,
  envLine,
  envRows,
  folderLine,
  foundLine,
  heardConn,
  heardProject,
  held,
  NO_PICKS,
  pickedConns,
  pickedProjects,
  picksFor,
  progressLine,
  readLabel,
  runningLine,
  runningRows,
  sentence,
  shortPath,
  steps,
  tailPath,
  toRead,
  upFor,
  wasRead,
  whatHappens,
  whereLine,
  whereProject,
  withoutLine,
  type Picks,
} from "./discover.ts";

const NOW = 1_791_650_000_000;

const found = (over: Partial<FoundConnection> = {}): FoundConnection => ({
  id: "u_1",
  kind: "postgres",
  url: "postgres://acme:****@127.0.0.1:5432/acme",
  host: "127.0.0.1:5432",
  database: "acme",
  user: "acme",
  remote: false,
  connectable: true,
  capture_available: true,
  suggested: true,
  found_in: [{ project_id: "p_web", project: "acme-web", key: "DATABASE_URL", file: ".env.local", effective: true }],
  saved: false,
  listening: { port: 5432, pid: 611, what: "Postgres" },
  ...over,
});

const project = (over: Partial<FoundProject> = {}): FoundProject => ({
  id: "p_web",
  name: "acme-web",
  path: "/Users/acme/code/acme-web",
  sources: ["listening"],
  stack: "node",
  frameworks: [{ id: "next", name: "Next.js", version: "16.0.1" }],
  label: "Next.js 16",
  package_manager: "pnpm",
  dev_command: "pnpm dev",
  opentelemetry: false,
  sends: { already: false, because: [] },
  git: { root: "/Users/acme/code/acme-web", branch: "main", worktree: false },
  connect: {
    kind: "next-plugin",
    summary: "Add the Next.js plugin: one development dependency and one line in next.config.ts.",
    command: "pnpm add -D @leadline/next",
    file: "next.config.ts",
    lines: ['import { withLeadline } from "@leadline/next";', "export default withLeadline(nextConfig);"],
  },
  running: [{ port: 3000, pid: 4242, what: "next dev", uptime_s: 10_800 }],
  // As a scan gives them: named, not opened.
  env_files: [
    { file: ".env.local", read: false, urls: 0, other_keys: 0 },
    { file: ".env", read: false, urls: 0, other_keys: 0 },
  ],
  connection_ids: [],
  ...over,
});

const wrap = (over: Partial<FoundProject> = {}): FoundProject =>
  project({
    id: "p_api",
    name: "acme-api",
    path: "/Users/acme/code/acme-api",
    label: "Node.js",
    frameworks: [],
    dev_command: "npm run dev",
    running: [],
    env_files: [{ file: ".env", read: false, urls: 0, other_keys: 0 }],
    connect: { kind: "run-wrap", summary: "Put leadline run in front of the dev command.", command: "leadline run -- npm run dev", traces: { summary: "For traces too, add the Node.js provider. leadline run loads it.", command: "npm install --save-dev @leadline/node" } },
    ...over,
  });

const scan = (over: Partial<Scan> = {}): Scan => ({
  id: "s_1",
  state: "done",
  started_ms: NOW,
  progress: { phase: "done", folders_done: 0, folders_total: 0, directories: 0, projects: 0 },
  folders: [],
  machine: { os: "darwin", hostname: "acme-mbp" },
  listeners: [],
  limits: { method: "libproc", complete: true, skipped_processes: 0, unowned_ports: 0 },
  projects: [],
  connections: [],
  env_projects: [],
  env: { files_read: 0, urls: 0, other_keys: 0 },
  ...over,
});

const redis = found({ id: "u_2", kind: "redis", url: "redis://127.0.0.1:6379", host: "127.0.0.1:6379", database: "0", user: "" });
const remote = found({ id: "u_3", url: "postgres://reader:****@db.acme.test:5432/analytics", host: "db.acme.test:5432", database: "analytics", remote: true, capture_available: false, suggested: false, listening: undefined });
const mysql = found({ id: "u_4", kind: "mysql", url: "mysql://acme:****@127.0.0.1:3306/ledger", host: "127.0.0.1:3306", database: "ledger", connectable: false, reason: "not_supported", note: "Leadline connects to Postgres and Redis.", capture_available: false, suggested: false });

test("names are joined as a sentence lists them, and a home folder is a tilde", () => {
  assert.equal(sentence([]), "");
  assert.equal(sentence(["a"]), "a");
  assert.equal(sentence(["a", "b", "c"]), "a, b and c");
  assert.equal(shortPath("/Users/acme/code/acme"), "~/code/acme");
  assert.equal(shortPath("/home/dev/acme"), "~/acme");
  assert.equal(shortPath("/Users/acme"), "~");
  assert.equal(shortPath("/srv/acme"), "/srv/acme");
  assert.equal(tailPath("/Users/acme/code/acme"), "~/code/acme");
  assert.equal(tailPath("/private/var/folders/zz/acme-made-up-temp-folder/work/projects/acme-web"), "…/work/projects/acme-web");
  assert.equal(tailPath("/a-very-long-single-folder-name-that-goes-on-and-on-and-on"), "/a-very-long-single-folder-name-that-goes-on-and-on-and-on");
});

test("a running scan says what it is doing", () => {
  const at = (phase: string, more: Partial<Scan["progress"]> = {}) => progressLine({ progress: { phase, folders_done: 0, folders_total: 1, directories: 0, projects: 0, ...more }, folders: [{ path: "~/code", state: "pending", projects: 0, directories: 0 }] });
  assert.match(at("listening"), /listening/);
  assert.match(at("projects"), /projects behind/);
  assert.equal(at("folders", { directories: 212, projects: 3 }), "Looking in ~/code: 212 folders so far, 3 projects found…");
  // A scan has no phase that reads env files: nothing it does is said to.
  for (const phase of ["listening", "projects", "folders", "something-new"]) assert.doesNotMatch(at(phase), /env/i);
  assert.match(at("something-new"), /Looking around/);
});

test("what the daemon suggests is ticked: local servers on, a remote one off, and capture for Postgres only", () => {
  const s = scan({ connections: [found(), redis, remote, mysql, found({ id: "u_5", url: "postgres://acme:****@127.0.0.1:5432/other", saved: true, suggested: false })] });
  const picks = picksFor(s);
  assert.deepEqual(picks.conns[connKey(found())], { on: true, capture: true });
  // MONITOR costs a busy Redis: connected, and capture left for the person to turn on.
  assert.deepEqual(picks.conns[connKey(redis)], { on: true, capture: false });
  assert.deepEqual(picks.conns[connKey(remote)], { on: false, capture: false });
  // What can't be connected, or already is, has nothing to tick.
  assert.equal(picks.conns[connKey(mysql)], undefined);
  assert.equal(Object.keys(picks.conns).length, 3);
  assert.equal(canPick(mysql), false);
  assert.deepEqual(pickedConns(s, picks), [{ id: "u_1", capture: true }, { id: "u_2", capture: false }]);
});

test("a person's own ticks outlast a new scan, whose ids are new", () => {
  const first = scan({ connections: [found(), redis, remote], projects: [project(), wrap()] });
  let picks = picksFor(first);
  picks = { ...picks, conns: { ...picks.conns, [connKey(found())]: { on: false, capture: false }, [connKey(remote)]: { on: true, capture: false }, [connKey(redis)]: { on: true, capture: true } }, projects: { ...picks.projects, p_api: false } };
  const second = scan({ connections: [found({ id: "u_9" }), { ...redis, id: "u_8" }, { ...remote, id: "u_7" }], projects: [project(), wrap(), wrap({ id: "p_new", name: "acme-new" })] });
  const kept = picksFor(second, picks);
  assert.deepEqual(kept.conns[connKey(found())], { on: false, capture: false });
  assert.deepEqual(kept.conns[connKey(redis)], { on: true, capture: true });
  // A remote server can't be captured whatever was ticked.
  assert.deepEqual(kept.conns[connKey(remote)], { on: true, capture: false });
  assert.equal(kept.projects.p_api, false);
  assert.equal(kept.projects.p_new, true);
  assert.deepEqual(pickedConns(second, kept).map((c) => c.id), ["u_8", "u_7"]);
});

test("a few projects are all ticked; among many, the ones running now", () => {
  const few = scan({ projects: [project(), wrap(), project({ id: "p_done", sends: { already: true, because: ["service"], service: "acme-old" } })] });
  assert.deepEqual(picksFor(few).projects, { p_web: true, p_api: true });
  assert.deepEqual(pickedProjects(few, picksFor(few)).map((p) => p.id), ["p_web", "p_api"]);
  const many = scan({ projects: [project(), ...[1, 2, 3, 4, 5].map((i) => wrap({ id: `p_${i}`, name: `acme-${i}` }))] });
  const picks = picksFor(many);
  assert.equal(picks.projects.p_web, true);
  assert.equal(Object.values(picks.projects).filter(Boolean).length, 1);
});

test("the button says what Connect will do, and nothing when nothing is ticked", () => {
  const s = scan({ connections: [found(), redis, remote], projects: [project(), wrap()] });
  assert.equal(connectLabel(s, picksFor(s)), "Connect 1 database, 1 cache and 2 projects");
  assert.equal(connectLabel(scan({ connections: [found()] }), picksFor(scan({ connections: [found()] }))), "Connect 1 database");
  assert.equal(connectLabel(s, NO_PICKS), "");
  assert.equal(emptyScan(scan()), true);
  assert.equal(emptyScan(s), false);
});

// The scan as it is after the env step was asked for acme-web: its files have counts, and the scan says whose were read.
const readWeb = (files = [{ file: ".env.local", read: true, urls: 2, other_keys: 14 }, { file: ".env", read: true, urls: 0, other_keys: 3 }]) =>
  scan({ projects: [project({ env_files: files, connection_ids: ["u_1", "u_2"] }), wrap(), wrap({ id: "p_bill", name: "acme-billing", env_files: [] })], connections: [found(), redis], env_projects: ["p_web"], env: { files_read: 2, urls: 2, other_keys: 17 } });

test("no project is ticked for reading its env files until the person ticks it", () => {
  const s = scan({ projects: [project(), wrap(), wrap({ id: "p_bill", name: "acme-billing", env_files: [] })] });
  const picks = picksFor(s);
  assert.deepEqual(picks.env, {});
  assert.deepEqual(toRead(s, picks), []);
  assert.equal(readLabel(s, picks), "");
  // Ticked for connecting is another matter, and is as it was.
  assert.deepEqual(picks.projects, { p_web: true, p_api: true, p_bill: true });
  // A scan found dozens: still none.
  const many = scan({ projects: [1, 2, 3, 4, 5, 6, 7, 8].map((i) => project({ id: `p_${i}`, name: `acme-${i}` })) });
  assert.deepEqual(picksFor(many).env, {});
});

test("the button says exactly what a press will read", () => {
  const s = scan({ projects: [project(), wrap(), wrap({ id: "p_bill", name: "acme-billing", env_files: [] })] });
  const tick = (...ids: string[]): Picks => ({ ...picksFor(s), env: Object.fromEntries(ids.map((id) => [id, true])) });
  assert.equal(readLabel(s, tick("p_web", "p_api")), "Read 3 env files in 2 projects for database addresses");
  assert.equal(readLabel(s, tick("p_api")), "Read 1 env file in 1 project for database addresses");
  assert.equal(readLabel(s, tick("p_web")), "Read 2 env files in 1 project for database addresses");
  assert.deepEqual(toRead(s, tick("p_api", "p_web")).map((p) => p.id), ["p_web", "p_api"]);
  // A project with no env file has nothing to read, ticked or not; nor has one the scan does not have.
  assert.equal(readLabel(s, tick("p_bill", "p_gone")), "");
  assert.equal(canRead(s, s.projects[2]), false);
  // An unticked project is not read because another is.
  assert.deepEqual(toRead(s, { ...picksFor(s), env: { p_web: true, p_api: false } }).map((p) => p.id), ["p_web"]);
});

test("a project whose env files were read can't be ticked again, and its tick is let go", () => {
  const before = scan({ projects: [project(), wrap()] });
  const ticked: Picks = { ...picksFor(before), env: { p_web: true, p_api: true } };
  const after = readWeb();
  assert.equal(wasRead(after, after.projects[0]), true);
  assert.equal(canRead(after, after.projects[0]), false);
  assert.equal(canRead(after, after.projects[1]), true);
  // acme-api's tick stays: the person made it, and its files are still unread.
  assert.deepEqual(picksFor(after, ticked).env, { p_api: true });
  assert.equal(readLabel(after, picksFor(after, ticked)), "Read 1 env file in 1 project for database addresses");
  // A new scan has read nothing: the ticks the person made come back with it, and nothing is read until they press.
  assert.deepEqual(picksFor(before, ticked).env, { p_web: true, p_api: true });
  // A daemon that doesn't say whose were read has read none.
  assert.equal(wasRead({}, after.projects[0]), false);
});

test("each project's env files are named, and what became of them is said", () => {
  const unread = envRows(scan({ projects: [project(), wrap(), wrap({ id: "p_bill", name: "acme-billing", env_files: [] })] }));
  assert.deepEqual(unread.rows.map((r) => [r.project.name, r.state, r.line]), [["acme-web", "ask", ""], ["acme-api", "ask", ""]]);
  assert.deepEqual(unread.without.map((p) => p.name), ["acme-billing"]);

  const read = envRows(readWeb());
  assert.deepEqual(read.rows.map((r) => [r.project.name, r.state]), [["acme-web", "read"], ["acme-api", "ask"]]);
  assert.equal(read.rows[0].line, "2 connection URLs taken, 17 other values passed over");
  assert.deepEqual(read.rows[0].unopened, []);

  // One file opened and one did not: the daemon leaves that one unread.
  const partial = envRows(readWeb([{ file: ".env.local", read: true, urls: 1, other_keys: 0 }, { file: ".env", read: false, urls: 0, other_keys: 0 }])).rows[0];
  assert.equal(partial.state, "partial");
  assert.equal(partial.line, "1 connection URL taken");
  assert.deepEqual(partial.unopened, [".env"]);
  // None of them opened.
  const none = envRows(readWeb([{ file: ".env.local", read: false, urls: 0, other_keys: 0 }, { file: ".env", read: false, urls: 0, other_keys: 0 }])).rows[0];
  assert.deepEqual([none.state, none.line, none.unopened], ["partial", "", [".env.local", ".env"]]);
  // Read, with nothing in them to take.
  assert.equal(envRows(readWeb([{ file: ".env", read: true, urls: 0, other_keys: 5 }])).rows[0].line, "No connection URL in it, 5 other values passed over");
  assert.equal(envRows(readWeb([{ file: ".env.local", read: true, urls: 0, other_keys: 0 }, { file: ".env", read: true, urls: 0, other_keys: 1 }])).rows[0].line, "No connection URL in them, 1 other value passed over");
});

test("projects with no env file are said by name when few and by count when many", () => {
  const p = (name: string) => ({ name });
  assert.equal(withoutLine([]), "");
  assert.equal(withoutLine([p("acme-billing")]), "acme-billing has no env file, so there is nothing of its to read.");
  assert.equal(withoutLine([p("a"), p("b")]), "a and b have no env file, so there is nothing of theirs to read.");
  assert.equal(withoutLine([p("a"), p("b"), p("c"), p("d")]), "4 projects have no env file, so there is nothing of theirs to read.");
});

test("the URLs the daemon holds are said to be going, and then gone", () => {
  const until = NOW + 30 * 60_000;
  const s = { ...readWeb(), secrets_until_ms: until };
  assert.deepEqual(held(s, NOW), { state: "held", line: "" });
  assert.equal(held(s, until - 11 * 60_000).state, "held");
  const soon = held(s, until - 8 * 60_000 + 1000);
  assert.equal(soon.state, "soon");
  assert.equal(soon.line, `Leadline holds these URLs for 8 more minutes, until ${fmtTick(until, false)}, and then forgets them. Connect what you want before then.`);
  assert.match(held(s, until - 20_000).line, /for one more minute, until/);
  const gone = held(s, until);
  assert.equal(gone.state, "gone");
  assert.match(gone.line, /has let go of the URLs it read/);
  assert.equal(held(s, until + 3_600_000).state, "gone");
  // Nothing held that could still be connected: nothing to say, however late it is.
  assert.equal(held({ ...s, connections: [found({ saved: true }), mysql] }, until + 1).state, "none");
  assert.equal(held(scan(), NOW).state, "none");
  assert.equal(againLabel(s), "Look again and read that project's env files");
  assert.equal(againLabel({ env_projects: ["p_web", "p_api"] }), "Look again and read those 2 projects' env files");
});

test("a found connection says what it is, where it was found and where it is", () => {
  assert.equal(connTitle(found()), "acme");
  assert.equal(connTitle(redis), "Redis");
  assert.equal(connTitle({ kind: "redis", database: "2", host: "x" }), "Redis, database 2");
  assert.equal(connTitle({ kind: "postgres", database: "", host: "localhost:5432" }), "localhost:5432");
  assert.equal(foundLine(found()), "DATABASE_URL in acme-web/.env.local");
  // The file the dev tools would use comes first, whatever order they were found in.
  const two = found({ found_in: [{ project_id: "p", project: "acme-web", key: "DATABASE_URL", file: ".env", effective: false }, { project_id: "q", project: "acme-api", key: "DATABASE_URL", file: ".env", effective: true }] });
  assert.equal(foundLine(two), "DATABASE_URL in acme-api/.env and 1 more place");
  assert.equal(foundLine(found({ found_in: [] })), "");
  assert.equal(whereLine(found(), "this Mac"), "On this Mac, port 5432");
  assert.equal(whereLine(found({ listening: undefined }), "this Mac"), "On this Mac, port 5432. Nothing is listening there now");
  assert.equal(whereLine(remote, "this Mac"), "At db.acme.test:5432, on another machine");
  assert.equal(whatHappens(found(), true), "Capture every query");
  assert.equal(whatHappens(redis, true), "Capture every command");
  assert.equal(whatHappens(remote, false), "Read its statistics only");
  assert.equal(whatHappens(mysql, false), "MySQL can't be connected yet");
  assert.equal(whatHappens(found({ saved: true }), false), "Already added");
});

test("what was read from env files is said from the daemon's counts, and no more", () => {
  assert.equal(envLine({ files_read: 0, urls: 0, other_keys: 0 }, "this Mac"), "No env file has been opened.");
  assert.equal(
    envLine({ files_read: 4, urls: 3, other_keys: 21 }, "this Mac"),
    "Leadline took only the 3 connection URLs from 4 env files. The other 21 values in those files were passed over: counted, never kept. Passwords stay in the daemon on this Mac, and are never shown here.",
  );
  assert.equal(envLine({ files_read: 1, urls: 1, other_keys: 1 }, "this Mac").includes("took only the 1 connection URL from 1 env file. The other 1 value in that file was passed over"), true);
  assert.equal(envLine({ files_read: 2, urls: 0, other_keys: 9 }, "this Mac"), "Leadline looked in 2 env files and found no connection URL. The other 9 values in those files were passed over: counted, never kept.");
  assert.equal(envLine({ files_read: 1, urls: 2, other_keys: 0 }, "this Mac").includes("other"), false);
});

test("each kind of advice has a title, a line for the list, and steps to copy", () => {
  const next = project();
  assert.equal(adviceTitle(next.connect), "Add the Next.js plugin");
  assert.equal(adviceCommand(next), "pnpm add -D @leadline/next");
  assert.deepEqual(steps(next).map((s) => s.text), ["pnpm add -D @leadline/next", 'import { withLeadline } from "@leadline/next";\nexport default withLeadline(nextConfig);']);

  const api = wrap();
  assert.equal(adviceTitle(api.connect), "Put leadline run in front of its dev command");
  assert.deepEqual(steps(api).map((s) => [s.label, s.text]), [
    ["Start it with", "leadline run -- npm run dev"],
    ["For traces too, add the Node.js provider. leadline run loads it.", "npm install --save-dev @leadline/node"],
  ]);
  assert.equal(steps(wrap({ running: [{ port: 3001, pid: 9, what: "node" }] }))[0].label, "Stop it, then start it with");

  const otel = project({ connect: { kind: "otel-env", summary: "It already has OpenTelemetry.", file: ".env", lines: ["OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4320", "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf"], command: "leadline run -- go run ./cmd/billing" } });
  assert.equal(adviceTitle(otel.connect), "Point its OpenTelemetry at Leadline");
  assert.equal(adviceCommand(otel), "OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4320");
  assert.deepEqual(steps(otel).map((s) => s.label), ["Add to .env", "Or start it with leadline run, which sets them itself"]);

  const done = project({ connect: { kind: "already", summary: "Already sends to Leadline as acme-web." } });
  assert.equal(adviceTitle(done.connect), "Already sending");
  assert.equal(adviceCommand(done), "");
  assert.deepEqual(steps(done), []);
});

test("a daemon somewhere else is named by the daemon's own advice, as it gave it", () => {
  // The daemon puts its address in the command and gives the plugin's env line: the app adds neither.
  const at = "http://127.0.0.1:4342";
  const api = wrap({ running: [{ port: 3001, pid: 9, what: "node" }], connect: { kind: "run-wrap", summary: "", command: `leadline run --url ${at} -- npm run dev` } });
  assert.equal(adviceCommand(api), `leadline run --url ${at} -- npm run dev`);
  assert.deepEqual(steps(api).map((s) => s.text), [`leadline run --url ${at} -- npm run dev`]);

  const next = project({ connect: { ...project().connect, env_file: ".env.local", env_lines: [`LEADLINE_URL=${at}`] } });
  assert.deepEqual(steps(next)[2], { label: "Tell it where this Leadline is, in .env.local", text: `LEADLINE_URL=${at}`, copy: "Copy the line for .env.local" });
  // At the usual address the daemon gives no such line, and there is no such step.
  assert.equal(steps(project()).length, 2);
  assert.equal(steps(project()).some((s) => /LEADLINE_URL|--url/.test(s.text)), false);

  const otel = project({ connect: { kind: "otel-env", summary: "", file: ".env", lines: [`OTEL_EXPORTER_OTLP_ENDPOINT=${at}`], command: `leadline run --url ${at} -- go run ./cmd/billing` } });
  assert.equal(steps(otel)[1].text, `leadline run --url ${at} -- go run ./cmd/billing`);
});

test("a project says where it is and what of it is running", () => {
  assert.equal(whereProject(project()), "~/code/acme-web · main");
  assert.equal(whereProject(project({ git: { root: "/x", branch: "fix-orders", worktree: true } })), "~/code/acme-web · worktree fix-orders");
  assert.equal(whereProject(project({ git: undefined })), "~/code/acme-web");
  assert.equal(runningLine(project()), "next dev on port 3000, up 3 hours");
  assert.equal(runningLine(wrap()), "");
  assert.equal(upFor(undefined), "");
  assert.equal(upFor(45), "up 45 seconds");
});

test("running right now lists dev servers and databases, and counts the rest", () => {
  const listener = (port: number, kind: string, what: string, more = {}) => ({ port, address: "127.0.0.1", scope: "loopback", pid: port, process: "node", command: "", cwd: "", kind, what, ...more });
  const s = scan({ projects: [project()], listeners: [listener(3000, "dev-server", "next dev", { project_id: "p_web", uptime_s: 7200 }), listener(5432, "postgres", "Postgres"), listener(7000, "other", "ControlCenter"), listener(7001, "other", "")] });
  const { rows, others } = runningRows(s);
  assert.deepEqual(rows.map((r) => [r.what, r.aside]), [["next dev on port 3000", "acme-web · up 2 hours"], ["Postgres on port 5432", ""]]);
  assert.equal(others, 2);
});

test("a folder says what became of it", () => {
  assert.deepEqual(folderLine({ path: "~/code", state: "scanned", projects: 3, directories: 212 }), { text: "3 projects", bad: false });
  assert.match(folderLine({ path: "~/code", state: "scanned", projects: 200, directories: 5000, truncated: true }).text, /stopped at its limit/);
  assert.deepEqual(folderLine({ path: "~", state: "refused", reason: "The home folder is too wide.", projects: 0, directories: 0 }), { text: "The home folder is too wide.", bad: true });
  assert.equal(folderLine({ path: "~/nope", state: "missing", projects: 0, directories: 0 }).bad, true);
  assert.equal(folderLine({ path: "~/x", state: "pending", projects: 0, directories: 0 }).text, "not looked in yet");
});

test("Shipyard boxes are listed only when Shipyard answered, and never this computer", () => {
  const box = (name: string, more = {}) => ({ name, state: "online", leadline: { state: "unsupported", installed: false, reachable: false }, ...more });
  assert.deepEqual(boxesOf(null), []);
  assert.deepEqual(boxesOf(undefined), []);
  assert.deepEqual(boxesOf({ available: false, boxes: [box("hetzner-1")] }), []);
  assert.deepEqual(boxesOf({ available: true, boxes: [box("hetzner-1"), box("acme-mbp", { local: true })] }).map((b) => b.name), ["hetzner-1"]);
  assert.equal(boxState({ state: "online", latency_ms: 86 }, NOW), "online, 86 ms away");
  assert.equal(boxState({ state: "online" }, NOW), "online");
  assert.equal(boxState({ state: "offline", since_ms: NOW - 26 * 3_600_000 }, NOW), "offline for 1 day");
  assert.equal(boxState({ state: "untrusted" }, NOW), "not trusted yet");
  assert.equal(boxState({ state: "connecting" }, NOW), "connecting");
});

const saved = (over: Partial<Connection> = {}): Connection => ({
  id: "c_1",
  kind: "postgres",
  name: "acme",
  url: "postgres://acme:****@127.0.0.1:5432/acme",
  host: "127.0.0.1:5432",
  database: "acme",
  user: "acme",
  remote: false,
  capture: false,
  capture_offer: { available: true, how: "proxy", note: "" },
  health: { state: "ok", version: "17.10", version_short: "17.10" },
  created_ms: NOW,
  ...over,
});

const proxy = (statements: number, state: "listening" | "error" = "listening"): NonNullable<Connection["proxy"]> => ({
  id: "p_1", kind: "postgres", listen: "127.0.0.1:15432", port: 15432, upstream: "127.0.0.1:5432", origins: ["connection"], connection_ids: ["c_1"], state,
  error: state === "error" ? "another program is listening on it" : undefined,
  connections_open: 0, connections_total: 0, statements, failed_statements: 0, unread_connections: 0, last_activity_ms: 0,
});

test("a connection is heard from when its server answers, not when it was saved", () => {
  assert.deepEqual(heardConn(saved({ health: { state: "unknown" } })), { tone: "wait", line: "Checking that it answers…" });
  const down = heardConn(saved({ health: { state: "error", message: "Nothing is listening at 127.0.0.1:5432.", hint: "Is Postgres running?" } }));
  assert.deepEqual(down, { tone: "err", line: "Nothing is listening at 127.0.0.1:5432.", hint: "Is Postgres running?" });
  assert.deepEqual(heardConn(saved()), { tone: "done", line: "Postgres 17.10 answers. Its statistics are being read." });
});

test("a capture says how much has come through, and what is wrong when it can't open", () => {
  const none = heardConn(saved({ capture: true, proxy: proxy(0) }));
  assert.equal(none.tone, "done");
  assert.match(none.line, /port 15432: no query has come through it yet/);
  assert.equal(none.aside, undefined);
  const some = heardConn(saved({ capture: true, proxy: proxy(38) }));
  assert.equal(some.line, "Postgres 17.10 answers. Capturing through port 15432.");
  assert.equal(some.aside, "38 queries");
  assert.equal(heardConn(saved({ capture: true, proxy: proxy(1) })).aside, "1 query");
  const taken = heardConn(saved({ capture: true, proxy: proxy(0, "error") }));
  assert.equal(taken.tone, "err");
  assert.match(taken.line, /proxy on port 15432 couldn't open: another program is listening on it\./);
  const monitor = { state: "streaming" as const, seen: 12, recorded: 12, dropped: 0, max_per_sec: 1000, last_activity_ms: NOW, note: "" };
  const cache = heardConn(saved({ kind: "redis", name: "cache", capture: true, monitor, health: { state: "ok", version: "7.4.11" } }));
  assert.deepEqual(cache, { tone: "done", line: "Redis 7.4.11 answers. Capturing every command.", aside: "12 commands" });
});

const svc = (name: string, over: Partial<ServiceSource> = {}): ServiceSource => ({ name, spans: 120, logs: 14, metric_points: 0, last_seen_ms: NOW - 1000, via: ["otlp"], ...over });
const run = (over: Partial<Run> = {}): Run => ({ id: "r1", command: "npm run dev", cwd: "/Users/acme/code/acme-api", service: "acme-api", pid: 1, terminal: true, running: true, started_ms: NOW - 5000, ...over });

test("a project is heard from when a service by its name has sent something lately", () => {
  const since = NOW - 60_000;
  const nothing = heardProject(wrap(), { now_ms: NOW, services: [svc("acme-web")] }, [], since);
  assert.equal(nothing.tone, "wait");
  assert.equal(nothing.line, "");
  const heard = heardProject(wrap(), { now_ms: NOW, services: [svc("acme-api")] }, [], since);
  assert.equal(heard.tone, "done");
  assert.equal(heard.line, "acme-api is sending: traces and logs.");
  assert.equal(heard.aside, "120 spans");
  // Before the daemon has answered at all.
  assert.equal(heardProject(wrap(), undefined, null, since).tone, "wait");
});

test("a service that went quiet long ago is not news; one under another name is found by its run or by what the daemon says", () => {
  const since = NOW - 60_000;
  const stale = svc("acme-api", { last_seen_ms: NOW - 3_600_000 });
  assert.equal(heardProject(wrap(), { now_ms: NOW, services: [stale] }, [], since).tone, "wait");
  // `leadline run --name orders` in the project's folder, or a folder inside it.
  const named = heardProject(wrap(), { now_ms: NOW, services: [svc("orders", { via: ["postgres-proxy"], logs: 0 })] }, [run({ service: "orders", cwd: "/Users/acme/code/acme-api/server" })], since);
  assert.equal(named.tone, "done");
  assert.equal(named.line, "orders is sending: queries.");
  // A folder that only starts with the same letters is another project.
  assert.equal(heardProject(wrap(), { now_ms: NOW, services: [svc("orders")] }, [run({ service: "orders", cwd: "/Users/acme/code/acme-api-v2" })], since).tone, "wait");
  const known = wrap({ sends: { already: true, because: ["service"], service: "acme-orders" } });
  assert.equal(heardProject(known, { now_ms: NOW, services: [svc("acme-orders")] }, [], since).tone, "done");
});

test("a run that has started and said nothing yet is still a wait, and says so", () => {
  const h = heardProject(wrap(), { now_ms: NOW, services: [] }, [run()], NOW - 60_000);
  assert.equal(h.tone, "wait");
  assert.match(h.line, /npm run dev is running under leadline run as acme-api/);
  // A run that has ended is not one.
  assert.equal(heardProject(wrap(), { now_ms: NOW, services: [] }, [run({ running: false })], NOW - 60_000).line, "");
});
