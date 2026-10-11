import { useEffect, useState } from "react";
import { invoke } from "@tauri-apps/api/core";
import { listen } from "@tauri-apps/api/event";

import type { UpdateStatus } from "./updateText";

const inTauri = "__TAURI_INTERNALS__" in window;

/**
 * useUpdate follows the app's updates to itself. The app finds, downloads
 * and checks a newer Leadline on its own (src-tauri/src/update.rs); the
 * window only hears when one is ready. It is null in a browser tab, where
 * there is no app to update.
 */
export function useUpdate(): UpdateStatus | null {
  const [status, setStatus] = useState<UpdateStatus | null>(null);
  useEffect(() => {
    if (!inTauri) return;
    let alive = true;
    // Listen first, then ask: a change in between is not missed.
    const stop = listen<UpdateStatus>("update", (e) => alive && setStatus(e.payload));
    stop.then(() => invoke<UpdateStatus>("update_status")).then((s) => alive && setStatus(s), () => {});
    return () => {
      alive = false;
      stop.then((off) => off(), () => {});
    };
  }, []);
  return status;
}

/** installUpdate puts the downloaded update in place and restarts the app. Only ever from a press. */
export function installUpdate(): void {
  invoke("update_install").catch(() => {});
}
