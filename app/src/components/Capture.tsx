import { useEffect, useRef, useState } from "react";
import { api, ApiError, type Connection, type Fault } from "@/lib/api";
import { captureWord } from "@/lib/sources";
import { Tip } from "@/components/ui";
import { Switch } from "@/components/ui/switch";

export type Capturing = {
  /** What the switch shows: the connection's own state, or the one just asked for while the daemon answers. */
  on: boolean;
  /** A change is with the daemon. */
  busy: boolean;
  /** Why the last change was refused, in the daemon's words. */
  fault: Fault | null;
  set: (on: boolean) => void;
};

/**
 * useCapture turns a saved connection's capture on and off. The daemon may
 * refuse (a remote database can't be captured, the proxy's port is taken):
 * the switch then goes back, and `fault` says why.
 */
export function useCapture(conn: Connection, onChanged?: () => void): Capturing {
  const [asked, setAsked] = useState<boolean | null>(null);
  const [fault, setFault] = useState<Fault | null>(null);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => void (alive.current = false);
  }, []);
  // The daemon's own word, once it arrives, is what the switch shows.
  useEffect(() => setAsked(null), [conn.capture]);

  const set = (on: boolean) => {
    if (asked !== null) return;
    setAsked(on);
    setFault(null);
    api.editConnection(conn.id, { capture: on }).then(
      () => {
        // The switch holds what was asked until the list is fetched again and
        // says the same (the effect above); if that never comes, it lets go.
        onChanged?.();
        setTimeout(() => alive.current && setAsked(null), 3000);
      },
      (e: unknown) => {
        if (!alive.current) return;
        setAsked(null);
        setFault(e instanceof ApiError ? { code: e.code, message: e.message, hint: e.hint || undefined } : { code: "", message: e instanceof Error ? e.message : String(e) });
      },
    );
  };
  return { on: asked ?? conn.capture, busy: asked !== null, fault, set };
}

/**
 * CaptureSwitch is the one choice a connection has: record everything it is
 * asked, or only read its statistics. Its tooltip is the daemon's own note
 * on what capture does and costs, or why it isn't offered.
 */
export function CaptureSwitch({ conn, capture }: { conn: Connection; capture: Capturing }) {
  const label = `Capture every ${captureWord(conn.kind)} of ${conn.name}`;
  return (
    <Tip label={<span className="block max-w-80 text-pretty">{conn.capture_offer.note}</span>}>
      {/* The tooltip hangs on a span: a disabled switch takes no pointer. */}
      <span className="inline-flex shrink-0" onClick={(e) => e.stopPropagation()}>
        <Switch aria-label={label} checked={capture.on} disabled={!conn.capture_offer.available || capture.busy} onCheckedChange={capture.set} />
      </span>
    </Tip>
  );
}
