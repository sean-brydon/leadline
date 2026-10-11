// The page reports to the dev server it came from, at one path, and the
// server passes it on to Leadline. Going through the server is what lets the
// page stay ignorant: it needs no address for the daemon, makes no
// cross-origin request, and prints no network error when the daemon is not
// running, because the server already knows and answers 200 either way.
//
// The path is answered before Next.js sees the request. A plugin in
// next.config cannot add a route without adding a file to the app, so this
// takes the same road OpenTelemetry's http instrumentation does: the
// server's "request" event.
import * as http from "node:http";
import * as https from "node:https";

export const RELAY_PATH = "/__leadline/b";
const MAX_BODY = 256 * 1024;

type Headers = Record<string, string | string[] | undefined>;
/** `port` is the dev server's own, and 0 when it speaks TLS. */
export type RelayHandler = (body: string, headers: Headers, port: number) => void;

/** Answers RELAY_PATH on every http(s) server in this process. Returns a function that stops. */
export function installRelay(handle: RelayHandler): () => void {
  const undo: Array<() => void> = [];
  for (const proto of [http.Server.prototype, https.Server.prototype] as Array<{ emit: Function }>) {
    const own = Object.prototype.hasOwnProperty.call(proto, "emit");
    const original = proto.emit;
    const patched = function (this: unknown, event: unknown, ...args: unknown[]): unknown {
      if (event === "request") {
        const request = args[0] as http.IncomingMessage | undefined;
        const url = request?.url;
        if (request && url && url.startsWith(RELAY_PATH) && (url.length === RELAY_PATH.length || url[RELAY_PATH.length] === "?")) {
          answer(request, args[1] as http.ServerResponse, handle);
          return true;
        }
      }
      return original.apply(this, [event, ...args]);
    };
    proto.emit = patched;
    undo.push(() => {
      // Leave it if something else has wrapped it since.
      if (proto.emit !== patched) return;
      if (own) proto.emit = original;
      else delete (proto as { emit?: Function }).emit;
    });
  }
  return () => undo.forEach((fn) => fn());
}

function answer(request: http.IncomingMessage, response: http.ServerResponse, handle: RelayHandler): void {
  const reply = (status: number) => {
    if (response.headersSent) return;
    response.statusCode = status;
    response.setHeader("cache-control", "no-store");
    response.setHeader("content-type", "text/plain");
    // A body, however short: Chromium's tools list a fetch whose response
    // was empty as a cancelled request, which reads as something being wrong.
    response.end(status === 200 ? "ok" : "");
  };
  if (request.method !== "POST") return reply(405);
  if (!sameOrigin(request.headers)) return reply(403);

  const chunks: Buffer[] = [];
  let size = 0;
  request.on("data", (chunk: Buffer) => {
    size += chunk.length;
    if (size > MAX_BODY) {
      reply(413);
      request.destroy();
    } else {
      chunks.push(chunk);
    }
  });
  request.on("error", () => {});
  request.on("end", () => {
    if (size > MAX_BODY) return;
    // The page is told nothing but "received": what happens next is not its
    // concern, and it should not wait for it.
    reply(200);
    try {
      const secure = (request.socket as { encrypted?: boolean }).encrypted === true;
      handle(Buffer.concat(chunks).toString("utf8"), request.headers, secure ? 0 : (request.socket.localPort ?? 0));
    } catch {
      // A report that cannot be read is dropped.
    }
  });
}

/**
 * Whether a request came from a page of this same server. Another site open
 * in the developer's browser could otherwise post made-up errors here.
 */
export function sameOrigin(headers: Headers): boolean {
  const site = headers["sec-fetch-site"];
  if (typeof site === "string") return site === "same-origin";
  const origin = headers.origin;
  // No Origin and no Sec-Fetch-Site: not a browser, so not another site's page.
  if (typeof origin !== "string") return true;
  try {
    return new URL(origin).host === headers.host;
  } catch {
    return false;
  }
}
