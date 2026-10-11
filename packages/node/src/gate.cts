import * as net from "node:net";

/** Trust "the daemon is up" this long before looking again. */
const UP_FOR_MS = 1000;
/** With the daemon down, look again this often, and only when there is something to send. */
const RETRY_MS = 2000;
/** A loopback connect answers in well under a millisecond; this is for a mistyped remote host. */
const PROBE_TIMEOUT_MS = 250;

export type Probe = (host: string, port: number) => Promise<boolean>;

/**
 * Knows whether the Leadline daemon is there, so that nothing is sent, retried
 * or complained about while it is not.
 *
 * The OTLP exporters retry a refused connection for seconds on timers that
 * keep the process alive, and report each failure. A TCP connect first costs
 * next to nothing, and its answer lets a batch be dropped at once and in
 * silence. There is no timer: the gate only looks when there is something to
 * send, so an idle app does no work, and a daemon started later is found by
 * the first batch after RETRY_MS.
 */
export class Gate {
  state: "unknown" | "up" | "down" = "unknown";
  private checkedAt = 0;
  private probing: Promise<boolean> | undefined;

  constructor(
    private readonly host: string,
    private readonly port: number,
    private readonly log: (message: string) => void = () => {},
    private readonly probe: Probe = tcpProbe,
  ) {}

  /** Whether to send now. Answers at once when it looked recently. */
  ready(): boolean | Promise<boolean> {
    const age = Date.now() - this.checkedAt;
    if (this.state === "up" && age < UP_FOR_MS) return true;
    if (this.state === "down" && age < RETRY_MS) return false;
    this.probing ??= this.probe(this.host, this.port).then(
      (up) => this.settle(up),
      () => this.settle(false),
    );
    return this.probing;
  }

  /** An export finished: its outcome is as good as a probe. */
  sent(ok: boolean): void {
    this.settle(ok);
  }

  private settle(up: boolean): boolean {
    this.probing = undefined;
    const next = up ? "up" : "down";
    if (next !== this.state) this.log(`daemon at ${this.host}:${this.port} is ${next}`);
    this.state = next;
    this.checkedAt = Date.now();
    return up;
  }
}

function tcpProbe(host: string, port: number): Promise<boolean> {
  return new Promise((resolve) => {
    const socket = net.connect({ host, port });
    const done = (up: boolean) => {
      socket.destroy();
      resolve(up);
    };
    socket.setTimeout(PROBE_TIMEOUT_MS, () => done(false));
    socket.once("connect", () => done(true));
    socket.once("error", () => done(false));
  });
}

interface Exporter<T> {
  export(items: T, done: (result: { code: number; error?: Error }) => void): void;
}

/** ExportResultCode.SUCCESS, without loading @opentelemetry/core for one number. */
const SUCCESS = 0;

/**
 * Wraps an OTLP exporter so it only sends while the daemon is up, and never
 * reports a failure to the SDK (which would log it). `onExport` is told what
 * was taken off the SDK's hands, sent or dropped.
 */
export function gated<T, E extends Exporter<T>>(
  exporter: E,
  gate: Gate,
  log: (message: string) => void = () => {},
  onExport: (items: T) => void = () => {},
): E {
  const send: Exporter<T>["export"] = (items, done) => {
    onExport(items);
    const go = (up: boolean) => {
      if (!up) return done({ code: SUCCESS }); // dropped: nobody is listening
      try {
        exporter.export(items, (result) => {
          const ok = result.code === SUCCESS;
          if (!ok) log(`export failed: ${result.error?.message ?? "unknown error"}`);
          // An HTTP status (a number, where a refused connection has
          // "ECONNREFUSED") means the daemon answered and said no, as one
          // without metrics does to /v1/metrics. It is still there for the
          // other signals.
          const answered = typeof (result.error as { code?: unknown } | undefined)?.code === "number";
          gate.sent(ok || answered);
          done({ code: SUCCESS });
        });
      } catch (err) {
        log(`export failed: ${String(err)}`);
        gate.sent(false);
        done({ code: SUCCESS });
      }
    };
    const ready = gate.ready();
    if (typeof ready === "boolean") go(ready);
    else void ready.then(go);
  };
  // A proxy, not a subclass: the trace, log and metric exporters each have
  // other methods the SDK calls (shutdown, forceFlush, temporality), and
  // they all pass through untouched.
  return new Proxy(exporter, {
    get(target, prop) {
      if (prop === "export") return send;
      const value = Reflect.get(target, prop, target);
      return typeof value === "function" ? value.bind(target) : value;
    },
  });
}
