import React from "react";
import ReactDOM from "react-dom/client";
import App from "./App";
import { machines } from "./lib/machines";
import "./styles.css";

// Development only. Machines are added, renamed and removed in the Machines
// view (the machine switcher at the top of the sidebar leads to it). This
// is the same list from the console, in `pnpm dev` or `pnpm tauri dev`, for
// putting the window in a state without clicking through it:
//
//   leadlineMachines.add("Staging", "http://127.0.0.1:4336")  // by address; returns the machine. Its address must be on this computer
//   leadlineMachines.addBox("hetzner-1", { id, url, opened })  // a Shipyard box, by a forward that is already open (what POST /api/shipyard/forwards answers)
//   leadlineMachines.use(id)       // show that machine's Leadline ("local" is this Mac)
//   leadlineMachines.rename(id, "Staging box")
//   leadlineMachines.remove(id)    // forgets it; unlike the view, it closes no forward
//   leadlineMachines.list()
//
// Nothing here checks that a Leadline answers, or opens or closes a forward:
// the view does. The list is kept in localStorage ("leadline.machines"). A
// released build has no such global.
if (import.meta.env.DEV) {
  (window as unknown as Record<string, unknown>).leadlineMachines = {
    add: machines.add,
    addBox: machines.addBox,
    use: machines.use,
    rename: machines.rename,
    remove: machines.remove,
    list: () => machines.get().machines,
  };
}

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
