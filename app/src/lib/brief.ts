// Sending a problem to a coding agent: what the "Send to Shipyard" dialog
// works out before and after it asks the daemon. Nothing here touches React
// or the network, so it can be tested.
//
// The daemon writes the prompt (internal/brief); this file only says how
// to ask for one, which of its lines to mark, and what a task may be named.

/** What a brief is asked about: the four places the app shows a problem. */
export type BriefAsk =
  | { kind: "query"; db_system: string; fingerprint: string }
  | { kind: "endpoint"; service: string; name: string }
  | { kind: "trace"; id: string }
  /** A log record is named by its place in the logs list and its service. */
  | { kind: "log"; cursor: string; service: string };

/** A run of a prompt's lines, counted from 1, that hold recorded data. */
export type Mark = { from: number; to: number; kind: string; values: boolean };

/** One of a Shipyard project's worktrees, as far as the dialog needs it. */
export type Worktree = { name: string; branch?: string; main?: boolean };
export type Project = { name: string; repo: boolean; default_branch?: string; worktrees: Worktree[] };

/** briefPath is the route that writes a brief of this kind. */
export const briefPath = (ask: BriefAsk): string => `/api/brief/${ask.kind}`;

/**
 * briefParams is the query a brief is asked with. The window is the one
 * the view shows; `service` stays the window's own filter, and the thing
 * asked about carries its own service under another name.
 */
export function briefParams(ask: BriefAsk, scope: { sinceMs: number; service: string }, example: boolean): Record<string, string | number | undefined> {
  const common = { since_ms: scope.sinceMs, example: example ? 1 : undefined };
  switch (ask.kind) {
    case "query":
      return { ...common, service: scope.service, db_system: ask.db_system, fingerprint: ask.fingerprint };
    case "endpoint":
      return { ...common, service: scope.service, endpoint_service: ask.service, name: ask.name };
    case "trace":
      return { id: ask.id, example: common.example };
    case "log":
      return { ...common, cursor: ask.cursor, log_service: ask.service };
  }
}

/** askKey names what is asked, so one dialog can tell it from another. */
export function askKey(ask: BriefAsk): string {
  switch (ask.kind) {
    case "query":
      return `query\n${ask.db_system}\n${ask.fingerprint}`;
    case "endpoint":
      return `endpoint\n${ask.service}\n${ask.name}`;
    case "trace":
      return `trace\n${ask.id}`;
    case "log":
      return `log\n${ask.service}\n${ask.cursor}`;
  }
}

/**
 * recordedTexts is the text of every line the daemon marked as recorded
 * data. The prompt can be edited, so a mark can't stay tied to a line
 * number: a line is marked for as long as it still reads exactly as it was
 * recorded, wherever it has moved to.
 */
export function recordedTexts(prompt: string, marks: Mark[]): Set<string> {
  const lines = prompt.split("\n");
  const out = new Set<string>();
  for (const m of marks) {
    for (let i = m.from; i <= m.to && i <= lines.length; i++) {
      const line = lines[i - 1];
      if (line !== undefined && line.trim() !== "") out.add(line);
    }
  }
  return out;
}

/** markedLines says, for each line of the text as it is now, whether it is recorded data. */
export function markedLines(text: string, recorded: Set<string>): boolean[] {
  return text.split("\n").map((line) => recorded.has(line));
}

/** countMarked is how many of the text's lines are still recorded data. */
export const countMarked = (text: string, recorded: Set<string>): number => markedLines(text, recorded).filter(Boolean).length;

// What the daemon takes for a task's name (internal/discover/task.go): it
// becomes a branch, a folder and part of a URL in Shipyard.
const TASK_NAME = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/;

/** taskNameFault says what is wrong with a task's name, or null when nothing is. */
export function taskNameFault(name: string, project?: Project): string | null {
  if (name === "") return "A task needs a name.";
  if (name.length > 63) return "That is longer than a name can be (63 characters).";
  if (!TASK_NAME.test(name)) return "Use lower-case letters, digits and dashes, starting with a letter.";
  if (name.startsWith("art-")) return "A name can't start with art-: Shipyard keeps those.";
  if (project?.worktrees.some((w) => w.name === name || w.branch === name)) return `${project.name} already has a worktree named ${name}.`;
  return null;
}

/** tidyTaskName turns what a person types into a name as they type it: capitals lowered, anything else a dash. */
export function tidyTaskName(typed: string): string {
  return typed
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, "-")
    .replace(/-{2,}/g, "-")
    .replace(/^[-0-9]+/, "")
    .slice(0, 63);
}

/** The base that means "what the project has checked out": no ref is named. */
export const PROJECT_CHECKOUT = "";

/**
 * baseChoices lists where a task's branch can start from: what the project
 * has checked out, or the branch of one of its worktrees. `suggested` is a
 * base the daemon proposed; it is listed even when no worktree has it now.
 */
export function baseChoices(project: Project | undefined, suggested = ""): { value: string; label: string; hint: string }[] {
  const main = project?.worktrees.find((w) => w.main)?.branch || project?.default_branch || "";
  const out = [{ value: PROJECT_CHECKOUT, label: main ? `${main}, as the project has it` : "What the project has checked out", hint: "The project's own checkout" }];
  const seen = new Set<string>([PROJECT_CHECKOUT]);
  for (const w of project?.worktrees ?? []) {
    if (w.main || !w.branch || seen.has(w.branch)) continue;
    seen.add(w.branch);
    out.push({ value: w.branch, label: w.branch, hint: w.name === w.branch ? "A worktree's branch" : `The branch of the worktree ${w.name}` });
  }
  if (suggested && !seen.has(suggested)) out.push({ value: suggested, label: suggested, hint: "A branch" });
  return out;
}

/** kb writes a size the way the dialog says it: "3.0 KB", "412 bytes". */
export function kb(bytes: number): string {
  if (bytes < 1000) return `${bytes} bytes`;
  return `${(bytes / 1024).toFixed(1)} KB`;
}

/** byteLength is a text's size as it will be sent. */
export const byteLength = (text: string): number => new TextEncoder().encode(text).length;

/** The most the daemon sends (internal/discover/task.go). */
export const MAX_PROMPT_BYTES = 64 * 1024;

/** promptFault says why a prompt can't be sent as it is, or null. */
export function promptFault(text: string): string | null {
  if (text.trim() === "") return "There is nothing to send.";
  const size = byteLength(text);
  if (size > MAX_PROMPT_BYTES) return `The prompt is ${Math.ceil(size / 1024)} KB, and ${MAX_PROMPT_BYTES / 1024} KB is the most that can be sent.`;
  return null;
}
