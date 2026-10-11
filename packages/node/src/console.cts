import { format } from "node:util";

export const METHODS = ["debug", "log", "info", "warn", "error"] as const;
export type ConsoleMethod = (typeof METHODS)[number];

/** One console call, as it will go out as a log record. */
export interface ConsoleRecord {
  method: ConsoleMethod;
  /** The OTLP severity number: 5 debug, 9 info, 13 warn, 17 error. */
  severityNumber: number;
  severityText: string;
  /** What the console printed, without colours. */
  body: string;
  /** stdout for debug, log and info; stderr for warn and error. */
  stream: "stdout" | "stderr";
  /** The first Error among the arguments, if there was one. */
  error?: Error;
}

const SEVERITY: Record<ConsoleMethod, [number, string, "stdout" | "stderr"]> = {
  debug: [5, "DEBUG", "stdout"],
  log: [9, "INFO", "stdout"],
  info: [9, "INFO", "stdout"],
  warn: [13, "WARN", "stderr"],
  error: [17, "ERROR", "stderr"],
};

// A megabyte of JSON logged by accident should not ride along in every batch.
const MAX_BODY = 64 * 1024;

type Target = Record<ConsoleMethod, (...args: unknown[]) => void>;

export interface CaptureOptions {
  /** True when a call made right now must be printed and not captured. */
  skip?: () => boolean;
  /**
   * Called before a call that will be captured is printed, with the stream it
   * prints to; what it returns is called once the printing is done. For
   * `leadline run`, which has to tell the lines that were captured here from
   * the ones that were not (see run.cts).
   */
  around?: (stream: "stdout" | "stderr") => (() => void) | undefined;
}

/**
 * Hands every `console.debug/log/info/warn/error` call to `emit` after the
 * console has printed it. What the console prints does not change. Returns a
 * function that puts the console back.
 */
export function captureConsole(
  emit: (record: ConsoleRecord) => void,
  target: Target = console,
  options: CaptureOptions = {},
): () => void {
  // True while a record is being handed over. Whatever `emit` itself logs
  // (an exporter complaining, a debug line) is printed and not captured, or
  // one log line would become an endless stream of them.
  let emitting = false;
  const undo: Array<() => void> = [];

  for (const method of METHODS) {
    const original = target[method];
    if (typeof original !== "function") continue;
    const [severityNumber, severityText, stream] = SEVERITY[method];

    const patched = function (this: unknown, ...args: unknown[]): void {
      // Settled before printing, because `around` has to know.
      let capturing = !emitting;
      let after: (() => void) | undefined;
      if (capturing) {
        try {
          capturing = !options.skip?.();
          if (capturing) after = options.around?.(stream);
        } catch {
          // Then it is printed and captured as if neither had been given.
        }
      }
      // Print first, exactly as before: the developer's terminal comes first,
      // and it still gets the line if capturing goes wrong.
      try {
        original.apply(this, args);
      } finally {
        after?.();
      }
      if (!capturing) return;
      emitting = true;
      try {
        let body = format(...args);
        if (body.length > MAX_BODY) body = `${body.slice(0, MAX_BODY)}… (${body.length} characters)`;
        emit({
          method,
          severityNumber,
          severityText,
          body,
          stream,
          error: args.find((arg): arg is Error => arg instanceof Error),
        });
      } catch {
        // An argument that throws when formatted, or a broken exporter: the
        // line was printed, and that is the part the app relies on.
      } finally {
        emitting = false;
      }
    };
    Object.defineProperty(patched, "name", { value: method });

    target[method] = patched;
    undo.push(() => {
      // Leave it alone if something else has wrapped the console since.
      if (target[method] === patched) target[method] = original;
    });
  }
  return () => undo.forEach((fn) => fn());
}
