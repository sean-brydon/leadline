// What the window says about an update, kept apart from React and from the
// app's own API so it can be tested (lib/update.ts has the rest).

/** What the desktop app says about updates to itself (src-tauri/src/update.rs). */
export type UpdateStatus =
  | { state: "idle" }
  | { state: "ready"; version: string }
  | { state: "installing"; version: string }
  | { state: "failed"; version: string; reason: string };

/** The one control an update gets: its words, what it says when pointed at, and whether it can be pressed. */
export type UpdateOffer = { label: string; tip: string; busy: boolean };

/**
 * updateOffer is what to show for a status, or null for nothing: the window
 * says nothing about updates until one is downloaded and waiting.
 */
export function updateOffer(status: UpdateStatus | null): UpdateOffer | null {
  if (!status || status.state === "idle") return null;
  const v = status.version;
  switch (status.state) {
    case "ready":
      return {
        label: "Restart to update",
        tip: `Leadline ${v} is downloaded. Restarting installs it and starts its daemon, so nothing is recorded for a second or two. What is already recorded stays.`,
        busy: false,
      };
    case "installing":
      return { label: "Updating", tip: `Installing Leadline ${v}. The window comes back by itself.`, busy: true };
    case "failed":
      return {
        label: "Update didn't install",
        tip: `Leadline ${v} couldn't be put in place: ${status.reason}. Press to try again; if the app isn't in Applications, move it there first.`,
        busy: false,
      };
  }
}
