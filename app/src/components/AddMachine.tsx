// "Add a machine": put another machine's Leadline beside this Mac's, so the
// window can be turned to it. Two ways in. One of the owner's Shipyard
// boxes, which this computer reaches through a port Shipyard forwards: the
// dialog lists the boxes, opens the forward to the one chosen and checks
// that a Leadline answers through it. Or an address on this computer that
// a person forwarded themselves, checked the same way.
//
// Shipyard is asked for its boxes when this dialog opens and at no other
// time, and a forward is opened when a box is chosen: both run Shipyard's
// command line, through this computer's own daemon (lib/forwards.ts).

import { useCallback, useEffect, useId, useRef, useState, useSyncExternalStore, type FormEvent, type ReactNode } from "react";
import { CheckIcon, CircleAlertIcon, RefreshCwIcon } from "lucide-react";
import { api, localBase, type Box, type Fault } from "@/lib/api";
import { boxesOf, boxState } from "@/lib/discover";
import { answers, faultOf, reachBox, type Reached } from "@/lib/forwards";
import { useMachines } from "@/lib/live";
import { LOCAL_ID, loopbackUrl, machines, NOT_LOOPBACK, portOf, type Machine } from "@/lib/machines";
import { INSTALL_LINE, LEADLINE_PORT, noAnswer, sshLine, versionNote, type Probe } from "@/lib/reach";
import { cn } from "@/lib/utils";
import { keepTab } from "@/components/focus";
import { Code, CopyLine, Dot, PickOne } from "@/components/ui";
import { Button } from "@/components/ui/button";
import { Dialog, DialogDescription, DialogPopup, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";

// The dialog is one for the whole window, opened from the machine switcher
// and from the Machines view: a word here, so neither has to be handed the
// other's state.
let isOpen = false;
const openListeners = new Set<() => void>();
function setOpen(next: boolean) {
  if (isOpen === next) return;
  isOpen = next;
  for (const l of openListeners) l();
}
const subscribeOpen = (fn: () => void) => {
  openListeners.add(fn);
  return () => openListeners.delete(fn);
};

/** openAddMachine opens the dialog. Called from a press: opening it is what asks Shipyard for its boxes. */
export const openAddMachine = (): void => setOpen(true);

type Way = "shipyard" | "address";
const WAYS: { value: Way; label: string }[] = [
  { value: "shipyard", label: "A Shipyard box" },
  { value: "address", label: "By address" },
];

/** AddMachine is the dialog, mounted once. Its inside is made when it opens and dropped when it closes, so every opening asks afresh. */
export function AddMachine() {
  const open = useSyncExternalStore(subscribeOpen, () => isOpen);
  // A check under way is not walked away from by a stray press outside: its answer decides whether a forward stays open.
  const busy = useRef(false);
  return (
    <Dialog open={open} onOpenChange={(o) => !o && !busy.current && setOpen(false)}>
      <DialogPopup className="max-w-[600px] overflow-hidden" bottomStickOnMobile={false} onKeyDown={keepTab}>
        <Panel busy={busy} onClose={() => setOpen(false)} />
      </DialogPopup>
    </Dialog>
  );
}

// One height for every step, so nothing jumps as the answers arrive; it gives way in a window shorter than that.
const FRAME = "flex min-h-0 w-full flex-[0_1_520px] flex-col text-xs";

const Small = ({ children, className, id }: { children: ReactNode; className?: string; id?: string }) => (
  <p id={id} className={cn("text-[11.5px] leading-snug text-pretty text-muted-foreground", className)}>
    {children}
  </p>
);

/** Said is one line of outcome: a tick or a warning mark, what happened, and what to try. */
function Said({ ok, children, hint }: { ok: boolean; children: ReactNode; hint?: ReactNode }) {
  return (
    <div role={ok ? "status" : "alert"} className="flex items-start gap-2">
      {ok ? <CheckIcon aria-hidden className="mt-0.5 size-3.5 shrink-0 text-success-foreground" /> : <CircleAlertIcon aria-hidden className="mt-0.5 size-3.5 shrink-0 text-destructive-foreground" />}
      <div className="min-w-0">
        <p className={cn("selectable text-pretty", !ok && "text-destructive-foreground")}>{children}</p>
        {hint && <Small className="selectable mt-0.5">{hint}</Small>}
      </div>
    </div>
  );
}

/** What the dialog shows in place of its two ways once something has come of one. */
type Outcome =
  | { kind: "added"; machine: Machine; version: string; reused: boolean }
  | { kind: "silent"; box: string; port: number; probe: Probe; closed: boolean; opened: boolean };

function Panel({ busy, onClose }: { busy: { current: boolean }; onClose: () => void }) {
  const [way, setWay] = useState<Way>("shipyard");
  const [outcome, setOutcome] = useState<Outcome | null>(null);
  // This Mac's own Leadline version, to say when another machine's differs. Asked of this computer's daemon, like everything in the dialog.
  const [ours, setOurs] = useState<string>();
  useEffect(() => {
    let alive = true;
    api.probe(localBase()).then((p) => alive && p.state === "leadline" && setOurs(p.version));
    return () => {
      alive = false;
    };
  }, []);

  // A box that was chosen: the forward is opened, then asked through.
  const [trying, setTrying] = useState<string | null>(null);
  const [fault, setFault] = useState<{ box: string; fault: Fault } | null>(null);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const choose = useCallback(
    async (box: string) => {
      if (busy.current) return;
      busy.current = true;
      setTrying(box);
      setFault(null);
      // The port it had before, when it is on the list already (Check again after an install, or a box added twice).
      const was = machines.get().machines.find((m) => m.box === box && m.via === "shipyard");
      // A forward this try opens is closed again if nothing answers through it, unless the box is already a machine that keeps it.
      const got: Reached = await reachBox(box, was ? Number(portOf(was)) : undefined, !was);
      busy.current = false;
      if (got.state === "leadline") {
        const machine = machines.addBox(box, got.forward);
        machines.seen(machine.id, got.version);
        if (alive.current) setOutcome({ kind: "added", machine, version: got.version, reused: !got.forward.opened });
      } else if (!alive.current) {
        return;
      } else if (got.state === "failed") {
        setFault({ box, fault: got.fault });
      } else {
        setOutcome({ kind: "silent", box, port: got.forward.local, probe: got.probe, closed: got.closed, opened: got.forward.opened });
      }
      if (alive.current) setTrying(null);
    },
    [busy],
  );

  const heading = (
    <header className="flex shrink-0 flex-col gap-1 border-b py-3 pr-12 pl-5">
      <DialogTitle className="text-[15px] leading-normal font-medium">Add a machine</DialogTitle>
      <DialogDescription className="text-xs text-pretty">Another machine's Leadline, shown in this window. Leadline only ever talks to localhost, so it is reached through a port forwarded to this Mac.</DialogDescription>
    </header>
  );

  if (outcome?.kind === "added") {
    return (
      <div className={FRAME}>
        {heading}
        <Added outcome={outcome} ours={ours} onClose={onClose} />
      </div>
    );
  }
  if (outcome?.kind === "silent") {
    return (
      <div className={FRAME}>
        {heading}
        <NotThere outcome={outcome} trying={trying !== null} fault={fault?.fault ?? null} onAgain={() => void choose(outcome.box)} onBack={() => setOutcome(null)} onClose={onClose} />
      </div>
    );
  }

  return (
    <div className={FRAME}>
      {heading}
      <div className="flex shrink-0 items-center px-5 pt-3">
        <PickOne label="How the machine is reached" value={way} options={WAYS} onChange={(w) => !busy.current && setWay(w)} />
      </div>
      {way === "shipyard" ? (
        <Boxes trying={trying} fault={fault} onChoose={(b) => void choose(b)} onAddress={() => setWay("address")} onClose={onClose} />
      ) : (
        <ByAddress busy={busy} onAdded={(machine, version) => setOutcome({ kind: "added", machine, version, reused: true })} onClose={onClose} />
      )}
    </div>
  );
}

function Foot({ children }: { children: ReactNode }) {
  return <footer className="mt-auto flex shrink-0 items-center justify-end gap-2 border-t bg-muted/72 px-5 py-3">{children}</footer>;
}

type Listing = { state: "asking" } | { state: "none"; why: Fault; missing: boolean } | { state: "ready"; boxes: Box[]; at: number };

/** Boxes lists the owner's Shipyard boxes, asked for as this is first drawn, each with its state and the way to add it. */
function Boxes({ trying, fault, onChoose, onAddress, onClose }: { trying: string | null; fault: { box: string; fault: Fault } | null; onChoose: (box: string) => void; onAddress: () => void; onClose: () => void }) {
  const id = useId();
  const [listing, setListing] = useState<Listing>({ state: "asking" });
  const [asking, setAsking] = useState(false);
  const { machines: listed } = useMachines();
  const ask = useCallback((fresh = false) => {
    let wanted = true;
    // A list that is there stays drawn while it is asked for again: nothing jumps under the person.
    setListing((was) => (was.state === "ready" ? was : { state: "asking" }));
    setAsking(true);
    api.boxes(fresh).then(
      (s) => {
        if (!wanted) return;
        if (!s) return setListing({ state: "none", missing: false, why: { code: "old", message: "This Mac's Leadline is too old to ask Shipyard.", hint: "Update Leadline on this Mac, then try again." } });
        if (!s.available) {
          return setListing({ state: "none", missing: !s.installed, why: s.installed ? (s.problem ?? { code: "", message: "Shipyard isn't answering." }) : { code: "shipyard_not_found", message: "Shipyard isn't installed on this Mac." } });
        }
        setListing({ state: "ready", boxes: boxesOf(s), at: Date.now() });
      },
      (e: unknown) => wanted && setListing({ state: "none", missing: false, why: faultOf(e) }),
    ).finally(() => wanted && setAsking(false));
    return () => {
      wanted = false;
    };
  }, []);
  // The dialog opened: this is the one moment Shipyard is asked for its boxes.
  useEffect(() => ask(), [ask]);
  // A box that turned out to be offline or gone since the list was made: the list is asked for again, so its row says so.
  const stale = fault?.fault.code === "box_offline" || fault?.fault.code === "no_box" ? fault.box : "";
  useEffect(() => {
    if (stale) return ask(true);
  }, [stale, ask]);

  return (
    <>
      <section aria-labelledby={`${id}-h`} className="flex min-h-0 flex-1 flex-col gap-2 px-5 pt-3 pb-4">
        <div className="flex shrink-0 items-center gap-2">
          <h3 id={`${id}-h`} className="font-medium">
            Your Shipyard boxes
          </h3>
          {listing.state === "ready" && (
            <Button variant="ghost" size="xs" onClick={() => ask(true)} disabled={trying !== null} loading={asking} className="ml-auto font-normal text-muted-foreground hover:text-foreground">
              <RefreshCwIcon />
              Ask again
            </Button>
          )}
        </div>
        {listing.state === "asking" ? (
          <p role="status" className="flex items-center gap-2 rounded-lg border px-3 py-6 text-muted-foreground">
            <Spinner aria-hidden className="size-3.5" />
            Asking Shipyard for your boxes…
          </p>
        ) : listing.state === "none" ? (
          <div className="flex flex-col items-start gap-2.5 rounded-lg border px-3 py-3">
            {listing.missing ? (
              <>
                <p role="status">{listing.why.message}</p>
                <Small>Shipyard keeps an encrypted connection to each of your machines and forwards the port for you. Without it, forward the machine's port yourself and add it by its address.</Small>
              </>
            ) : (
              <Said ok={false} hint={listing.why.hint}>
                {listing.why.message}
              </Said>
            )}
            <div className="flex gap-2">
              {!listing.missing && (
                <Button size="sm" variant="outline" onClick={() => ask(true)} className="font-normal">
                  Ask again
                </Button>
              )}
              <Button size="sm" variant="outline" onClick={onAddress} className="font-normal">
                Add by address
              </Button>
            </div>
          </div>
        ) : listing.boxes.length === 0 ? (
          <div className="flex flex-col items-start gap-2.5 rounded-lg border px-3 py-3">
            <p role="status">Shipyard has no box other than this Mac.</p>
            <Small>Pair a machine in Shipyard and ask again, or forward a machine's port yourself and add it by its address.</Small>
            <Button size="sm" variant="outline" onClick={onAddress} className="font-normal">
              Add by address
            </Button>
          </div>
        ) : (
          <ul aria-labelledby={`${id}-h`} className="min-h-0 overflow-y-auto rounded-lg border">
            {listing.boxes.map((b) => {
              const on = listed.find((m) => m.via === "shipyard" && m.box === b.name);
              const online = b.state === "online";
              const busyHere = trying === b.name;
              const wrong = fault?.box === b.name ? fault.fault : null;
              return (
                <li key={b.name} className="grid grid-cols-[14px_minmax(0,1fr)_auto] items-center gap-x-2.5 border-t px-3 py-2 first:border-t-0">
                  <span className="flex justify-center">
                    <Dot tone={online ? "ok" : b.state === "connecting" ? "wait" : "quiet"} />
                  </span>
                  <div className="min-w-0">
                    <p className="flex min-w-0 items-baseline gap-2">
                      <span className="selectable truncate text-[13px]">{b.name}</span>
                      <span className="shrink-0 text-[11.5px] text-muted-foreground">{boxState(b, listing.at)}</span>
                    </p>
                    {busyHere ? (
                      <Small>
                        <span role="status">Opening a forward to its Leadline, and asking through it…</span>
                      </Small>
                    ) : wrong ? (
                      <Small className="text-destructive-foreground">
                        <span role="alert">{wrong.message}</span> <span className="text-muted-foreground">{wrong.hint}</span>
                      </Small>
                    ) : on ? (
                      <Small>
                        On the list{on.name === b.name ? "" : ` as ${on.name}`}, through port {portOf(on)}.
                      </Small>
                    ) : !online ? (
                      <Small>{b.state === "offline" ? "Shipyard can't reach it now, so no forward can be opened." : b.state === "untrusted" ? "Pair it again in Shipyard first." : "Shipyard is still connecting to it. Ask again in a moment."}</Small>
                    ) : null}
                  </div>
                  {on ? (
                    <span className="text-[11.5px] text-muted-foreground">Added</span>
                  ) : (
                    <Button size="sm" variant="outline" onClick={() => onChoose(b.name)} disabled={!online || (trying !== null && !busyHere)} loading={busyHere} aria-label={`Add ${b.name}`} className="font-normal">
                      Add
                    </Button>
                  )}
                </li>
              );
            })}
          </ul>
        )}
        {listing.state === "ready" && listing.boxes.length > 0 && (
          <Small className="shrink-0">
            Adding a box opens a forward in Shipyard from a port on this Mac to the box's port {LEADLINE_PORT}, then asks through it for Leadline. Nothing is installed or changed on the box.
          </Small>
        )}
      </section>
      <Foot>
        <Button variant="outline" onClick={onClose} disabled={trying !== null}>
          Close
        </Button>
      </Foot>
    </>
  );
}

/** NotThere is a box whose forward opened and no Leadline answered through it: said plainly, with how to install one and the way to ask again. */
function NotThere({ outcome, trying, fault, onAgain, onBack, onClose }: { outcome: Extract<Outcome, { kind: "silent" }>; trying: boolean; fault: Fault | null; onAgain: () => void; onBack: () => void; onClose: () => void }) {
  const { box, port, probe, closed, opened } = outcome;
  const head = useRef<HTMLHeadingElement>(null);
  useEffect(() => head.current?.focus(), []);
  return (
    <>
      <div className="flex min-h-0 flex-1 flex-col gap-3.5 overflow-y-auto px-5 py-4">
        <div className="flex items-start gap-2">
          <CircleAlertIcon aria-hidden className="mt-0.5 size-3.5 shrink-0 text-warning-foreground" />
          <div className="min-w-0">
            <h3 ref={head} tabIndex={-1} className="font-medium outline-none">
              {probe.state === "other" ? `Something else answers on ${box}'s port ${LEADLINE_PORT}` : `Leadline isn't answering on ${box}`}
            </h3>
            <Small className="mt-0.5 text-xs">
              {noAnswer(probe, box, String(port))}{" "}
              {closed
                ? "Leadline has closed the forward it opened for this."
                : opened
                  ? "The forward Leadline opened for this could not be closed again: berth forwards lists it."
                  : "That forward was there before Leadline looked, so it is left open."}{" "}
              {probe.state === "other" ? `Leadline's daemon listens on port ${LEADLINE_PORT}, and another program has it there.` : `${box} was not added. Leadline is probably not installed there yet, or its daemon is stopped.`}
            </Small>
          </div>
        </div>

        <section className="flex flex-col gap-1.5">
          <h4 className="font-medium">Install Leadline on {box}</h4>
          <Small>Run this on {box}, in a terminal there. It installs the daemon as a user service that listens on that machine's localhost only.</Small>
          <CopyLine text={INSTALL_LINE} label="Copy the install command" wrap />
          <Small>
            Leadline can't install itself on a box from this window yet. It also has no published release yet, so for now this line has nothing to download: until one is out, copy a tarball made by <Code>make release</Code> to {box} and install it with <Code>sh install.sh --from</Code>.
          </Small>
        </section>

        <div role="status" className="flex items-center gap-2.5 rounded-lg bg-muted px-3 py-2.5">
          {trying ? <Spinner aria-hidden className="size-3.5 shrink-0" /> : <Dot tone="wait" className="mx-[3.5px]" />}
          <p className="min-w-0 flex-1">{trying ? `Opening the forward to ${box} and asking again…` : `Once Leadline is running on ${box}, check again.`}</p>
        </div>
        {fault && !trying && (
          <Said ok={false} hint={fault.hint}>
            {fault.message}
          </Said>
        )}
      </div>
      <Foot>
        <Button variant="outline" onClick={onBack} disabled={trying} className="mr-auto font-normal">
          Back to the boxes
        </Button>
        <Button variant="outline" onClick={onClose} disabled={trying}>
          Close
        </Button>
        <Button onClick={onAgain} loading={trying}>
          Check again
        </Button>
      </Foot>
    </>
  );
}

/** Added is the dialog once a machine is on the list: what answered, and the way to look at it. */
function Added({ outcome, ours, onClose }: { outcome: Extract<Outcome, { kind: "added" }>; ours: string | undefined; onClose: () => void }) {
  const { machine, version } = outcome;
  const note = versionNote(version, ours);
  const head = useRef<HTMLHeadingElement>(null);
  useEffect(() => head.current?.focus(), []);
  return (
    <>
      <div className="flex min-h-0 flex-1 flex-col gap-3.5 overflow-y-auto px-5 py-4">
        <div className="flex items-start gap-2">
          <CheckIcon aria-hidden className="mt-0.5 size-3.5 shrink-0 text-success-foreground" />
          <div className="min-w-0">
            <h3 ref={head} tabIndex={-1} className="font-medium outline-none">
              {machine.name} is on the list
            </h3>
            <Small className="mt-0.5 text-xs">
              <span role="status">
                Leadline {version} answers {machine.via === "shipyard" ? `on ${machine.box}, through port ${portOf(machine)} on this Mac` : `at ${machine.url.replace("http://", "")}`}.
              </span>{" "}
              {machine.via === "shipyard" && (outcome.reused ? "The forward to it was already open in Shipyard, so Leadline uses that one." : "Leadline opened that forward in Shipyard, and closes it when you remove the machine.")}
            </Small>
          </div>
        </div>
        {note && (
          <div className="flex items-start gap-2">
            <CircleAlertIcon aria-hidden className="mt-0.5 size-3.5 shrink-0 text-warning-foreground" />
            <Small className="text-xs text-foreground">{note}</Small>
          </div>
        )}
        <Small>It is in the machine switcher at the top of the sidebar now. Looking at it turns every view to what {machine.name} recorded; this Mac's own data stays where it is.</Small>
      </div>
      <Foot>
        <Button variant="outline" onClick={onClose}>
          Done
        </Button>
        <Button
          onClick={() => {
            onClose();
            machines.use(machine.id);
          }}
        >
          Look at {machine.name}
        </Button>
      </Foot>
    </>
  );
}

/** ByAddress adds a machine whose port a person forwarded to this computer themselves, once a Leadline answers there. */
function ByAddress({ busy, onAdded, onClose }: { busy: { current: boolean }; onAdded: (machine: Machine, version: string) => void; onClose: () => void }) {
  const id = useId();
  const [address, setAddress] = useState("");
  const [name, setName] = useState("");
  const [checking, setChecking] = useState(false);
  const [wrong, setWrong] = useState<{ message: string; hint?: string } | null>(null);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const add = async (e: FormEvent) => {
    e.preventDefault();
    if (checking) return;
    const url = loopbackUrl(address);
    if (!url) return setWrong({ message: address.trim() ? NOT_LOOPBACK : "Type the address the machine's port is forwarded to, such as 127.0.0.1:4336." });
    const at = machines.get().machines.find((m) => m.url === url);
    if (at) return setWrong({ message: at.id === LOCAL_ID ? "That is this Mac's own Leadline, which is on the list already." : `That address is on the list already, as ${at.name}.` });
    setWrong(null);
    setChecking(true);
    busy.current = true;
    const probe = await answers(url, 2);
    busy.current = false;
    if (!alive.current) return;
    setChecking(false);
    if (probe.state !== "leadline") {
      const bare = url.replace("http://", "");
      return setWrong(
        probe.state === "other"
          ? { message: `Something answers at ${bare}, and it isn't Leadline.`, hint: `Forward the port to the machine's port ${LEADLINE_PORT}, where its Leadline listens.` }
          : { message: `Nothing answers at ${bare}.`, hint: "Check that the forward is running and that Leadline is on the machine. It was not added." },
      );
    }
    const machine = machines.add(name, url);
    machines.seen(machine.id, probe.version);
    onAdded(machine, probe.version);
  };

  return (
    <form onSubmit={add} className="flex min-h-0 flex-1 flex-col">
      <div className="flex min-h-0 flex-1 flex-col gap-3.5 overflow-y-auto px-5 pt-3 pb-4">
        <section className="flex flex-col gap-1.5">
          <h3 className="font-medium">Forward its port to this Mac</h3>
          <Small>
            For a machine you reach yourself. Its Leadline listens on that machine's port {LEADLINE_PORT}; this carries it to port 4336 here. Put your own user and host in place of <Code>user@host</Code>, and leave it running.
          </Small>
          <CopyLine text={sshLine()} label="Copy the ssh command" />
        </section>

        <section className="flex flex-col gap-1.5">
          <label htmlFor={`${id}-address`} className="font-medium">
            The address on this Mac
          </label>
          <Input
            id={`${id}-address`}
            value={address}
            onChange={(e) => {
              setAddress(e.target.value);
              setWrong(null);
            }}
            placeholder="127.0.0.1:4336"
            spellCheck={false}
            autoComplete="off"
            autoCapitalize="off"
            autoCorrect="off"
            autoFocus
            aria-invalid={wrong ? true : undefined}
            aria-describedby={`${id}-said`}
            className="max-w-72 font-mono *:text-[12px]"
          />
          <div id={`${id}-said`} className="min-h-8">
            {checking ? (
              <p role="status" className="flex items-center gap-2 text-muted-foreground">
                <Spinner aria-hidden className="size-3.5" />
                Asking whether Leadline answers there…
              </p>
            ) : wrong ? (
              <Said ok={false} hint={wrong.hint}>
                {wrong.message}
              </Said>
            ) : (
              <Small>Only an address on this computer is taken: 127.0.0.1 or localhost, with the port you forwarded.</Small>
            )}
          </div>
        </section>

        <section className="flex flex-col gap-1.5">
          <label htmlFor={`${id}-name`} className="font-medium">
            Name
          </label>
          <Input id={`${id}-name`} value={name} onChange={(e) => setName(e.target.value)} placeholder="staging" spellCheck={false} autoComplete="off" className="max-w-72" />
          <Small>What to call it in the switcher. Without one it goes by its address.</Small>
        </section>
      </div>
      <Foot>
        <Button variant="outline" onClick={onClose} disabled={checking}>
          Cancel
        </Button>
        <Button type="submit" loading={checking}>
          Check and add
        </Button>
      </Foot>
    </form>
  );
}
