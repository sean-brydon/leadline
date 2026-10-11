// Sends the app's route list to Leadline as one log record, when the dev
// server starts and whenever the list changes.
import { watch } from "node:fs";
import type { FSWatcher } from "node:fs";
import { routeDirs, scanRoutes } from "./routes.cjs";
import { keyValues, nanos } from "./send.cjs";
import type { Sender } from "./send.cjs";
import type { RouteEntry } from "./types.cjs";

/** Long enough for a save, a rename or a `git checkout` to settle. */
const SETTLE_MS = 300;
/** Sent again this often while the app is in use, in case the daemon has lost it. */
const REFRESH_MS = 10 * 60 * 1000;

export interface RouteListOptions {
  dir: string;
  pageExtensions?: string[];
  service: string;
  nextVersion?: string;
  sender: Sender;
  log?: (message: string) => void;
}

export class RouteList {
  current: RouteEntry[] = [];
  private sent = "";
  private sentAt = 0;
  private sending = false;
  private timer: NodeJS.Timeout | undefined;
  private readonly watchers: FSWatcher[] = [];

  constructor(private readonly options: RouteListOptions) {}

  /** Reads the routes, sends them, and watches the route folders for changes. */
  start(): void {
    this.rescan();
    const { app, pages } = routeDirs(this.options.dir);
    for (const dir of [app, pages]) {
      if (!dir) continue;
      try {
        // One recursive watch per tree: the OS reports changes, nothing polls.
        const watcher = watch(dir, { recursive: true, persistent: false }, () => this.changed());
        watcher.on("error", () => watcher.close());
        this.watchers.push(watcher);
      } catch {
        // No recursive watching here: the list is still sent at start.
      }
    }
  }

  stop(): void {
    clearTimeout(this.timer);
    for (const watcher of this.watchers) watcher.close();
    this.watchers.length = 0;
  }

  /** Next.js's `pageExtensions`, when the config turned out to set them. */
  setPageExtensions(pageExtensions: string[]): void {
    this.options.pageExtensions = pageExtensions;
    this.changed();
  }

  /** Forget that the list was delivered: the daemon may have restarted empty. */
  forget(): void {
    this.sent = "";
  }

  /**
   * Called as requests end. Sends the list if the daemon has not had it (it
   * was not running when the dev server started) or has not had it lately.
   */
  nudge(): void {
    const list = JSON.stringify(this.current);
    if (list !== this.sent || Date.now() - this.sentAt > REFRESH_MS) void this.send(list);
  }

  private changed(): void {
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.rescan(), SETTLE_MS);
    this.timer.unref?.();
  }

  private rescan(): void {
    try {
      this.current = scanRoutes(this.options.dir, this.options.pageExtensions);
    } catch (err) {
      this.options.log?.(`could not read the routes: ${String(err)}`);
      return;
    }
    const list = JSON.stringify(this.current);
    if (list !== this.sent) void this.send(list);
  }

  private async send(list: string): Promise<void> {
    if (this.sending) return;
    this.sending = true;
    try {
      const ok = await this.options.sender.post("/v1/logs", routesRecord(this.options, this.current));
      if (ok) {
        this.sent = list;
        this.sentAt = Date.now();
        this.options.log?.(`sent ${this.current.length} routes`);
      }
    } finally {
      this.sending = false;
    }
  }
}

/**
 * The route list as an OTLP log record: `leadline.kind` is "next.routes" and
 * the body is JSON. Debug severity, so that until Leadline reads it as a
 * route list it stays out of the way among the app's logs.
 */
export function routesRecord(
  options: Pick<RouteListOptions, "dir" | "service" | "nextVersion">,
  routes: RouteEntry[],
  now = Date.now(),
): unknown {
  return {
    resourceLogs: [
      {
        resource: { attributes: keyValues({ "service.name": options.service }) },
        scopeLogs: [
          {
            scope: { name: "@leadline/next" },
            logRecords: [
              {
                timeUnixNano: nanos(now),
                severityNumber: 5,
                severityText: "DEBUG",
                body: {
                  stringValue: JSON.stringify({
                    version: 1,
                    framework: "next",
                    next: options.nextVersion,
                    root: options.dir,
                    routes,
                  }),
                },
                attributes: keyValues({ "leadline.kind": "next.routes", "leadline.routes.count": routes.length }),
              },
            ],
          },
        ],
      },
    ],
  };
}
