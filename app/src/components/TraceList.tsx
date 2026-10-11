import { fmtClock, fmtMs } from "@/lib/format";
import { Meter, RowButton } from "./ui";

export type TraceLine = {
  trace_id: string;
  start_ms: number;
  /** The duration the row is about, and its bar. */
  ms: number;
  failed: boolean;
  /** What the trace is, when the list doesn't already say. */
  name?: string;
  note?: string;
};

/** TraceList is a short list of traces; a click opens one in the Traces tab. */
export function TraceList({ rows, onTrace }: { rows: TraceLine[]; onTrace: (id: string) => void }) {
  const max = Math.max(0.001, ...rows.map((r) => r.ms));
  return (
    <div className="pb-1">
      {rows.map((r) => (
        <RowButton key={r.trace_id} onClick={() => onTrace(r.trace_id)} className="h-7 gap-2.5 px-3">
          <span className="w-[82px] shrink-0 font-mono text-[11px] text-muted-foreground tabular-nums">{fmtClock(r.start_ms)}</span>
          <span className={`size-1.5 shrink-0 rounded-full ${r.failed ? "bg-err" : "bg-ok"}`} />
          {r.name !== undefined && <span className="min-w-0 flex-1 truncate">{r.name || r.trace_id.slice(0, 16)}</span>}
          {r.note && <span className="shrink-0 text-[11px] text-muted-foreground tabular-nums">{r.note}</span>}
          <span className={`flex items-center ${r.name !== undefined ? "w-14 shrink-0" : "min-w-8 flex-1"}`}>
            <Meter value={r.ms} max={max} tone={r.failed ? "err" : "accent"} />
          </span>
          <span className="w-14 shrink-0 text-right tabular-nums">{fmtMs(r.ms)}</span>
        </RowButton>
      ))}
    </div>
  );
}
