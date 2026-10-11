import { useCallback, useEffect, useId, useMemo, useRef, useState, type FormEvent, type ReactNode, type Ref } from "react";
import { BoxIcon, CheckIcon, CircleAlertIcon, DatabaseIcon, FolderIcon, RefreshCwIcon, ScanSearchIcon, XIcon, ZapIcon } from "lucide-react";
import { api, ApiError, base, ownBase, showingLocal, type Box, type Connection, type Fault, type FoundConnection, type FoundProject, type Scan, type TestResult } from "@/lib/api";
import {
  adviceCommand,
  adviceTitle,
  againLabel,
  boxState,
  canPick,
  connectLabel,
  connKey,
  connTitle,
  emptyScan,
  envLine,
  envRows,
  folderLine,
  foundKind,
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
  steps,
  tailPath,
  toRead,
  whatHappens,
  whereLine,
  whereProject,
  withoutLine,
  type EnvRow,
  type Heard,
  type Held,
  type Picks,
} from "@/lib/discover";
import { fmtMs } from "@/lib/format";
import { LOCAL_ID } from "@/lib/machines";
import { kindVersion, versionOf } from "@/lib/sources";
import { cn } from "@/lib/utils";
import { AgentPrompt, usePolled } from "@/components/AddSource";
import { Code, CopyLine, Dot, Surface, Tip } from "@/components/ui";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";

const faultOf = (e: unknown): Fault => (e instanceof ApiError ? { code: e.code, message: e.message, hint: e.hint || undefined } : { code: "", message: e instanceof Error ? e.message : String(e) });

// The folders a person has pointed the scan at are kept between visits, so
// "Look for more" looks where they looked before. They are paths, typed by
// the person; nothing found in them is kept here. Each machine has its own:
// a folder on this Mac is not one on another machine.
const FOLDERS_KEY = "leadline.look.folders";
const MAX_FOLDERS = 8;
const foldersKey = (machine: string) => (machine === LOCAL_ID ? FOLDERS_KEY : `${FOLDERS_KEY}:${machine}`);

function loadFolders(machine: string): string[] {
  try {
    const raw = JSON.parse(localStorage.getItem(foldersKey(machine)) ?? "[]") as unknown;
    return Array.isArray(raw) ? raw.filter((f): f is string => typeof f === "string" && f.trim() !== "").slice(0, MAX_FOLDERS) : [];
  } catch {
    return [];
  }
}

function saveFolders(machine: string, folders: string[]) {
  try {
    localStorage.setItem(foldersKey(machine), JSON.stringify(folders));
  } catch {
    // A full or forbidden store forgets the folders at the next launch, no more.
  }
}

const STOPPED: Fault = { code: "no_scan", message: "The scan was stopped before it finished.", hint: "Look again to start another." };
const GONE: Fault = { code: "no_scan", message: "The list this came from is gone: another window looked again, or the daemon restarted.", hint: "Look again to make a new one. No env file was read for it." };
const REPLACED: Fault = { code: "cancelled", message: "Another scan replaced this one before it finished.", hint: "Look again to see what it found." };

/** Which projects' env files are being read now: the ones asked for, in order, and how far along. */
type Reading = { ids: string[]; at: number };

/**
 * Look is looking around the machine whose daemon the window shows, as the
 * window holds it: the scan, the reading of env files, and the only ways
 * either starts. The daemon looks around its own machine, so turned to
 * another machine it is that one that is scanned.
 */
export type Look = {
  /** The folders a person has named to look in as well. */
  folders: string[];
  /** A scan has been asked for, by a press, and not left since. Until then nothing has been looked at. */
  started: boolean;
  scan?: Scan;
  fault?: Fault;
  /** A scan is under way. */
  busy: boolean;
  /** This daemon's scan is the older one-step kind (or it has none), so it was not asked to scan. */
  old: boolean;
  reading: Reading | null;
  /** By project id: why its env files could not be read, when the others could. */
  envFaults: Record<string, Fault>;
  /**
   * Starts a scan, of `folders` when given and else of the ones looked in
   * before, and then (`thenRead`) reads the env files of projects the
   * person had already let Leadline read. Called from a press of a button
   * and from nowhere else: no effect in this file calls it.
   */
  start: (folders?: string[], thenRead?: string[]) => void;
  /** The env step, for the projects a person ticked: one request each, so one that fails leaves the others read. */
  read: (ids: string[]) => Promise<void>;
  /** Asks for the scan as the daemon has it now: what was just saved is marked so. */
  refresh: () => void;
  /** The person left: the daemon is told to forget what it found, if anything was ever asked of it. */
  leave: () => void;
};

/**
 * useLook holds a look around the machine. It asks the daemon nothing by
 * itself: not when it mounts, not on a timer. `start` and `read` are the
 * only things that do, and they run when a person presses a button.
 *
 * `on` is whether this window may look at all (its daemon is connected);
 * a daemon that comes back may be a newer one, so what was known of an old
 * one is dropped. `machine` is the id of the machine being shown, whose
 * folders these are.
 */
export function useLook(on: boolean, machine: string = LOCAL_ID): Look {
  const [folders, setFolders] = useState(() => loadFolders(machine));
  const [got, setGot] = useState<{ scan?: Scan; fault?: Fault; busy: boolean; started: boolean }>({ busy: false, started: false });
  const [old, setOld] = useState(false);
  const [reading, setReading] = useState<Reading | null>(null);
  const [envFaults, setEnvFaults] = useState<Record<string, Fault>>({});
  // Every scan asked for has a number: an answer that belongs to an earlier one is dropped.
  const turn = useRef(0);
  // The daemon has been asked to scan and not yet to forget.
  const asked = useRef(false);
  const where = useRef(folders);
  where.current = folders;

  useEffect(() => {
    if (on) setOld(false);
  }, [on]);
  useEffect(
    () => () => {
      turn.current++;
    },
    [],
  );

  const read = useCallback(async (ids: string[], mine = turn.current) => {
    if (ids.length === 0) return;
    setEnvFaults((was) => Object.fromEntries(Object.entries(was).filter(([id]) => !ids.includes(id))));
    for (let at = 0; at < ids.length; at++) {
      if (turn.current !== mine) return;
      setReading({ ids, at });
      try {
        // One project to a request. The daemon reads nothing unless every
        // project named is in its scan, so asked together, one it no longer
        // has would stop the rest from being read.
        const scan = await api.readEnv([ids[at]]);
        if (turn.current !== mine) return;
        setGot((was) => ({ ...was, scan, fault: undefined }));
      } catch (e) {
        if (turn.current !== mine) return;
        const fault = faultOf(e);
        if (fault.code === "no_scan" || fault.code === "scan_not_done") {
          setGot((was) => ({ ...was, scan: undefined, busy: false, fault: GONE }));
          break;
        }
        setEnvFaults((was) => ({ ...was, [ids[at]]: fault }));
      }
    }
    if (turn.current === mine) setReading(null);
  }, []);

  const start = useCallback(
    (next?: string[], thenRead?: string[]) => {
      const look = next ?? where.current;
      if (next) {
        setFolders(next);
        saveFolders(machine, next);
      }
      const mine = ++turn.current;
      setReading(null);
      setEnvFaults({});
      setGot((was) => ({ scan: was.scan, busy: true, started: true }));
      void (async () => {
        try {
          // A daemon whose scan reads every env file by itself is not asked to scan at all.
          if (!(await api.envStep())) {
            if (turn.current !== mine) return;
            setOld(true);
            setGot({ busy: false, started: false });
            return;
          }
          if (turn.current !== mine) return;
          asked.current = true;
          let scan: Scan | null = await api.scanStart(look);
          // The daemon holds each answer back until the scan has moved on, so this asks without a timer.
          while (turn.current === mine && scan?.state === "running") {
            setGot({ scan, busy: true, started: true });
            scan = await api.scan(10_000);
          }
          if (turn.current !== mine) return;
          if (!scan) return setGot({ busy: false, started: true, fault: STOPPED });
          if (scan.state !== "done") return setGot({ busy: false, started: true, fault: scan.error ?? (scan.state === "cancelled" ? REPLACED : { code: "failed", message: "Leadline couldn't finish looking around." }) });
          setGot({ scan, busy: false, started: true });
          const again = (thenRead ?? []).filter((id) => scan.projects.some((p) => p.id === id && p.env_files.length > 0));
          await read(again, mine);
        } catch (e) {
          if (turn.current === mine) setGot({ busy: false, started: true, fault: faultOf(e) });
        }
      })();
    },
    [read, machine],
  );

  const refresh = useCallback(() => {
    const mine = turn.current;
    api.scan().then(
      (scan) => {
        if (turn.current === mine && scan?.state === "done") setGot((was) => ({ ...was, scan }));
      },
      () => {},
    );
  }, []);

  const leave = useCallback(() => {
    turn.current++;
    // Only a daemon that was asked to look has anything to forget: leaving a screen that never scanned asks it nothing.
    if (asked.current) {
      asked.current = false;
      api.scanForget().catch(() => {});
    }
    setGot({ busy: false, started: false });
    setReading(null);
    setEnvFaults({});
  }, []);

  return { folders, started: got.started, scan: got.scan, fault: got.fault, busy: got.busy, old, reading, envFaults, start, read, refresh, leave };
}

/** useNow is the time, told again every `everyMs`; it stands still when that is 0. */
function useNow(everyMs: number): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!everyMs) return;
    setNow(Date.now());
    const timer = setInterval(() => setNow(Date.now()), everyMs);
    return () => clearInterval(timer);
  }, [everyMs]);
  return now;
}

type Props = {
  look: Look;
  /** Nothing has ever reached this daemon: this is the window's first screen. */
  first: boolean;
  /** The machine being looked around, in a sentence: "this Mac", or the name of another. */
  here: string;
  /** The owner's Shipyard boxes, once they have been asked for and there are some. Only ever asked for when this Mac is the one looked around. */
  boxes: Box[];
  /** Go to the Machines view, where a box is added. */
  onMachines?: () => void;
  /** The foot of a finished scan has come into view: where the Shipyard boxes are said, so the moment to ask for them. */
  onFoot: () => void;
  /** Leave for the by-hand dialog, at a kind of source when one is asked for. */
  onByHand: (kind?: "postgres" | "redis" | "app") => void;
  /** Leave: to the overview on a first run, back to where the person was otherwise. */
  onDone: () => void;
  /** Connections were saved: whoever lists the sources should ask again. */
  onChanged: () => void;
};

type Tested = Record<string, { busy?: boolean; result?: TestResult; error?: Fault }>;
type SavedState = { saved: Connection[]; failed: { conn: FoundConnection; error: Fault }[]; projects: FoundProject[]; at: number };

const NOT_HELD: Held = { state: "none", line: "" };

/**
 * LookAround is the onboarding that looks before it asks, and asks before
 * it reads. It opens on what looking does and waits for a press. A scan
 * then lists the projects running on the machine with their env files by
 * name; the person ticks the projects whose env files Leadline may read
 * and presses the button that says so; the databases found there appear
 * with ticks of their own. Connect saves the ticked connections through
 * the daemon (this page never sees a password) and then waits, line by
 * line, until the daemon has really heard from each.
 */
export function LookAround({ look, first, here, boxes, onMachines, onFoot, onByHand, onDone, onChanged }: Props) {
  const { scan, fault, busy, folders, reading } = look;
  const [picks, setPicks] = useState<Picks>(NO_PICKS);
  const [tested, setTested] = useState<Tested>({});
  const [saving, setSaving] = useState(false);
  const [saveFault, setSaveFault] = useState<Fault | null>(null);
  const [connected, setConnected] = useState<SavedState | null>(null);

  // What a finished scan found is ticked as the daemon suggests, except where the person has already chosen. No project is ticked for reading.
  const done = scan?.state === "done" && !busy ? scan : undefined;
  useEffect(() => {
    if (done) setPicks((was) => picksFor(done, was));
  }, [done]);

  // The URLs the daemon read are held for half an hour: near the end, and after it, the list says so.
  const now = useNow(done?.secrets_until_ms ? 15_000 : 0);
  const hold = done ? held(done, now) : NOT_HELD;
  const gone = hold.state === "gone";
  // What was let go can't be connected, whatever is still ticked.
  const live = useMemo(() => (gone ? { ...picks, conns: {} } : picks), [gone, picks]);

  // A press that changes the whole screen leaves the keyboard where the new part begins.
  const top = useRef<HTMLHeadingElement>(null);
  const found = useRef<HTMLHeadingElement>(null);
  const [backs, setBacks] = useState(0);
  useEffect(() => {
    if (look.started) top.current?.focus();
  }, [look.started, backs]);
  const wasReading = useRef(false);
  useEffect(() => {
    if (wasReading.current && !reading) found.current?.focus();
    wasReading.current = !!reading;
  }, [reading]);

  if (!look.started) return <Intro first={first} here={here} onLook={() => look.start()} onByHand={() => onByHand()} />;

  const again = (next?: string[]) => {
    setTested({});
    look.start(next);
  };

  const test = async () => {
    if (!done) return;
    const local = done.connections.filter((c) => c.connectable && !c.remote);
    if (local.length === 0) return;
    setTested(Object.fromEntries(local.map((c) => [connKey(c), { busy: true }])));
    try {
      const { results } = await api.testFound(local.map((c) => c.id));
      setTested(Object.fromEntries(local.map((c) => [connKey(c), results.find((r) => r.id === c.id) ?? {}])));
    } catch (e) {
      const error = faultOf(e);
      setTested(Object.fromEntries(local.map((c) => [connKey(c), { error }])));
    }
  };

  const connect = async () => {
    if (!done) return;
    const items = pickedConns(done, live);
    const projects = pickedProjects(done, live);
    setSaving(true);
    setSaveFault(null);
    try {
      const res = items.length > 0 ? await api.saveFound(items) : { saved: [], failed: [] };
      const was = (id: string) => done.connections.find((c) => c.id === id);
      setConnected({
        saved: res.saved.map((s) => s.connection),
        failed: res.failed.flatMap((f) => (was(f.id) ? [{ conn: was(f.id) as FoundConnection, error: f.error }] : [])),
        projects,
        at: Date.now(),
      });
      if (res.saved.length > 0) onChanged();
    } catch (e) {
      setSaveFault(faultOf(e));
    } finally {
      setSaving(false);
    }
  };

  if (connected) {
    return (
      <Connecting
        state={connected}
        first={first}
        here={here}
        onBack={() => {
          setConnected(null);
          setBacks((n) => n + 1);
          // What was saved is now "already connected", which the daemon's scan says. Nothing is looked at again for it.
          look.refresh();
        }}
        onDone={onDone}
      />
    );
  }

  const label = done ? connectLabel(done, live) : "";
  const empty = !!done && emptyScan(done);
  const readSome = !!done && (done.env_projects ?? []).length > 0;
  const at = reading && done ? done.projects.find((p) => p.id === reading.ids[reading.at]) : undefined;
  return (
    <div className="flex h-full flex-col">
      <div className="@container min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-[1080px] flex-col gap-3 p-4">
          <div className="flex items-start gap-3 px-1">
            <div className="min-w-0 flex-1">
              <h2 ref={top} tabIndex={-1} className="text-[15px] font-medium outline-none">
                Here is what's on {here}
              </h2>
              <p role="status" className="mt-0.5 flex items-center gap-2 text-pretty text-muted-foreground">
                {(busy || reading) && <Spinner aria-hidden className="size-3.5 shrink-0" />}
                {busy
                  ? scan
                    ? progressLine(scan)
                    : "Looking around…"
                  : reading
                    ? `Reading ${at ? `${at.name}'s env files` : "env files"}${reading.ids.length > 1 ? ` (${reading.at + 1} of ${reading.ids.length})` : ""}…`
                    : done
                      ? empty
                        ? "Leadline looked at what is listening, and in the folders named here."
                        : readSome
                          ? "Leadline looked at what is listening and the projects behind it, and read the env files you let it. Untick anything you don't want connected."
                          : "Leadline looked at what is listening and the projects behind it. It has opened no env file."
                      : "Leadline couldn't finish looking around."}
              </p>
            </div>
            <Button variant="outline" size="sm" onClick={() => again()} disabled={busy || !!reading} className="shrink-0 font-normal">
              <RefreshCwIcon />
              Look again
            </Button>
          </div>

          <Folders folders={folders} scan={scan} busy={busy || !!reading} onLook={again} />

          {fault && !done ? (
            <Problem fault={fault} onAgain={() => again()} onByHand={() => onByHand()} />
          ) : !done ? (
            <Surface className="items-center gap-2 px-4 py-10 text-muted-foreground">
              <Spinner aria-hidden className="size-4" />
              <p>Nothing is changed by looking, and no env file is opened. This takes a moment.</p>
            </Surface>
          ) : (
            <>
              {empty && <NothingFound here={here} listening={runningRows(done).rows.length > 0} onAgain={() => again()} onByHand={onByHand} />}
              <div className="grid grid-cols-1 items-start gap-3 @4xl:grid-cols-2">
                {!empty && <Projects scan={done} picks={picks} onPick={(id, on) => setPicks((p) => ({ ...p, projects: { ...p.projects, [id]: on } }))} />}
                <div className="flex min-w-0 flex-col gap-3">
                  {/* Projects first, then what Leadline may read of them, then what it found there. */}
                  {!empty && (
                    <EnvFiles
                      scan={done}
                      picks={picks}
                      reading={reading}
                      faults={look.envFaults}
                      mac={done.machine?.os === "darwin"}
                      onPick={(ids, on) => setPicks((p) => ({ ...p, env: { ...p.env, ...Object.fromEntries(ids.map((id) => [id, on])) } }))}
                      onRead={() => void look.read(toRead(done, picks).map((p) => p.id))}
                      onRetry={(id) => void look.read([id])}
                    />
                  )}
                  {!empty && (
                    <Databases
                      scan={done}
                      reading={!!reading}
                      here={here}
                      picks={picks}
                      tested={tested}
                      hold={hold}
                      headRef={found}
                      onTest={test}
                      onPick={(key, v) => setPicks((p) => ({ ...p, conns: { ...p.conns, [key]: { ...p.conns[key], ...v } } }))}
                      onByHand={onByHand}
                      onAgain={() => {
                        setTested({});
                        look.start(undefined, done.env_projects ?? []);
                      }}
                    />
                  )}
                  <Running scan={done} />
                </div>
              </div>
              {/* Where the page ends. Shipyard is asked for its boxes when a person gets this far, and not before. */}
              <Foot onReach={onFoot} />
            </>
          )}

          {done && boxes.length > 0 && (
            <p className="px-1 text-[11.5px] leading-snug text-pretty text-muted-foreground">
              Your Shipyard boxes: {sentence(boxes.map((b) => `${b.name} (${boxState(b, Date.now())})`))}. This looked at {here} only. To look around a box, add it under{" "}
              {onMachines ? (
                <Inline onClick={onMachines} className="text-[11.5px] sm:text-[11.5px]">
                  Machines
                </Inline>
              ) : (
                "Machines"
              )}{" "}
              and turn the window to it.
            </p>
          )}
        </div>
      </div>

      <div className="flex shrink-0 flex-wrap items-center justify-end gap-x-3 gap-y-2 border-t px-4 py-2.5">
        <p className="min-w-0 flex-1 basis-64 text-[11.5px] leading-snug text-pretty text-muted-foreground">
          {saveFault ? (
            <span role="alert" className="text-destructive-foreground">
              {saveFault.message} {saveFault.hint}
            </span>
          ) : (
            "Nothing changes until you choose Connect. Leadline edits no project: a dev server is connected by a step you take, and a restart."
          )}
        </p>
        <Button variant="outline" onClick={() => (first ? onByHand() : onDone())} className="font-normal">
          {first ? "Skip, I'll add sources by hand" : "Close"}
        </Button>
        <Button onClick={connect} disabled={!label || !!reading} loading={saving}>
          {label || (done ? "Nothing is ticked" : "Connect")}
        </Button>
      </div>
    </div>
  );
}

/** Small is a quiet line under something. `one` keeps it to a single line, cut with an ellipsis; otherwise it wraps. */
const Small = ({ children, className, one }: { children: ReactNode; className?: string; one?: boolean }) => (
  <p className={cn("text-[11.5px] leading-snug text-muted-foreground", one ? "truncate" : "text-pretty", className)}>{children}</p>
);

/** A press inside a sentence: the same words as around it, underlined. */
const Inline = ({ onClick, children, className }: { onClick: () => void; children: ReactNode; className?: string }) => (
  <Button variant="link" size="xs" onClick={onClick} className={cn("h-auto px-0 text-xs font-normal text-foreground underline sm:h-auto", className)}>
    {children}
  </Button>
);

// What a scan reads and what it leaves alone, from the API's own account of it (internal/discover/API.md, "What is read, and what is not").
const LOOKS_AT = [
  "The ports your programs are listening on, with each program's name, command line and working folder.",
  "The project folder of each dev server that is running: found by walking up from where it runs, and never as far as your home folder.",
  "The project files there: package.json, go.mod, pyproject.toml, Gemfile or Cargo.toml, next.config, and the git branch.",
];
const doesNot = (here: string) => [
  "Open an env file. Each project's env files are listed by name, and Leadline reads them only for a project you tick.",
  "Look through your home folder, or read any program's environment.",
  `Connect to anything, change a file, or send anything off ${here}.`,
];

/**
 * Intro is the screen before anything has been looked at. It says what
 * looking around does and does not do, and waits: the scan starts when the
 * button is pressed. Beside it are the other two ways in, adding a source
 * by hand and handing the job to a coding agent.
 */
function Intro({ first, here, onLook, onByHand }: { first: boolean; here: string; onLook: () => void; onByHand: () => void }) {
  // Another machine is being looked at: what is run, is run there.
  const away = !showingLocal();
  return (
    <div className="@container h-full overflow-y-auto">
      <div className="mx-auto flex min-h-full w-full max-w-[820px] flex-col justify-center gap-3 p-4">
        <div className="px-1">
          <h2 className="text-[15px] font-medium">{first ? "Connect your first source" : "Look for more to connect"}</h2>
          <p className="mt-0.5 text-pretty text-muted-foreground">
            {first
              ? `${away ? `Leadline is running on ${here}, and nothing has reached it there yet.` : "Leadline is running, and nothing has reached it yet."} It can look for what you have${away ? " there" : ""}, you can add it yourself, or your coding agent can.`
              : `Leadline can look for projects and databases${away ? ` on ${here}` : ""} that aren't connected yet.`}
          </p>
        </div>

        <Surface render={<section aria-labelledby="look-intro" />} className="gap-3.5 px-4 py-4">
          <div>
            <h3 id="look-intro" className="font-medium">
              Look around {here}
            </h3>
            <p className="mt-0.5 text-pretty text-muted-foreground">Nothing has been looked at yet. When you press the button, Leadline lists the projects running on {here}, and you pick what to connect.</p>
          </div>
          <div className="grid grid-cols-1 gap-x-6 gap-y-3.5 @xl:grid-cols-2">
            <Points title="It looks at" points={LOOKS_AT} mark={<CheckIcon aria-hidden className="size-3.5 text-success-foreground" />} />
            <Points title="It does not" points={doesNot(here)} mark={<XIcon aria-hidden className="size-3.5 text-muted-foreground" />} />
          </div>
          <div className="flex flex-wrap items-center gap-2 pt-0.5">
            <Button onClick={onLook}>
              <ScanSearchIcon />
              Look around {here}
            </Button>
            <Button variant="outline" onClick={onByHand} className="font-normal">
              Add a source by hand
            </Button>
          </div>
        </Surface>

        <AgentPrompt>
          {(line) => (
            <Surface render={<section aria-label="Have your coding agent do it" />} className="gap-1.5 px-4 py-3.5">
              <h3 className="font-medium">Or have your coding agent do it</h3>
              <Small className="text-xs">Paste this into Claude Code, Codex or Cursor in your project{away ? ` on ${here}` : ""}. It wires the dev setup and checks that data arrives.</Small>
              {line}
            </Surface>
          )}
        </AgentPrompt>

        {first && (
          <Small className="px-1 text-xs">
            Or run <Code>leadline demo</Code>{away ? ` on ${here}` : ""} to see made-up data before any of your own.
          </Small>
        )}
      </div>
    </div>
  );
}

function Points({ title, points, mark }: { title: string; points: string[]; mark: ReactNode }) {
  return (
    <div className="min-w-0">
      <h4 className="pb-1.5 text-xs font-medium text-muted-foreground">{title}</h4>
      <ul className="flex flex-col gap-1.5">
        {points.map((p) => (
          <li key={p} className="grid grid-cols-[14px_minmax(0,1fr)] items-start gap-x-2.5">
            <span className="flex h-5 items-center">{mark}</span>
            <span className="text-pretty">{p}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

/** A card of the scan: its heading, a line about it, and something at the heading's end. */
function Group({ title, hint, end, foot, headRef, children }: { title: string; hint?: ReactNode; end?: ReactNode; foot?: ReactNode; headRef?: Ref<HTMLHeadingElement>; children: ReactNode }) {
  return (
    <Surface render={<section aria-label={title} />} className="overflow-hidden">
      <header className="flex min-h-[38px] items-center gap-2 px-3.5 py-1.5">
        <h3 ref={headRef} tabIndex={headRef ? -1 : undefined} className="shrink-0 text-xs font-medium outline-none">
          {title}
        </h3>
        {hint && <span className="min-w-0 flex-1 truncate text-[11px] text-muted-foreground">{hint}</span>}
        {end && <span className="ml-auto shrink-0">{end}</span>}
      </header>
      {children}
      {foot && <div className="border-t px-3.5 py-2.5">{foot}</div>}
    </Surface>
  );
}

const ITEM = "grid grid-cols-[18px_minmax(0,1fr)] items-start gap-x-3 gap-y-1 border-t px-3.5 py-2.5 @lg:grid-cols-[18px_minmax(0,1fr)_minmax(0,0.85fr)]";
const ROW = "grid grid-cols-[18px_minmax(0,1fr)] items-start gap-x-3 border-t px-3.5 py-2.5";
const NONE = "border-t px-3.5 py-3 text-xs text-pretty text-muted-foreground";

/** Tick is a row's first cell: a checkbox, or for something already connected a mark that it is. */
function Tick({ checked, onChange, label, done }: { checked: boolean; onChange?: (on: boolean) => void; label: string; done?: boolean }) {
  if (done) {
    return (
      <span className="flex h-5 items-center justify-center">
        <CheckIcon role="img" aria-label="Already added" className="size-3.5 text-success-foreground" />
      </span>
    );
  }
  if (!onChange) return <span aria-hidden />;
  return (
    <span className="flex h-5 items-center">
      <Checkbox checked={checked} onCheckedChange={(on) => onChange(on === true)} aria-label={label} />
    </span>
  );
}

function Tag({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <Badge variant="secondary" className={cn("font-normal text-muted-foreground", className)}>
      {children}
    </Badge>
  );
}

function Projects({ scan, picks, onPick }: { scan: Scan; picks: Picks; onPick: (id: string, on: boolean) => void }) {
  const ticked = scan.projects.filter((p) => !p.sends.already && picks.projects[p.id]).length;
  const sending = scan.projects.filter((p) => p.sends.already).length;
  const hint = scan.projects.length === 0 ? "" : [`${scan.projects.length} found`, ticked > 0 && `${ticked} ticked`, sending > 0 && `${sending} already sending`].filter(Boolean).join(", ");
  return (
    <Group title="Projects" hint={hint} foot={<Small>Leadline changes nothing in a project. Connect shows the exact steps for each ticked one, to take yourself or hand to your coding agent.</Small>}>
      {scan.projects.length === 0 && <p className={NONE}>No project was found behind what is running. Start a dev server and look again, or name the folder your projects are in.</p>}
      {scan.projects.map((p) => {
        const up = runningLine(p);
        const command = adviceCommand(p);
        return (
          <div key={p.id} className={ITEM}>
            <Tick checked={!!picks.projects[p.id]} onChange={(on) => onPick(p.id, on)} label={`Connect ${p.name}`} done={p.sends.already} />
            <div className="min-w-0">
              <p className="flex min-w-0 items-center gap-1.5">
                <span className="truncate font-medium">{p.name}</span>
                {p.label && <Tag>{p.label}</Tag>}
              </p>
              <Small one className="selectable">
                {whereProject(p)}
              </Small>
              <Small one className="flex items-center gap-1.5">
                {up ? (
                  <>
                    <Dot tone="ok" />
                    <span className="truncate">{up}</span>
                  </>
                ) : (
                  "Not running now"
                )}
              </Small>
            </div>
            <div className="col-start-2 min-w-0 @lg:col-start-3">
              <p className="text-pretty">{adviceTitle(p.connect)}</p>
              {p.connect.kind === "already" ? (
                <Small>{p.connect.summary}</Small>
              ) : (
                command && (
                  <Small one className="selectable font-mono text-[11px]">
                    {command}
                  </Small>
                )
              )}
            </div>
          </div>
        );
      })}
    </Group>
  );
}

const MANY_ROWS = 7;

type EnvFilesProps = {
  scan: Scan;
  picks: Picks;
  reading: Reading | null;
  faults: Record<string, Fault>;
  /** The machine looked around is a Mac, where the system can keep a folder from Leadline. */
  mac: boolean;
  onPick: (ids: string[], on: boolean) => void;
  onRead: () => void;
  onRetry: (id: string) => void;
};

/**
 * EnvFiles is where the person says which projects' env files Leadline
 * may open. The scan named the files and opened none; nothing here is
 * ticked to begin with; and the one button that reads says how many files
 * of how many projects a press will open.
 */
function EnvFiles({ scan, picks, reading, faults, mac, onPick, onRead, onRetry }: EnvFilesProps) {
  const id = useId();
  const { rows, without } = useMemo(() => envRows(scan), [scan]);
  const ask = rows.filter((r) => r.state === "ask");
  const asked = rows.length - ask.length;
  const label = readLabel(scan, picks);
  const all = ask.length > 0 && ask.every((r) => picks.env[r.project.id]);
  const now = reading ? reading.ids[reading.at] : "";
  const queued = (pid: string) => !!reading && reading.ids.indexOf(pid) > reading.at;
  const hint = rows.length === 0 ? "" : asked === 0 ? "named by the scan, not opened" : ask.length === 0 ? "all read" : `${asked} of ${rows.length} projects read`;

  return (
    <Group
      title="Env files"
      hint={hint}
      end={
        ask.length > 1 && (
          <Button variant="ghost" size="xs" disabled={!!reading} onClick={() => onPick(ask.map((r) => r.project.id), !all)} className="font-normal text-muted-foreground hover:text-foreground">
            {all ? "Untick all" : `Tick all ${ask.length}`}
          </Button>
        )
      }
      foot={
        rows.length === 0 ? undefined : ask.length === 0 ? (
          <Small>Every env file the scan named has been asked for. Looking again starts the list over, unread.</Small>
        ) : asked > 0 && !label && !reading ? (
          // Some are read and nothing more is ticked: the rule was said, and the button comes back with a tick.
          <Small>Leadline has opened the env files of the projects marked read, and of no other. Tick another to read its env files too.</Small>
        ) : (
          <div className="flex flex-col gap-2.5">
            <Small>
              <span id={`${id}-how`}>
                Leadline opens only the env files of the projects you tick. It takes the connection URLs in them (postgres://, redis:// and the like) and passes over every other value: counted, never kept.
              </span>
            </Small>
            <Button onClick={onRead} disabled={!label || !!reading} aria-describedby={`${id}-how`} className="h-auto min-h-8 self-start py-1.5 text-left whitespace-normal">
              {reading && <Spinner aria-hidden className="size-3.5" />}
              {reading ? `Reading${reading.ids.length > 1 ? ` ${reading.at + 1} of ${reading.ids.length}` : ""}…` : label || "Tick a project to read its env files"}
            </Button>
          </div>
        )
      }
    >
      {rows.length === 0 && (
        <p className={NONE}>None of these projects has an env file, so there is nothing for Leadline to read.</p>
      )}
      {/* A scan of a folder can find dozens of projects: past a few, the list scrolls inside the card, so what was found stays in reach below it. */}
      <div className={cn(rows.length > MANY_ROWS && "max-h-[292px] overflow-y-auto")}>
        {rows.map((r) => (
          <EnvLine
            key={r.project.id}
            cid={`${id}-${r.project.id}`}
            row={r}
            ticked={!!picks.env[r.project.id]}
            now={now === r.project.id}
            locked={!!reading}
            queued={queued(r.project.id)}
            fault={faults[r.project.id]}
            mac={mac}
            onPick={(on) => onPick([r.project.id], on)}
            onRetry={() => onRetry(r.project.id)}
          />
        ))}
      </div>
      {rows.length > 0 && without.length > 0 && <p className={NONE}>{withoutLine(without)}</p>}
    </Group>
  );
}

type EnvLineProps = { cid: string; row: EnvRow; ticked: boolean; now: boolean; locked: boolean; queued: boolean; fault?: Fault; mac: boolean; onPick: (on: boolean) => void; onRetry: () => void };

function EnvLine({ cid, row, ticked, now, locked, queued, fault, mac, onPick, onRetry }: EnvLineProps) {
  const p = row.project;
  const names = p.env_files.map((f) => f.file);
  return (
    <div className={ROW}>
      <span className="flex h-5 items-center justify-center">
        {now ? (
          <Spinner aria-label={`Reading ${p.name}'s env files`} className="size-3.5 text-muted-foreground" />
        ) : row.state === "ask" ? (
          <Checkbox id={cid} checked={ticked} disabled={locked} onCheckedChange={(on) => onPick(on === true)} aria-label={`Read ${p.name}'s env files: ${sentence(names)}`} />
        ) : row.state === "read" ? (
          <CheckIcon role="img" aria-label="Read" className="size-3.5 text-success-foreground" />
        ) : (
          <CircleAlertIcon role="img" aria-label="Not all read" className="size-3.5 text-warning-foreground" />
        )}
      </span>
      <div className="min-w-0">
        <div className="flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-1">
          <label htmlFor={row.state === "ask" ? cid : undefined} className="max-w-full truncate font-medium">
            {p.name}
          </label>
          {p.env_files.map((f) => (
            <Tag key={f.file} className={cn("selectable font-mono text-[11px]", row.state !== "ask" && !f.read && "text-warning-foreground")}>
              {f.file}
            </Tag>
          ))}
        </div>
        {row.state === "ask" && !fault && (now || queued) && <Small>{now ? "Reading…" : "Next"}</Small>}
        {row.line && <Small>{row.line}.</Small>}
        {row.unopened.length > 0 && (
          <Small className="text-warning-foreground">
            {sentence(row.unopened)} could not be opened, so {row.unopened.length === 1 ? "it was" : "they were"} not read.{mac && <span className="text-muted-foreground"> If macOS asked whether Leadline may reach this folder, it was told no.</span>}
          </Small>
        )}
        {fault && (
          <Small className="text-destructive-foreground">
            <span role="alert">Not read. {fault.message}</span> <span className="text-muted-foreground">{fault.hint}</span>{" "}
            {fault.code !== "no_project" && (
              <Inline onClick={onRetry} className="text-[11.5px] sm:text-[11.5px]">
                Try again
              </Inline>
            )}
          </Small>
        )}
      </div>
    </div>
  );
}

type DatabasesProps = {
  scan: Scan;
  /** Env files are being read now: more may be about to appear. */
  reading: boolean;
  here: string;
  picks: Picks;
  tested: Tested;
  hold: Held;
  headRef: Ref<HTMLHeadingElement>;
  onTest: () => void;
  onPick: (key: string, v: { on?: boolean; capture?: boolean }) => void;
  onByHand: (kind?: "postgres" | "redis") => void;
  /** Look again and read the same projects' env files: the way back from URLs that were let go. */
  onAgain: () => void;
};

function Databases({ scan, reading, here, picks, tested, hold, headRef, onTest, onPick, onByHand, onAgain }: DatabasesProps) {
  const id = useId();
  const gone = hold.state === "gone";
  const read = (scan.env_projects ?? []).length > 0;
  const local = scan.connections.filter((c) => c.connectable && !c.remote);
  const testing = Object.values(tested).some((t) => t.busy);
  const byHand = (
    <>
      <Inline onClick={() => onByHand("postgres")}>connect Postgres</Inline> or <Inline onClick={() => onByHand("redis")}>Redis</Inline> by its URL
    </>
  );
  return (
    <Group
      title="Databases and caches"
      headRef={headRef}
      hint={scan.connections.length > 0 ? "from the env files read" : ""}
      end={
        local.length > 0 &&
        !gone && (
          <Tip label={`Connects to the ${local.length === 1 ? "one" : local.length} on ${here}, asks each who it is, and saves nothing`}>
            <Button variant="outline" size="xs" onClick={onTest} loading={testing} className="font-normal">
              Check they answer
            </Button>
          </Tip>
        )
      }
      foot={
        read ? (
          <div className="flex flex-col gap-1.5">
            <Small>{envLine(scan.env, here)}</Small>
            {hold.state === "soon" && <Small className="text-warning-foreground">{hold.line}</Small>}
          </div>
        ) : undefined
      }
    >
      {gone && (
        <div role="alert" className="flex flex-col items-start gap-2 border-t bg-warning/6 px-3.5 py-3">
          <p className="text-pretty">{hold.line}</p>
          <Button size="sm" variant="outline" onClick={onAgain} className="h-auto min-h-7 py-1 text-left font-normal whitespace-normal">
            <RefreshCwIcon />
            {againLabel(scan)}
          </Button>
        </div>
      )}
      {scan.connections.length === 0 && (
        <p className={NONE}>
          {reading ? (
            "Reading env files…"
          ) : read ? (
            <>No connection URL was found in the env files read. A database is still one paste away: {byHand}.</>
          ) : scan.projects.some((p) => p.env_files.length > 0) ? (
            <>No env file has been opened, so Leadline knows of no database yet. Tick the projects whose env files it may read, under Env files. Or {byHand}.</>
          ) : (
            <>With no env file to read, Leadline knows of no database here. One is still a paste away: {byHand}.</>
          )}
        </p>
      )}
      {scan.connections.map((c) => {
        const key = connKey(c);
        const pick = picks.conns[key];
        const can = canPick(c) && !gone;
        const t = tested[key];
        const Glyph = c.kind === "redis" ? ZapIcon : DatabaseIcon;
        return (
          <div key={c.id} className={cn(ITEM, gone && !c.saved && "opacity-60")}>
            <Tick checked={!!pick?.on} onChange={can ? (on) => onPick(key, { on }) : undefined} label={`Connect ${connTitle(c)}`} done={c.saved} />
            <div className="min-w-0">
              <p className="flex min-w-0 items-center gap-1.5">
                <Glyph aria-hidden className="size-3.5 shrink-0 text-muted-foreground" />
                <span className="truncate font-medium">{connTitle(c)}</span>
                {/* A Redis is called Redis already. */}
                {connTitle(c) !== foundKind(c.kind) && <Tag>{foundKind(c.kind)}</Tag>}
                {c.remote && <Tag>remote</Tag>}
              </p>
              <Small one className="selectable font-mono text-[11px]">
                {foundLine(c)}
              </Small>
              <Small>{whereLine(c, here)}</Small>
              {t?.result?.ok && (
                <Small className="flex items-center gap-1.5 text-foreground">
                  <CheckIcon aria-hidden className="size-3 shrink-0 text-success-foreground" />
                  {kindVersion(c.kind, versionOf(t.result))} answers{t.result.latency_ms != null ? ` in ${fmtMs(t.result.latency_ms)}` : ""}.
                </Small>
              )}
              {t && !t.busy && !t.result?.ok && (t.error || t.result?.error) && (
                <Small className="text-destructive-foreground">
                  <span role="alert">{(t.error ?? t.result?.error)?.message}</span> <span className="text-muted-foreground">{(t.error ?? t.result?.error)?.hint}</span>
                </Small>
              )}
            </div>
            <div className="col-start-2 min-w-0 @lg:col-start-3">
              {can && c.capture_available ? (
                <div className="flex items-start gap-2">
                  <span className="flex h-5 items-center">
                    <Switch id={`${id}-${c.id}`} checked={!!pick?.capture} disabled={!pick?.on} onCheckedChange={(capture) => onPick(key, { capture })} />
                  </span>
                  <div className="min-w-0">
                    <label htmlFor={`${id}-${c.id}`}>{c.kind === "redis" ? "Capture every command" : "Capture every query"}</label>
                    <Small>{pick?.capture ? (c.kind === "redis" ? "With MONITOR, which can slow a busy Redis." : `Through a port Leadline opens on ${here}.`) : "Off: only its statistics are read."}</Small>
                  </div>
                </div>
              ) : (
                <>
                  <p className="text-pretty">{gone && canPick(c) ? "No longer held" : whatHappens(c, false)}</p>
                  <Small>{c.saved ? "It is in the sidebar already." : !c.connectable ? c.note : gone ? "Read its env file again to connect it." : c.remote ? "Read-only, and off until you tick it. A remote server can't be captured." : ""}</Small>
                </>
              )}
            </div>
          </div>
        );
      })}
    </Group>
  );
}

function Running({ scan }: { scan: Scan }) {
  const { rows, others } = useMemo(() => runningRows(scan), [scan]);
  return (
    <Group title="Running right now" hint={rows.length > 0 ? "dev servers and databases that are listening" : ""}>
      {rows.length === 0 && <p className={NONE}>Nothing that looks like a dev server or a database is listening.</p>}
      {rows.map((r) => (
        <div key={r.key} className="flex items-center gap-2.5 border-t px-3.5 py-2">
          <Dot tone="ok" />
          <p className="min-w-0 flex-1 truncate">{r.what}</p>
          <span className="max-w-[55%] shrink-0 truncate text-[11.5px] text-muted-foreground">{r.aside}</span>
        </div>
      ))}
      {(others > 0 || scan.limits.note) && (
        <div className="border-t px-3.5 py-2.5">
          <Small>
            {others > 0 && `${others} other ${others === 1 ? "port is" : "ports are"} open that ${others === 1 ? "isn't" : "aren't"} a dev server or a database. `}
            {scan.limits.note}
          </Small>
        </div>
      )}
    </Group>
  );
}

/** Foot marks the end of a finished scan's page, and says once it has come into view: by scrolling to it, or because the page was short enough to show it. */
function Foot({ onReach }: { onReach: () => void }) {
  const ref = useRef<HTMLDivElement>(null);
  const reach = useRef(onReach);
  reach.current = onReach;
  useEffect(() => {
    const el = ref.current;
    if (!el || typeof IntersectionObserver === "undefined") return;
    const seen = new IntersectionObserver((entries) => {
      if (entries.some((e) => e.isIntersecting)) reach.current();
    });
    seen.observe(el);
    return () => seen.disconnect();
  }, []);
  return <div ref={ref} aria-hidden className="h-px" />;
}

/** Folders is the way to have the scan look somewhere more: a path, typed, and what became of each one named. */
function Folders({ folders, scan, busy, onLook }: { folders: string[]; scan: Scan | undefined; busy: boolean; onLook: (folders: string[]) => void }) {
  const id = useId();
  const [path, setPath] = useState("");
  const full = folders.length >= MAX_FOLDERS;
  const add = (e: FormEvent) => {
    e.preventDefault();
    const p = path.trim();
    if (!p || full) return;
    setPath("");
    onLook(folders.includes(p) ? folders : [...folders, p]);
  };
  return (
    <Surface render={<section aria-label="Folders to look in" />} className="gap-2 px-3.5 py-2.5">
      <form onSubmit={add} className="flex flex-wrap items-center gap-x-2.5 gap-y-2">
        <label htmlFor={id} className="flex shrink-0 items-center gap-2">
          <FolderIcon aria-hidden className="size-3.5 text-muted-foreground" />
          Look in a folder too
        </label>
        <Input
          id={id}
          size="sm"
          value={path}
          onChange={(e) => setPath(e.target.value)}
          placeholder="~/code"
          spellCheck={false}
          autoComplete="off"
          autoCapitalize="off"
          autoCorrect="off"
          aria-describedby={`${id}-how`}
          className="min-w-40 flex-1 font-mono *:text-[12px]"
        />
        <Button type="submit" variant="outline" size="sm" disabled={!path.trim() || busy || full} className="shrink-0 font-normal">
          Look there
        </Button>
      </form>
      {folders.length > 0 && (
        <ul className="flex flex-col gap-1">
          {folders.map((f) => {
            const at = scan?.folders.find((x) => x.path === f);
            const said = at ? folderLine(at) : { text: busy ? "waiting" : "", bad: false };
            return (
              <li key={f} className="flex min-w-0 items-center gap-2 text-xs">
                <span className="selectable min-w-0 shrink truncate font-mono text-[11.5px]">{tailPath(at?.resolved ?? f, 3, 64)}</span>
                <span className={cn("min-w-0 flex-1 truncate text-[11.5px]", said.bad ? "text-warning-foreground" : "text-muted-foreground")}>{said.text}</span>
                <Tip label={`Stop looking in ${f}`}>
                  <Button variant="ghost" size="icon-xs" aria-label={`Stop looking in ${f}`} disabled={busy} onClick={() => onLook(folders.filter((x) => x !== f))} className="size-5 shrink-0 text-muted-foreground hover:text-foreground sm:size-5">
                    <XIcon className="size-3" />
                  </Button>
                </Tip>
              </li>
            );
          })}
        </ul>
      )}
      <Small>
        <span id={`${id}-how`}>
          Type the path of the folder your projects are in. Leadline looks up to three folders down for projects, reads their project files, and lists their env files by name without opening them. Your whole home folder is too wide: name a folder inside it.
        </span>
      </Small>
    </Surface>
  );
}

/** NothingFound is a scan that found no project: what to do next, not just that. */
function NothingFound({ here, listening, onAgain, onByHand }: { here: string; listening: boolean; onAgain: () => void; onByHand: Props["onByHand"] }) {
  const link = "text-[13px] sm:text-[13px]";
  return (
    <Surface render={<section aria-label="Nothing found" />} className="gap-2.5 px-4 py-4">
      <h3 className="font-medium">No project was found on {here}</h3>
      <Small className="text-xs">
        Leadline finds a project by its dev server listening, or in a folder you name. {listening ? "Something is listening, below, but no project was behind it." : "Nothing that looks like a dev server is listening now."}
      </Small>
      <ol className="flex list-decimal flex-col gap-1.5 pl-5 text-pretty marker:text-muted-foreground">
        <li>
          Start your app's dev server, then{" "}
          <Inline onClick={onAgain} className={link}>
            look again
          </Inline>
          .
        </li>
        <li>Or type the folder your projects are in, above, and Leadline looks there.</li>
        <li>
          Or connect one by hand:{" "}
          <Inline onClick={() => onByHand("app")} className={link}>
            your app
          </Inline>
          ,{" "}
          <Inline onClick={() => onByHand("postgres")} className={link}>
            a Postgres
          </Inline>{" "}
          or{" "}
          <Inline onClick={() => onByHand("redis")} className={link}>
            a Redis
          </Inline>{" "}
          by its URL.
        </li>
      </ol>
    </Surface>
  );
}

/** Problem is a scan that failed: why, in the daemon's words, and the two ways on. */
function Problem({ fault, onAgain, onByHand }: { fault: Fault; onAgain: () => void; onByHand: () => void }) {
  return (
    <Surface render={<section aria-label="The scan failed" />} className="items-start gap-2.5 px-4 py-4">
      <div role="alert" className="flex items-start gap-2">
        <CircleAlertIcon aria-hidden className="mt-0.5 size-3.5 shrink-0 text-destructive-foreground" />
        <div className="min-w-0">
          <p className="selectable text-destructive-foreground">{fault.message}</p>
          {fault.hint && <Small className="selectable mt-0.5">{fault.hint}</Small>}
        </div>
      </div>
      <div className="flex gap-2">
        <Button size="sm" onClick={onAgain}>
          Look again
        </Button>
        <Button variant="outline" size="sm" onClick={onByHand} className="font-normal">
          Add a source by hand
        </Button>
      </div>
    </Surface>
  );
}

/** Mark is a checklist line's state: heard from, still waiting, or failed. */
function Mark({ tone }: { tone: Heard["tone"] }) {
  if (tone === "done") {
    return (
      <span role="img" aria-label="Heard from" className="flex size-[22px] items-center justify-center rounded-full bg-success/16 text-success-foreground">
        <CheckIcon aria-hidden className="size-3.5" />
      </span>
    );
  }
  if (tone === "err") {
    return (
      <span role="img" aria-label="Failed" className="flex size-[22px] items-center justify-center rounded-full bg-destructive/12 text-destructive-foreground">
        <CircleAlertIcon aria-hidden className="size-3.5" />
      </span>
    );
  }
  return <span role="img" aria-label="Waiting" className="block size-[22px] animate-spin rounded-full border-2 border-muted-foreground/25 border-t-warn [animation-duration:1.2s] motion-reduce:animate-none" />;
}

function Line({ tone, icon, title, sub, hint, aside, children }: { tone: Heard["tone"]; icon?: ReactNode; title: ReactNode; sub?: ReactNode; hint?: ReactNode; aside?: ReactNode; children?: ReactNode }) {
  return (
    <li className="grid grid-cols-[22px_minmax(0,1fr)_auto] items-start gap-x-3 border-t px-3.5 py-3 first:border-t-0">
      <Mark tone={tone} />
      <div className="min-w-0">
        <p className="flex min-w-0 items-center gap-1.5">
          {icon}
          <span className="min-w-0 truncate">{title}</span>
        </p>
        {sub && <Small className={cn("selectable", tone === "err" && "text-destructive-foreground")}>{sub}</Small>}
        {hint && <Small className="selectable">{hint}</Small>}
        {children}
      </div>
      <span className="pt-0.5 text-[11.5px] whitespace-nowrap text-muted-foreground tabular-nums">{aside}</span>
    </li>
  );
}

/**
 * Connecting is the checklist after Connect: one line for the daemon, one
 * for each connection saved or refused, one for each project the person
 * ticked. A line turns to a tick only when the daemon has heard from that
 * source, which the page learns by asking it every second.
 */
function Connecting({ state, first, here, onBack, onDone }: { state: SavedState; first: boolean; here: string; onBack: () => void; onDone: () => void }) {
  // Where the daemon answers: on its own machine, and for another machine the port on this one it is read through.
  const daemon = ownBase().replace("http://", "");
  const answers = showingLocal() ? `Its daemon answers at ${daemon}. Nothing it holds leaves ${here}.` : `Its daemon answers at ${daemon} there, and this window reads it through port ${base().split(":").pop()} on this Mac. What it holds is kept on ${here}.`;
  const sources = usePolled(() => api.sources(), true);
  const runs = usePolled(() => api.runs(), state.projects.length > 0);

  const conns = state.saved.map((was) => sources?.connections.find((c) => c.id === was.id) ?? was);
  const heardConns = conns.map((c) => ({ c, heard: heardConn(c) }));
  const heardProjects = state.projects.map((p) => ({ p, heard: heardProject(p, sources, runs, state.at) }));
  const all = [...heardConns.map((x) => x.heard), ...heardProjects.map((x) => x.heard)];
  const heardCount = all.filter((h) => h.tone === "done").length;
  const waiting = all.length - heardCount + state.failed.length;
  // The button that led here is gone: the keyboard starts at the list it made.
  const head = useRef<HTMLHeadingElement>(null);
  useEffect(() => head.current?.focus(), []);

  return (
    <div className="flex h-full flex-col">
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-[760px] flex-col gap-3 p-4">
          <div className="px-1">
            <h2 ref={head} tabIndex={-1} className="text-[15px] font-medium outline-none">
              Connecting
            </h2>
            <p className="mt-0.5 text-pretty text-muted-foreground">Each line turns to a tick when Leadline has really heard from it, not when a setting was written.</p>
            <p role="status" className="sr-only">
              {heardCount} of {all.length} heard from.
            </p>
          </div>

          <Surface render={<section aria-label="What is connected" />} className="overflow-hidden">
            <ul>
              <Line tone={sources ? "done" : "wait"} title={`Leadline is running on ${here}`} sub={sources ? answers : "Asking its daemon…"} />
              {heardConns.map(({ c, heard }) => (
                <Line
                  key={c.id}
                  tone={heard.tone}
                  icon={c.kind === "redis" ? <ZapIcon aria-hidden className="size-3.5 shrink-0 text-muted-foreground" /> : <DatabaseIcon aria-hidden className="size-3.5 shrink-0 text-muted-foreground" />}
                  title={
                    <>
                      {c.kind === "redis" ? "Redis" : "Postgres"} <span className="text-muted-foreground">{c.name}</span>
                    </>
                  }
                  sub={heard.line}
                  hint={heard.hint}
                  aside={heard.aside}
                >
                  {c.proxy?.state === "listening" && c.proxy.statements === 0 && c.proxy.url && (
                    <div className="mt-1.5 flex flex-col gap-1">
                      <Small>An app started with leadline run goes through the port by itself. For one you start yourself, put this in its env file, with your password in place of ****: Leadline never shows it.</Small>
                      <CopyLine text={`DATABASE_URL=${c.proxy.url}`} label={`Copy the line for ${c.name}'s .env`} wrap />
                    </div>
                  )}
                </Line>
              ))}
              {state.failed.map(({ conn, error }) => (
                <Line
                  key={conn.id}
                  tone="err"
                  icon={conn.kind === "redis" ? <ZapIcon aria-hidden className="size-3.5 shrink-0 text-muted-foreground" /> : <DatabaseIcon aria-hidden className="size-3.5 shrink-0 text-muted-foreground" />}
                  title={
                    <>
                      {foundKind(conn.kind)} <span className="text-muted-foreground">{connTitle(conn)}</span>
                    </>
                  }
                  sub={`It wasn't saved. ${error.message}`}
                  hint={error.code === "expired" || error.code === "not_found" ? "Go back and read its project's env files again: then it can be ticked once more." : error.hint}
                />
              ))}
              {heardProjects.map(({ p, heard }) => {
                const todo = heard.tone === "done" ? [] : steps(p);
                const up = runningLine(p);
                return (
                  <Line
                    key={p.id}
                    tone={heard.tone}
                    icon={<BoxIcon aria-hidden className="size-3.5 shrink-0 text-muted-foreground" />}
                    title={
                      <>
                        {p.name} {p.git?.branch && <span className="text-muted-foreground">{p.git.branch}</span>}
                      </>
                    }
                    sub={
                      heard.line ||
                      (up
                        ? `Its dev server is up (${up}) and isn't reporting. It will after ${todo.length === 1 ? "this step" : p.connect.kind === "otel-env" ? "one of these steps" : "these steps"}${p.connect.kind === "run-wrap" ? "" : " and a restart"}.`
                        : `Not running now. It will report the next time it starts, once ${todo.length === 1 ? "this is" : p.connect.kind === "otel-env" ? "one of these is" : "these are"} done.`)
                    }
                    aside={heard.aside}
                  >
                    {todo.length > 0 && (
                      <ol className="mt-1.5 flex flex-col gap-1.5">
                        {todo.map((s) => (
                          <li key={s.label} className="flex flex-col gap-1">
                            <Small>{s.label}</Small>
                            <CopyLine text={s.text} label={`${s.copy} for ${p.name}`} wrap={s.text.includes("\n")} />
                          </li>
                        ))}
                        {up && p.connect.kind !== "run-wrap" && (
                          <li>
                            <Small>Then stop its dev server and start it again.</Small>
                          </li>
                        )}
                      </ol>
                    )}
                  </Line>
                );
              })}
            </ul>
          </Surface>

          {heardProjects.some((x) => x.heard.tone !== "done") && (
            <AgentPrompt>
              {(line) => (
                <Surface render={<section aria-label="Have your coding agent do it" />} className="gap-1.5 px-3.5 py-3">
                  <h3 className="text-xs font-medium">Or have your coding agent do it</h3>
                  <Small>Paste this into Claude Code, Codex or Cursor in the project{showingLocal() ? "" : ` on ${here}`}. It takes the steps above and checks that data arrives.</Small>
                  {line}
                </Surface>
              )}
            </AgentPrompt>
          )}

          <Small className="rounded-lg bg-muted px-3.5 py-2.5">
            {waiting > 0
              ? "You don't have to wait here. Anything still pending shows in the sidebar when it arrives, and under Sources until then."
              : "Everything ticked here has been heard from."}
          </Small>
        </div>
      </div>

      <div className="flex shrink-0 items-center justify-end gap-2 border-t px-4 py-2.5">
        <Button variant="outline" onClick={onBack} className="font-normal">
          Back
        </Button>
        <Button onClick={onDone}>{first ? "Open the overview" : "Done"}</Button>
      </div>
    </div>
  );
}
