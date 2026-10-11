// What sort of record a log is, when its provider says ("leadline.kind"):
// today the records @leadline/next sends from the browser. Kept apart from
// React so it can be tested.

export type LogKind = {
  kind: string;
  /** A word for the row's mark. */
  word: string;
  /** What the filter calls records of this kind. */
  label: string;
};

/** The kinds the browser's side of a provider sends, in the order the filter lists them. */
export const BROWSER_KINDS: LogKind[] = [
  { kind: "browser.error", word: "error", label: "Errors" },
  { kind: "browser.console", word: "console", label: "Console" },
  { kind: "browser.navigation", word: "navigation", label: "Navigations" },
  { kind: "browser.web_vital", word: "web vital", label: "Web Vitals" },
  { kind: "browser.fetch_failed", word: "fetch failed", label: "Failed fetches" },
  { kind: "browser.fetch_slow", word: "slow fetch", label: "Slow fetches" },
];

export type KindMark = {
  /** The record was written in the browser, not by the server. */
  browser: boolean;
  word: string;
  /** The mark in full, for a tooltip and a screen reader. */
  label: string;
};

/** kindMark is how a log row marks its kind; null for a record with none, which is most of them. */
export function kindMark(kind: string | undefined): KindMark | null {
  if (!kind) return null;
  const known = BROWSER_KINDS.find((k) => k.kind === kind);
  if (known) return { browser: true, word: known.word, label: `From the browser: ${known.word}` };
  // A kind a newer provider sends is still worth showing, in its own words.
  if (kind.startsWith("browser.")) {
    const word = kind.slice("browser.".length).replaceAll("_", " ");
    return { browser: true, word, label: `From the browser: ${word}` };
  }
  return { browser: false, word: kind, label: `Kind: ${kind}` };
}

/** kindLabel is what the filter's button says for the kind it is set to. */
export function kindLabel(kind: string): string {
  if (!kind) return "Any kind";
  return BROWSER_KINDS.find((k) => k.kind === kind)?.label ?? kind;
}
