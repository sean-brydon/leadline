import React from "react";
import ReactDOM from "react-dom/client";
import App from "./App";
import { machines } from "./lib/machines";
import "./styles.css";

// Development only. Leadline will show daemons on other machines (a VPS,
// reached through a port forwarded to this one), and lib/machines.ts already
// holds the list, but there is no screen for adding to it yet. In `pnpm dev`
// or `pnpm tauri dev`, from the console:
//
//   leadlineMachines.add("Staging", "http://127.0.0.1:4336")  // returns the machine; its address must be on this computer
//   leadlineMachines.use(id)       // show that machine's daemon ("local" is this Mac)
//   leadlineMachines.remove(id)
//   leadlineMachines.list()
//
// The list is kept in localStorage ("leadline.machines"). With two or more
// machines on it a switcher appears at the top of the sidebar. A released
// build has no such global, so its list is this Mac alone.
if (import.meta.env.DEV) {
  (window as unknown as Record<string, unknown>).leadlineMachines = {
    add: machines.add,
    use: machines.use,
    remove: machines.remove,
    list: () => machines.get().machines,
  };
}

ReactDOM.createRoot(document.getElementById("root") as HTMLElement).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
);
