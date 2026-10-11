// The daemon's JSON API. Times are unix milliseconds and durations are
// milliseconds, both as floats.

import { briefParams, briefPath, type BriefAsk, type Mark } from "./brief.ts";
import { LOCAL_ID, machines } from "./machines";
import { dbParams, type Scope } from "./scope.ts";

export type { Scope } from "./scope";

/** base is the address of the daemon the window is showing now (lib/machines.ts). */
export const base = (): string => machines.current().url;

/**
 * localBase is the address of this computer's own daemon, whichever
 * machine the window is showing. What only this computer can do goes to
 * it: Shipyard's command line is here, not on the machine being looked at.
 */
export const localBase = (): string => (machines.get().machines.find((m) => m.id === LOCAL_ID) ?? machines.current()).url;

/** Whether the window is showing this computer's own daemon. */
export const showingLocal = (): boolean => machines.current().url === localBase();

export type Summary = { spans: number; logs: number; traces: number; services: string[] };

/** What the daemon keeps on disk and for how long (internal/store/retention.go). */
export type Storage = {
  /** The data held; 0 for a store kept in memory. */
  size_bytes: number;
  /** What the files take on disk, which is more after old data was dropped. */
  file_bytes: number;
  /** The size past which the oldest data goes early; 0 is no cap. */
  max_bytes: number;
  /** How long a row is kept; 0 is for ever. */
  retain_ms: number;
  /** The time of the oldest row held; 0 when there is none. */
  oldest_ms: number;
};

export type TraceRow = {
  trace_id: string;
  name: string;
  service: string;
  start_ms: number;
  duration_ms: number;
  spans: number;
  errors: number;
  queries: number;
  db_ms: number;
  /** Who the request was made for ("user.id"), when a span of it says. */
  user_id?: string;
  /** This row's exact place in the list: pass it as `before` for the rows after it. */
  cursor?: string;
};

export type Span = {
  span_id: string;
  parent_id: string;
  service: string;
  name: string;
  kind: string;
  start_ms: number;
  duration_ms: number;
  status: string;
  status_message: string;
  db_system: string;
  db_statement: string;
  http_method: string;
  http_route: string;
  http_status: number;
  attrs: Record<string, unknown>;
  /**
   * What a database call counts as: a query; transaction and session
   * control (BEGIN, COMMIT, SET), kept out of the query list; or a second
   * sighting of a call another span of the trace already counts. Absent on
   * a span that isn't a database call, and from an older daemon.
   */
  db_role?: SpanRole;
  /** The span is a request: what Endpoints counts. */
  entry?: boolean;
  /** The host:port a database call went to, when known. */
  db_server?: string;
};

export type SpanRole = "query" | "control" | "duplicate";

export type LogRow = {
  ts_ms: number;
  service: string;
  severity: string;
  body: string;
  trace_id: string;
  span_id: string;
  attrs: Record<string, unknown>;
  /** What sort of record a provider says this is ("browser.error", "browser.web_vital"). Only the logs list has it. */
  kind?: string;
  /** Who it was written for. Only the logs list has it. */
  user_id?: string;
  /** Set in the logs list, as TraceRow's is; a trace's own logs have none. */
  cursor?: string;
};

export type TraceDetail = { trace_id: string; spans: Span[]; logs: LogRow[] };

export type QueryRow = {
  db_system: string;
  fingerprint: string;
  example: string;
  calls: number;
  total_ms: number;
  avg_ms: number;
  p50_ms: number;
  p95_ms: number;
  max_ms: number;
  errors: number;
  last_ms: number;
  traces: number;
  max_per_trace: number;
};

export type Series = {
  bucket_ms: number;
  t: number[];
  requests: number[];
  errors: number[];
  queries: number[];
  p50_ms: number[];
  p95_ms: number[];
  db_ms: number[];
  logs: number[];
  log_errors: number[];
};

// The breakdowns (internal/store/breakdown.go). A "request" is an entry
// span: a trace's root, or where a call arrives at a service.

export type EndpointRow = {
  service: string;
  name: string;
  requests: number;
  /** Requests in which any span failed. */
  errors: number;
  total_ms: number;
  avg_ms: number;
  p50_ms: number;
  p95_ms: number;
  max_ms: number;
  /** Queries per request, on average. */
  queries: number;
  /** Time in databases per request, on average. */
  db_ms: number;
  /** Part of all request time in the window, 0..1. */
  share: number;
  last_ms: number;
  /** Requests in each of Endpoints.t's buckets. */
  spark?: number[];
  /** What a framework's plugin says the endpoint is: Next.js's "page", "route", "action", "api". Only the list has it. */
  kind?: string;
  /** A server action's name. */
  action?: string;
};

/** A route the app has that no request in the window asked for. */
export type QuietRoute = { service: string; route: string; kind: string; router?: string; file?: string; dynamic?: boolean; methods?: string[] };

export type Endpoints = {
  bucket_ms: number;
  t: number[];
  total: number;
  rows: EndpointRow[];
  /** Filled when a framework sent its route list; absent from an older daemon. */
  never_hit?: QuietRoute[];
};

export type EndpointKey = { service: string; name: string };
export type QueryKey = { db_system: string; fingerprint: string };

/** A value per bucket; null where the bucket held nothing to measure. */
export type Gappy = (number | null)[];

export type Bin = { lo_ms: number; hi_ms: number; count: number };
export type Histogram = { per_decade: number; bins: Bin[] };

/** Sparse cells: x[i] is a column of t, y[i] a latency band, n[i] the count. */
export type Heatmap = {
  bucket_ms: number;
  t: number[];
  per_decade: number;
  x: number[];
  y: number[];
  n: number[];
  y_min: number;
  y_max: number;
  max: number;
};

export type EndpointSeries = {
  bucket_ms: number;
  t: number[];
  requests: number[];
  errors: number[];
  p50_ms: Gappy;
  p95_ms: Gappy;
  avg_ms: Gappy;
  queries: Gappy;
  db_ms: Gappy;
};

export type EndpointQueryRow = {
  db_system: string;
  fingerprint: string;
  example: string;
  calls: number;
  requests: number;
  /** Calls over all of the endpoint's requests. */
  per_request: number;
  max_per_request: number;
  avg_ms: number;
  max_ms: number;
  total_ms: number;
};

export type RequestRow = {
  trace_id: string;
  start_ms: number;
  duration_ms: number;
  queries: number;
  db_ms: number;
  failed: boolean;
};

export type EndpointDetail = {
  stats: EndpointRow | null;
  series: EndpointSeries;
  histogram: Histogram;
  heatmap: Heatmap;
  queries: EndpointQueryRow[];
  slowest: RequestRow[];
  failing: RequestRow[];
};

export type QueryStats = QueryRow & { share: number };

export type QuerySeries = {
  bucket_ms: number;
  t: number[];
  calls: number[];
  errors: number[];
  total_ms: number[];
  avg_ms: Gappy;
  p95_ms: Gappy;
};

export type QueryEndpointRow = {
  service: string;
  name: string;
  calls: number;
  requests: number;
  endpoint_requests: number;
  per_request: number;
  max_per_request: number;
  avg_ms: number;
  total_ms: number;
};

export type QueryTraceRow = {
  trace_id: string;
  name: string;
  service: string;
  start_ms: number;
  calls: number;
  errors: number;
  total_ms: number;
  slowest_ms: number;
};

export type QueryDetail = {
  stats: QueryStats | null;
  series: QuerySeries;
  histogram: Histogram;
  heatmap: Heatmap;
  endpoints: QueryEndpointRow[];
  traces: QueryTraceRow[];
};

export type Grouping = "db_time" | "db_calls" | "status" | "service" | "service_time";
export type GroupedSeries = { bucket_ms: number; t: number[]; groups: { key: string; total: number; values: number[] }[] };

export type EndpointSort = "time" | "name" | "requests" | "errors" | "avg" | "p50" | "p95" | "max" | "queries" | "db";

// Metrics (internal/store/metrics.go).

/** How a metric's series reads: a level, an increase per bucket, or a histogram. */
export type MetricKind = "gauge" | "rate" | "histogram";

export type MetricInfo = {
  name: string;
  unit: string;
  description: string;
  type: "gauge" | "sum" | "histogram" | "summary";
  /** "cumulative" or "delta"; empty for a gauge. */
  temporality: string;
  monotonic: boolean;
  kind: MetricKind;
  services: string[];
  series: number;
  points: number;
  last_value: number;
  last_ms: number;
};

/** A group's figures over the whole window. */
export type MetricStats = {
  /** A counter's increase, or a histogram's observations. */
  total: number;
  /** A gauge's newest value. */
  last: number | null;
  avg: number | null;
  p50: number | null;
  p95: number | null;
};

export type MetricGroup = {
  label: string;
  /** By kind: a gauge's level, a counter's increase in the bucket, a histogram's observations in it. Null where the metric said nothing. */
  value: (number | null)[];
  /** Histograms only. */
  avg?: (number | null)[];
  p50?: (number | null)[];
  p95?: (number | null)[];
  stats: MetricStats;
};

export type MetricSeries = {
  name: string;
  unit: string;
  description: string;
  type: string;
  temporality: string;
  monotonic: boolean;
  /** Empty when Leadline has no points of this metric. */
  kind: MetricKind | "";
  bucket_ms: number;
  t: number[];
  /** What the groups are split on: "" for one group of everything. */
  by: string;
  /** The attribute keys the metric can be split on. */
  keys: string[];
  groups: MetricGroup[];
  /** Groups left out for being the smallest. */
  more: number;
};

// Sources, connections and their statistics (internal/integrations/API.md).

/** Why something failed, in a sentence for the person, and what to try. */
export type Fault = { code: string; message: string; hint?: string };

export type CaptureOffer = { available: boolean; how: string; note: string };

/**
 * Who made a connection, a proxy or a source: a person, a worktree that was
 * attached (by Shipyard, or by `leadline attach`), a `leadline run`, the
 * daemon's own flag, or a scan that found its URL in a project's env file.
 * `label` says it in a few words. Absent from an older daemon, and from a
 * service nobody attached.
 */
export type Owner = {
  kind: "user" | "worktree" | "run" | "flag" | "scan" | (string & {});
  label: string;
  /** For a worktree, the tool that attached it: "shipyard" or "cli". */
  via?: string;
  /** The source it belongs to: the worktree's service name, or the run's. */
  service?: string;
  project?: string;
  /** Absent for the project's main checkout. */
  worktree?: string;
  branch?: string;
  box?: string;
  path?: string;
  /** For a scan: the variable the URL was found in, and the env file, by name. `project` is then the project that file is in. */
  env_key?: string;
  env_file?: string;
};

export type Proxy = {
  id: string;
  kind: string;
  listen: string;
  port: number;
  upstream: string;
  label?: string;
  origins: string[];
  connection_ids: string[];
  state: "listening" | "error";
  error?: string;
  /** The person's own URL pointed at the proxy, its password masked. */
  url?: string;
  connections_open: number;
  connections_total: number;
  statements: number;
  failed_statements: number;
  unread_connections: number;
  last_activity_ms: number;
  /** Statements since the sources' `since_ms`, counted by the minute. Absent from an older daemon. */
  window_statements?: number;
  owner?: Owner;
};

export type Monitor = {
  state: "connecting" | "streaming" | "error";
  error?: string;
  seen: number;
  recorded: number;
  dropped: number;
  max_per_sec: number;
  last_activity_ms: number;
  note: string;
  /** Commands recorded since the sources' `since_ms`. Absent from an older daemon. */
  window_recorded?: number;
};

export type Health = {
  state: "unknown" | "ok" | "error";
  code?: string;
  message?: string;
  hint?: string;
  version?: string;
  /** The version's number alone: 17.11 of "17.11 (Debian 17.11-1.pgdg13+2)". */
  version_short?: string;
  latency_ms?: number;
  checked_ms?: number;
};

export type ConnectionKind = "postgres" | "redis";

export type Connection = {
  id: string;
  kind: ConnectionKind;
  name: string;
  /** Password masked as ****. */
  url: string;
  host: string;
  database: string;
  user: string;
  remote: boolean;
  capture: boolean;
  capture_offer: CaptureOffer;
  proxy?: Proxy;
  monitor?: Monitor;
  health: Health;
  created_ms: number;
  owner?: Owner;
  /** For a worktree's connection, the variables its URL was found in. */
  env?: string[];
};

export type ServiceSource = {
  name: string;
  /** Counts of what is stored now, not of a window. */
  spans: number;
  logs: number;
  metric_points: number;
  last_seen_ms: number;
  /** "otlp", "postgres-proxy", "redis-monitor". */
  via: string[];
  /** The captures behind a "postgres-proxy" or "redis-monitor": which proxy, and which saved connections it is for. */
  through?: Through[];
  /** There when the service belongs to an attached worktree. */
  owner?: Owner;
};

export type Through = { via: string; proxy_id?: string; connection_ids?: string[] };

export type Sources = {
  now_ms: number;
  /** Where the windowed counts (window_statements, window_recorded) start. Absent from an older daemon. */
  since_ms?: number;
  otlp: { endpoints: string[] };
  services: ServiceSource[];
  proxies: Proxy[];
  connections: Connection[];
};

export type PgStatStatements = {
  available: boolean;
  version?: string;
  /** "not_loaded", "not_installed", "not_available" or "error" when it isn't there. */
  reason?: string;
  message?: string;
  how_to?: string[];
  statements: { query: string; calls: number; total_ms: number; mean_ms: number; rows: number; cache_hit_rate: number | null }[];
};

export type TestResult = {
  ok: boolean;
  kind: ConnectionKind;
  url?: string;
  host?: string;
  database?: string;
  user?: string;
  remote?: boolean;
  version?: string;
  version_short?: string;
  latency_ms?: number;
  tls?: boolean;
  capture?: CaptureOffer;
  pg_stat_statements?: PgStatStatements;
  error?: Fault;
};

export type Incomplete = { section?: string; message: string }[];

export type PostgresStats = {
  kind: "postgres";
  collected_ms: number;
  took_ms: number;
  remote: boolean;
  read_only: boolean;
  version: string;
  database: string;
  user: string;
  size_bytes: number;
  started_ms: number;
  stats_since_ms: number | null;
  replica: boolean;
  connections: { used: number; max: number; reserved: number; active: number; idle: number; idle_in_transaction: number; waiting: number };
  table_count: number;
  tables: { schema: string; name: string; rows: number; total_bytes: number; table_bytes: number; index_bytes: number; seq_scans: number; index_scans: number; dead_rows: number }[];
  missing_index_hints: { schema: string; table: string; rows: number; seq_scans: number; rows_read: number; rows_per_scan: number; index_scans: number; table_bytes: number; why: string; next_step: string }[];
  indexes: {
    count: number;
    unused_count: number;
    unused_bytes: number;
    largest: { schema: string; table: string; name: string; scans: number; bytes: number; unique: boolean; primary: boolean; unused: boolean }[];
  };
  running: { pid: number; user: string; application: string; state: string; waiting: string; duration_ms: number; transaction_ms: number; query: string }[];
  locks: { pid: number; user: string; application: string; lock_type: string; mode: string; waiting_ms: number; query: string; blocked_by: number[] }[];
  pg_stat_statements: PgStatStatements;
  incomplete: Incomplete;
};

export type RedisStats = {
  kind: "redis";
  collected_ms: number;
  took_ms: number;
  remote: boolean;
  version: string;
  mode: string;
  uptime_s: number;
  memory: { used_bytes: number; rss_bytes: number; peak_bytes: number; max_bytes: number; policy: string; fragmentation: number };
  clients: { connected: number; blocked: number; max: number };
  ops_per_sec: number;
  total_commands: number;
  keyspace: { keys: number; hits: number; misses: number; hit_rate: number | null; expired: number; evicted: number };
  databases: { db: number; keys: number; expiring: number; avg_ttl_ms: number }[];
  commands: { command: string; calls: number; total_ms: number; avg_ms: number }[];
  slow_log: { threshold_ms: number | null; entries: { id: number; at_ms: number; duration_ms: number; command: string; client: string }[] };
  biggest_keys: { db: number; sampled: number; total: number; complete: boolean; note: string; keys: { key: string; type: string; bytes: number }[] };
  incomplete: Incomplete;
};

export type ConnectionStats = PostgresStats | RedisStats;

/** A command started with `leadline run` (internal/api/runs.go). */
export type Run = {
  id: string;
  command: string;
  args?: string[];
  cwd: string;
  service: string;
  pid: number;
  provider?: string;
  proxies?: { kind: string; env?: string; listen: string; upstream: string }[];
  terminal: boolean;
  running: boolean;
  started_ms: number;
  ended_ms?: number;
  exit_code?: number;
  signal?: string;
  lost?: boolean;
};

/** The line a person pastes into a coding agent to have it set Leadline up (internal/api/setup.go). */
export type SetupPrompt = { prompt: string; url: string; setup_url: string };

// Looking around (internal/discover/API.md), in two steps. A scan lists
// what is listening on the machine and the projects behind it, with each
// project's env files by name: it opens none. Reading is the second step,
// for the projects a person names: their env files are opened for
// connection URLs and nothing else. No type here can hold a URL as
// written: a URL is always the masked one, and saving is by id.

/** One port a process is listening on. */
export type Listener = {
  port: number;
  address: string;
  /** "loopback", "all" or "address". */
  scope: string;
  /** 0, and the fields after it empty, for a port whose process can't be seen. */
  pid: number;
  process: string;
  command: string;
  cwd: string;
  started_ms?: number;
  uptime_s?: number;
  /** "dev-server", "postgres", "redis", "mysql", "mongodb" or "other". */
  kind: string;
  /** That in words: "next dev", "Postgres". */
  what: string;
  framework?: string;
  basis?: string;
  project_id?: string;
};

export type ScanLimits = { method: string; complete: boolean; skipped_processes: number; unowned_ports: number; note?: string };

/** A folder a person pointed the scan at, and what became of it. */
export type ScanFolder = {
  path: string;
  resolved?: string;
  /** "pending", "scanned", "refused", "missing" or "cancelled". */
  state: string;
  reason?: string;
  projects: number;
  directories: number;
  truncated?: boolean;
};

/** Advice on connecting a project. Nothing is changed by it. */
export type ConnectAdvice = {
  /** "next-plugin", "run-wrap", "otel-env" or "already". */
  kind: string;
  summary: string;
  command?: string;
  file?: string;
  lines?: string[];
  /** Lines for an env file as well, when the daemon is not at its usual address: where the plugin should send. */
  env_file?: string;
  env_lines?: string[];
  /** A further step, for a project whose first gives logs and queries but not traces. */
  traces?: { summary: string; command?: string };
};

export type ProjectRunning = { port: number; pid: number; what: string; uptime_s?: number };

/** One env file of a project. `read` is false, and the counts zero, for one that is only known by name: it has not been opened. */
export type EnvFile = { file: string; read?: boolean; urls: number; other_keys: number };

export type FoundProject = {
  /** The same for the same path in every scan. */
  id: string;
  name: string;
  path: string;
  /** How it was found: "listening", "run", "folder". */
  sources: string[];
  stack: string;
  frameworks: { id: string; name: string; version?: string }[];
  /** The stack as a person would say it: "Next.js 16". */
  label: string;
  package_manager?: string;
  dev_command?: string;
  dev_script?: { file: string; name?: string; command?: string };
  opentelemetry: boolean;
  sends: { already: boolean; because: string[]; service?: string };
  git?: { root: string; branch?: string; worktree: boolean; main_repo?: string; worktrees?: { path: string; branch?: string }[] };
  connect: ConnectAdvice;
  running: ProjectRunning[];
  /** The env files it has, by name. None is opened until the project is named in the env step. */
  env_files: EnvFile[];
  /** The connections found in them; empty until then. */
  connection_ids: string[];
};

export type FoundIn = { project_id: string; project: string; key: string; file: string; effective: boolean };

/** A connection URL found in an env file. The daemon keeps the URL itself, under `id`. */
export type FoundConnection = {
  /** Opaque, and only good for the scan it came in. */
  id: string;
  /** "postgres", "redis", "mysql" or "mongodb". */
  kind: string;
  /** With **** for the password. */
  url: string;
  host: string;
  database: string;
  user: string;
  remote: boolean;
  /** False with `reason` "not_supported", "interpolated" or "bad_url"; `note` is the sentence. */
  connectable: boolean;
  reason?: string;
  note?: string;
  capture_available: boolean;
  /** Whether to tick it to begin with. */
  suggested: boolean;
  found_in: FoundIn[];
  saved: boolean;
  saved_id?: string;
  listening?: ProjectRunning;
};

export type Scan = {
  id: string;
  /** "running", "done", "cancelled" or "failed". */
  state: string;
  started_ms: number;
  finished_ms?: number;
  error?: Fault;
  progress: { phase: string; folders_done: number; folders_total: number; directories: number; projects: number };
  folders: ScanFolder[];
  machine: { os: string; hostname: string };
  listeners: Listener[];
  limits: ScanLimits;
  projects: FoundProject[];
  /** Empty after a scan: filled by the env step, from the projects named to it. */
  connections: FoundConnection[];
  /** The ids of the projects whose env files have been read. Absent from a daemon whose scan is one step. */
  env_projects?: string[];
  /** What was taken from env files, and what was only counted. */
  env: { files_read: number; urls: number; other_keys: number };
  /** Until when the daemon keeps the found URLs so they can be saved: 30 minutes after the last env step. */
  secrets_until_ms?: number;
};

export type SaveFound = {
  saved: { id: string; already: boolean; connection: Connection }[];
  failed: { id: string; error: Fault }[];
};

export type TestFound = { results: { id: string; result?: TestResult; error?: Fault }[] };

/** One of the owner's Shipyard boxes. Leadline cannot run on one yet: `leadline.state` is "unsupported". */
export type Box = {
  name: string;
  /** Shipyard's own: "online", "offline", "connecting" or "untrusted". */
  state: string;
  error?: string;
  latency_ms?: number;
  since_ms?: number;
  /** The box is this computer itself. */
  local?: boolean;
  leadline: { state: string; installed: boolean; reachable: boolean; url?: string; note?: string };
};

export type Shipyard = { installed: boolean; available: boolean; cli?: string; boxes: Box[]; checked_ms: number; problem?: Fault };

// Sending a problem to a coding agent (internal/brief/API.md).

/** A problem written up as a prompt, by the daemon that holds its data. */
export type Brief = {
  kind: "query" | "endpoint" | "trace" | "log";
  /** What Leadline's numbers flag. "slow" is also what a brief about something unflagged says: its cost. */
  problem: "n_plus_one" | "failing" | "slow";
  /** The prompt's first line. */
  title: string;
  /** What is wrong, in one sentence, with the numbers. */
  summary: string;
  /** The Markdown to give an agent. */
  prompt: string;
  /** Which of the prompt's lines were copied from what the app sent. */
  recorded: Mark[];
  /** Whether real values could be included (they are left out unless asked for), and were. */
  example: { available: boolean; included: boolean };
  /** What was shortened to keep the prompt small. */
  cuts: string[];
  bytes: number;
  /** A name for the work: valid as a branch and a worktree, and its own each time. */
  task_name: string;
  /** The service the problem is in. */
  service: string;
  /** Where that service's code is, as far as the daemon that wrote the brief knows. */
  source?: BriefSource;
};

/** What a daemon knows of where a service's code is: the worktree that attached it, the folders its commands ran in. */
export type BriefSource = { service: string; via?: string; project?: string; worktree?: string; branch?: string; box?: string; path?: string; folders?: string[] };

export type ShipyardAgent = { id: string; name: string };
export type ShipyardProject = {
  name: string;
  path: string;
  /** Only a git repository can have a worktree made of it. */
  repo: boolean;
  default_branch?: string;
  worktrees: { name: string; path: string; branch?: string; main?: boolean; title?: string }[];
  /** Agents the repository's own config adds to the box's. */
  agents?: ShipyardAgent[];
};

/** A place a task could start, and why Leadline thinks so. */
export type ShipyardTarget = {
  box: string;
  project: string;
  /** The ref the task's branch starts from; "" is what the project has checked out. */
  base: string;
  /** "worktree": the service was attached by that Shipyard worktree. "folder": its folder is that project's. */
  how: string;
  why: string;
  base_why: string;
};

/** Where a task could go: Shipyard's boxes, one box's projects and agents, and the place Leadline would pick. */
export type ShipyardTargets = {
  installed: boolean;
  available: boolean;
  problem?: Fault;
  boxes: Box[];
  /** Null when nothing ties the service to a project: the person picks. */
  suggested: ShipyardTarget | null;
  /** The box whose projects and agents are listed. */
  box: string;
  projects: ShipyardProject[];
  agents: ShipyardAgent[];
  /** Why that box's projects could not be listed. */
  box_problem?: Fault;
};

/** A task to start, and what Shipyard reported of one it started. */
export type ShipyardSend = { box: string; project: string; name: string; prompt: string; agent?: string; base?: string };
export type ShipyardTask = {
  box: string;
  project: string;
  name: string;
  /** How Shipyard addresses the worktree: "devl/acme/fix-orders". */
  ref: string;
  path?: string;
  branch?: string;
  base?: string;
  agent: string;
  session?: string;
  /** The name its telemetry will be filed under in Leadline once it sends any. */
  service: string;
  prompt_bytes: number;
};

/**
 * ApiError is a request the daemon refused. `code` and `hint` come from the
 * integrations API's error body; other routes answer in plain text and have
 * neither.
 */
export class ApiError extends Error {
  status: number;
  code: string;
  hint: string;
  constructor(message: string, status: number, code = "", hint = "") {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.hint = hint;
  }
}

type Params = Record<string, string | number | undefined>;

async function answer<T>(res: Response): Promise<T> {
  if (res.ok) return res.json();
  const text = (await res.text()).trim();
  try {
    const e = (JSON.parse(text) as { error?: Fault }).error;
    if (e?.message) throw new ApiError(e.message, res.status, e.code, e.hint ?? "");
  } catch (err) {
    if (err instanceof ApiError) throw err;
  }
  throw new ApiError(text || res.statusText, res.status);
}

async function get<T>(path: string, params: Params = {}): Promise<T> {
  const url = new URL(path, base());
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== "") url.searchParams.set(k, String(v));
  }
  return answer<T>(await fetch(url));
}

// The daemon's guard lets a page send GET and POST, so every change is a POST.
async function post<T>(path: string, body?: unknown, at: string = base()): Promise<T> {
  return answer<T>(
    await fetch(new URL(path, at), {
      method: "POST",
      headers: body === undefined ? undefined : { "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    }),
  );
}

async function del<T>(path: string): Promise<T> {
  return answer<T>(await fetch(new URL(path, base()), { method: "DELETE" }));
}

// Routes an older daemon doesn't have, by address: asked once, then left alone.
const missing = new Set<string>();
// And one a daemon was found to have, so it is not asked about twice.
const there = new Set<string>();

/** maybe asks a route that not every daemon has: from one without it the answer is null, not an error. */
async function maybe<T>(path: string): Promise<T | null> {
  const key = base() + path;
  if (missing.has(key)) return null;
  try {
    return await get<T>(path);
  } catch (e) {
    if (e instanceof ApiError && (e.status === 404 || e.status === 405)) {
      missing.add(key);
      return null;
    }
    throw e;
  }
}

/** A daemon that has just (re)started may be a newer one: ask it everything again. */
export function forgetMissing(): void {
  missing.clear();
  there.clear();
}

export const api = {
  summary: () => get<Summary>("/api/summary"),
  // `before` is the cursor of the last row already held: the page after it.
  // `user` keeps the traces made for one user.
  traces: (s: Scope, q: string, before?: string, limit = 300, user = "") =>
    get<TraceRow[]>("/api/traces", { since_ms: s.sinceMs, service: s.service, ...dbParams(s), q, user, before, limit }),
  trace: (id: string) => get<TraceDetail>(`/api/traces/${id}`),
  // `control` lists transaction and session control (BEGIN, COMMIT, SET) in place of the queries.
  queries: (s: Scope, q: string, o: { control?: boolean; limit?: number } = {}) =>
    get<QueryRow[]>("/api/queries", { since_ms: s.sinceMs, service: s.service, ...dbParams(s), q, control: o.control ? 1 : undefined, limit: o.limit }),
  logs: (s: Scope, q: string, minSeverity: number, before?: string, limit = 2000, o: { kind?: string; user?: string } = {}) =>
    get<LogRow[]>("/api/logs", { since_ms: s.sinceMs, service: s.service, ...dbParams(s), q, min_severity: minSeverity, kind: o.kind, user: o.user, before, limit }),
  series: (s: Scope, untilMs: number, buckets: number) =>
    get<Series>("/api/series", { since_ms: s.sinceMs, until_ms: untilMs, service: s.service, buckets }),
  // Breakdowns.
  endpoints: (s: Scope, o: { q?: string; sort?: EndpointSort; asc?: boolean; limit?: number; spark?: number } = {}) =>
    get<Endpoints>("/api/endpoints", {
      since_ms: s.sinceMs, service: s.service, ...dbParams(s), q: o.q, sort: o.sort, dir: o.asc ? "asc" : undefined, limit: o.limit, spark: o.spark,
    }),
  endpoint: (s: Scope, e: EndpointKey, buckets: number) =>
    get<EndpointDetail>("/api/endpoints/detail", {
      since_ms: s.sinceMs, service: s.service, ...dbParams(s), endpoint_service: e.service, name: e.name, buckets,
    }),
  // A query's detail is every server's calls of that shape: the daemon has no server filter for it.
  query: (s: Scope, k: QueryKey, buckets: number) =>
    get<QueryDetail>("/api/queries/detail", {
      since_ms: s.sinceMs, service: s.service, db_system: k.db_system, fingerprint: k.fingerprint, buckets,
    }),
  heatmap: (s: Scope, buckets: number) => get<Heatmap>("/api/heatmap", { since_ms: s.sinceMs, service: s.service, buckets }),
  // `limit` is how many groups come back, largest first; the daemon's own is
  // 6. `untilMs` ends the window somewhere other than now.
  breakdown: (s: Scope, by: Grouping, buckets: number, limit?: number, untilMs?: number) =>
    get<GroupedSeries>("/api/breakdown", { since_ms: s.sinceMs, until_ms: untilMs, service: s.service, by, buckets, limit }),
  // Metrics.
  metrics: (s: Scope, q: string) => get<MetricInfo[]>("/api/metrics", { since_ms: s.sinceMs, service: s.service, q }),
  metricSeries: (s: Scope, untilMs: number, buckets: number, name: string, by = "") =>
    get<MetricSeries>("/api/metrics/series", { since_ms: s.sinceMs, until_ms: untilMs, service: s.service, buckets, name, by }),

  // Sources and connections. `sinceMs` is where the captures' windowed counts start; without it, 15 minutes ago.
  sources: (sinceMs?: number) => get<Sources>("/api/sources", { since_ms: sinceMs }),
  addConnection: (c: { url: string; name?: string; capture?: boolean }) => post<Connection>("/api/connections", c),
  editConnection: (id: string, c: { name?: string; url?: string; capture?: boolean }) => post<Connection>(`/api/connections/${id}`, c),
  deleteConnection: (id: string) => post<{ ok: boolean }>(`/api/connections/${id}/delete`),
  /** Tries a URL without saving it. A server that refuses is an answer with ok false, not an error. */
  testUrl: (url: string) => post<TestResult>("/api/connections/test", { url }),
  testConnection: (id: string) => post<TestResult>(`/api/connections/${id}/test`),
  connectionStats: (id: string, fresh = false) => get<ConnectionStats>(`/api/connections/${id}/stats`, { fresh: fresh ? 1 : undefined }),
  // Newer routes: null from a daemon that doesn't have them.
  runs: () => maybe<{ runs: Run[] }>("/api/runs").then((r) => r?.runs ?? null),
  setupPrompt: () => maybe<SetupPrompt>("/api/setup/prompt"),
  storage: () => maybe<Storage>("/api/storage"),

  // Looking around. None of these is asked when the window opens: each
  // is called from a press of a button, or after one.
  /**
   * Whether this daemon reads env files only for the projects it is given
   * (the env step). It is asked by naming none, which reads nothing: a
   * daemon that has the step refuses the empty list in its own words, and
   * one without it has no such route. That older daemon's scan read every
   * project's env files in one go, so it is never asked to scan.
   *
   * Only ever called from a press of a button: nothing about looking
   * around is asked when the window opens.
   */
  envStep: async (): Promise<boolean> => {
    const key = base() + "/api/discover/env";
    if (missing.has(key)) return false;
    if (there.has(key)) return true;
    try {
      await post<unknown>("/api/discover/env", { projects: [] });
    } catch (e) {
      if (!(e instanceof ApiError)) throw e;
      if (!e.code && (e.status === 404 || e.status === 405)) {
        missing.add(key);
        return false;
      }
      // Any answer in the API's own shape means the route is there.
      if (!e.code) throw e;
    }
    there.add(key);
    return true;
  },
  /**
   * The env step: has the daemon open the env files of these projects, and
   * of no other, for connection URLs. Projects are named by the id the scan
   * gave them. The answer is the scan with what was found.
   */
  readEnv: (projects: string[]) => post<Scan>("/api/discover/env", { projects }),
  /** Starts a scan of what is listening, and of the folders named. It opens no env file. It replaces one under way. */
  scanStart: (folders: string[]) => post<Scan>("/api/discover/scan", folders.length ? { folders } : {}),
  /** The latest scan, held back up to `waitMs` while it is still running; null when there is none. */
  scan: async (waitMs?: number): Promise<Scan | null> => {
    try {
      return await get<Scan>("/api/discover/scan", { wait_ms: waitMs });
    } catch (e) {
      if (e instanceof ApiError && e.code === "no_scan") return null;
      throw e;
    }
  },
  /** Stops the scan and has the daemon forget the URLs it found. */
  scanForget: () => del<unknown>("/api/discover/scan"),
  /** Saves found connections by id: the page never has their URLs. */
  saveFound: (connections: { id: string; capture: boolean; name?: string }[]) => post<SaveFound>("/api/discover/save", { connections }),
  /** Tries found connections by id, saving nothing. Remote servers only when asked for. */
  testFound: (ids: string[], remote = false) => post<TestFound>("/api/discover/test", { ids, remote }),
  /**
   * Whether Shipyard is on this machine, and its boxes; null from a daemon
   * that can't ask. Asking has the daemon run Shipyard's command line, which
   * starts Shipyard's own background agent when it is not running: so this
   * is asked when a person opens the machine switcher or reaches the foot
   * of a scan they started, and never because the window opened.
   */
  shipyard: () => maybe<Shipyard>("/api/discover/shipyard"),

  // Sending a problem to a coding agent. Like the boxes above, none of this
  // is asked until a person opens the dialog: the brief only reads the
  // store, but the targets have the daemon run Shipyard's command line.
  /**
   * A problem written up as a prompt, by the daemon the window is showing:
   * it is the one that holds the data. `example` asks for real values (one
   * statement, a log line as written); without it only their shapes are in.
   */
  brief: (ask: BriefAsk, s: Scope, example: boolean) => get<Brief>(briefPath(ask), briefParams(ask, s, example)),
  /**
   * Where a task for a service could start. Always asked of this
   * computer's own daemon, since Shipyard's command line is here: `source`
   * carries what the daemon that wrote the brief knows of the service, and
   * `remote` says that daemon is another machine's. `box` lists another
   * box's projects. Null from a daemon too old to ask Shipyard.
   */
  shipyardTargets: async (source: BriefSource | undefined, box?: string): Promise<ShipyardTargets | null> => {
    try {
      return await post<ShipyardTargets>("/api/shipyard/targets", { source: { ...(source ?? { service: "" }), remote: !showingLocal() }, box: box ?? "" }, localBase());
    } catch (e) {
      if (e instanceof ApiError && !e.code && (e.status === 404 || e.status === 405)) return null;
      throw e;
    }
  },
  /** Starts a Shipyard task with the prompt, through this computer's own daemon. The one request in the app that starts anything outside Leadline. */
  shipyardSend: (task: ShipyardSend) => post<ShipyardTask>("/api/shipyard/tasks", task, localBase()),
};
