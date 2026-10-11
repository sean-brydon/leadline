import { useEffect, useId, useRef, useState, type ComponentType, type ReactNode } from "react";
import { BoxIcon, CheckIcon, CircleAlertIcon, DatabaseIcon, EyeIcon, EyeOffIcon, RadioIcon, ScanSearchIcon, SquareTerminalIcon, ZapIcon } from "lucide-react";
import { api, ApiError, ownBase, type Connection, type Fault, type Run, type Sources, type TestResult } from "@/lib/api";
import { hidden, masked, urlKind, withPassword } from "@/lib/connurl";
import { fmtCount, fmtMs } from "@/lib/format";
import { useMachine } from "@/lib/live";
import { hereOf, LOCAL_ID } from "@/lib/machines";
import { captureWord, kindName, kindVersion, QUIET_AFTER_MS, signals, versionOf } from "@/lib/sources";
import { cn } from "@/lib/utils";
import type { AddKind } from "@/components/Sidebar";
import { Code, CopyLine, Dot, Tip } from "@/components/ui";
import { Button } from "@/components/ui/button";
import { Dialog, DialogPopup, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";

type Icon = ComponentType<{ className?: string; "aria-hidden"?: boolean }>;

const KINDS: { kind: AddKind; label: string; icon: Icon }[] = [
  { kind: "app", label: "Your app", icon: BoxIcon },
  { kind: "postgres", label: "Postgres", icon: DatabaseIcon },
  { kind: "redis", label: "Redis", icon: ZapIcon },
  { kind: "otel", label: "OpenTelemetry", icon: RadioIcon },
  { kind: "run", label: "A dev command", icon: SquareTerminalIcon },
];

const isDb = (k: AddKind): k is "postgres" | "redis" => k === "postgres" || k === "redis";

/**
 * useHere is the machine whose daemon the window shows, as a sentence says
 * it: "this Mac", or the name of another. A source is added to that
 * machine's Leadline, so its apps and databases are that machine's, and
 * what is copied from here is run there. `away` says it is another machine.
 */
function useHere(): { here: string; away: boolean } {
  const machine = useMachine();
  return { here: hereOf(machine), away: machine.id !== LOCAL_ID };
}

// Where a daemon listens unless told otherwise: `leadline run` and an
// OpenTelemetry SDK need telling only when this one is somewhere else.
const USUAL = "http://127.0.0.1:4320";

type PanelProps = {
  kind: AddKind;
  /** A connection was saved or removed: whoever lists them should ask again. */
  onChanged?: () => void;
  /** Open a saved connection's stats, and leave. */
  onStats?: (id: string) => void;
  /** Leave: the dialog's Cancel and Done. Left out when there is nowhere to leave to. */
  onClose?: () => void;
  /** Leave for the scan of this machine, which finds sources instead of asking for them. The press starts it. Left out when the daemon can't look around. */
  onLook?: () => void;
  /** Said where the way to the scan would be, when a press found that this daemon can't look around. */
  lookNote?: string;
  /** Draws the heading: a dialog's title has to be the dialog's own element. */
  heading: (text: string) => ReactNode;
};

/**
 * AddSource is the dialog for connecting something: the kinds of source
 * down the left, and for the chosen one what to do, a thing to copy, and a
 * line that turns to a tick when its first data arrives.
 */
export function AddSource({ open, kind, onClose, onChanged, onStats, onLook, lookNote }: { open: boolean; kind: AddKind; onClose: () => void; onChanged?: () => void; onStats?: (id: string) => void; onLook?: () => void; lookNote?: string }) {
  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogPopup className="max-w-[820px] overflow-hidden" bottomStickOnMobile={false}>
        {/* Unmounted with the dialog, so what was typed (a password in a URL) goes with it. */}
        <AddSourcePanel kind={kind} className="flex-[0_1_556px]" onChanged={onChanged} onStats={onStats} onClose={onClose} onLook={onLook} lookNote={lookNote} heading={(text) => <DialogTitle className="text-[15px] leading-normal font-medium">{text}</DialogTitle>} />
      </DialogPopup>
    </Dialog>
  );
}

/** AddSourcePanel is the dialog's inside, which is also the first-run screen. `className` sets its height: one height for every kind, so picking another doesn't move the dialog. */
export function AddSourcePanel({ kind: first, className, onLook, lookNote, ...rest }: PanelProps & { className?: string }) {
  const { here } = useHere();
  // `n` goes up when a kind is picked by hand, which starts its form afresh.
  // A URL that turns out to be for the other database moves the highlight
  // and keeps what was typed.
  const [pick, setPick] = useState({ kind: first, n: 0 });
  useEffect(() => setPick((p) => (p.kind === first ? p : { kind: first, n: p.n + 1 })), [first]);
  const kind = pick.kind;

  return (
    <div className={cn("flex min-h-0 w-full", className)}>
      <nav aria-label="Kind of source" className="flex w-[196px] shrink-0 flex-col gap-px border-r bg-muted/50 p-2">
        <div aria-hidden className="px-2.5 pt-1 pb-2 text-[11px] text-muted-foreground">
          Add a source
        </div>
        {KINDS.map(({ kind: k, label, icon: Glyph }) => (
          <button
            key={k}
            type="button"
            tabIndex={0}
            aria-current={k === kind ? "true" : undefined}
            data-on={k === kind ? "" : undefined}
            onClick={() => setPick((p) => ({ kind: k, n: p.n + 1 }))}
            className="flex h-8 w-full cursor-default items-center gap-2.5 rounded-md px-2.5 text-left text-muted-foreground outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset data-on:bg-accent data-on:text-foreground"
          >
            <Glyph aria-hidden className="size-3.5 shrink-0" />
            <span className="truncate">{label}</span>
          </button>
        ))}
        <p className="px-2.5 pt-3 text-[11.5px] leading-snug text-muted-foreground">MySQL and MongoDB are seen through OpenTelemetry for now.</p>
        {onLook ? (
          <div className="mt-auto flex flex-col gap-1.5 border-t px-0.5 pt-2.5">
            <p className="px-2 text-[11.5px] leading-snug text-pretty text-muted-foreground">Not sure what you have? Leadline can list the projects running on {here}. It opens no env file unless you tick its project.</p>
            {/* A machine's name can be longer than the column: the button grows a line for it. */}
            <Button variant="outline" size="sm" onClick={onLook} className="h-auto min-h-8 w-full justify-start py-1 text-left font-normal whitespace-normal sm:h-auto sm:min-h-7">
              <ScanSearchIcon />
              <span className="min-w-0 break-words">Look around {here}</span>
            </Button>
          </div>
        ) : (
          lookNote && (
            <p role="status" className="mt-auto border-t px-2.5 pt-2.5 text-[11.5px] leading-snug text-pretty text-muted-foreground">
              {lookNote}
            </p>
          )
        )}
      </nav>
      {/* The column scrolls when the window is shorter than the form; the form inside keeps its height, so the buttons stay at its foot. */}
      <div className="min-w-0 flex-1 overflow-y-auto">
        <div className="flex min-h-full flex-col px-5 pt-4 pb-4">
          {isDb(kind) ? (
            <DbForm key={`db-${pick.n}`} kind={kind} onKind={(k) => setPick((p) => ({ ...p, kind: k }))} {...rest} />
          ) : kind === "app" ? (
            <AppSteps key={`app-${pick.n}`} onOtel={() => setPick((p) => ({ kind: "otel", n: p.n + 1 }))} {...rest} />
          ) : kind === "otel" ? (
            <OtelSteps key={`otel-${pick.n}`} {...rest} />
          ) : (
            <RunSteps key={`run-${pick.n}`} {...rest} />
          )}
        </div>
      </div>
    </div>
  );
}

/** A block of the form: its label, then what it holds. */
function Block({ label, children }: { label?: ReactNode; children: ReactNode }) {
  return (
    <section className="flex flex-col gap-1.5">
      {label && <div className="text-foreground">{label}</div>}
      {children}
    </section>
  );
}

const Small = ({ children, className }: { children: ReactNode; className?: string }) => <p className={cn("text-[11.5px] leading-snug text-muted-foreground", className)}>{children}</p>;

/** Foot is the form's last row: its buttons, kept at the bottom however short the form. */
function Foot({ children }: { children: ReactNode }) {
  return <div className="mt-auto flex items-center justify-end gap-2 pt-4">{children}</div>;
}

/** Said is one line of outcome: a tick or a warning mark, and what happened. */
function Said({ ok, children, hint }: { ok: boolean; children: ReactNode; hint?: ReactNode }) {
  return (
    <div role="status" className="flex items-start gap-2">
      {ok ? <CheckIcon aria-hidden className="mt-0.5 size-3.5 shrink-0 text-success-foreground" /> : <CircleAlertIcon aria-hidden className="mt-0.5 size-3.5 shrink-0 text-destructive-foreground" />}
      <div className="min-w-0">
        <p className={cn("selectable", !ok && "text-destructive-foreground")}>{children}</p>
        {hint && <Small className="selectable mt-0.5">{hint}</Small>}
      </div>
    </div>
  );
}

const faultOf = (e: unknown): Fault => (e instanceof ApiError ? { code: e.code, message: e.message, hint: e.hint || undefined } : { code: "", message: e instanceof Error ? e.message : String(e) });

// What capture does and costs, before the daemon has seen the URL. Once it
// has (a test, or the saved connection) its own note is shown instead: it
// knows whether this server can be captured at all.
const captureNote = (kind: "postgres" | "redis", here: string, away: boolean) =>
  kind === "postgres"
    ? `Your app reaches Postgres through a port Leadline opens on ${here}, so each statement is recorded with its timing and who ran it. Only for a database on ${away ? `${here} itself` : "this machine"}.`
    : `Leadline has Redis report every command it runs (MONITOR). That can cost a busy server up to half its throughput, and commands are recorded with when they ran but not how long they took. Only for a Redis on ${away ? `${here} itself` : "this machine"}.`;
const PLACEHOLDER = { postgres: "postgres://user:password@localhost:5432/dbname", redis: "redis://localhost:6379" };
// A Postgres is worth capturing from the start; a Redis pays for it, so that is the person's call.
const CAPTURE_FIRST = { postgres: true, redis: false };

type Tested = { url: string; busy: boolean; result?: TestResult; fault?: Fault };

function DbForm({ kind, onKind, onChanged, onStats, onClose, heading }: Omit<PanelProps, "kind"> & { kind: "postgres" | "redis"; onKind: (k: "postgres" | "redis") => void }) {
  const id = useId();
  const { here, away } = useHere();
  // The URL as typed, password and all. It lives here and nowhere else: not
  // in storage, not in any state above this form.
  const [url, setUrl] = useState("");
  const [show, setShow] = useState(true);
  const [name, setName] = useState("");
  const [capture, setCapture] = useState<boolean | null>(null);
  const [tested, setTested] = useState<Tested | null>(null);
  const [saving, setSaving] = useState(false);
  const [fault, setFault] = useState<Fault | null>(null);
  // The daemon said this URL's server can't be captured, when asked to save it so.
  const [refused, setRefused] = useState<{ url: string; fault: Fault } | null>(null);
  const [saved, setSaved] = useState<Connection | null>(null);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => void (alive.current = false);
  }, []);

  // A URL for the other kind of database moves the form to it.
  const typedKind = urlKind(url);
  useEffect(() => {
    if (typedKind && typedKind !== kind) onKind(typedKind);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [typedKind]);

  const result = tested && tested.url === url.trim() ? tested : null;
  const offer = result?.result?.capture;
  const no = refused && refused.url === url.trim() ? refused.fault : null;
  const can = !no && (offer?.available ?? true);
  const wants = can && (capture ?? CAPTURE_FIRST[kind]);
  const word = captureWord(kind);

  const test = async () => {
    const asked = url.trim();
    if (!asked) return;
    setFault(null);
    setTested({ url: asked, busy: true });
    try {
      const r = await api.testUrl(asked);
      if (alive.current) setTested({ url: asked, busy: false, result: r });
    } catch (e) {
      if (alive.current) setTested({ url: asked, busy: false, fault: faultOf(e) });
    }
  };

  const save = async (withCapture: boolean) => {
    const asked = url.trim();
    if (!asked) return;
    setSaving(true);
    setFault(null);
    try {
      const c = await api.addConnection({ url: asked, name: name.trim() || undefined, capture: withCapture });
      if (!alive.current) return;
      setSaved(c);
      onChanged?.();
    } catch (e) {
      if (!alive.current) return;
      const f = faultOf(e);
      // Not a failure so much as an answer: the switch goes off, says why, and Save is still there.
      if (f.code === "capture_remote") setRefused({ url: asked, fault: f });
      else setFault(f);
    } finally {
      if (alive.current) setSaving(false);
    }
  };
  // An error from saving is the last thing in the form, which may be below what the window shows.
  const faultRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (fault) faultRef.current?.scrollIntoView({ block: "nearest" });
  }, [fault]);

  if (saved) return <Saved conn={saved} typed={url} here={here} onStats={onStats} onClose={onClose} heading={heading} />;

  return (
    <form
      className="flex min-h-0 flex-1 flex-col gap-4"
      onSubmit={(e) => {
        e.preventDefault();
        void save(wants);
      }}
    >
      {heading(`Connect ${kindName(kind)}`)}

      <Block label={<label htmlFor={`${id}-url`}>Connection URL</label>}>
        <div className="flex items-center gap-2">
          <div className="relative min-w-0 flex-1">
            <Input
              id={`${id}-url`}
              // The whole URL is hidden, not just its password: an input can't do less.
              type={show ? "text" : "password"}
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder={PLACEHOLDER[kind]}
              spellCheck={false}
              autoComplete="off"
              autoCapitalize="off"
              autoCorrect="off"
              autoFocus
              aria-describedby={`${id}-said`}
              className="font-mono *:pr-8 *:text-[12px]"
            />
            <Tip label={show ? "Hide the URL" : "Show the URL"}>
              <Button variant="ghost" size="icon-xs" aria-label={show ? "Hide the URL" : "Show the URL"} aria-pressed={!show} onClick={() => setShow(!show)} className="absolute top-1/2 right-1 -translate-y-1/2 text-muted-foreground">
                {show ? <EyeOffIcon /> : <EyeIcon />}
              </Button>
            </Tip>
          </div>
          <Button variant="outline" onClick={test} disabled={!url.trim()} loading={result?.busy}>
            Test
          </Button>
        </div>
        <div id={`${id}-said`} className="flex min-h-9 flex-col justify-start gap-1 pt-0.5">
          {!result || result.busy ? (
            <Small>Leadline reads its statistics over this, read-only. The password stays in a file only you can read, and is never shown again.</Small>
          ) : result.fault ? (
            <Said ok={false} hint={result.fault.hint}>
              {result.fault.message}
            </Said>
          ) : result.result?.ok ? (
            <>
              <Said ok>{connectedLine(result.result, here)}</Said>
              {result.result.pg_stat_statements && !result.result.pg_stat_statements.available && (
                <Small className="pl-[22px]">Per-query totals (pg_stat_statements) are off on this server. Its stats say how to turn them on.</Small>
              )}
            </>
          ) : (
            <Said ok={false} hint={result.result?.error?.hint}>
              {result.result?.error?.message ?? "The server refused."}
            </Said>
          )}
        </div>
      </Block>

      <Block label="What Leadline does with it">
        <div className="grid grid-cols-[30px_minmax(0,1fr)] items-start gap-x-2.5 rounded-lg border px-3 py-2.5">
          <span className="flex h-5 items-center justify-center">
            <CheckIcon aria-hidden className="size-3.5 text-success-foreground" />
          </span>
          <div>
            <p>Reads its statistics</p>
            <Small>
              {kind === "postgres"
                ? "Sizes, connections, missing indexes, locks and long-running statements. Read-only, always on."
                : "Memory, clients, the slow log and the biggest keys. Read-only, always on."}
            </Small>
          </div>
        </div>
        <div className="grid grid-cols-[30px_minmax(0,1fr)] items-start gap-x-2.5 rounded-lg border px-3 py-2.5">
          <span className="flex h-5 items-center">
            <Switch id={`${id}-capture`} checked={wants} disabled={!can} onCheckedChange={setCapture} aria-describedby={`${id}-capture-note`} />
          </span>
          <div>
            <label htmlFor={`${id}-capture`}>{can ? `Capture every ${word}` : `Can't capture every ${word}`}</label>
            <Small>
              <span id={`${id}-capture-note`} role={no ? "alert" : undefined}>
                {no ? `${no.message} ${no.hint ?? ""}` : (offer?.note ?? captureNote(kind, here, away))}
              </span>
            </Small>
          </div>
        </div>
      </Block>

      <Block label={<label htmlFor={`${id}-name`}>Name</label>}>
        <Input
          id={`${id}-name`}
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder={result?.result?.database && kind === "postgres" ? result.result.database : kind === "postgres" ? "The database's name" : "redis"}
          spellCheck={false}
          autoComplete="off"
          className="max-w-64"
        />
        {kind === "redis" && <Small>Captured commands are listed under this name.</Small>}
      </Block>

      {fault && (
        <div ref={faultRef} className="rounded-lg border border-destructive/24 bg-destructive/4 px-3 py-2.5">
          <Said ok={false} hint={fault.hint}>
            {fault.message}
          </Said>
        </div>
      )}

      <Foot>
        {onClose && (
          <Button variant="outline" onClick={onClose}>
            Cancel
          </Button>
        )}
        <Button type="submit" disabled={!url.trim()} loading={saving}>
          {no ? "Save with capture off" : "Save connection"}
        </Button>
      </Foot>
    </form>
  );
}

function connectedLine(r: TestResult, here: string): string {
  const what = kindVersion(r.kind, versionOf(r));
  const where = r.remote ? `at ${r.host}` : `on ${here}`;
  const took = r.latency_ms != null ? `, answering in ${fmtMs(r.latency_ms)}` : "";
  return `Connected. ${what} ${where}${took}${r.tls ? ", over TLS" : ""}.`;
}

/** Saved is the form after the connection was added: what to change in the app, and the wait for its first query. */
function Saved({ conn, typed, here, onStats, onClose, heading }: { conn: Connection; typed: string; here: string } & Pick<PanelProps, "onStats" | "onClose" | "heading">) {
  const word = captureWord(conn.kind);
  const proxyUrl = conn.proxy?.url ? withPassword(conn.proxy.url, typed) : "";
  const line = `DATABASE_URL=${proxyUrl}`;
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      {heading(`${kindName(conn.kind)} connected`)}
      <div className="flex flex-col gap-1">
        <Said ok>
          Saved as <span className="font-medium">{conn.name}</span>.{" "}
          {conn.health.state === "ok" ? `${kindVersion(conn.kind, versionOf(conn.health))} ${conn.remote ? `at ${conn.host}` : `on ${here}`} answers.` : ""}
        </Said>
        {conn.health.state === "error" && (
          <Said ok={false} hint={conn.health.hint}>
            Leadline can't reach it yet. {conn.health.message}
          </Said>
        )}
      </div>

      {conn.proxy && conn.proxy.state === "listening" && (
        <Block label="Point your app at Leadline">
          <CopyLine text={line} label="Copy the line for your .env" wrap>
            {`DATABASE_URL=${hidden(proxyUrl)}`}
          </CopyLine>
          <Small>
            {masked(proxyUrl) && "Put your password in place of ****. "}
            Port {conn.proxy.port} is kept for this database, so your .env keeps working. Or start the app with <Code>leadline run</Code>, which does this for you.
          </Small>
        </Block>
      )}
      {conn.proxy && conn.proxy.state === "error" && (
        <Said ok={false} hint="The connection is saved. Leadline tries the port again every few seconds.">
          The proxy on port {conn.proxy.port} couldn't open: {conn.proxy.error}
        </Said>
      )}
      {conn.monitor && (
        <Block label="Nothing to change in your app">
          <Small>{conn.monitor.note || conn.capture_offer.note}</Small>
        </Block>
      )}
      {!conn.capture && (
        <Block label="Reading its statistics">
          <Small>
            Nothing to change in your app. {conn.capture_offer.available ? `Capturing every ${word} can be turned on later, under Sources.` : conn.capture_offer.note}
          </Small>
        </Block>
      )}

      {conn.capture && <WaitForCapture conn={conn} />}

      <Foot>
        {onStats && (
          <Button variant="outline" onClick={() => onStats(conn.id)}>
            Open its stats
          </Button>
        )}
        {onClose && <Button onClick={onClose}>Done</Button>}
      </Foot>
    </div>
  );
}

/** usePolled asks once a second while `on`, and keeps the last answer. */
export function usePolled<T>(ask: () => Promise<T>, on: boolean): T | undefined {
  const [got, setGot] = useState<T>();
  const askRef = useRef(ask);
  askRef.current = ask;
  useEffect(() => {
    if (!on) return;
    let alive = true;
    let out = false;
    const go = () => {
      if (out) return;
      out = true;
      askRef.current().then(
        (v) => {
          out = false;
          if (alive) setGot(v);
        },
        () => {
          out = false;
        },
      );
    };
    go();
    const timer = setInterval(go, 1000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [on]);
  return got;
}

/** Waiting is the line that waits for first data: an amber dot and what it waits for, then a tick and what came. */
function Waiting({ done, children, aside }: { done: ReactNode | null; children: ReactNode; aside?: ReactNode }) {
  return (
    <div role="status" className="flex items-center gap-2.5 rounded-lg bg-muted px-3 py-2.5">
      {done ? <CheckIcon aria-hidden className="size-3.5 shrink-0 text-success-foreground" /> : <Dot tone="wait" className="mx-[3.5px] animate-pulse" />}
      <p className="min-w-0 flex-1">{done ?? children}</p>
      {!done && aside && <span className="shrink-0 text-[11.5px] text-muted-foreground">{aside}</span>}
    </div>
  );
}

function WaitForCapture({ conn }: { conn: Connection }) {
  const [seen, setSeen] = useState(0);
  const sources = usePolled(() => api.sources(), seen === 0);
  const now = sources?.connections.find((c) => c.id === conn.id);
  const count = now?.proxy?.statements ?? now?.monitor?.seen ?? 0;
  // The first count that isn't nothing is kept: the wait is over for good.
  useEffect(() => {
    if (count > 0 && seen === 0) setSeen(count);
  }, [count, seen]);
  const pg = conn.kind === "postgres";
  return (
    <Waiting
      done={seen > 0 ? (pg ? `The first query came through port ${conn.proxy?.port}. It is in Queries now.` : "The first command arrived. It is in Queries now.") : null}
      aside={pg ? "Restart your app to pick up the new URL" : "Any client's next command will do"}
    >
      {pg ? `Waiting for the first query through port ${conn.proxy?.port ?? "the proxy"}.` : `Waiting for the first command from ${conn.host}.`}
    </Waiting>
  );
}

/**
 * useNewcomer waits for a source that wasn't there when the form opened: a
 * service by a new name, or one that had gone quiet and speaks again, or
 * (with `runs`) a command started with `leadline run`.
 */
function useNewcomer(runs = false): { service?: Sources["services"][number]; run?: Run } | null {
  const [found, setFound] = useState<{ service?: Sources["services"][number]; run?: Run } | null>(null);
  const sources = usePolled(() => api.sources(), found === null);
  const list = usePolled(() => api.runs(), runs && found === null);
  // What was already there, from the first answer.
  const known = useRef<{ at: number; names: Set<string>; live: Set<string>; runs: Set<string> | null } | null>(null);
  useEffect(() => {
    if (!sources || found) return;
    if (!known.current) {
      // The services that were sending as the form opened. One of those carrying on is not news.
      const live = sources.services.filter((s) => sources.now_ms - s.last_seen_ms <= QUIET_AFTER_MS).map((s) => s.name);
      known.current = { at: sources.now_ms, names: new Set(sources.services.map((s) => s.name)), live: new Set(live), runs: null };
      return;
    }
    const k = known.current;
    // A name that wasn't there, or one that had gone quiet and has spoken since.
    const service = sources.services.find((s) => !k.names.has(s.name) || (!k.live.has(s.name) && s.last_seen_ms > k.at));
    if (service) setFound({ service });
  }, [sources, found]);
  useEffect(() => {
    if (!list || found || !known.current) return;
    const k = known.current;
    if (!k.runs) {
      k.runs = new Set(list.map((r) => r.id));
      return;
    }
    const run = list.find((r) => !k.runs?.has(r.id));
    if (run) setFound({ run });
  }, [list, found]);
  return found;
}

function WaitForApp({ runs }: { runs?: boolean }) {
  const found = useNewcomer(runs);
  const said = found?.service
    ? `${found.service.name} is sending${signals(found.service) ? `: ${signals(found.service)}` : ""}${found.service.spans ? ` (${fmtCount(found.service.spans)} spans so far)` : ""}.`
    : found?.run
      ? `${found.run.command} is running as ${found.run.service}. Its output is in Logs.`
      : null;
  return (
    <Waiting done={said} aside="Start the app, then use it once">
      {runs ? "Waiting for a command started with leadline run." : "Waiting for the first data from a new app."}
    </Waiting>
  );
}

/** useAddress is where an app should send OpenTelemetry: what the daemon says, or its own address until it has. Both are addresses on the daemon's machine, where the app runs. */
function useAddress(): string {
  const machine = useMachine();
  const [at, setAt] = useState<string | null>(null);
  useEffect(() => {
    let alive = true;
    api.sources().then(
      (s) => alive && setAt(s.otlp.endpoints[0] ?? null),
      () => {},
    );
    return () => {
      alive = false;
    };
  }, [machine.url]);
  return at ?? ownBase();
}

/** runLine is `leadline run` in front of a command, told where the daemon is when that isn't the usual place. */
export function runLine(command: string, daemon: string): string {
  return `leadline run ${daemon === USUAL ? "" : `--url ${daemon} `}-- ${command}`;
}

function usePrompt(): string | null | undefined {
  const [prompt, setPrompt] = useState<string | null>();
  useEffect(() => {
    let alive = true;
    api.setupPrompt().then(
      (p) => alive && setPrompt(p?.prompt ?? null),
      () => alive && setPrompt(null),
    );
    return () => {
      alive = false;
    };
  }, []);
  return prompt;
}

/** AgentPrompt is the line to paste into a coding agent; nothing at all from a daemon too old to write one. */
export function AgentPrompt({ children }: { children: (line: ReactNode) => ReactNode }) {
  const prompt = usePrompt();
  if (prompt === null) return null;
  return (
    <>
      {children(
        prompt === undefined ? (
          <div className="flex h-[34px] items-center gap-2 rounded-lg border bg-muted/50 px-2.5 text-[11.5px] text-muted-foreground">
            <Spinner aria-hidden className="size-3" />
            Loading…
          </div>
        ) : (
          <CopyLine text={prompt} label="Copy the prompt" wrap />
        ),
      )}
    </>
  );
}

function Done({ onClose }: Pick<PanelProps, "onClose">) {
  return (
    <Foot>
      {onClose && (
        <Button variant="outline" onClick={onClose}>
          Done
        </Button>
      )}
    </Foot>
  );
}

type StepProps = Omit<PanelProps, "kind">;

function AppSteps({ onOtel, onClose, heading }: StepProps & { onOtel: () => void }) {
  // The command is run on the daemon's machine: it names the daemon as that machine reaches it, never a port forwarded to this one.
  const daemon = ownBase();
  const { here, away } = useHere();
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      {heading("Add your app")}
      <AgentPrompt>
        {(line) => (
          <Block label="Have your coding agent do it">
            <Small>Paste this into Claude Code, Codex or Cursor in the project{away ? ` on ${here}` : ""}. It wires the dev setup and checks that data arrives.</Small>
            {line}
          </Block>
        )}
      </AgentPrompt>
      <Block label={away ? `Or put it in front of your dev command, on ${here}` : "Or put it in front of your dev command"}>
        <CopyLine text={runLine("pnpm dev", daemon)} label="Copy the command" />
        <Small>
          Use your own command in place of <Code>pnpm dev</Code>. Its output becomes logs, it is set up to send traces, and a Postgres on {here} is reached through a proxy that records its queries.
        </Small>
      </Block>
      <Small>
        An app that already has OpenTelemetry only needs the address:{" "}
        <Button variant="link" size="xs" onClick={onOtel} className="h-auto px-0 text-[11.5px] font-normal text-foreground underline sm:h-auto sm:text-[11.5px]">
          see OpenTelemetry
        </Button>
        .
      </Small>
      <WaitForApp runs />
      <Done onClose={onClose} />
    </div>
  );
}

function OtelSteps({ onClose, heading }: StepProps) {
  const at = useAddress();
  const { here, away } = useHere();
  const env = `OTEL_EXPORTER_OTLP_ENDPOINT=${at}\nOTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf\nOTEL_SERVICE_NAME=my-app`;
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      {heading("Send OpenTelemetry")}
      <Block label={away ? `Where to send, from an app on ${here}` : "Where to send"}>
        <CopyLine text={at} label="Copy the OpenTelemetry address" />
        <Small>
          OTLP over HTTP, as protobuf or JSON: traces to <Code>/v1/traces</Code>, logs to <Code>/v1/logs</Code>, metrics to <Code>/v1/metrics</Code>.
        </Small>
      </Block>
      <Block label="For an OpenTelemetry SDK, in the app's environment">
        <CopyLine text={env} label="Copy the environment lines" wrap />
        <Small>
          <Code>OTEL_SERVICE_NAME</Code> is the name the app shows under here. Set these for development only.
        </Small>
      </Block>
      <WaitForApp />
      <Done onClose={onClose} />
    </div>
  );
}

function RunSteps({ onClose, heading }: StepProps) {
  const daemon = ownBase();
  const { here, away } = useHere();
  return (
    <div className="flex min-h-0 flex-1 flex-col gap-4">
      {heading("Run a dev command")}
      <Block label={away ? `On ${here}, put leadline run in front of the command you already use` : "Put leadline run in front of the command you already use"}>
        <CopyLine text={runLine("pnpm dev", daemon)} label="Copy the command" />
        <Small>Every line it prints becomes a log, it is set up to send its traces, and a Postgres on {here} is reached through a proxy that records its queries. It stops when the command does.</Small>
      </Block>
      <Block label="To name it yourself">
        <CopyLine text={runLine("go run ./cmd/api", daemon).replace("leadline run ", "leadline run --name api ")} label="Copy the command with a name" />
        <Small>
          Without <Code>--name</Code> it shows under the project's name, or the folder's.
        </Small>
      </Block>
      <WaitForApp runs />
      <Done onClose={onClose} />
    </div>
  );
}

/**
 * FirstRun is the window before anything has ever been captured: the same
 * choices as the dialog, set in the page. It stays up while the first data
 * arrives, so the line that was waiting for it is seen to turn; `arrived`
 * then offers the way on.
 */
export function FirstRun({ kind = "app", arrived, onGo, onChanged, onStats, onLook, lookNote }: { /** The kind of source the panel opens on. */ kind?: AddKind; arrived: boolean; onGo: () => void; onChanged?: () => void; onStats?: (id: string) => void; onLook?: () => void; lookNote?: string }) {
  const machine = useMachine();
  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto flex min-h-full w-full max-w-[852px] flex-col justify-center gap-4 p-4">
        <div className="flex items-end gap-4 px-1">
          <div className="min-w-0 flex-1">
            <h2 className="text-[15px] font-medium">Connect your first source</h2>
            <p className="mt-0.5 text-muted-foreground">
              {arrived ? (
                "The first data has arrived. The views are filling in."
              ) : (
                <>
                  {machine.id === LOCAL_ID ? "Leadline is running, and nothing has reached it yet." : `Leadline is running on ${machine.name}, and nothing has reached it there yet.`} Pick what you have{machine.id === LOCAL_ID ? "" : " on that machine"}, or run <Code>leadline demo</Code>{machine.id === LOCAL_ID ? "" : " there"} to see made-up data.
                </>
              )}
            </p>
          </div>
          {arrived && (
            <Button onClick={onGo} className="shrink-0">
              Open the overview
            </Button>
          )}
        </div>
        <div className="flex min-w-0 overflow-hidden rounded-xl border bg-card">
          <AddSourcePanel kind={kind} className="h-[520px] flex-1" onChanged={onChanged} onStats={onStats} onLook={onLook} lookNote={lookNote} heading={(text) => <h3 className="text-[15px] leading-normal font-medium">{text}</h3>} />
        </div>
      </div>
    </div>
  );
}
