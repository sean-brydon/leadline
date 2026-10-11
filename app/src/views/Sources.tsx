import { useState, type ComponentType, type ReactNode } from "react";
import { BoxIcon, DatabaseIcon, EllipsisIcon, ZapIcon } from "lucide-react";
import { api, base, type Connection, type ConnectionStats as Stats, type Series, type Sources as SourcesData } from "@/lib/api";
import { fmtAgo, fmtBytes, fmtCount, fmtFor, fmtSpan } from "@/lib/format";
import { useEvery, useLive } from "@/lib/live";
import type { Kept } from "@/lib/kept";
import { ALL, scopeOf, type Source } from "@/lib/scope";
import { captureWord, connProblem, kindVersion, listed, ownerLine, signals, versionOf, windowed, type AppRow, type DbRow, type Model } from "@/lib/sources";
import { AgentPrompt, runLine } from "@/components/AddSource";
import { CaptureSwitch, useCapture } from "@/components/Capture";
import type { AddKind } from "@/components/Sidebar";
import { ConnectionStats } from "@/views/ConnectionStats";
import { CopyLine, Dot, Meter, PickRow, Rows, Surface, Waiting } from "@/components/ui";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogDescription, DialogFooter, DialogHeader, DialogPopup, DialogTitle } from "@/components/ui/dialog";
import { Menu, MenuItem, MenuPopup, MenuSeparator, MenuTrigger } from "@/components/ui/menu";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

type Props = {
  sources: SourcesData | undefined;
  error: string | null;
  model: Model;
  rangeMs: number;
  /** The connection whose stats are open. */
  open: string | null;
  onOpen: (id: string | null) => void;
  onAdd: (kind?: AddKind) => void;
  /** Narrow the app to a source and show it. */
  onSource: (s: Source) => void;
  /** A connection changed: fetch the sources again. */
  onChanged: () => void;
  /** What is kept on disk, and how full that is. */
  kept: Kept;
};

// More apps than this and the page stops asking for each one's own numbers.
const MAX_ASKED = 16;
// A database is asked for its own figures this often while the page is open.
const STATS_EVERY_MS = 30_000;

type Icon = ComponentType<{ className?: string; "aria-hidden"?: boolean }>;

/**
 * Sources is everything feeding Leadline, as a table: how each reaches it,
 * what it sent, when it was last heard, and what is wrong when something
 * is. A database's row opens what the server knows about itself.
 */
export function Sources({ sources, error, model, rangeMs, open, onOpen, onAdd, onSource, onChanged, kept }: Props) {
  const names = model.apps.slice(0, MAX_ASKED).map((a) => a.service);
  // What each app sent in the window: one bucket of the series, per app.
  const sent = useLive(async () => {
    const now = Date.now();
    const all = await Promise.all(names.map((service) => api.series(scopeOf({ kind: "app", service }, now - rangeMs), now, 1)));
    return Object.fromEntries(names.map((n, i) => [n, all[i]])) as Record<string, Series>;
  }, [rangeMs, names.join("\n")]);
  const requests = useLive(() => api.breakdown(scopeOf(ALL, Date.now() - rangeMs), "service", 1, 20), [rangeMs]);
  const [removing, setRemoving] = useState<Connection | null>(null);

  if (!sources) return <Waiting error={error} />;
  const conns = [...model.databases, ...model.caches];
  const opened = conns.find((d) => d.conn.id === open);
  const wide = !opened;
  const window = fmtSpan(rangeMs);
  const requestsOf = (service: string) => requests.data?.groups.find((g) => g.key === service)?.total;

  const table = (
    <Rows className={wide ? undefined : "[&_td]:px-2.5"}>
      <TableHeader>
        <TableRow>
          <TableHead className={wide ? "w-[28%]" : undefined}>Source</TableHead>
          {wide && (
            <>
              <TableHead>How it reaches Leadline</TableHead>
              <TableHead className="w-[20%]">Sent in the last {window}</TableHead>
              <TableHead className="w-[84px]">Last heard</TableHead>
            </>
          )}
          <TableHead className="w-10">
            <span className="sr-only">More</span>
          </TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        <Section cols={wide ? 5 : 2}>Apps</Section>
        {model.apps.length === 0 && (
          <NoneRow cols={wide ? 5 : 2}>
            No app is sending yet.{" "}
            <Button variant="link" size="xs" onClick={() => onAdd("app")} className="h-auto px-0 text-xs font-normal underline sm:h-auto">
              Add your app
            </Button>
          </NoneRow>
        )}
        {model.apps.map((a) => (
          <AppLine key={a.service} row={a} wide={wide} nowMs={sources.now_ms} sent={sent.data?.[a.service]} requests={requestsOf(a.service)} onSource={onSource} />
        ))}
        <Section cols={wide ? 5 : 2}>Databases and caches</Section>
        {conns.length === 0 && (
          <NoneRow cols={wide ? 5 : 2}>
            None connected.{" "}
            <Button variant="link" size="xs" onClick={() => onAdd("postgres")} className="h-auto px-0 text-xs font-normal underline sm:h-auto">
              Connect Postgres
            </Button>{" "}
            or{" "}
            <Button variant="link" size="xs" onClick={() => onAdd("redis")} className="h-auto px-0 text-xs font-normal underline sm:h-auto">
              Redis
            </Button>
          </NoneRow>
        )}
        {conns.map((d) => (
          <DbLine
            key={d.conn.id}
            row={d}
            wide={wide}
            on={d.conn.id === open}
            nowMs={sources.now_ms}
            onOpen={() => onOpen(d.conn.id === open ? null : d.conn.id)}
            onSource={onSource}
            onChanged={onChanged}
            onRemove={() => setRemoving(d.conn)}
          />
        ))}
      </TableBody>
    </Rows>
  );

  return (
    <div className="flex h-full">
      {wide ? (
        <div className="@container min-w-0 flex-1 overflow-y-auto">
          <div className="flex flex-col items-start gap-3 p-3 @4xl:flex-row">
            <Surface render={<section aria-label="Sources" />} className="w-full min-w-0 flex-1 overflow-hidden">
              {table}
            </Surface>
            <Rail sources={sources} kept={kept} />
          </div>
        </div>
      ) : (
        <>
          <div className="w-[30%] max-w-[340px] min-w-[236px] shrink-0 overflow-y-auto border-r">{table}</div>
          <ConnectionStats key={opened.conn.id} conn={opened.conn} onClose={() => onOpen(null)} onChanged={onChanged} />
        </>
      )}
      <Remove
        conn={removing}
        onClose={() => setRemoving(null)}
        onRemoved={(id) => {
          if (id === open) onOpen(null);
          onChanged();
        }}
      />
    </div>
  );
}

function Section({ cols, children }: { cols: number; children: ReactNode }) {
  return (
    <TableRow className="hover:bg-transparent">
      <TableHead colSpan={cols} scope="colgroup" className="h-7! bg-muted/50 text-[11px] tracking-wide">
        {children}
      </TableHead>
    </TableRow>
  );
}

function NoneRow({ cols, children }: { cols: number; children: ReactNode }) {
  return (
    <TableRow className="hover:bg-transparent">
      <TableCell colSpan={cols} className="py-2.5! text-muted-foreground">
        {children}
      </TableCell>
    </TableRow>
  );
}

/** Name is a row's first cell: what kind of thing it is, its status, its name, and a line about it. */
function Name({ icon: Glyph, tone, name, sub, badge }: { icon: Icon; tone: AppRow["tone"]; name: string; sub: ReactNode; badge?: ReactNode }) {
  return (
    <div className="flex min-w-0 items-center gap-2.5">
      <span className="flex size-[30px] shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground">
        <Glyph aria-hidden className="size-3.5" />
      </span>
      <div className="min-w-0">
        <div className="flex min-w-0 items-center gap-1.5">
          <Dot tone={tone} label={tone === "ok" ? "Sending" : tone === "quiet" ? "Quiet" : tone === "err" ? "Failing" : "Waiting"} />
          <span className="selectable truncate text-[13px]">{name}</span>
          {badge}
        </div>
        <div className="truncate text-[11.5px] text-muted-foreground">{sub}</div>
      </div>
    </div>
  );
}

/** Two is a cell of two lines: what, and a quieter line about it. `wrap` lets them run on to another line, where they are to be read whole. */
const Two = ({ top, bottom, mono, wrap }: { top: ReactNode; bottom?: ReactNode; mono?: boolean; wrap?: boolean }) => (
  <div className="min-w-0">
    <div className={`${wrap ? "leading-snug text-pretty" : "truncate"} ${mono ? "selectable font-mono text-[11.5px]" : ""}`}>{top}</div>
    {bottom && <div className={`text-[11.5px] text-muted-foreground ${wrap ? "leading-snug text-pretty" : "truncate"}`}>{bottom}</div>}
  </div>
);

/** Wrong is what is the matter with a source, in the daemon's words, and what to try. */
const Wrong = ({ message, hint }: { message: string; hint?: string }) => (
  <div role="alert" className="selectable mt-1 text-[11.5px] leading-snug text-pretty">
    <span className="text-destructive-foreground">{message}</span> {hint && <span className="text-muted-foreground">{hint}</span>}
  </div>
);

const list = (parts: (string | false | undefined)[]) => parts.filter(Boolean).join(", ");
const n = (count: number, one: string, many = `${one}s`) => `${fmtCount(count)} ${count === 1 ? one : many}`;

function AppLine({ row, wide, nowMs, sent, requests, onSource }: { row: AppRow; wide: boolean; nowMs: number; sent?: Series; requests?: number; onSource: (s: Source) => void }) {
  const { held, run } = row;
  const quiet = row.tone === "quiet";
  const otlp = held.via.includes("otlp");
  const proxied = held.via.includes("postgres-proxy");
  // Which database its captured calls went to, when the daemon knows: what ties the app to it.
  const pg = row.through.filter((v) => v.kind === "postgres").map((v) => v.name);
  const proxy = pg.length > 0 ? `the proxy to ${listed(pg)}` : "the Postgres proxy";

  let how: ReactNode;
  if (run?.running) {
    how = (
      <Two
        mono
        top={`leadline run -- ${run.command}`}
        bottom={`Running for ${fmtFor(nowMs - run.started_ms)}${run.provider ? `, with the ${run.provider === "node" ? "Node" : run.provider} provider` : ""}${pg.length > 0 ? `. Its queries go through ${proxy}.` : ""}`}
        wrap={pg.length > 0}
      />
    );
  } else if (row.agoMs === Infinity) {
    // Listed before anything has come from it: a worktree that was attached, or a run that has ended without a word.
    how = <Two wrap top={row.owner ? "Attached" : "Nothing yet"} bottom="Nothing has arrived from it yet. It reports when its dev command next runs under leadline run." />;
  } else if (quiet) {
    const ended = run && !run.running && run.ended_ms;
    how = (
      <Two
        wrap
        top={otlp ? "OpenTelemetry" : proxied ? `Through ${proxy}` : "OpenTelemetry"}
        bottom={
          <span className="text-warning-foreground">
            {ended
              ? run.lost
                ? `Its command (${run.command}) is gone, without saying how it ended.`
                : `Its command (${run.command}) ended ${fmtAgo(nowMs - (run.ended_ms as number))}${run.exit_code ? ` with exit code ${run.exit_code}` : run.signal ? ` on ${run.signal}` : ""}.`
              : "Quiet. Its process may have stopped."}
          </span>
        }
      />
    );
  } else if (otlp) {
    how = <Two wrap={proxied} top={proxied ? `OpenTelemetry, and ${proxy}` : "OpenTelemetry"} bottom={signals(held) ? `Sends ${signals(held)} itself` : "Nothing held from it now"} />;
  } else if (proxied) {
    how = <Two wrap top={`Through ${proxy}`} bottom="Only its queries are seen. Add OpenTelemetry or leadline run to see its requests and logs." />;
  } else {
    how = <Two top={held.via.includes("redis-monitor") ? "Redis commands, captured" : "Nothing yet"} />;
  }

  // What a capture recorded for an app is its queries: nothing was asked of the app itself that Leadline saw.
  const figures = sent ? list([requests && otlp ? n(requests, "request") : undefined, sent.logs[0] > 0 && n(sent.logs[0], "log")]) : "";
  const queries = sent && sent.queries[0] > 0 ? n(sent.queries[0], "query", "queries") : "";
  const failed = sent ? list([sent.errors[0] > 0 && n(sent.errors[0], "failed span"), sent.log_errors[0] > 0 && n(sent.log_errors[0], "error log")]) : "";

  return (
    <PickRow onPick={() => onSource(row.source)}>
      <TableCell>
        <Name icon={BoxIcon} tone={row.tone} name={row.service} sub={wide ? [ownerLine(row.owner), `${n(held.spans, "span")}, ${n(held.logs, "log")} held`].filter(Boolean).join(" · ") : row.how} />
      </TableCell>
      {wide && (
        <>
          <TableCell>{how}</TableCell>
          <TableCell>
            {sent === undefined ? (
              <span className="text-muted-foreground">…</span>
            ) : (
              <Two
                wrap
                top={figures || queries ? <span className="tabular-nums">{figures || queries}</span> : <span className="text-muted-foreground">Nothing</span>}
                bottom={failed ? <span className="text-destructive-foreground tabular-nums">{failed}</span> : figures && queries ? <span className="tabular-nums">{queries}</span> : undefined}
              />
            )}
          </TableCell>
          <TableCell className="text-muted-foreground tabular-nums">{row.agoMs === Infinity ? "Never" : fmtAgo(row.agoMs)}</TableCell>
        </>
      )}
      <TableCell className="px-1!">
        <More label={`More for ${row.service}`}>
          <MenuItem onClick={() => onSource(row.source)}>Show only this app</MenuItem>
        </More>
      </TableCell>
    </PickRow>
  );
}

type DbLineProps = {
  row: DbRow;
  wide: boolean;
  on: boolean;
  nowMs: number;
  onOpen: () => void;
  onSource: (s: Source) => void;
  onChanged: () => void;
  onRemove: () => void;
};

function DbLine({ row, wide, on, nowMs, onOpen, onSource, onChanged, onRemove }: DbLineProps) {
  const c = row.conn;
  const capture = useCapture(c, onChanged);
  const problem = connProblem(c);
  const word = captureWord(c.kind);
  const where = c.remote ? `at ${c.host}` : "on this Mac";
  // A database that isn't captured has nothing in Leadline's own store to
  // count, so its row shows what the server says about itself. That is asked
  // of the database, so it is asked seldom, and only while this page is up.
  const stats = useEvery<Stats | null>(() => (c.capture || c.health.state === "error" || !wide ? Promise.resolve(null) : api.connectionStats(c.id)), STATS_EVERY_MS, [c.id, c.capture, c.health.state === "error", wide]);

  let how: ReactNode;
  if (!c.capture_offer.available) {
    how = <Two wrap top="Reading its statistics" bottom={c.remote ? "Read-only. A remote server can't be captured." : c.capture_offer.note} />;
  } else {
    how = (
      <div className="flex min-w-0 items-start gap-2.5">
        <span className="flex h-[18px] items-center">
          <CaptureSwitch conn={c} capture={capture} />
        </span>
        <div className="min-w-0 flex-1">
          <div className="truncate">{capture.on ? `Capturing every ${word}` : "Statistics only"}</div>
          {capture.fault ? (
            <Wrong message={capture.fault.message} hint={capture.fault.hint} />
          ) : c.proxy?.state === "listening" ? (
            <>
              <div className="selectable font-mono text-[11px] leading-snug text-muted-foreground">
                {c.proxy.listen} <span aria-label="forwards to">→</span> {c.proxy.upstream}
              </div>
              {row.apps.length > 0 && <div className="text-[11.5px] leading-snug text-pretty text-muted-foreground">Used by {listed(row.apps)}</div>}
            </>
          ) : c.monitor && c.monitor.state !== "error" ? (
            <div className="text-[11.5px] leading-snug text-pretty text-muted-foreground">{c.monitor.state === "connecting" ? "Connecting to listen…" : "Listening with MONITOR, which says when a command ran but not how long it took."}</div>
          ) : !c.capture ? (
            <div className="text-[11.5px] leading-snug text-pretty text-muted-foreground">
              {c.kind === "redis" ? "Turn on to see every command. It can cost a busy Redis up to half its throughput." : "Turn on to see every query, through a port Leadline opens."}
            </div>
          ) : null}
        </div>
      </div>
    );
  }

  // What the capture recorded in the window, as an app's row says what it sent, over its count since it began. A daemon too old to count by window gives only the second.
  const inWindow = windowed(c);
  const began = (total: number, failed?: ReactNode) =>
    inWindow === null ? (
      (failed ?? "since capture began")
    ) : (
      <span className="tabular-nums">
        {fmtCount(total)} since capture began{failed ? <>, {failed}</> : null}
      </span>
    );
  let sent: ReactNode;
  let heard: string;
  if (c.proxy) {
    const failed = c.proxy.failed_statements > 0 ? <span className="text-destructive-foreground tabular-nums">{fmtCount(c.proxy.failed_statements)} failed{inWindow === null ? " since capture began" : ""}</span> : undefined;
    sent = <Two wrap top={<span className="tabular-nums">{n(inWindow ?? c.proxy.statements, "statement")}</span>} bottom={began(c.proxy.statements, failed)} />;
    heard = c.proxy.last_activity_ms > 0 ? fmtAgo(nowMs - c.proxy.last_activity_ms) : "Never";
  } else if (c.monitor) {
    const dropped =
      c.monitor.dropped > 0 ? (
        <span className="text-warning-foreground tabular-nums">
          {fmtCount(c.monitor.dropped)} dropped, over {fmtCount(c.monitor.max_per_sec)} a second
        </span>
      ) : undefined;
    sent = <Two wrap top={<span className="tabular-nums">{n(inWindow ?? c.monitor.recorded, "command")}</span>} bottom={began(c.monitor.recorded, dropped)} />;
    heard = c.monitor.last_activity_ms > 0 ? fmtAgo(nowMs - c.monitor.last_activity_ms) : "Never";
  } else {
    const s = stats.data;
    sent =
      s?.kind === "postgres" ? (
        <Two top={<span className="tabular-nums">{`${fmtBytes(s.size_bytes)}, ${s.connections.used} of ${s.connections.max} connections`}</span>} bottom={s.missing_index_hints.length > 0 ? <span className="text-warning-foreground">{n(s.missing_index_hints.length, "missing-index hint")}</span> : n(s.table_count, "table")} />
      ) : s?.kind === "redis" ? (
        <Two top={<span className="tabular-nums">{`${fmtBytes(s.memory.used_bytes)}, ${n(s.keyspace.keys, "key")}`}</span>} bottom={`${fmtCount(s.ops_per_sec)} commands a second`} />
      ) : (
        <span className="text-muted-foreground">{c.health.state === "error" ? "–" : "…"}</span>
      );
    heard = c.health.checked_ms ? fmtAgo(nowMs - c.health.checked_ms) : "–";
  }

  return (
    <PickRow on={on} onPick={onOpen} className="[&>td]:align-top">
      <TableCell>
        <Name
          icon={c.kind === "redis" ? ZapIcon : DatabaseIcon}
          tone={row.tone}
          name={c.name}
          badge={
            c.remote && (
              <Badge variant="secondary" className="font-normal text-muted-foreground">
                remote
              </Badge>
            )
          }
          sub={wide ? [ownerLine(c.owner), `${kindVersion(c.kind, versionOf(c.health))} ${where}`].filter(Boolean).join(" · ") : row.how}
        />
      </TableCell>
      {wide && (
        <>
          <TableCell>
            {how}
            {problem && !capture.fault && <Wrong message={problem.message} hint={problem.hint} />}
          </TableCell>
          <TableCell>{sent}</TableCell>
          <TableCell className="text-muted-foreground tabular-nums">{heard}</TableCell>
        </>
      )}
      <TableCell className="px-1!">
        <More label={`More for ${c.name}`}>
          <MenuItem onClick={onOpen}>{on ? "Close its stats" : "Open its stats"}</MenuItem>
          <MenuItem onClick={() => onSource(row.source)}>Show only its {c.kind === "redis" ? "commands" : "queries"}</MenuItem>
          <MenuSeparator />
          <MenuItem variant="destructive" onClick={onRemove}>
            Remove…
          </MenuItem>
        </More>
      </TableCell>
    </PickRow>
  );
}

function More({ label, children }: { label: string; children: ReactNode }) {
  return (
    // A press on the menu is not a press on the row it sits in.
    <span onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
      <Menu>
        <MenuTrigger render={<Button variant="ghost" size="icon-xs" aria-label={label} className="text-muted-foreground hover:text-foreground" />}>
          <EllipsisIcon />
        </MenuTrigger>
        <MenuPopup align="end">{children}</MenuPopup>
      </Menu>
    </span>
  );
}

/** Remove asks before a connection is deleted, and says what goes with it. */
function Remove({ conn, onClose, onRemoved }: { conn: Connection | null; onClose: () => void; onRemoved: (id: string) => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // The last connection asked about is kept while the dialog closes, so its words don't blank on the way out.
  const [shown, setShown] = useState<Connection | null>(null);
  if (conn && conn !== shown) {
    setShown(conn);
    setError(null);
  }
  const remove = () => {
    if (!conn) return;
    setBusy(true);
    api.deleteConnection(conn.id).then(
      () => {
        setBusy(false);
        onRemoved(conn.id);
        onClose();
      },
      (e: unknown) => {
        setBusy(false);
        setError(e instanceof Error ? e.message : String(e));
      },
    );
  };
  return (
    <Dialog open={conn !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogPopup className="max-w-md" showCloseButton={false} bottomStickOnMobile={false}>
        <DialogHeader className="gap-1.5 p-5 pb-4">
          <DialogTitle className="text-[15px] leading-normal font-medium">Remove {shown?.name}?</DialogTitle>
          <DialogDescription>
            Leadline forgets its URL and stops reading its statistics.{shown?.capture ? (shown.kind === "redis" ? " Capture stops." : " Its proxy closes, so an app still pointed at it loses its database.") : ""} What was already recorded stays.
          </DialogDescription>
          {error && <p className="text-destructive-foreground">{error}</p>}
        </DialogHeader>
        <DialogFooter className="px-5 py-3">
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="destructive" onClick={remove} loading={busy}>
            Remove
          </Button>
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}

/** Rail is the column beside the table: how to add an app, where to send, and what is kept. */
function Rail({ sources, kept }: { sources: SourcesData; kept: Kept }) {
  const daemon = base();
  const otlp = sources.otlp.endpoints.length ? sources.otlp.endpoints : [daemon];
  return (
    <aside aria-label="Adding and keeping" className="flex w-full shrink-0 flex-col gap-3 @4xl:w-[320px]">
      <Card title="Add an app in one paste">
        <AgentPrompt>
          {(line) => (
            <>
              <p className="text-muted-foreground">Paste this into Claude Code, Codex or Cursor in the project. It wires the dev setup and checks that data arrives.</p>
              {line}
              <p className="pt-1 text-muted-foreground">Or put it in front of your dev command:</p>
            </>
          )}
        </AgentPrompt>
        <CopyLine text={runLine("pnpm dev", daemon)} label="Copy the command" />
      </Card>
      <Card title="Where to send">
        <p className="text-[11.5px] text-muted-foreground">OpenTelemetry, over HTTP</p>
        {otlp.map((at, i) => (
          <CopyLine key={at} text={at} label={i === 0 ? "Copy the OpenTelemetry address" : "Copy the standard-port OpenTelemetry address"} />
        ))}
        <p className="pt-1 text-[11.5px] text-muted-foreground">For coding agents (MCP)</p>
        <CopyLine text={`${daemon}/mcp`} label="Copy the MCP address" />
      </Card>
      <Card title="Kept on this machine">
        {kept.full !== null && (
          <div className="flex items-center gap-2.5 py-0.5">
            <Meter value={kept.full} label="How full the store is against its size cap" />
            <span className="shrink-0 text-[11.5px] text-muted-foreground tabular-nums">{kept.fullText}</span>
          </div>
        )}
        <p className="text-pretty text-muted-foreground">{kept.long}</p>
      </Card>
    </aside>
  );
}

function Card({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Surface render={<section />} className="gap-1.5 px-3 py-2.5 text-xs">
      <h2 className="text-xs font-medium">{title}</h2>
      {children}
    </Surface>
  );
}
