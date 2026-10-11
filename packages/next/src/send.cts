// A small OTLP/JSON sender for the two things that do not come out of the
// OpenTelemetry SDK: what the browser reports, and the route list. Like the
// provider's own exporters it says nothing and retries nothing when the
// daemon is not there.
import * as http from "node:http";
import * as https from "node:https";
import { context } from "@opentelemetry/api";
import { suppressTracing } from "@opentelemetry/core";

/** With the daemon down, do not try again for this long. */
const RETRY_MS = 2000;
const TIMEOUT_MS = 2000;

export type AttributeValue = string | number | boolean | undefined | null;

export interface KeyValue {
  key: string;
  value: { stringValue: string } | { intValue: string } | { doubleValue: number } | { boolValue: boolean };
}

/** Attributes in OTLP/JSON's shape. Missing values are left out. */
export function keyValues(attributes: Record<string, AttributeValue>): KeyValue[] {
  const out: KeyValue[] = [];
  for (const [key, value] of Object.entries(attributes)) {
    if (value === undefined || value === null) continue;
    if (typeof value === "string") out.push({ key, value: { stringValue: value } });
    else if (typeof value === "boolean") out.push({ key, value: { boolValue: value } });
    else if (Number.isInteger(value)) out.push({ key, value: { intValue: String(value) } });
    else if (Number.isFinite(value)) out.push({ key, value: { doubleValue: value } });
  }
  return out;
}

/** Unix milliseconds as the nanosecond string OTLP wants. A float cannot hold the product. */
export function nanos(ms: number): string {
  const whole = Math.floor(ms);
  const rest = Math.round((ms - whole) * 1e6);
  return (BigInt(whole) * 1_000_000n + BigInt(rest)).toString();
}

export class Sender {
  private downUntil = 0;
  private wasDown = false;
  /** Called when a send gets through after one that did not: the daemon is back, perhaps with nothing in it. */
  onBack?: () => void;

  constructor(
    private readonly url: string,
    private readonly log: (message: string) => void = () => {},
  ) {}

  /** Posts one OTLP/JSON request. Resolves to whether the daemon took it; never rejects. */
  post(path: "/v1/traces" | "/v1/logs", body: unknown): Promise<boolean> {
    if (Date.now() < this.downUntil) return Promise.resolve(false);
    return new Promise<boolean>((resolve) => {
      const done = (ok: boolean, why?: string) => {
        if (!ok) {
          this.downUntil = Date.now() + RETRY_MS;
          this.wasDown = true;
          if (why) this.log(`browser and route data not sent: ${why}`);
        } else if (this.wasDown) {
          this.wasDown = false;
          this.onBack?.();
        }
        resolve(ok);
      };
      try {
        const target = new URL(this.url + path);
        const payload = Buffer.from(JSON.stringify(body));
        // Inside suppressTracing the http instrumentation leaves this request
        // alone: telemetry about sending telemetry is noise.
        context.with(suppressTracing(context.active()), () => {
          const request = (target.protocol === "https:" ? https : http).request(
            target,
            {
              method: "POST",
              headers: { "content-type": "application/json", "content-length": payload.length },
              timeout: TIMEOUT_MS,
            },
            (response) => {
              response.resume();
              response.once("end", () => done((response.statusCode ?? 500) < 300, `HTTP ${response.statusCode}`));
              response.once("error", () => done(false));
            },
          );
          request.once("timeout", () => request.destroy(new Error("timed out")));
          request.once("error", (err) => done(false, (err as NodeJS.ErrnoException).code ?? err.message));
          request.end(payload);
        });
      } catch (err) {
        done(false, String(err));
      }
    });
  }
}
