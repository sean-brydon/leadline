// The app's own pieces, each a thin layer over a coss-ui component
// (components/ui): they hold the sizes and spacing this app uses everywhere,
// so a view says what it shows and not how dense it is.

import { useEffect, useRef, useState, type ComponentProps, type ReactElement, type ReactNode } from "react";
import { ArrowUpIcon, CheckIcon, ChevronDownIcon, ChevronUpIcon, CopyIcon, XIcon } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardDescription, CardTitle } from "@/components/ui/card";
import { Empty as EmptyState, EmptyDescription } from "@/components/ui/empty";
import { Input } from "@/components/ui/input";
import { Meter as MeterRoot, MeterIndicator, MeterTrack } from "@/components/ui/meter";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableHead, TableRow, type TableProps } from "@/components/ui/table";
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group";
import { Tooltip, TooltipPopup, TooltipTrigger } from "@/components/ui/tooltip";
import { cn } from "@/lib/utils";

// A coss Card, at the corner a small panel wants.
const SURFACE = "min-w-0 rounded-xl before:rounded-[calc(var(--radius-xl)-1px)]";

/** Surface is the card every panel, tile and listing is drawn on. */
export function Surface({ className, ...props }: ComponentProps<typeof Card>) {
  return <Card className={cn(SURFACE, className)} {...props} />;
}

function Head({ title, hint, className }: { title: ReactNode; hint?: ReactNode; className?: string }) {
  return (
    <header className={cn("flex items-baseline gap-2 px-3 pt-2.5", className)}>
      <CardTitle render={<h2 />} className="shrink-0 text-xs leading-normal font-medium">
        {title}
      </CardTitle>
      {hint && <CardDescription className="truncate text-[11px]">{hint}</CardDescription>}
    </header>
  );
}

export function Panel({ title, hint, children }: { title: ReactNode; hint?: ReactNode; children: ReactNode }) {
  return (
    <Surface render={<section />}>
      <Head title={title} hint={hint} />
      <div className="p-2">{children}</div>
    </Surface>
  );
}

export function Stat({ label, value, note, tone, className }: { label: string; value: string; /** A line under the number: what it is made of, or how it moved. */ note?: ReactNode; tone?: "err"; className?: string }) {
  return (
    <Surface className={cn("justify-center px-3 py-2.5", className)}>
      <div className="truncate text-[11px] text-muted-foreground">{label}</div>
      <div className={cn("mt-0.5 truncate text-xl/7 font-medium tabular-nums", tone === "err" && "text-destructive-foreground")}>{value}</div>
      {note !== undefined && <div className="min-h-4 truncate text-[11px] text-muted-foreground tabular-nums">{note}</div>}
    </Surface>
  );
}

/** Bar is the strip above a list or a detail: its filter, its title, a count. */
export function Bar({ className, children }: { className?: string; children: ReactNode }) {
  return <div className={cn("flex h-[45px] shrink-0 items-center gap-3 border-b px-3", className)}>{children}</div>;
}

export function Search({ value, onChange, placeholder }: { value: string; onChange: (v: string) => void; placeholder: string }) {
  return (
    <Input
      type="search"
      size="sm"
      value={value}
      onChange={(e) => onChange(e.target.value)}
      placeholder={placeholder}
      aria-label={placeholder}
      spellCheck={false}
      autoComplete="off"
      className="w-72 min-w-24 shrink"
    />
  );
}

/** Empty fills a view, or a pane of one, that has nothing to show, and says why. */
export function Empty({ children }: { children: ReactNode }) {
  return (
    <EmptyState className="h-full gap-2 p-8 md:py-8">
      <EmptyDescription className="max-w-md">{children}</EmptyDescription>
    </EmptyState>
  );
}

/** Waiting fills a view whose first answer hasn't come: a spinner, or why it won't. */
export function Waiting({ error }: { error?: string | null }) {
  return (
    <EmptyState className="h-full gap-2 p-8 md:py-8">
      {error ? (
        <EmptyDescription className="max-w-md text-destructive-foreground">{error}</EmptyDescription>
      ) : (
        <>
          <Spinner aria-hidden className="size-4 text-muted-foreground" />
          <EmptyDescription>Loading…</EmptyDescription>
        </>
      )}
    </EmptyState>
  );
}

/**
 * A database system's ink: its line in a chart split by system and its
 * badge are one colour. They are series colours, since no database is good
 * or bad.
 */
export const DB_INK: Record<string, string> = {
  postgresql: "--series-1",
  mysql: "--series-2",
  mongodb: "--series-3",
  sqlite: "--series-4",
  redis: "--series-5",
};

const DB_NAME: Record<string, string> = { postgresql: "Postgres", mysql: "MySQL", mongodb: "MongoDB", sqlite: "SQLite", redis: "Redis" };

/** dbName is a database system as people write it: "postgresql" is Postgres. */
export const dbName = (system: string): string => DB_NAME[system] ?? system;

/** DbBadge names a database system in its own colour. */
export function DbBadge({ system }: { system: string }) {
  const ink = DB_INK[system];
  return (
    <Badge
      variant="secondary"
      className={cn("px-1.5 font-mono text-[10.5px] font-normal sm:text-[10.5px]", !ink && "text-muted-foreground")}
      style={ink ? { color: `var(${ink})` } : undefined}
    >
      {system}
    </Badge>
  );
}

/** A chart with its legend is this tall; a panel holding anything else matches it. */
export const CHART_SLOT = 176;

/** Hold stands in for a panel's content while there is none: loading, empty, or failed. */
export function Hold({ height = CHART_SLOT, tone, children }: { height?: number; tone?: "err"; children: ReactNode }) {
  return (
    <div
      className={cn("flex items-center justify-center gap-2 px-4 text-center text-xs", tone === "err" ? "text-destructive-foreground" : "text-muted-foreground")}
      style={{ height }}
    >
      {children}
    </div>
  );
}

/** Loading is a Hold that says so. */
export function Loading({ height }: { height?: number }) {
  return (
    <Hold height={height}>
      <Spinner aria-hidden className="size-3.5" />
      Loading…
    </Hold>
  );
}

/**
 * Loaded draws its children once data has arrived and holds something:
 * until then it says the data is loading, failed to load, or is empty.
 */
export function Loaded<T>({
  data,
  error,
  empty,
  none,
  height,
  children,
}: {
  data: T | undefined;
  error?: string | null;
  /** Says whether data that arrived has nothing in it. */
  empty?: (d: T) => boolean;
  /** What to say then. */
  none: string;
  height?: number;
  children: (d: T) => ReactNode;
}) {
  if (data === undefined) {
    return error ? (
      <Hold height={height} tone="err">
        {error}
      </Hold>
    ) : (
      <Loading height={height} />
    );
  }
  if (empty?.(data)) return <Hold height={height}>{none}</Hold>;
  return <>{children(data)}</>;
}

const METER_INK = { accent: "bg-brand", blue: "bg-blue", err: "bg-err" };

/**
 * Meter is a thin bar: a row's part of the whole, or of the largest row.
 * The number it draws is nearly always written beside it, so it is hidden
 * from a screen reader unless `label` says what it measures.
 */
export function Meter({ value, max = 1, tone = "accent", label }: { value: number; max?: number; tone?: "accent" | "blue" | "err"; label?: string }) {
  const top = max > 0 ? max : 1;
  return (
    <MeterRoot
      value={Math.min(top, Math.max(0, value))}
      max={top}
      aria-label={label}
      aria-hidden={label ? undefined : true}
      render={<span />}
      className="block w-auto min-w-0 flex-1"
    >
      <MeterTrack render={<span />} className="h-1.5 rounded-full bg-input">
        {/* The ink is the chart's, and the bar moves with live data: no easing. */}
        <MeterIndicator render={<span />} className={cn("block rounded-full transition-none", METER_INK[tone], value > 0 && "min-w-0.5")} />
      </MeterTrack>
    </MeterRoot>
  );
}

/** Figure is one number with its name, small enough to line up eight in a row. */
export function Figure({ label, value, note, tone }: { label: string; value: string; note?: string; tone?: "err" | "warn" }) {
  return (
    <div className="min-w-0">
      <div className="truncate text-[11px] text-muted-foreground">{label}</div>
      <div className={cn("truncate text-[15px] font-medium tabular-nums", tone === "err" && "text-destructive-foreground", tone === "warn" && "text-warning-foreground")}>
        {value}
      </div>
      {note && <div className="truncate text-[11px] text-muted-foreground tabular-nums">{note}</div>}
    </div>
  );
}

/** Figures is the card a detail's Figure cells sit in, four across or `className`'s number. */
export function Figures({ className, children }: { className?: string; children: ReactNode }) {
  return <Surface className={cn("grid grid-cols-4 gap-x-4 gap-y-2.5 px-3 py-2.5", className)}>{children}</Surface>;
}

/**
 * Rows is coss's Table set as dense as this app's lists: 12px rows, small
 * quiet headings, and cells that wrap, since a row may carry a second line.
 * `scroll` makes the table's own box the one that scrolls, under a heading
 * that stays; for that it takes the place of a `flex-1` child.
 */
export function Rows({ className, scroll, ...props }: TableProps & { scroll?: boolean }) {
  return (
    <Table
      render={scroll ? <div className="min-h-0 flex-1 overflow-y-auto [&_thead]:sticky [&_thead]:top-0 [&_thead]:z-10 [&_thead]:bg-background" /> : undefined}
      className={cn(
        "table-fixed text-xs [&_td]:px-3 [&_td]:py-1.5 [&_td]:leading-normal [&_td]:whitespace-normal [&_th]:h-8 [&_th]:px-3 [&_th]:text-[11px] [&_th]:font-normal",
        // A sticky heading loses a collapsed border as the rows pass under it, so its line is a shadow.
        scroll && "[&_thead_tr]:border-0 [&_th]:shadow-[inset_0_-1px_0_var(--border)]",
        className,
      )}
      {...props}
    />
  );
}

/**
 * RowButton is a row of a list that opens what it shows: a trace, an
 * endpoint, a span. `on` marks the one that is open. It has a tabindex
 * because the app's window is WebKit, where Tab passes over a button
 * without one unless macOS's "Keyboard navigation" is on.
 */
export function RowButton({ on, className, ...props }: ComponentProps<"button"> & { on?: boolean }) {
  return (
    <button
      type="button"
      tabIndex={0}
      aria-pressed={on}
      className={cn(
        "flex w-full cursor-default items-center text-left text-xs outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset",
        on && "bg-accent",
        className,
      )}
      {...props}
    />
  );
}

/** PickRow is a table row that opens something: click it, or move to it and press Enter. */
export function PickRow({
  on,
  onPick,
  className,
  children,
}: {
  /** Whether what it opens is open now. */
  on?: boolean;
  onPick?: () => void;
  className?: string;
  children: ReactNode;
}) {
  return (
    <TableRow
      data-state={on ? "selected" : undefined}
      tabIndex={onPick ? 0 : undefined}
      onClick={onPick}
      onKeyDown={(e) => e.key === "Enter" && e.target === e.currentTarget && onPick?.()}
      className={cn("cursor-default focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring", className)}
    >
      {children}
    </TableRow>
  );
}

/** SortTh is a column heading that sorts by its column; again to turn the order round. */
export function SortTh<K extends string>({
  id,
  sort,
  asc,
  onSort,
  className = "",
  children,
}: {
  id: K;
  sort: K;
  asc: boolean;
  onSort: (id: K) => void;
  className?: string;
  children: ReactNode;
}) {
  const on = sort === id;
  const Arrow = asc ? ChevronUpIcon : ChevronDownIcon;
  // The arrow hangs off the heading, on the side away from the column's
  // edge, and takes no room: a heading lines up with its numbers whether or
  // not the table is sorted by it.
  const right = /\btext-right\b/.test(className);
  return (
    <TableHead className={className} aria-sort={on ? (asc ? "ascending" : "descending") : undefined}>
      <button
        type="button"
        tabIndex={0}
        onClick={() => onSort(id)}
        className={cn(
          "relative inline-flex h-5 items-center rounded-sm outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background",
          on && "text-foreground",
        )}
      >
        {children}
        {on && <Arrow aria-hidden className={cn("absolute size-3", right ? "right-full mr-0.5" : "left-full ml-0.5")} />}
      </button>
    </TableHead>
  );
}

/** Close is the button that shuts a pane; `label` says which, to a screen reader and in its tooltip. */
export function Close({ label, onClose }: { label: string; onClose: () => void }) {
  return (
    <Tip label={label}>
      <Button variant="ghost" size="icon-xs" onClick={onClose} aria-label={label} className="-mr-1 shrink-0 text-muted-foreground hover:text-foreground">
        <XIcon />
      </Button>
    </Tip>
  );
}

/**
 * NewPill floats over the top of a live list that is scrolled away from its
 * newest rows: how many have arrived, and a press goes back up to them.
 */
export function NewPill({ count, atLeast, onClick }: { count: string; atLeast?: boolean; onClick: () => void }) {
  return (
    <Button
      variant="outline"
      size="xs"
      onClick={onClick}
      // Over rows, so its fill is solid in dark too, where an outline button's is see-through.
      className="absolute top-2 left-1/2 -translate-x-1/2 rounded-full px-2.5 text-[11px] text-muted-foreground shadow-sm before:rounded-full hover:bg-popover hover:text-foreground sm:text-[11px] dark:bg-popover dark:hover:bg-popover"
    >
      {count}
      {atLeast ? "+" : ""} new
      <ArrowUpIcon />
    </Button>
  );
}

/** Side is the detail pane beside a list: a title bar, then a column that scrolls. */
export function Side({ title, onClose, label, children }: { title: ReactNode; onClose: () => void; label: string; children: ReactNode }) {
  return (
    <div className="@container flex min-w-0 flex-1 flex-col">
      <Bar className="gap-2">
        <div className="flex min-w-0 flex-1 items-center gap-2">{title}</div>
        <Close label={label} onClose={onClose} />
      </Bar>
      <div className="min-h-0 flex-1 overflow-y-auto p-3">{children}</div>
    </div>
  );
}

/** Listing is a titled list in a panel, which says so when it is empty. */
export function Listing({ title, hint, none, count, children }: { title: ReactNode; hint?: ReactNode; none: string; count: number; children: ReactNode }) {
  return (
    <Surface render={<section />} className="overflow-hidden">
      <Head title={title} hint={hint} className="pb-1.5" />
      {count === 0 ? <div className="px-3 pb-3 text-xs text-muted-foreground">{none}</div> : children}
    </Surface>
  );
}

/** Tip gives an element a tooltip, on hover and on keyboard focus alike: the app has no `title` attributes. */
export function Tip({ label, side, children }: { label: ReactNode; side?: "top" | "bottom" | "left" | "right"; children: ReactElement }) {
  return (
    <Tooltip>
      <TooltipTrigger render={children} />
      <TooltipPopup side={side}>{label}</TooltipPopup>
    </Tooltip>
  );
}

// Base UI's ToggleGroup never presses an item whose value is the empty
// string, so "" (Metrics' "split by nothing") travels through it as NONE.
const NONE = "__none__";
const toItem = (v: string) => (v === "" ? NONE : v);
const fromItem = (v: string) => (v === NONE ? "" : v);

/**
 * PickOne is the app's one control for picking one of a few values: a coss
 * ToggleGroup in a muted track, 28px high like the inputs beside it, the
 * chosen item raised. Arrow keys move along it; Space or Enter chooses.
 */
export function PickOne<T extends string>({
  value,
  options,
  onChange,
  label,
  className,
}: {
  value: T;
  options: { value: T; label: ReactNode; className?: string }[];
  onChange: (v: T) => void;
  /** Names the group for a screen reader. */
  label: string;
  className?: string;
}) {
  return (
    <ToggleGroup
      aria-label={label}
      size="sm"
      value={[toItem(value)]}
      // Clicking the chosen item again keeps it chosen.
      onValueChange={(v) => v.length && onChange(fromItem(v[v.length - 1] as string) as T)}
      className={cn("w-fit max-w-full shrink-0 flex-wrap rounded-lg bg-muted p-0.5", className)}
    >
      {options.map((o) => (
        <ToggleGroupItem
          key={toItem(o.value)}
          value={toItem(o.value)}
          className={cn(
            "rounded-md px-2 text-xs font-normal text-foreground/80 hover:bg-background/60 hover:text-foreground data-pressed:bg-background data-pressed:font-medium data-pressed:text-foreground data-pressed:shadow-xs/5 sm:h-6 sm:min-w-6 sm:text-xs dark:hover:bg-input/32 dark:data-pressed:bg-input",
            o.className,
          )}
        >
          {o.label}
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
  );
}

/** Code is a command or an address in running text, there to be copied. */
export function Code({ children }: { children: ReactNode }) {
  return <code className="selectable rounded-sm bg-muted px-1.5 py-0.5 text-[11.5px] text-foreground">{children}</code>;
}

const DOT = { ok: "bg-ok", quiet: "bg-muted-foreground/45", err: "bg-err", wait: "bg-warn" };

/**
 * Dot is a source's status light. Colour alone says nothing to a screen
 * reader or to red-green colour blindness, so wherever a dot stands by
 * itself it takes a `label`; beside words that already say it, it has none.
 */
export function Dot({ tone, label, className }: { tone: keyof typeof DOT; label?: string; className?: string }) {
  return <span role={label ? "img" : undefined} aria-label={label} className={cn("inline-block size-[7px] shrink-0 rounded-full", DOT[tone], className)} />;
}

/** copyText puts text on the clipboard, and says whether it could. */
export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // No clipboard permission (an http page that isn't localhost): the old way.
    const area = document.createElement("textarea");
    area.value = text;
    area.style.cssText = "position:fixed;opacity:0";
    document.body.append(area);
    area.select();
    let ok = false;
    try {
      ok = document.execCommand("copy");
    } catch {
      ok = false;
    }
    area.remove();
    return ok;
  }
}

/**
 * CopyLine is something to paste somewhere else: a command, an address, a
 * line of a .env. `text` is what is copied; `children` is what is shown,
 * when that differs (a URL with its password as dots).
 */
export function CopyLine({ text, label, children, wrap, className }: { text: string; /** Names the button: "Copy the prompt". */ label: string; children?: ReactNode; wrap?: boolean; className?: string }) {
  const [copied, setCopied] = useState(false);
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const copy = async () => {
    if (!(await copyText(text))) return;
    setCopied(true);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setCopied(false), 1600);
  };
  return (
    <div className={cn("flex min-w-0 gap-2 rounded-lg border bg-muted/50 py-1 pr-1 pl-2.5", wrap ? "items-start" : "items-center", className)}>
      <code className={cn("selectable min-w-0 flex-1 text-[11.5px] leading-5 text-foreground", wrap ? "py-0.5 break-words whitespace-pre-wrap" : "truncate")}>{children ?? text}</code>
      <Button variant="outline" size="xs" onClick={copy} aria-label={label} className="shrink-0 font-normal">
        {copied ? <CheckIcon className="text-success-foreground" /> : <CopyIcon />}
        {/* The words change in place; a screen reader hears the new ones. */}
        <span aria-live="polite">{copied ? "Copied" : "Copy"}</span>
      </Button>
    </div>
  );
}

/**
 * FilterChip names a filter that is on, in the bar of the list it narrows,
 * with the way to take it off. `label` says the whole of it to a screen
 * reader; `clear` names the button.
 */
export function FilterChip({ label, clear, onClear, children }: { label: string; clear: string; onClear: () => void; children: ReactNode }) {
  return (
    <span role="group" aria-label={label} className="flex h-6 max-w-48 min-w-0 shrink-0 items-center gap-1 rounded-md bg-muted pr-0.5 pl-2 text-xs">
      {children}
      <Tip label={clear}>
        <Button variant="ghost" size="icon-xs" aria-label={clear} onClick={onClear} className="size-5 shrink-0 text-muted-foreground hover:text-foreground sm:size-5">
          <XIcon className="size-3" />
        </Button>
      </Tip>
    </span>
  );
}

/** Notice is one quiet line across the top of a view: something true about what is shown that the view can't show itself. */
export function Notice({ children, action }: { children: ReactNode; action?: ReactNode }) {
  return (
    <div role="note" className="flex shrink-0 items-center gap-3 border-b bg-muted/50 px-3 py-1.5 text-[11.5px] text-muted-foreground">
      <span className="min-w-0 flex-1">{children}</span>
      {action}
    </div>
  );
}
