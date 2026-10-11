import { useEffect, useLayoutEffect, useMemo, useRef, useState } from "react";
import { useVirtualizer } from "@tanstack/react-virtual";
import { UserIcon } from "lucide-react";
import { api, type Scope, type Span, type TraceDetail, type TraceRow } from "@/lib/api";
import { fmtClock, fmtCount, fmtMs } from "@/lib/format";
import { useDebounced, useLive, useLiveList } from "@/lib/live";
import { traceBefore, type Paging } from "@/lib/pages";
import { scopeKey } from "@/lib/scope";
import { queryCount, ROLE_MARK, secondary, userLabel, userOf } from "@/lib/spans";
import { waterfall } from "@/lib/tree";
import { cn } from "@/lib/utils";
import { SendButton } from "@/components/SendToShipyard";
import { Bar, Close, DbBadge, Empty, FilterChip, Meter, NewPill, RowButton, Search, Tip, Waiting } from "@/components/ui";
import { Toggle } from "@/components/ui/toggle";
import { Spinner } from "@/components/ui/spinner";

type Props = { scope: () => Scope; rangeMs: number; selected: string | null; onSelect: (id: string | null) => void };

const ROW = 32;
// How near the end of what is loaded the user gets before the next page is
// fetched: far enough that steady scrolling never reaches the end.
const AHEAD = 60;

const PAGING: Paging<TraceRow> = {
  size: 200,
  key: (t) => t.trace_id,
  cursor: (t) => t.cursor ?? "",
  before: (a, b) => traceBefore(a.cursor ?? "0.", b.cursor ?? "0."),
};

export function Traces({ scope, rangeMs, selected, onSelect }: Props) {
  const [search, setSearch] = useState("");
  const q = useDebounced(search);
  // Only the requests made for one user ("user.id"), when a person asks for that from a trace.
  const [user, setUser] = useState("");
  const list = useLiveList((before) => api.traces(scope(), q, before, PAGING.size, user), PAGING, [rangeMs, scopeKey(scope()), q, user]);
  const rows = list.rows ?? [];
  const max = useMemo(() => rows.reduce((m, t) => Math.max(m, t.duration_ms), 1), [rows]);

  const scroller = useRef<HTMLDivElement>(null);
  const virt = useVirtualizer({
    count: rows.length,
    getScrollElement: () => scroller.current,
    estimateSize: () => ROW,
    overscan: 12,
    getItemKey: (i) => rows[i].trace_id,
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
    <div className="flex h-full">
      <div className={`flex min-w-0 flex-col ${selected ? "w-[46%] border-r" : "flex-1"}`}>
        <Bar>
          <Search value={search} onChange={setSearch} placeholder="Filter by name" />
          {user && (
            <FilterChip label={`Requests for user ${user}`} clear="Show every user's requests" onClear={() => setUser("")}>
              <UserIcon aria-hidden className="size-3 shrink-0 text-muted-foreground" />
              <span className="truncate">{user}</span>
            </FilterChip>
          )}
          <span className="truncate text-[11px] text-muted-foreground tabular-nums">
            {list.rows ? `${fmtCount(rows.length)} ${list.more ? "newest" : "in this window"}` : ""}
          </span>
        </Bar>
        {!list.rows ? (
          <Waiting error={list.error} />
        ) : rows.length === 0 ? (
          <Empty>{user ? `No traces for user ${user} in this window.` : scope().db ? "No trace in this window sent this database a call." : "No traces in this window."}</Empty>
        ) : (
          <div className="relative min-h-0 flex-1">
            <div
              ref={scroller}
              data-list="traces"
              onScroll={(e) => list.hold(e.currentTarget.scrollTop > ROW / 2)}
              className="h-full overflow-y-auto"
            >
              <div style={{ height: virt.getTotalSize() + (list.more ? ROW : 0), position: "relative" }}>
                {items.map((v) => {
                  const t = rows[v.index];
                  return (
                    <RowButton
                      key={v.key}
                      on={t.trace_id === selected}
                      onClick={() => onSelect(t.trace_id === selected ? null : t.trace_id)}
                      className="absolute left-0 gap-3 border-b px-3"
                      style={{ top: v.start, height: ROW }}
                    >
                      <span className="w-[86px] shrink-0 font-mono text-[11px] text-muted-foreground tabular-nums">{fmtClock(t.start_ms)}</span>
                      <span className={`size-1.5 shrink-0 rounded-full ${t.errors > 0 ? "bg-err" : "bg-ok"}`} />
                      <span className="min-w-0 flex-1 truncate">
                        {t.name} <span className="text-muted-foreground">{t.service}</span>
                      </span>
                      {t.user_id && !user && (
                        <span className="flex max-w-28 shrink-0 items-center gap-1 text-[11px] text-muted-foreground">
                          <UserIcon role="img" aria-label="For user" className="size-3 shrink-0" />
                          <span className="truncate">{t.user_id}</span>
                        </span>
                      )}
                      {t.queries > 0 && <span className="shrink-0 text-[11px] text-muted-foreground tabular-nums">{t.queries === 1 ? "1 query" : `${t.queries} queries`}</span>}
                      <span className="flex w-20 shrink-0 items-center">
                        <Meter value={t.duration_ms} max={max} />
                      </span>
                      <span className="w-14 shrink-0 text-right tabular-nums">{fmtMs(t.duration_ms)}</span>
                    </RowButton>
                  );
                })}
                {list.more && (
                  <div
                    className="absolute left-0 flex w-full items-center gap-2 px-3 text-[11px] text-muted-foreground"
                    style={{ top: virt.getTotalSize(), height: ROW }}
                  >
                    <Spinner aria-hidden className="size-3" />
                    Loading older traces…
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
      {selected && <Detail id={selected} user={user} onUser={setUser} onClose={() => onSelect(null)} />}
    </div>
  );
}

function Detail({ id, user, onUser, onClose }: { id: string; /** The user the list is narrowed to, or "". */ user: string; onUser: (id: string) => void; onClose: () => void }) {
  // A trace grows while its request runs: spans are exported as they end.
  const { data, error } = useLive<TraceDetail>(() => api.trace(id), [id]);
  const [spanId, setSpanId] = useState<string | null>(null);
  useEffect(() => setSpanId(null), [id]);

  const rows = useMemo(() => waterfall(data?.spans ?? []), [data]);
  if (!data) {
    return (
      <div className="min-w-0 flex-1">
        <Waiting error={error} />
      </div>
    );
  }

  const start = Math.min(...data.spans.map((s) => s.start_ms));
  const end = Math.max(...data.spans.map((s) => s.start_ms + s.duration_ms));
  const total = Math.max(end - start, 0.001);
  const picked = data.spans.find((s) => s.span_id === spanId) ?? null;
  const queries = queryCount(data.spans);
  const who = userOf(data.spans);

  return (
    <div className="@container flex min-w-0 flex-1 flex-col">
      <Bar className="gap-2.5">
        <span className="shrink-0 font-medium tabular-nums">{fmtMs(total)}</span>
        <span className="truncate text-[11px] text-muted-foreground">
          {plural(data.spans.length, "span")}
          {queries > 0 ? `, ${plural(queries, "query", "queries")}` : ""}, {plural(data.logs.length, "log")}
        </span>
        {who && (
          <Tip label={who.id === user ? "Showing only this user's requests. Press to show everyone's." : `Show only the requests of ${userLabel(who)}${userLabel(who) === who.id ? "" : ` (user ${who.id})`}`}>
            <Toggle
              size="sm"
              pressed={who.id === user}
              onPressedChange={(on) => onUser(on ? who.id : "")}
              aria-label={`Only the requests of user ${userLabel(who)}`}
              className="max-w-44 shrink-0 gap-1 px-1.5 text-[11px] font-normal sm:h-6 sm:text-[11px]"
            >
              <UserIcon className="size-3 sm:size-3" />
              <span className="truncate">{userLabel(who)}</span>
            </Toggle>
          </Tip>
        )}
        <span className="selectable ml-auto min-w-0 truncate font-mono text-[11px] text-muted-foreground">{id}</span>
        <SendButton words ask={{ kind: "trace", id }} label="Send this trace to Shipyard: start a coding agent on it" />
        <Close label="Close trace" onClose={onClose} />
      </Bar>
      <div className="flex-1 overflow-y-auto">
        {rows.map(({ span, depth }) => {
          // A statement seen twice, and BEGIN and its like, are in the trace but are not queries of their own.
          const mark = secondary(span) ? ROLE_MARK[span.db_role as string] : undefined;
          const row = (
            <RowButton
              key={span.span_id}
              on={span.span_id === spanId}
              onClick={() => setSpanId(span.span_id === spanId ? null : span.span_id)}
              className="h-7 gap-2 px-3"
            >
              <span className={cn("flex w-[42%] min-w-0 shrink-0 items-center gap-1.5", mark && "text-muted-foreground")} style={{ paddingLeft: depth * 12 }}>
                {span.db_system && (
                  <span className={cn("flex shrink-0", mark && "opacity-60")}>
                    <DbBadge system={span.db_system} />
                  </span>
                )}
                <span className={`truncate ${span.db_system ? "font-mono text-[11px]" : ""}`}>{span.db_statement || span.name}</span>
                {mark && <span className="shrink-0 rounded-sm border px-1 text-[10px] leading-[14px]">{mark.word}</span>}
              </span>
              <span className="relative h-3 min-w-0 flex-1">
                <span
                  className={cn("absolute top-0 h-full rounded-[2px]", span.status === "error" ? "bg-err" : span.db_system ? "bg-blue" : "bg-brand", mark && "opacity-35")}
                  style={{
                    left: `${((span.start_ms - start) / total) * 100}%`,
                    width: `max(2px, ${(span.duration_ms / total) * 100}%)`,
                  }}
                />
              </span>
              <span className="w-14 shrink-0 text-right text-[11px] text-muted-foreground tabular-nums">{fmtMs(span.duration_ms)}</span>
            </RowButton>
          );
          return mark ? (
            <Tip key={span.span_id} label={<span className="block max-w-72 text-pretty">{mark.why}</span>} side="left">
              {row}
            </Tip>
          ) : (
            row
          );
        })}
        {data.logs.length > 0 && (
          <div className="mt-2 border-t px-3 py-2">
            <div className="mb-1 text-[11px] text-muted-foreground">Logs in this trace</div>
            {data.logs.map((l, i) => (
              <div key={i} className="selectable flex gap-3 py-0.5 font-mono text-[11.5px]">
                <span className="shrink-0 text-muted-foreground">+{fmtMs(l.ts_ms - start)}</span>
                <span className={`shrink-0 ${l.severity === "error" || l.severity === "fatal" ? "text-destructive-foreground" : "text-muted-foreground"}`}>
                  {l.severity}
                </span>
                <span className="min-w-0 break-words">{l.body}</span>
              </div>
            ))}
          </div>
        )}
      </div>
      {picked && <SpanInfo span={picked} />}
    </div>
  );
}

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

function SpanInfo({ span }: { span: Span }) {
  const attrs = Object.entries(span.attrs).sort(([a], [b]) => a.localeCompare(b));
  const mark = secondary(span) ? ROLE_MARK[span.db_role as string] : undefined;
  return (
    <div className="selectable max-h-[40%] overflow-y-auto border-t bg-card px-3 py-2 text-xs">
      <div className="mb-1.5 flex flex-wrap items-center gap-x-2">
        <span className="font-medium">{span.name}</span>
        <span className="text-muted-foreground">
          {[span.service, span.kind, fmtMs(span.duration_ms), span.entry && "counted as a request", span.db_role === "query" && "counted as a query", span.db_server && `to ${span.db_server}`].filter(Boolean).join(" · ")}
        </span>
        {span.status === "error" && <span className="text-destructive-foreground">{span.status_message || "failed"}</span>}
      </div>
      {mark && <p className="mb-1.5 text-[11.5px] text-pretty text-muted-foreground">{mark.why}</p>}
      {span.db_statement && (
        <pre className="mb-2 rounded-md bg-muted p-2 text-[11.5px] break-words whitespace-pre-wrap">{span.db_statement}</pre>
      )}
      <table className="font-mono text-[11.5px]">
        <tbody>
          {attrs.map(([k, v]) => (
            <tr key={k} className="align-top">
              <td className="pr-4 text-muted-foreground">{k}</td>
              <td className="break-all">{typeof v === "string" ? v : JSON.stringify(v)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
