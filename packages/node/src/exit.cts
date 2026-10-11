import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

/**
 * What the SDK has been given but has not yet passed to an exporter: the last
 * half second of spans and logs. When the process dies they would die with it.
 */
export class Tail<T> {
  // Spans the sampler dropped, or records dropped from a full queue, never
  // reach the exporter to be forgotten. The cap forgets them.
  private static readonly MAX = 4096;
  private readonly items = new Set<T>();

  add(item: T): void {
    if (this.items.size >= Tail.MAX) {
      for (const oldest of this.items) {
        this.items.delete(oldest);
        break;
      }
    }
    this.items.add(item);
  }

  forget(items: readonly T[]): void {
    for (const item of items) this.items.delete(item);
  }

  take(): T[] {
    const out = [...this.items];
    this.items.clear();
    return out;
  }
}

/** One OTLP/protobuf request body and where it goes. */
export interface Parcel {
  url: string;
  body: Uint8Array;
}

// Runs in the helper process: post each file, delete it, say nothing.
const HELPER = `
const fs = require("node:fs");
(async () => {
  for (const { url, file } of JSON.parse(process.env.LEADLINE_PARCELS)) {
    try {
      await fetch(url, {
        method: "POST",
        headers: { "content-type": "application/x-protobuf" },
        body: fs.readFileSync(file),
        signal: AbortSignal.timeout(2000),
      });
    } catch {}
    try { fs.rmSync(file, { force: true }); } catch {}
  }
})();
`;

/**
 * Sends parcels from a process that is exiting.
 *
 * Nothing asynchronous survives `process.exit()` or an uncaught exception, so
 * the request cannot be made from here. The bodies are written to temporary
 * files, which is synchronous, and a small detached Node process posts them
 * and removes them. The exiting process does not wait for it.
 */
export function sendFromExit(parcels: Parcel[]): void {
  // `process.execPath -e` is only Node's command line. Under Electron it
  // would open a window; under other runtimes it may mean something else.
  if (process.release?.name !== "node" || process.versions.electron || process.versions.bun) return;
  const files: Array<{ url: string; file: string }> = [];
  try {
    for (const { url, body } of parcels) {
      if (body.length === 0) continue;
      const file = join(tmpdir(), `leadline-node-${process.pid}-${randomBytes(6).toString("hex")}.bin`);
      writeFileSync(file, body, { mode: 0o600 });
      files.push({ url, file });
    }
    if (files.length === 0) return;
    spawn(process.execPath, ["-e", HELPER], {
      detached: true,
      stdio: "ignore",
      windowsHide: true,
      // Its own small environment: with the app's NODE_OPTIONS the helper
      // would load this provider too. (Windows needs SystemRoot to open a
      // socket at all.)
      env: {
        LEADLINE_PARCELS: JSON.stringify(files),
        ...(process.env.SystemRoot ? { SystemRoot: process.env.SystemRoot } : {}),
      },
    }).unref();
  } catch {
    for (const { file } of files) {
      try {
        rmSync(file, { force: true });
      } catch {
        // Nothing left to do for it.
      }
    }
  }
}
