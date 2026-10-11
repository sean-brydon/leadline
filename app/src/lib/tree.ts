// The minimum a span needs for the waterfall; api.ts's Span fits it.
export type TreeSpan = { span_id: string; parent_id: string; start_ms: number };

export type Row<S> = { span: S; depth: number };

/**
 * waterfall orders a trace's spans for drawing: each parent followed by its
 * children in start order, with how deep each one sits. A span whose parent
 * isn't in the trace (not exported yet, or from a service that isn't
 * instrumented) is shown at the top level, not dropped.
 */
export function waterfall<S extends TreeSpan>(spans: S[]): Row<S>[] {
  const ids = new Set(spans.map((s) => s.span_id));
  const children = new Map<string, S[]>();
  const roots: S[] = [];
  for (const s of spans) {
    if (s.parent_id && ids.has(s.parent_id) && s.parent_id !== s.span_id) {
      const list = children.get(s.parent_id);
      if (list) list.push(s);
      else children.set(s.parent_id, [s]);
    } else {
      roots.push(s);
    }
  }
  const byStart = (a: S, b: S) => a.start_ms - b.start_ms;
  const out: Row<S>[] = [];
  const seen = new Set<string>();
  const walk = (s: S, depth: number) => {
    // A cycle in parent ids would loop for ever.
    if (seen.has(s.span_id)) return;
    seen.add(s.span_id);
    out.push({ span: s, depth });
    for (const c of (children.get(s.span_id) ?? []).sort(byStart)) walk(c, depth + 1);
  };
  for (const r of roots.sort(byStart)) walk(r, 0);
  // Spans only reachable through a cycle have no root; show them flat.
  for (const s of spans) if (!seen.has(s.span_id)) walk(s, 0);
  return out;
}
