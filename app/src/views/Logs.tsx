import { useEffect, useLayoutEffect, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { AppWindowIcon, ChevronDownIcon, UserIcon } from "lucide-react";
import { api, type LogRow, type Scope } from "@/lib/api";
import { fmtClock, fmtCount } from "@/lib/format";
import { useDebounced, useLiveList } from "@/lib/live";
import { BROWSER_KINDS, kindLabel, kindMark } from "@/lib/logkinds";
import { logBefore, type Paging } from "@/lib/pages";
import { scopeKey } from "@/lib/scope";
import { SendButton } from "@/components/SendToShipyard";
import { Bar, Empty, FilterChip, NewPill, PickOne, Search, Tip, Waiting } from "@/components/ui";
import { Button } from "@/components/ui/button";
import { Menu, MenuGroup, MenuGroupLabel, MenuPopup, MenuRadioGroup, MenuRadioItem, MenuSeparator, MenuTrigger } from "@/components/ui/menu";
import { Spinner } from "@/components/ui/spinner";

// OTLP severity numbers: the lowest number of each level.
const LEVELS = [
  { label: "All", min: 0 },
  { label: "Info", min: 9 },
  { label: "Warn", min: 13 },
  { label: "Error", min: 17 },
];
const LEVEL_OPTIONS = LEVELS.map((l) => ({ value: String(l.min), label: l.label }));

const TONE: Record<string, string> = {
  trace: "text-muted-foreground",
  debug: "text-muted-foreground",
  info: "text-info-foreground",
  warn: "text-warning-foreground",
  error: "text-destructive-foreground",
  fatal: "text-destructive-foreground",
};

const ROW = 24;
// How near the end of what is loaded the user gets before the next page is
// fetched: far enough that steady scrolling never reaches the end.
const AHEAD = 120;

// A log has no id. Its cursor, the time and its place among logs of that
// instant, names it.
const PAGING: Paging<LogRow> = {
  size: 500,
  key: (l) => l.cursor ?? "",
  cursor: (l) => l.cursor ?? "",
  before: (a, b) => logBefore(a.cursor ?? "0.0", b.cursor ?? "0.0"),
};

export function Logs({ scope, rangeMs, onTrace }: { scope: () => Scope; rangeMs: number; onTrace: (id: string) => void }) {
  const [search, setSearch] = useState("");
  const q = useDebounced(search);
  const [min, setMin] = useState(0);
  // One kind of record ("browser.error"), and one user's records, when asked for.
  const [kind, setKind] = useState("");
  const [user, setUser] = useState("");
  const list = useLiveList((before) => api.logs(scope(), q, min, before, PAGING.size, { kind, user }), PAGING, [rangeMs, scopeKey(scope()), q, min, kind, user]);
  const rows = list.rows ?? [];
  const db = scope().db !== "";

  const scroller = useRef<HTMLDivElement>(null);
  const virt = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scroller.current,
    estimateSize: () => ROW,
    overscan: 20,
    getItemKey: (i) => rows[i].cursor ?? i,
  });
  const items = virt.getVirtualItems();
  const lastShown = items.length ? items[items.length - 1].index : -1;
  useEffect(() => {
    if (lastShown >= rows.length - AHEAD) list.loadMore();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [lastShown, rows.length]);
  // A new question starts at the top of its answer.
  useLayoutEffect(() => {
    if (scroller.current) scroller.current.scrollTop = 0;
  }, [list.epoch]);

  return (
    <div className="flex h-full flex-col">
      <Bar>
        <Search value={search} onChange={setSearch} placeholder="Filter by message" />
        <PickOne label="Lowest level" value={String(min)} options={LEVEL_OPTIONS} onChange={(v) => setMin(Number(v))} />
        <Menu>
          <MenuTrigger render={<Button variant="outline" size="xs" aria-label={`Kind of record: ${kindLabel(kind)}`} className={`shrink-0 font-normal ${kind ? "" : "text-muted-foreground"}`} />}>
            {kind && <AppWindowIcon />}
            {kindLabel(kind)}
            <ChevronDownIcon />
          </MenuTrigger>
          <MenuPopup align="start" className="min-w-44">
            <MenuRadioGroup value={kind} onValueChange={(v) => setKind(v as string)}>
              <MenuRadioItem value="" closeOnClick>
                Any kind
              </MenuRadioItem>
              <MenuSeparator />
              <MenuGroup>
                <MenuGroupLabel>From the browser</MenuGroupLabel>
                {BROWSER_KINDS.map((k) => (
                  <MenuRadioItem key={k.kind} value={k.kind} closeOnClick>
                    {k.label}
                  </MenuRadioItem>
                ))}
              </MenuGroup>
            </MenuRadioGroup>
          </MenuPopup>
        </Menu>
        {user && (
          <FilterChip label={`Logs for user ${user}`} clear="Show every user's logs" onClear={() => setUser("")}>
            <UserIcon aria-hidden className="size-3 shrink-0 text-muted-foreground" />
            <span className="truncate">{user}</span>
          </FilterChip>
        )}
        <span className="truncate text-[11px] text-muted-foreground tabular-nums">
          {list.rows ? `${fmtCount(rows.length)} ${list.more ? "newest" : "in this window"}${db ? ", written during requests that called this database" : ""}` : ""}
        </span>
      </Bar>
      {!list.rows ? (
        <Waiting error={list.error} />
      ) : rows.length === 0 ? (
        <Empty>
          {kind && !q && !user && min === 0
            ? `No ${kindLabel(kind).toLowerCase()} from the browser in this window. They come from an app with the browser's side of a Leadline provider, such as @leadline/next.`
            : db && !q && !kind && !user && min === 0
              ? "No log in this window was written during a request that called this database."
              : "No logs match in this window."}
        </Empty>
      ) : (
        <div className="relative min-h-0 flex-1">
          <div
            ref={scroller}
            data-list="logs"
            onScroll={(e) => list.hold(e.currentTarget.scrollTop > ROW / 2)}
            className="h-full overflow-y-auto font-mono text-[11.5px]"
          >
            <div style={{ height: virt.getTotalSize() + (list.more ? ROW : 0), position: "relative" }}>
              {items.map((v) => {
                const l = rows[v.index];
                const mark = kindMark(l.kind);
                return (
                  <div
                    key={v.key}
                    className="selectable absolute left-0 flex w-full items-center gap-3 px-3 hover:bg-accent"
                    style={{ top: v.start, height: ROW }}
                  >
                    <span className="w-24 shrink-0 text-muted-foreground tabular-nums">{fmtClock(l.ts_ms)}</span>
                    <span className={`w-10 shrink-0 ${TONE[l.severity] ?? "text-muted-foreground"}`}>{l.severity}</span>
                    <span className="w-28 shrink-0 truncate text-muted-foreground">{l.service}</span>
                    {mark && (
                      <span role="img" aria-label={mark.label} className="flex shrink-0 items-center gap-1 rounded-sm border px-1 font-sans text-[10px] leading-[14px] text-muted-foreground">
                        {mark.browser && <AppWindowIcon aria-hidden className="size-2.5" />}
                        {mark.word}
                      </span>
                    )}
                    <span className="min-w-0 flex-1 truncate">{l.body}</span>
                    {l.user_id && l.user_id !== user && (
                      <Tip label={`Show only the logs of user ${l.user_id}`}>
                        <Button
                          variant="ghost"
                          size="xs"
                          onClick={() => setUser(l.user_id as string)}
                          aria-label={`Show only the logs of user ${l.user_id}`}
                          className="max-w-28 gap-1 px-1 font-sans text-[11px] font-normal text-muted-foreground hover:text-foreground sm:h-5 sm:text-[11px]"
                        >
                          <UserIcon className="size-3 sm:size-3" />
                          <span className="truncate">{l.user_id}</span>
                        </Button>
                      </Tip>
                    )}
                    {(l.severity === "error" || l.severity === "fatal") && l.cursor && (
                      <SendButton ask={{ kind: "log", cursor: l.cursor, service: l.service }} label="Send this error to Shipyard: start a coding agent on it" />
                    )}
                    {l.trace_id && (
                      <Button
                        variant="link"
                        size="xs"
                        onClick={() => onTrace(l.trace_id)}
                        className="px-1 font-sans text-[11px] font-normal text-brand sm:h-5 sm:text-[11px]"
                      >
                        trace
                      </Button>
                    )}
                  </div>
                );
              })}
              {list.more && (
                <div
                  className="absolute left-0 flex w-full items-center gap-2 px-3 font-sans text-[11px] text-muted-foreground"
                  style={{ top: virt.getTotalSize(), height: ROW }}
                >
                  <Spinner aria-hidden className="size-3" />
                  Loading older logs…
                </div>
              )}
            </div>
          </div>
          {list.fresh.count > 0 && (
            <NewPill count={fmtCount(list.fresh.count)} atLeast={list.fresh.atLeast} onClick={() => scroller.current && (scroller.current.scrollTop = 0)} />
          )}
        </div>
      )}
    </div>
  );
}
