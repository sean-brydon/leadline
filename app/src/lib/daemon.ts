import { useEffect, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";

/**
 * What the desktop app says about the daemon, which it finds or starts by
 * itself (src-tauri/src/daemon.rs). `owned` is whether this app started it.
 * A failure has a `reason`, a `hint` for what to do, the end of the
 * daemon's log in `detail` and that file's path in `log`; the last two may
 * be empty.
 */
export type DaemonStatus =
  | { state: "starting" }
  | { state: "running"; owned: boolean }
  | { state: "failed"; reason: string; hint: string; detail: string; log: string };

const inTauri = "__TAURI_INTERNALS__" in window;

/**
 * useDaemon follows the app's daemon. It is null in a browser tab, where
 * there is no app to run one.
 */
export function useDaemon(): DaemonStatus | null {
  const [status, setStatus] = useState<DaemonStatus | null>(inTauri ? { state: "starting" } : null);
  useEffect(() => {
    if (!inTauri) return;
    let alive = true;
    // Listen first, then ask: a change in between is not missed.
    const stop = listen<DaemonStatus>("daemon", (e) => alive && setStatus(e.payload));
    stop.then(() => invoke<DaemonStatus>("daemon_status")).then((s) => alive && setStatus(s), () => {});
    return () => {
      alive = false;
      stop.then((off) => off(), () => {});
    };
  }, []);
  return status;
}

/** startDaemon asks the app to try again after a failure. */
export function startDaemon(): void {
  invoke("daemon_start").catch(() => {});
}
