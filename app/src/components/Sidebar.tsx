import type { ComponentType, KeyboardEvent, ReactNode } from "react";
import {
  ActivityIcon,
  BoxIcon,
  CableIcon,
  ChartGanttIcon,
  ChartNoAxesColumnIcon,
  ChevronsUpDownIcon,
  DatabaseIcon,
  GlobeIcon,
  ListIcon,
  MonitorIcon,
  PanelLeftIcon,
  PlusIcon,
  SquareTerminalIcon,
  TablePropertiesIcon,
  ZapIcon,
} from "lucide-react";
import type { Box } from "@/lib/api";
import { boxState } from "@/lib/discover";
import { fmtRate } from "@/lib/format";
import { machines as machineList } from "@/lib/machines";
import { useMachines } from "@/lib/live";
import { ALL, sameSource, sourceKey, type Source, type View } from "@/lib/scope";
import { byProject, ownerLine, total, type AppRow, type DbRow, type Entry, type Model, type Tone } from "@/lib/sources";
import { cn } from "@/lib/utils";
import { Dot, Tip } from "@/components/ui";
import { Button } from "@/components/ui/button";
import { Menu, MenuGroupLabel, MenuGroup, MenuPopup, MenuRadioGroup, MenuRadioItem, MenuSeparator, MenuTrigger } from "@/components/ui/menu";
import { Spinner } from "@/components/ui/spinner";

export type Page = View | "Sources";

/**
 * What the window knows of the owner's Shipyard boxes, and the way to ask.
 * Asking makes the daemon run Shipyard's command line, which can start
 * Shipyard's own background agent, so it happens when a person opens the
 * machine switcher or reaches the foot of a scan, and at no other time.
 */
export type Boxes = {
  boxes: Box[];
  /** Shipyard had boxes when it was last asked, in this window or an earlier one: the switcher is worth drawing. */
  known: boolean;
  asking: boolean;
  /** Shipyard has been asked in this window, and has answered. */
  asked: boolean;
  ask: () => void;
};
/** The kinds of source the Add a source dialog offers. */
export type AddKind = "app" | "postgres" | "redis" | "otel" | "run";

type Icon = ComponentType<{ className?: string; "aria-hidden"?: boolean }>;

export const VIEWS: { page: View; icon: Icon }[] = [
  { page: "Overview", icon: ChartNoAxesColumnIcon },
  { page: "Endpoints", icon: ListIcon },
  { page: "Traces", icon: ChartGanttIcon },
  { page: "Queries", icon: TablePropertiesIcon },
  { page: "Logs", icon: SquareTerminalIcon },
  { page: "Metrics", icon: ActivityIcon },
];

// The sidebar's top strip is the window's title bar in the desktop app
// (titleBarStyle "Overlay"): it drags the window, and what is in it starts
// to the right of the window's three buttons.
//
// The numbers go together. The strip is 44px of content over a 1px line, so
// its centre line is y=22. macOS (measured on 26.3) draws the buttons 14px
// across, 23px apart, their top 9px above trafficLightPosition.y: y=24 in
// src-tauri/tauri.conf.json puts them at 15..29, centred on 22 too. Its
// x=15 leaves them the same 15px from the window's left edge as from its
// top, and they end at 15+2*23+14 = 75, so the strip's content starts
// another 15px on, at 90. Change one of these and change the others.
//
// Folded to a rail the sidebar is narrower than the buttons are wide (52px
// against 75), so nothing of the rail's own sits in the strip, and the bar
// over the content starts 90-52 = 38px in instead (PAST_LIGHTS_RAIL).
export const STRIP = "h-[45px]";
export const WIDE = "w-[248px]";
export const RAIL = "w-[52px]";
export const PAST_LIGHTS = "pl-[90px]";
export const PAST_LIGHTS_RAIL = "pl-[38px]";

const TONE_WORD: Record<Tone, string> = { ok: "Sending", quiet: "Quiet", err: "Failing", wait: "Waiting" };

type Props = {
  page: Page;
  onPage: (p: Page) => void;
  source: Source;
  onSource: (s: Source) => void;
  /** Undefined until the daemon has said what its sources are. */
  model: Model | undefined;
  /** A count per minute for each source, by sourceKey; missing where there is none to give. */
  rates: Record<string, number | null | undefined>;
  onAdd: (kind?: AddKind) => void;
  /** Folded to a rail of icons. */
  rail: boolean;
  onFold: () => void;
  /** The window's own buttons are drawn over the top-left corner. */
  lights: boolean;
  /** What is kept on disk, for the foot of the sidebar. */
  kept: ReactNode;
  /** The owner's Shipyard boxes: listed with the machines, since Leadline can't run on one yet, and asked for when the list is opened. */
  boxes?: Boxes;
};

/**
 * Sidebar is the window's left edge: the views, then everything feeding
 * Leadline, each with its status. A source narrows every view to it. Up and
 * down arrows move along it; Tab does too.
 */
const NO_SHIPYARD: Boxes = { boxes: [], known: false, asking: false, asked: false, ask: () => {} };

export function Sidebar({ page, onPage, source, onSource, model, rates, onAdd, rail, onFold, lights, kept, boxes = NO_SHIPYARD }: Props) {
  const arrows = (e: KeyboardEvent<HTMLElement>) => {
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    const stops = [...e.currentTarget.querySelectorAll<HTMLElement>("[data-stop]")];
    const at = stops.indexOf(document.activeElement as HTMLElement);
    if (at < 0) return;
    e.preventDefault();
    stops[(at + (e.key === "ArrowDown" ? 1 : -1) + stops.length) % stops.length].focus();
  };
  const count = model ? total(model) : 0;

  return (
    // "deep": a press anywhere in the sidebar that isn't on a control drags the window.
    <aside data-tauri-drag-region="deep" onKeyDown={arrows} className={cn("flex h-full shrink-0 flex-col", rail ? RAIL : WIDE)}>
      {/* Under the window's buttons a rail's edge would cut between them, so there the strip is part of the bar beside it: its line runs along the bottom, not down the side. */}
      <div className={cn("flex shrink-0 items-center", STRIP, rail && lights ? "border-b" : "border-r", rail ? "justify-center" : cn("pr-2", lights ? PAST_LIGHTS : "pl-4"))}>
        {/* Beside the window's buttons there is no room in a rail: its fold button sits under them. */}
        {!rail && <span className="font-medium tracking-tight">Leadline</span>}
        {!(rail && lights) && <Fold rail={rail} onFold={onFold} className={rail ? "" : "ml-auto"} />}
      </div>
      <div className="flex min-h-0 flex-1 flex-col border-r">
        {rail && lights && (
          <div className="flex shrink-0 justify-center pt-2 pb-1">
            <Fold rail={rail} onFold={onFold} />
          </div>
        )}

        <Machines rail={rail} shipyard={boxes} />

        <nav aria-label="Views" className={cn("flex shrink-0 flex-col gap-px", rail ? "items-center px-0" : "px-2")}>
          {VIEWS.map(({ page: p, icon }) => (
            <Item key={p} icon={icon} label={p} on={page === p} rail={rail} onClick={() => onPage(p)} />
          ))}
        </nav>

        <nav aria-label="Sources" className={cn("mt-2 flex min-h-0 flex-1 flex-col border-t pt-2", rail ? "items-center" : "px-2")}>
          <div className={cn("flex shrink-0 items-center gap-1", rail && "flex-col")}>
            <Item icon={CableIcon} label="Sources" on={page === "Sources"} rail={rail} onClick={() => onPage("Sources")} className="min-w-0 flex-1" />
            <Tip label="Add a source" side={rail ? "right" : "top"}>
              <Button data-stop variant="ghost" size="icon-xs" aria-label="Add a source" onClick={() => onAdd()} className={cn("shrink-0 text-muted-foreground hover:text-foreground", rail && "size-8 sm:size-8")}>
                <PlusIcon />
              </Button>
            </Tip>
          </div>

          <div className={cn("mt-px flex min-h-0 flex-1 flex-col gap-px overflow-y-auto pb-2", rail && "w-full items-center [scrollbar-width:none]")}>
            <SourceRow
              rail={rail}
              on={sameSource(source, ALL)}
              onClick={() => onSource(ALL)}
              mark={<GlobeIcon aria-hidden className="size-3.5 text-muted-foreground" />}
              name="All sources"
              end={model ? <span className="text-[11.5px] text-muted-foreground tabular-nums">{count}</span> : <Spinner aria-hidden className="size-3 text-muted-foreground" />}
              tip={model ? `All sources: ${count}` : "All sources"}
              single
            />
            {model && (
              <>
                <Group label="Apps" rail={rail} count={model.apps.length}>
                  <Entries entries={byProject(model.apps, (a) => a.owner, (a) => a.service)} rail={rail} id={(a) => a.service}>
                    {(a, name) => <App key={a.service} row={a} name={name} rail={rail} on={sameSource(source, a.source)} rate={rates[sourceKey(a.source)]} onSource={onSource} />}
                  </Entries>
                  {model.apps.length === 0 && !rail && <Ghost onClick={() => onAdd("app")}>Add your app</Ghost>}
                </Group>
                <Group label="Databases" rail={rail} count={model.databases.length}>
                  <Entries entries={byProject(model.databases, (d) => d.conn.owner, (d) => d.conn.name)} rail={rail} id={(d) => d.conn.id}>
                    {(d, name) => <Db key={d.conn.id} row={d} name={name} rail={rail} on={sameSource(source, d.source)} rate={rates[sourceKey(d.source)]} onSource={onSource} />}
                  </Entries>
                  {model.databases.length === 0 && !rail && <Ghost onClick={() => onAdd("postgres")}>Connect Postgres</Ghost>}
                </Group>
                <Group label="Caches" rail={rail} count={model.caches.length}>
                  <Entries entries={byProject(model.caches, (d) => d.conn.owner, (d) => d.conn.name)} rail={rail} id={(d) => d.conn.id}>
                    {(d, name) => <Db key={d.conn.id} row={d} name={name} rail={rail} on={sameSource(source, d.source)} rate={rates[sourceKey(d.source)]} onSource={onSource} />}
                  </Entries>
                  {model.caches.length === 0 && !rail && <Ghost onClick={() => onAdd("redis")}>Connect Redis</Ghost>}
                </Group>
              </>
            )}
          </div>
        </nav>

        {!rail && (
          <div className="shrink-0 border-t px-2 pt-2 pb-2.5">
            <Button data-stop variant="outline" size="sm" onClick={() => onAdd()} className="w-full font-normal">
              <PlusIcon />
              Add source
            </Button>
            <p className="px-2 pt-2.5 text-[11.5px] leading-snug text-muted-foreground">{kept}</p>
          </div>
        )}
      </div>
    </aside>
  );
}

function Fold({ rail, onFold, className }: { rail: boolean; onFold: () => void; className?: string }) {
  const label = rail ? "Show the sidebar" : "Fold the sidebar";
  return (
    <Tip label={label} side={rail ? "right" : "bottom"}>
      <Button variant="ghost" size="icon-xs" aria-label={label} aria-expanded={!rail} onClick={onFold} className={cn("shrink-0 text-muted-foreground hover:text-foreground", className)}>
        <PanelLeftIcon />
      </Button>
    </Tip>
  );
}

const ROW =
  "cursor-default rounded-md text-left outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset data-on:bg-accent data-on:text-foreground";

/** Item is a view's row: its icon and name, or in a rail the icon with the name as its tooltip. */
function Item({ icon: Glyph, label, on, rail, onClick, className }: { icon: Icon; label: string; on: boolean; rail: boolean; onClick: () => void; className?: string }) {
  const button = (
    <button
      type="button"
      data-stop
      tabIndex={0}
      aria-current={on ? "page" : undefined}
      data-on={on ? "" : undefined}
      aria-label={rail ? label : undefined}
      onClick={onClick}
      className={cn(ROW, "flex h-[30px] items-center gap-2.5 text-muted-foreground", rail ? "size-8 justify-center" : "w-full px-2", className)}
    >
      <Glyph aria-hidden className="size-3.5 shrink-0" />
      {!rail && <span className="truncate">{label}</span>}
    </button>
  );
  return rail ? (
    <Tip label={label} side="right">
      {button}
    </Tip>
  ) : (
    button
  );
}

function Group({ label, rail, count, children }: { label: string; rail: boolean; count: number; children: ReactNode }) {
  // A rail has no room for the heading: a short line keeps the groups apart.
  if (rail) {
    if (count === 0) return null;
    return (
      <div role="group" aria-label={label} className="flex w-full flex-col items-center gap-px">
        <span aria-hidden className="my-1.5 h-px w-4 bg-border" />
        {children}
      </div>
    );
  }
  return (
    <div role="group" aria-label={label} className="flex flex-col gap-px">
      <div aria-hidden className="px-2 pt-3 pb-1 text-[11px] tracking-wide text-muted-foreground">
        {label}
      </div>
      {children}
    </div>
  );
}

/**
 * Entries draws a group's rows, with the worktrees of one project gathered
 * under the project's name and called by their own. In a rail there is no
 * room for the name: the rows stand as they are.
 */
function Entries<R>({ entries, rail, id, children }: { entries: Entry<R>[]; rail: boolean; id: (r: R) => string; children: (row: R, name?: string) => ReactNode }) {
  return (
    <>
      {entries.map((e) =>
        e.kind === "row" ? (
          children(e.row)
        ) : rail ? (
          e.rows.map((r) => children(r.row))
        ) : (
          <div key={`project:${e.project}:${id(e.rows[0].row)}`} role="group" aria-label={`${e.project}${e.shipyard ? ", from Shipyard" : ""}`} className="flex flex-col gap-px">
            <div aria-hidden className="flex items-baseline gap-1.5 px-2 pt-1.5 pb-0.5 text-[11.5px]">
              <span className="truncate text-foreground/80">{e.project}</span>
              <span className="shrink-0 text-muted-foreground">{e.shipyard ? "from Shipyard" : "worktrees"}</span>
            </div>
            <div className="ml-[14px] flex flex-col gap-px border-l pl-px">{e.rows.map((r) => children(r.row, r.name))}</div>
          </div>
        ),
      )}
    </>
  );
}

/** Ghost stands where a group has no rows: the way to add the first. */
function Ghost({ onClick, children }: { onClick: () => void; children: ReactNode }) {
  return (
    <button type="button" data-stop tabIndex={0} onClick={onClick} className={cn(ROW, "flex h-[30px] w-full items-center gap-2.5 px-2 text-muted-foreground/80 hover:text-foreground")}>
      <PlusIcon aria-hidden className="size-3.5 shrink-0" />
      <span className="truncate">{children}</span>
    </button>
  );
}

type RowProps = {
  rail: boolean;
  on: boolean;
  onClick: () => void;
  /** The 14px mark at the row's start: a status dot or an icon. */
  mark: ReactNode;
  name: string;
  /** The second line: how it is connected. */
  how?: ReactNode;
  /** At the row's end: a rate or a badge. */
  end?: ReactNode;
  /** Everything about the row in one line, for a rail's tooltip. */
  tip: string;
  railIcon?: Icon;
  railTone?: Tone;
  /** One line tall: "All sources". */
  single?: boolean;
};

function SourceRow({ rail, on, onClick, mark, name, how, end, tip, railIcon: Glyph = GlobeIcon, railTone, single }: RowProps) {
  if (rail) {
    return (
      <Tip label={tip} side="right">
        <button type="button" data-stop tabIndex={0} aria-current={on ? "true" : undefined} data-on={on ? "" : undefined} aria-label={tip} onClick={onClick} className={cn(ROW, "relative flex size-8 shrink-0 items-center justify-center text-muted-foreground")}>
          <Glyph aria-hidden className="size-3.5" />
          {railTone && <Dot tone={railTone} className="absolute top-1 right-1 ring-2 ring-background" />}
        </button>
      </Tip>
    );
  }
  return (
    <button
      type="button"
      data-stop
      tabIndex={0}
      aria-current={on ? "true" : undefined}
      data-on={on ? "" : undefined}
      onClick={onClick}
      className={cn(ROW, "grid w-full shrink-0 grid-cols-[14px_minmax(0,1fr)_auto] items-center gap-x-2.5 px-2", single ? "h-[30px]" : "min-h-[40px] py-1")}
    >
      <span className="flex justify-center">{mark}</span>
      <span className="min-w-0">
        <span className="block truncate leading-[1.35]">{name}</span>
        {how && <span className="block truncate text-[11.5px] leading-[1.35] text-muted-foreground">{how}</span>}
      </span>
      {end}
    </button>
  );
}

const rateText = (rate: number | null | undefined) => (rate == null ? "" : fmtRate(rate));

function App({ row, name, rail, on, rate, onSource }: { row: AppRow; /** What to call it under its project's name, when it is one of a project's worktrees. */ name?: string; rail: boolean; on: boolean; rate: number | null | undefined; onSource: (s: Source) => void }) {
  const per = row.tone === "quiet" ? "" : rateText(rate);
  const owner = ownerLine(row.owner);
  return (
    <SourceRow
      rail={rail}
      on={on}
      onClick={() => onSource(row.source)}
      mark={<Dot tone={row.tone} label={TONE_WORD[row.tone]} />}
      name={name ?? row.service}
      // Under its project's name the row already says whose it is.
      how={owner && !name ? `${row.how} · ${owner}` : row.how}
      end={per && <span className="text-[11.5px] text-muted-foreground tabular-nums">{per}</span>}
      tip={[row.service, row.how, owner, per && `${per.replace("/m", "")} requests a minute`].filter(Boolean).join(" · ")}
      railIcon={BoxIcon}
      railTone={row.tone}
    />
  );
}

function Db({ row, name, rail, on, rate, onSource }: { row: DbRow; name?: string; rail: boolean; on: boolean; rate: number | null | undefined; onSource: (s: Source) => void }) {
  const c = row.conn;
  const per = rateText(rate);
  const owner = ownerLine(c.owner);
  return (
    <SourceRow
      rail={rail}
      on={on}
      onClick={() => onSource(row.source)}
      mark={<Dot tone={row.tone} label={TONE_WORD[row.tone]} />}
      name={name ?? c.name}
      how={<span className={row.tone === "err" ? "text-destructive-foreground" : undefined}>{owner && !name && row.tone !== "err" ? `${row.how} · ${owner}` : row.how}</span>}
      end={
        per ? (
          <span className="text-[11.5px] text-muted-foreground tabular-nums">{per}</span>
        ) : c.remote ? (
          <span className="rounded-sm bg-muted px-1.5 py-px text-[10.5px] text-muted-foreground">remote</span>
        ) : undefined
      }
      tip={[c.name, row.how, owner, c.remote && "remote", per && `${per.replace("/m", "")} ${c.kind === "redis" ? "commands" : "queries"} a minute`].filter(Boolean).join(" · ")}
      railIcon={c.kind === "redis" ? ZapIcon : DatabaseIcon}
      railTone={row.tone}
    />
  );
}

/**
 * Machines picks which machine's daemon the window shows. It is only drawn
 * when there is something to say about machines: more than one to pick
 * from, or Shipyard boxes to list. With this Mac alone and no boxes, which
 * is most installs, the sidebar has nothing about machines in it.
 *
 * Shipyard is asked for its boxes when the list is opened, not before: the
 * asking runs Shipyard's own command. Whether to draw the switcher for
 * boxes alone goes by what Shipyard said the last time it was asked.
 *
 * A Shipyard box is listed and can't be picked: Leadline can't run on one
 * yet. It is words in the popup, not an item, so there is nothing to press
 * that does nothing.
 */
function Machines({ rail, shipyard }: { rail: boolean; shipyard: Boxes }) {
  const { machines, current } = useMachines();
  const { boxes } = shipyard;
  if (machines.length < 2 && boxes.length === 0 && !shipyard.known) return null;
  const now = machines.find((m) => m.id === current) ?? machines[0];
  const nowMs = Date.now();
  return (
    <div className={cn("shrink-0 pb-2", rail ? "flex justify-center" : "px-2")}>
      <Menu onOpenChange={(open) => open && shipyard.ask()}>
        <Tip label={`Machine: ${now.name}`} side="right">
          <MenuTrigger
            render={
              rail ? (
                <Button variant="outline" size="icon-sm" aria-label={`Machine: ${now.name}`} />
              ) : (
                <Button variant="outline" size="sm" aria-label={`Machine: ${now.name}`} className="w-full justify-start font-normal" />
              )
            }
          >
            <MonitorIcon />
            {!rail && (
              <>
                <span className="min-w-0 flex-1 truncate text-left">{now.name}</span>
                <ChevronsUpDownIcon className="text-muted-foreground" />
              </>
            )}
          </MenuTrigger>
        </Tip>
        <MenuPopup align="start" side={rail ? "right" : "bottom"} className="max-w-72 min-w-56">
          <MenuGroup>
            <MenuGroupLabel>Show the daemon on</MenuGroupLabel>
            <MenuRadioGroup value={current} onValueChange={(id) => machineList.use(id as string)}>
              {machines.map((m) => (
                <MenuRadioItem key={m.id} value={m.id}>
                  <span className="flex min-w-0 items-baseline gap-2">
                    <span className="truncate">{m.name}</span>
                    <span className="shrink-0 font-mono text-[10.5px] text-muted-foreground">{m.url.replace("http://", "")}</span>
                  </span>
                </MenuRadioItem>
              ))}
            </MenuRadioGroup>
          </MenuGroup>
          {boxes.length > 0 ? (
            <>
              <MenuSeparator />
              <div role="group" aria-label="Your Shipyard boxes" className="px-2 pt-1 pb-1.5">
                <div className="pb-1 text-xs font-medium text-muted-foreground">Your Shipyard boxes</div>
                <ul className="flex flex-col gap-1">
                  {boxes.map((b) => (
                    <li key={b.name} className="flex min-w-0 items-center gap-2 text-sm">
                      <Dot tone={b.state === "online" ? "ok" : "quiet"} />
                      <span className="truncate">{b.name}</span>
                      <span className="ml-auto shrink-0 text-[11.5px] text-muted-foreground">{boxState(b, nowMs)}</span>
                    </li>
                  ))}
                </ul>
                <p className="pt-1.5 text-[11.5px] leading-snug text-pretty text-muted-foreground">Leadline can't run on a box yet, so there is nothing to switch to there.</p>
              </div>
            </>
          ) : shipyard.asking ? (
            <>
              <MenuSeparator />
              <p role="status" className="flex items-center gap-2 px-2 pt-1 pb-1.5 text-[11.5px] text-muted-foreground">
                <Spinner aria-hidden className="size-3" />
                Asking Shipyard for your boxes…
              </p>
            </>
          ) : shipyard.known && shipyard.asked ? (
            <>
              <MenuSeparator />
              <p className="px-2 pt-1 pb-1.5 text-[11.5px] leading-snug text-pretty text-muted-foreground">Shipyard has no boxes to list now.</p>
            </>
          ) : null}
        </MenuPopup>
      </Menu>
    </div>
  );
}
