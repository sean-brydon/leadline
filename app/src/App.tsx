import { useCallback, useEffect, useMemo, useReducer, useRef, useState, type ReactNode } from "react";
import { getCurrentWindow } from "@tauri-apps/api/window";
import { PlusIcon, ScanSearchIcon, XIcon } from "lucide-react";
import { api, type Box, type EndpointKey, type QueryKey, type Scope, type Sources as SourcesData } from "@/lib/api";
import { boxesOf } from "@/lib/discover";
import { startDaemon, useDaemon, type DaemonStatus } from "@/lib/daemon";
import { kept as keptOf } from "@/lib/kept";
import { useConnected, useEvery, useLive, useMachine } from "@/lib/live";
import { LOCAL_ID, machines, type Machine } from "@/lib/machines";
import { ALL, sameSource, scopeNote, scopeOf, sourceKey, sourceName, type Source } from "@/lib/scope";
import { captured, model as modelOf, ratePerMin, sampled, tally, type Model, type Sample } from "@/lib/sources";
import { cn } from "@/lib/utils";
import { AddSource, FirstRun } from "@/components/AddSource";
import { LookAround, useLook } from "@/components/LookAround";
import { PAST_LIGHTS_RAIL, Sidebar, STRIP, type AddKind, type Boxes, type Page } from "@/components/Sidebar";
import { Code, Notice, PickOne, Tip } from "@/components/ui";
import { Button } from "@/components/ui/button";
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty";
import { Spinner } from "@/components/ui/spinner";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Overview } from "@/views/Overview";
import { SendProvider } from "@/components/SendToShipyard";
import { Endpoints } from "@/views/Endpoints";
import { Traces } from "@/views/Traces";
import { Queries } from "@/views/Queries";
import { Logs } from "@/views/Logs";
import { Metrics } from "@/views/Metrics";
import { Sources } from "@/views/Sources";

const RANGES = [
  { label: "5m", ms: 5 * 60_000 },
  { label: "15m", ms: 15 * 60_000 },
  { label: "1h", ms: 60 * 60_000 },
  { label: "6h", ms: 6 * 60 * 60_000 },
  { label: "24h", ms: 24 * 60 * 60_000 },
];
const RANGE_OPTIONS = RANGES.map((r) => ({ value: String(r.ms), label: r.label, className: "tabular-nums" }));

// Below this the window is too narrow for the sidebar and a view side by
// side, so the sidebar folds to a rail by itself. The window's own minimum
// is 900 (src-tauri/tauri.conf.json).
const NARROW = "(max-width: 1079px)";

// What is kept on disk moves slowly: asked this often, and not with every span that arrives.
const STORAGE_EVERY_MS = 15_000;

const inTauri = "__TAURI_INTERNALS__" in window;

/** useLights says whether the window's buttons are over the page now: in the app, and not in full screen, where macOS puts them away. */
function useLights(): boolean {
  const [full, setFull] = useState(false);
  useEffect(() => {
    if (!inTauri) return;
    let alive = true;
    const win = getCurrentWindow();
    const ask = () => win.isFullscreen().then((f) => alive && setFull(f), () => {});
    ask();
    // Entering and leaving full screen both resize the window.
    const stop = win.onResized(ask);
    return () => {
      alive = false;
      stop.then((off) => off(), () => {});
    };
  }, []);
  return inTauri && !full;
}

function useMedia(query: string): boolean {
  const [on, setOn] = useState(() => matchMedia(query).matches);
  useEffect(() => {
    const mq = matchMedia(query);
    const change = () => setOn(mq.matches);
    change();
    mq.addEventListener("change", change);
    return () => mq.removeEventListener("change", change);
  }, [query]);
  return on;
}

export default function App() {
  const machine = useMachine();
  return (
    <TooltipProvider delay={300}>
      {/* Another machine is another window's worth of state: which trace is open, what is filtered. It all starts over. */}
      <Shell key={machine.id} machine={machine} />
    </TooltipProvider>
  );
}

function Shell({ machine }: { machine: Machine }) {
  const [page, setPage] = useState<Page>("Overview");
  const [rangeMs, setRangeMs] = useState(RANGES[1].ms);
  const [source, setSource] = useState<Source>(ALL);
  const connected = useConnected();
  const daemon = useDaemon();
  const lights = useLights();

  // The sidebar is a rail when the window is narrow, unless the person says
  // otherwise; their word lasts until the window crosses the line again.
  const narrow = useMedia(NARROW);
  const [fold, setFold] = useState<boolean | null>(null);
  useEffect(() => setFold(null), [narrow]);
  const rail = fold ?? narrow;

  // Everything feeding Leadline. `again` asks at once, after a change made here.
  const [asked, again] = useReducer((n: number) => n + 1, 0);
  // The captures' own counts ("38 statements") are of the window the top bar picks.
  const { data: sources, error: sourcesError } = useLive(() => api.sources(Date.now() - rangeMs), [asked, rangeMs]);
  const { data: runs } = useLive(() => api.runs(), [asked]);
  const { data: summary } = useLive(() => api.summary(), []);
  const { data: storage } = useEvery(() => api.storage(), STORAGE_EVERY_MS, []);
  const model = useMemo(() => (sources ? modelOf(sources, runs) : undefined), [sources, runs]);
  const rates = useRates(sources);

  // The window ends now, whenever a view asks: a function, not a value.
  const key = sourceKey(source);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const scope = useCallback((): Scope => scopeOf(source, Date.now() - rangeMs), [rangeMs, key]);
  const conn = source.kind === "db" ? sources?.connections.find((c) => c.id === source.id) : undefined;
  // A connection that was removed is no longer something to narrow to.
  useEffect(() => {
    if (source.kind === "db" && sources && !conn) setSource(ALL);
  }, [source, sources, conn]);

  // The trace, the endpoint and the query whose detail is open, kept here so one view can open another's.
  const [trace, setTrace] = useState<string | null>(null);
  const [endpoint, setEndpoint] = useState<EndpointKey | null>(null);
  const [query, setQuery] = useState<QueryKey | null>(null);
  // On the Sources page: the connection whose stats are open.
  const [stats, setStats] = useState<string | null>(null);
  const [adding, setAdding] = useState<{ open: boolean; kind: AddKind }>({ open: false, kind: "app" });
  // Looking around: the scan of this machine, which a first run offers and
  // "Look for more" starts later. Only this Mac's daemon is asked to: a
  // daemon on another machine would look around that one. Nothing is asked
  // of it here: a scan starts when a button is pressed (scanner.start), and
  // Shipyard is asked for its boxes when a person opens the machine
  // switcher or reaches the foot of a scan (shipyard.ask).
  const local = machine.id === LOCAL_ID;
  const scanner = useLook(local && connected);
  const canScan = local && connected && !scanner.old;
  const shipyard = useBoxes(local && connected);
  const [looking, setLooking] = useState(false);
  // On a first run: the person chose to add sources by hand instead, starting at this kind; or chose to go on to the views before anything arrived.
  const [byHand, setByHand] = useState<AddKind | null>(null);
  const [moved, setMoved] = useState(false);

  // Nothing has ever reached this daemon: the window opens on how to connect
  // something. Once shown, that stays up while the first data arrives (the
  // line waiting for it turns to a tick) until the person moves on.
  const nothing = sources !== undefined && sources.services.length === 0 && sources.connections.length === 0 && !(runs ?? []).some((r) => r.running);
  const [welcome, setWelcome] = useState(false);
  useEffect(() => {
    if (nothing) setWelcome(true);
  }, [nothing]);

  const go = (p: Page) => {
    if (!nothing) setWelcome(false);
    setLooking(false);
    setPage(p);
  };
  const openTrace = (id: string) => {
    setTrace(id);
    go("Traces");
  };
  const openEndpoint = (e: EndpointKey) => {
    setEndpoint(e);
    go("Endpoints");
  };
  const openQuery = (q: QueryKey) => {
    setQuery(q);
    go("Queries");
  };
  const openStats = (id: string) => {
    setAdding((a) => ({ ...a, open: false }));
    setStats(id);
    go("Sources");
  };
  const pick = (s: Source) => {
    // An endpoint's or a query's detail is of the source it was opened under: another source starts from its own list.
    if (!sameSource(s, source)) {
      setEndpoint(null);
      setQuery(null);
    }
    setSource(s);
    // The Sources page lists every source whatever is picked: show what the pick did.
    if (page === "Sources") go("Overview");
    else if (!nothing) setWelcome(false);
  };
  const add = (kind?: AddKind) => setAdding({ open: true, kind: kind ?? "app" });
  // A press of "Look for more" or "Look around this Mac": the press is what starts the scan.
  const look = () => {
    setAdding((a) => ({ ...a, open: false }));
    setByHand(null);
    // After a first run the scan is the Sources page's: that is where it leaves to.
    if (!(welcome || nothing)) setPage("Sources");
    setLooking(true);
    scanner.start();
  };

  const view = page === "Sources" ? null : page;
  const down = !connected && (sources === undefined || (machine.id === LOCAL_ID && daemon?.state === "failed"));
  const first = (welcome || nothing) && !moved && view !== null;
  // Looking around takes the whole view: on a first run, unless the person chose to add by hand, and later when asked for.
  const scanning = !down && canScan && ((first && byHand === null) || looking);
  // Leaving it has the daemon drop what a scan found: the connection URLs it
  // read are held in its memory until then (or for half an hour), and
  // nothing on the page needs them after. A screen that never scanned asks
  // the daemon nothing on the way out.
  const { leave } = scanner;
  const wasScanning = useRef(false);
  useEffect(() => {
    if (wasScanning.current && !scanning) leave();
    wasScanning.current = scanning;
  }, [scanning, leave]);
  // A press found the daemon too old to look around project by project: nothing was scanned, and adding by hand is the way on.
  useEffect(() => {
    if (!scanner.old || !looking) return;
    setLooking(false);
    // A first run has the by-hand panel in the page, which says why; anywhere else the dialog does.
    if (!first) setAdding((a) => ({ ...a, open: true }));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [scanner.old]);
  const note = view && !down && !first && !scanning ? scopeNote(view, source) : null;
  const kept = keptOf(machine.id === LOCAL_ID ? "this Mac" : machine.name, summary, storage, sources?.now_ms ?? Date.now());

  return (
    <div className="flex h-full">
      <Sidebar page={page} onPage={go} source={source} onSource={pick} model={down ? undefined : model} rates={rates} onAdd={add} rail={rail} onFold={() => setFold(!rail)} lights={lights} kept={kept.short} boxes={shipyard} />
      <div className="flex min-w-0 flex-1 flex-col">
        {/* "deep": a press anywhere in the bar that isn't on a control drags the window. */}
        <header data-tauri-drag-region="deep" className={cn("flex shrink-0 items-center gap-2.5 border-b pr-3", STRIP, rail && lights ? PAST_LIGHTS_RAIL : "pl-3.5")}>
          <h1 className="shrink-0 font-medium">{page}</h1>
          {scanning ? (
            <span className="truncate text-muted-foreground">{scanner.started ? "Looking around" : "Nothing connected yet"}</span>
          ) : page === "Sources" ? (
            <span className="truncate text-muted-foreground">{model ? tally(model) : ""}</span>
          ) : sameSource(source, ALL) ? (
            <span className="truncate text-muted-foreground">All sources</span>
          ) : (
            <Narrowed source={source} onClear={() => setSource(ALL)} />
          )}
          <div className="ml-auto flex min-w-0 shrink-0 items-center gap-2 pl-2">
            {page === "Sources" && !scanning && canScan && (
              <Tip label="Look at what is running on this Mac, and in folders you name, for more to connect. It opens no env file until you tick its project.">
                <Button variant="outline" size="sm" onClick={look} className="font-normal">
                  <ScanSearchIcon />
                  {/* At the window's narrowest the icon says it alone. */}
                  <span className="max-[1000px]:sr-only">Look for more</span>
                </Button>
              </Tip>
            )}
            {page === "Sources" && !scanning && (
              <Button size="sm" onClick={() => add()}>
                <PlusIcon />
                Add source
              </Button>
            )}
            <PickOne label="Time range" value={String(rangeMs)} options={RANGE_OPTIONS} onChange={(v) => setRangeMs(Number(v))} />
            <Tip label={connected ? `Showing what reaches the daemon at ${machine.url}, as it arrives.` : `No answer from the daemon at ${machine.url}.`}>
              <span role="status" className="flex shrink-0 items-center gap-1.5 pl-1 text-[11px] text-muted-foreground">
                <span className={cn("size-1.5 rounded-full", connected ? "bg-ok" : "bg-err")} />
                {/* At the window's narrowest the dot says it alone. */}
                <span className="max-[960px]:sr-only">{connected ? "Live" : "Offline"}</span>
              </span>
            </Tip>
          </div>
        </header>
        {note && <Notice>{note}</Notice>}
        <main className="min-h-0 flex-1">
          {/* The views' "Send to Shipyard" opens one dialog, held here. */}
          <SendProvider scope={scope}>
            {down ? (
              <NoDaemon daemon={machine.id === LOCAL_ID ? daemon : null} machine={machine} />
            ) : scanning ? (
              <LookAround
                look={scanner}
                first={first}
                here="this Mac"
                boxes={shipyard.boxes}
                onFoot={shipyard.ask}
                onByHand={(kind) => {
                  // A first run has the by-hand panel in the page; later it is the dialog.
                  if (first) setByHand(kind ?? "app");
                  else add(kind);
                  setLooking(false);
                }}
                onDone={() => {
                  setLooking(false);
                  if (first) {
                    // "You don't have to wait here": the views open even if nothing has arrived yet.
                    setMoved(true);
                    setWelcome(false);
                    setPage("Overview");
                  }
                }}
                onChanged={again}
              />
            ) : page === "Sources" ? (
              <Sources
                sources={sources}
                error={sourcesError}
                model={model ?? EMPTY}
                rangeMs={rangeMs}
                open={stats}
                onOpen={setStats}
                onAdd={add}
                onSource={pick}
                onChanged={again}
                kept={kept}
              />
            ) : first ? (
              <FirstRun kind={byHand ?? "app"} arrived={!nothing} onGo={() => go("Overview")} onChanged={again} onStats={openStats} onLook={canScan ? look : undefined} lookNote={local && scanner.old ? TOO_OLD : undefined} />
            ) : page === "Overview" ? (
              <Overview scope={scope} rangeMs={rangeMs} conn={conn} onEndpoint={openEndpoint} onQuery={openQuery} onTrace={openTrace} onLogs={() => go("Logs")} onStats={openStats} />
            ) : page === "Endpoints" ? (
              <Endpoints scope={scope} rangeMs={rangeMs} selected={endpoint} onSelect={setEndpoint} onTrace={openTrace} onQuery={openQuery} />
            ) : page === "Traces" ? (
              <Traces scope={scope} rangeMs={rangeMs} selected={trace} onSelect={setTrace} />
            ) : page === "Queries" ? (
              <Queries scope={scope} rangeMs={rangeMs} selected={query} onSelect={setQuery} onTrace={openTrace} onEndpoint={openEndpoint} />
            ) : page === "Logs" ? (
              <Logs scope={scope} rangeMs={rangeMs} onTrace={openTrace} />
            ) : (
              <Metrics scope={scope} rangeMs={rangeMs} />
            )}
          </SendProvider>
        </main>
      </div>
      <AddSource open={adding.open} kind={adding.kind} onClose={() => setAdding((a) => ({ ...a, open: false }))} onChanged={again} onStats={openStats} onLook={canScan && !scanning ? look : undefined} lookNote={local && scanner.old ? TOO_OLD : undefined} />
    </div>
  );
}

const EMPTY: Model = { apps: [], databases: [], caches: [] };

// What a person reads after pressing "Look around" on a daemon whose scan
// is the older kind, which read every project's env files in one go.
const TOO_OLD = "This daemon is from an older Leadline, which can't look around one project at a time, so nothing was scanned. Add what you have here, or update Leadline.";

// Shipyard's boxes are not asked for again within this long: asking runs Shipyard's own command.
const BOXES_FOR_MS = 30_000;
// Whether this Mac's Shipyard had boxes the last time it was asked: a word, so the machine switcher can be there to open without asking again.
const BOXES_KEY = "leadline.shipyard.boxes";

function hadBoxes(): boolean {
  try {
    return localStorage.getItem(BOXES_KEY) === "1";
  } catch {
    return false;
  }
}

/**
 * useBoxes is the owner's Shipyard boxes, asked for lazily. Asking has the
 * daemon run Shipyard's command line, and any `berth` command starts
 * Shipyard's own background agent if it is installed and not running. A
 * window opening must not do that, so nothing here asks on mount or on a
 * timer: `ask` is the only way, and it is called from a person's act.
 * None when Shipyard isn't installed, has no boxes, or the daemon can't ask.
 */
function useBoxes(on: boolean): Boxes {
  const [state, setState] = useState<{ boxes: Box[]; asking: boolean; asked: boolean; known: boolean }>(() => ({ boxes: NO_BOXES, asking: false, asked: false, known: hadBoxes() }));
  const last = useRef(0);
  const live = useRef(true);
  useEffect(() => {
    live.current = true;
    return () => {
      live.current = false;
    };
  }, []);
  const ask = useCallback(() => {
    if (!on || Date.now() - last.current < BOXES_FOR_MS) return;
    last.current = Date.now();
    setState((s) => ({ ...s, asking: true }));
    api.shipyard().then(
      (s) => {
        const boxes = boxesOf(s);
        try {
          if (boxes.length > 0) localStorage.setItem(BOXES_KEY, "1");
          else localStorage.removeItem(BOXES_KEY);
        } catch {
          // A forbidden store forgets that there were boxes, and the switcher is not drawn for them at the next launch.
        }
        // `known` stays as it was for this window: a switcher that is open does not vanish under the person.
        if (live.current) setState((was) => ({ boxes, asking: false, asked: true, known: was.known || boxes.length > 0 }));
      },
      () => {
        last.current = 0;
        if (live.current) setState((was) => ({ ...was, asking: false }));
      },
    );
  }, [on]);
  return useMemo(() => (on ? { ...state, ask } : { boxes: NO_BOXES, known: false, asking: false, asked: false, ask }), [on, state, ask]);
}
const NO_BOXES: Box[] = [];

/** Narrowed names the source the window is narrowed to, with the way back to all of them. */
function Narrowed({ source, onClear }: { source: Source; onClear: () => void }) {
  return (
    <span className="flex h-6 min-w-0 items-center gap-0.5 rounded-md bg-muted pr-0.5 pl-2 text-xs">
      <span className="truncate">{sourceName(source)}</span>
      <Tip label="Show all sources">
        <Button variant="ghost" size="icon-xs" aria-label="Show all sources" onClick={onClear} className="size-5 shrink-0 text-muted-foreground hover:text-foreground sm:size-5">
          <XIcon className="size-3" />
        </Button>
      </Tip>
    </span>
  );
}

/**
 * useRates is a count per minute for each source, by sourceKey. An app's is
 * its requests in the last minute, asked of the daemon. A captured
 * database's is how fast its capture's counter is rising, from one fetch of
 * the sources to the next. A database that isn't captured has none.
 */
function useRates(sources: SourcesData | undefined): Record<string, number | null> {
  const perApp = useLive(() => api.breakdown(scopeOf(ALL, Date.now() - 60_000), "service", 1, 20), []);
  const readings = useRef(new Map<string, Sample[]>());
  return useMemo(() => {
    const out: Record<string, number | null> = {};
    for (const g of perApp.data?.groups ?? []) out[sourceKey({ kind: "app", service: g.key })] = g.total;
    if (sources) {
      const seen = new Set<string>();
      for (const c of sources.connections) {
        const count = captured(c);
        if (count === null) continue;
        seen.add(c.id);
        const next = sampled(readings.current.get(c.id) ?? [], sources.now_ms, count);
        readings.current.set(c.id, next);
        out[`db:${c.id}`] = ratePerMin(next);
      }
      for (const id of readings.current.keys()) if (!seen.has(id)) readings.current.delete(id);
    }
    return out;
  }, [perApp.data, sources]);
}

// NoDaemon is the window with no daemon to show. The desktop app finds or
// starts this Mac's daemon itself (lib/daemon.ts), so there this is a moment
// at launch, or a failure with its reason; a browser tab has to be told. A
// daemon on another machine is not the app's to start.
function NoDaemon({ daemon, machine }: { daemon: DaemonStatus | null; machine: Machine }) {
  if (machine.id !== LOCAL_ID) {
    return (
      <Setup
        title={`No answer from ${machine.name}`}
        after={
          <Button size="sm" onClick={() => machines.use(LOCAL_ID)}>
            Back to this Mac
          </Button>
        }
      >
        Nothing answers at <Code>{machine.url}</Code>. If that is another machine's daemon, check that its port is still forwarded to this one.
      </Setup>
    );
  }
  if (!daemon) {
    return (
      <Setup title="Leadline's daemon isn't running">
        Start it with <Code>leadlined</Code>, and this page fills in by itself.
      </Setup>
    );
  }
  if (daemon.state !== "failed") {
    return (
      <Setup title="Starting Leadline…" media={<Spinner aria-hidden className="size-5 text-muted-foreground" />}>
        The daemon is starting. This window fills in by itself.
      </Setup>
    );
  }
  return (
    <Setup
      title={daemon.reason}
      after={
        <>
          {daemon.detail && (
            <pre className="selectable max-h-40 w-full overflow-auto rounded-lg border bg-card p-2.5 text-left text-[11px] whitespace-pre-wrap text-muted-foreground">
              {daemon.detail}
            </pre>
          )}
          <Button size="sm" onClick={startDaemon}>
            Start the daemon
          </Button>
          {daemon.log && (
            <p className="text-[11px] text-muted-foreground">
              Its log is <Code>{daemon.log}</Code>
            </p>
          )}
        </>
      }
    >
      {daemon.hint}
    </Setup>
  );
}

/** Setup is a whole view that has nothing to show yet: what is missing, what to do, and anything more after. */
function Setup({ title, children, media, after }: { title: string; children: ReactNode; media?: ReactNode; after?: ReactNode }) {
  return (
    <Empty className="h-full gap-4 p-8 md:py-8">
      <EmptyHeader className="max-w-md">
        {media && <EmptyMedia className="mb-3">{media}</EmptyMedia>}
        <EmptyTitle role="heading" aria-level={2} className="text-[15px] font-medium">
          {title}
        </EmptyTitle>
        <EmptyDescription>{children}</EmptyDescription>
      </EmptyHeader>
      {after && <EmptyContent className="max-w-2xl gap-3">{after}</EmptyContent>}
    </Empty>
  );
}
