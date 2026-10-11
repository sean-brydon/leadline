// A stack from the browser names built chunks: "_next/static/chunks/….js:21:15".
// The dev server knows the source maps, and has an endpoint its own error
// overlay asks for the original position of each frame. This asks the same
// question, so the stack Leadline shows names the file the developer wrote.
//
// The endpoint is Next.js's own business and may change. Whatever goes wrong
// here, the stack is sent as the browser gave it.
import * as http from "node:http";
import { context } from "@opentelemetry/api";
import { suppressTracing } from "@opentelemetry/core";

const ENDPOINT = "/__nextjs_original-stack-frames";
const MAX_FRAMES = 40;
const TIMEOUT_MS = 1500;

interface Frame {
  method: string;
  file: string;
  line: number;
  column: number;
}

interface Line {
  text: string;
  frame?: Frame;
}

// "    at onClick (http://localhost:3000/_next/static/chunks/a.js:21:15)"
// "    at async http://localhost:3000/_next/static/chunks/a.js:21:15"
const V8 = /^\s*at (?:async )?(?:(.*?) \()?((?:[a-z][a-z0-9+.-]*:\/\/|\/).*?):(\d+):(\d+)\)?\s*$/i;
// "onClick@http://localhost:3000/_next/static/chunks/a.js:21:15" (Firefox, Safari)
const GECKO = /^\s*(.*?)@((?:[a-z][a-z0-9+.-]*:\/\/|\/).*?):(\d+):(\d+)\s*$/i;

export function parseStack(stack: string): Line[] {
  return stack.split("\n").map((text) => {
    const match = V8.exec(text) ?? GECKO.exec(text);
    if (!match) return { text };
    return {
      text,
      frame: { method: match[1] || "<unknown>", file: match[2]!, line: Number(match[3]), column: Number(match[4]) },
    };
  });
}

/**
 * A chunk's URL as the dev server wants it: the file in its output folder.
 * Next.js's own overlay rewrites "http://host/_next/static/x.js" to
 * "file://<distDir>/static/x.js" before it asks, and so does this.
 */
export function served(file: string, distDir: string): string {
  const match = /^https?:\/\/[^/]+\/_next(\/static\/[^?#]+)(\?[^#]*)?/.exec(file);
  return match ? `file://${distDir.replace(/\\/g, "/").replace(/\/$/, "")}${match[1]}${match[2] ?? ""}` : file;
}

/** What the endpoint answers for one frame, in either spelling Next.js has used. */
interface Answer {
  status?: string;
  value?: {
    originalStackFrame?: {
      file?: string | null;
      methodName?: string;
      line1?: number | null;
      column1?: number | null;
      lineNumber?: number | null;
      column?: number | null;
    } | null;
  };
}

/** Rewrites the frames the dev server could place; leaves the rest as they were. */
export function applyAnswers(lines: Line[], answers: unknown): string {
  if (!Array.isArray(answers)) return lines.map((line) => line.text).join("\n");
  let index = 0;
  return lines
    .map((line) => {
      if (!line.frame || index >= MAX_FRAMES) return line.text;
      const original = (answers[index++] as Answer | undefined)?.value?.originalStackFrame;
      const lineNumber = original?.line1 ?? original?.lineNumber;
      if (!original?.file || typeof lineNumber !== "number") return line.text;
      const column = original.column1 ?? original.column;
      const where = `${original.file}:${lineNumber}${typeof column === "number" ? `:${column}` : ""}`;
      return `    at ${original.methodName || line.frame.method} (${where})`;
    })
    .join("\n");
}

/**
 * The stack with its frames in source positions, asked of the dev server at
 * `port` on this machine, whose output is in `distDir`. Resolves to the stack
 * unchanged if that fails.
 */
export function mapStack(
  stack: string,
  where: { port: number; distDir: string; appDirectory: boolean },
): Promise<string> {
  const { port } = where;
  const lines = parseStack(stack);
  const frames = lines.flatMap((line) => (line.frame ? [line.frame] : [])).slice(0, MAX_FRAMES);
  if (frames.length === 0 || !port) return Promise.resolve(stack);
  const body = Buffer.from(
    JSON.stringify({
      frames: frames.map((frame) => ({
        file: served(frame.file, where.distDir),
        methodName: frame.method,
        arguments: [],
        line1: frame.line,
        column1: frame.column,
        // What the fields were called before Next 16.
        lineNumber: frame.line,
        column: frame.column,
      })),
      isServer: false,
      isEdgeServer: false,
      isAppDirectory: where.appDirectory,
    }),
  );
  return new Promise<string>((resolve) => {
    const giveUp = () => resolve(stack);
    try {
      context.with(suppressTracing(context.active()), () => {
        const request = http.request(
          { host: "127.0.0.1", port, path: ENDPOINT, method: "POST", timeout: TIMEOUT_MS,
            headers: { "content-type": "application/json", "content-length": body.length } },
          (response) => {
            const chunks: Buffer[] = [];
            response.on("data", (chunk: Buffer) => chunks.push(chunk));
            response.once("error", giveUp);
            response.once("end", () => {
              try {
                if (response.statusCode !== 200) return giveUp();
                resolve(applyAnswers(lines, JSON.parse(Buffer.concat(chunks).toString("utf8"))));
              } catch {
                giveUp();
              }
            });
          },
        );
        request.once("timeout", () => request.destroy(new Error("timed out")));
        request.once("error", giveUp);
        request.end(body);
      });
    } catch {
      giveUp();
    }
  });
}
