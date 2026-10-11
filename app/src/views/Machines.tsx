// The Machines view: every machine whose Leadline this window can show,
// how each is reached, whether its Leadline answers, what it holds, and
// the ways to look at one, rename it and take it off the list. Adding is
// the dialog in components/AddMachine.tsx.
//
// What a row shows comes from three plain reads of that machine's own
// daemon (health, summary, storage), asked while this view is open.
// Shipyard is not asked anything here until a person presses Reconnect or
// Remove on a box.

import { useEffect, useMemo, useRef, useState, useSyncExternalStore, type ComponentType, type FormEvent, type ReactNode } from "react";
import { CableIcon, CircleAlertIcon, EllipsisIcon, MonitorIcon, PlusIcon, ServerIcon } from "lucide-react";
import { api, type Fault, type Storage, type Summary } from "@/lib/api";
import { forget, link, reconnect, type Link } from "@/lib/forwards";
import { fmtKeep } from "@/lib/kept";
import { useMachines } from "@/lib/live";
import { closing, hereOf, LOCAL_ID, machines, portOf, reach, reachLine, removing, type Machine } from "@/lib/machines";
import { answerLine, holdsLine, INSTALL_LINE, noAnswer, seenLine, sourcesLine, versionNote, type Probe } from "@/lib/reach";
import { cn } from "@/lib/utils";
import { openAddMachine } from "@/components/AddMachine";
import { keepTab } from "@/components/focus";
import { Code, CopyLine, Dot, Rows, Surface } from "@/components/ui";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogDescription, DialogFooter, DialogHeader, DialogPopup, DialogTitle } from "@/components/ui/dialog";
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty";
import { Input } from "@/components/ui/input";
import { Menu, MenuItem, MenuPopup, MenuSeparator, MenuTrigger } from "@/components/ui/menu";
import { Spinner } from "@/components/ui/spinner";
import { TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";

type Icon = ComponentType<{ className?: string; "aria-hidden"?: boolean }>;

// Each machine is asked how it is this often while the view is open.
const ASK_EVERY_MS = 10_000;

/** What a machine's own daemon said the last time it was asked. `probe` undefined: not answered yet. */
type Status = { probe?: Probe; summary?: Summary | null; storage?: Storage | null; atMs?: number };

/** useLink is a machine's reconnecting state, shared with the window's "no answer" screen. */
function useLink(id: string): Link {
  return useSyncExternalStore(link.subscribe, () => link.of(id));
}

/**
 * useStatuses asks every machine on the list how it is: whether a Leadline
 * answers at its address, and from one that does, what it holds. Three
 * reads of each daemon, by its address; nothing is asked of Shipyard.
 */
function useStatuses(list: Machine[]): { statuses: Record<string, Status>; again: (id: string) => void } {
  const [statuses, setStatuses] = useState<Record<string, Status>>({});
  const ask = useRef<(m: Machine) => void>(() => {});
  const key = list.map((m) => `${m.id}\n${m.url}`).join("\n");
  const listRef = useRef(list);
  listRef.current = list;
  useEffect(() => {
    let alive = true;
    const out = new Set<string>();
    ask.current = (m: Machine) => {
      if (out.has(m.id)) return;
      out.add(m.id);
      const url = m.url;
      void (async () => {
        const probe = await api.probe(url);
        let summary: Summary | null = null;
        let storage: Storage | null = null;
        if (probe.state === "leadline") {
          [summary, storage] = await Promise.all([api.summaryAt(url).catch(() => null), api.storageAt(url).catch(() => null)]);
          machines.seen(m.id, probe.version);
        }
        out.delete(m.id);
        if (alive) setStatuses((was) => ({ ...was, [m.id]: { probe, summary, storage, atMs: Date.now() } }));
      })();
    };
    const all = () => listRef.current.forEach((m) => ask.current(m));
    all();
    const timer = setInterval(all, ASK_EVERY_MS);
    return () => {
      alive = false;
      clearInterval(timer);
    };
    // A machine added, removed or moved to another port: ask afresh.
  }, [key]);
  const again = (id: string) => {
    const m = listRef.current.find((x) => x.id === id);
    if (m) ask.current(m);
  };
  return { statuses, again };
}

/**
 * Machines lists the machines and what can be done with each. The one the
 * window is showing is marked; "Look at it" turns the window to another.
 */
export function Machines() {
  const { machines: list, current } = useMachines();
  const { statuses, again } = useStatuses(list);
  const [renaming, setRenaming] = useState<Machine | null>(null);
  const [removingMachine, setRemoving] = useState<Machine | null>(null);
  // What the others' versions are compared with: this Mac's own.
  const ours = statuses[LOCAL_ID]?.probe?.state === "leadline" ? statuses[LOCAL_ID].probe.version : undefined;
  const nowMs = Date.now();

  return (
    <div className="@container h-full overflow-y-auto">
      <div className="mx-auto flex w-full max-w-[1120px] flex-col gap-3 p-3">
        <div className="flex items-start gap-3 px-1 pt-1">
          <div className="min-w-0 flex-1">
            <h2 className="text-[15px] font-medium">The machines this window can show</h2>
            <p className="mt-0.5 text-pretty text-muted-foreground">
              Each has a Leadline of its own, which keeps its own data. Add one of your Shipyard boxes, or a machine whose port you forwarded here, and turn the window to it.
            </p>
          </div>
          <Button size="sm" onClick={openAddMachine} aria-haspopup="dialog" className="shrink-0">
            <PlusIcon />
            Add a machine
          </Button>
        </div>

        <Surface render={<section aria-label="Machines" />} className="w-full min-w-0 overflow-hidden">
          <Rows>
            <TableHeader>
              <TableRow>
                <TableHead className="w-[30%]">Machine</TableHead>
                <TableHead className="w-[25%]">Its Leadline</TableHead>
                <TableHead className="hidden @2xl:table-cell">Holds</TableHead>
                <TableHead className="hidden w-[92px] @4xl:table-cell">Last reached</TableHead>
                <TableHead className="w-[142px]">
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.map((m) => (
                <Line key={m.id} machine={m} showing={m.id === current} status={statuses[m.id]} ours={ours} nowMs={nowMs} onAsk={() => again(m.id)} onRename={() => setRenaming(m)} onRemove={() => setRemoving(m)} />
              ))}
            </TableBody>
          </Rows>
          {list.length === 1 && (
            <p className="border-t px-3 py-3 text-pretty text-muted-foreground">
              Only this Mac so far. If your code runs on another machine, install Leadline there and add it: its requests, queries and logs show here, and are kept on that machine.
            </p>
          )}
        </Surface>

        <p className="px-1 text-[11.5px] leading-snug text-pretty text-muted-foreground">
          Leadline only ever talks to localhost. Another machine's Leadline listens on that machine's own localhost, and is reached through a port forwarded to this Mac: by Shipyard for a box, or by you.
        </p>
      </div>

      <Rename machine={renaming} onClose={() => setRenaming(null)} />
      <Remove machine={removingMachine} onClose={() => setRemoving(null)} />
    </div>
  );
}

const ICON: Record<ReturnType<typeof reach>, Icon> = { local: MonitorIcon, shipyard: ServerIcon, address: CableIcon };

type LineProps = { machine: Machine; showing: boolean; status: Status | undefined; ours: string | undefined; nowMs: number; onAsk: () => void; onRename: () => void; onRemove: () => void };

function Line({ machine: m, showing, status, ours, nowMs, onAsk, onRename, onRemove }: LineProps) {
  const how = reach(m);
  const Glyph = ICON[how];
  const probe = status?.probe;
  const up = probe?.state === "leadline";
  const linked = useLink(m.id);
  const trying = linked.state === "trying";
  const local = m.id === LOCAL_ID;
  const note = up && !local ? versionNote(probe.version, ours) : "";
  const holds = up ? holdsLine({ summary: status?.summary, storage: status?.storage }) : "";
  const count = up ? sourcesLine({ summary: status?.summary }) : "";
  const keep = up && status?.storage ? (status.storage.retain_ms > 0 ? `kept for ${fmtKeep(status.storage.retain_ms)}` : "kept until removed") : "";

  const again = () => {
    if (how === "shipyard") void reconnect(m).then(onAsk);
    else onAsk();
  };

  return (
    <TableRow className="hover:bg-transparent [&>td]:align-top" aria-current={showing ? "true" : undefined}>
      <TableCell>
        <div className="flex min-w-0 items-center gap-2.5">
          <span className="flex size-[30px] shrink-0 items-center justify-center rounded-lg bg-muted text-muted-foreground">
            <Glyph aria-hidden className="size-3.5" />
          </span>
          <div className="min-w-0">
            <div className="flex min-w-0 items-center gap-1.5">
              <span className="selectable truncate text-[13px]">{m.name}</span>
              {showing && (
                <Badge variant="secondary" className="shrink-0 font-normal text-muted-foreground">
                  showing
                </Badge>
              )}
            </div>
            <div className="selectable text-[11.5px] leading-snug text-pretty text-muted-foreground">{reachLine(m)}</div>
          </div>
        </div>
      </TableCell>
      <TableCell>
        <div className="min-w-0">
          <div className="flex min-w-0 items-center gap-1.5" role="status">
            {!probe || trying ? <Spinner aria-hidden className="size-3 shrink-0 text-muted-foreground" /> : <Dot tone={up ? "ok" : "err"} />}
            <span className={cn("truncate", !up && probe && !trying && "text-destructive-foreground")}>{trying ? "Opening the forward…" : answerLine(probe)}</span>
          </div>
          {up && note && <div className="text-[11.5px] leading-snug text-pretty text-warning-foreground">{note}</div>}
          {/* Where the window is too narrow for the column beside this one, what it holds is said here. */}
          {up && holds && <div className="truncate text-[11.5px] text-muted-foreground tabular-nums @2xl:hidden">{holds}</div>}
          {probe && !up && !trying && (
            <div className="text-[11.5px] leading-snug text-pretty text-muted-foreground">
              {linked.state === "failed" ? (
                <span role="alert">
                  <span className="text-destructive-foreground">{linked.fault.message}</span> {linked.fault.hint}
                </span>
              ) : local ? (
                "Its daemon isn't running."
              ) : (
                noAnswer(probe, how === "shipyard" ? (m.box ?? m.name) : "it", portOf(m))
              )}{" "}
              <Button variant="link" size="xs" onClick={again} className="h-auto px-0 text-[11.5px] font-normal text-foreground underline sm:h-auto sm:text-[11.5px]">
                {how === "shipyard" ? "Reconnect" : "Ask again"}
              </Button>
              {/* Where the window is too narrow for the last column, when it last answered is said here. */}
              {!local && <span className="block @4xl:hidden">{m.seenMs ? `Last reached ${seenLine(false, m.seenMs, nowMs)}.` : "Never reached."}</span>}
            </div>
          )}
        </div>
      </TableCell>
      <TableCell className="hidden @2xl:table-cell">
        {up ? (
          <div className="min-w-0">
            <div className="leading-snug text-pretty tabular-nums">{holds || "–"}</div>
            <div className="text-[11.5px] leading-snug text-pretty text-muted-foreground">{[count, keep].filter(Boolean).join(" · ")}</div>
          </div>
        ) : (
          <span className="text-muted-foreground">{probe ? "–" : "…"}</span>
        )}
      </TableCell>
      {/* This Mac's own is either answering now or isn't: when it last did is not kept. */}
      <TableCell className="hidden text-muted-foreground tabular-nums @4xl:table-cell">{!probe ? "…" : local && !up ? "–" : seenLine(up, m.seenMs, nowMs)}</TableCell>
      <TableCell className="px-1.5!">
        <div className="flex items-center justify-end gap-1">
          {showing ? (
            <span className="px-2 text-[11.5px] text-muted-foreground">On screen</span>
          ) : (
            <Button variant="outline" size="xs" onClick={() => machines.use(m.id)} aria-label={`Look at ${m.name}`} className="font-normal">
              Look at it
            </Button>
          )}
          {local ? (
            // This Mac can't be renamed or removed: the space keeps the buttons in a column.
            <span aria-hidden className="size-7 shrink-0 sm:size-6" />
          ) : (
            <Menu>
              <MenuTrigger render={<Button variant="ghost" size="icon-xs" aria-label={`More for ${m.name}`} className="text-muted-foreground hover:text-foreground" />}>
                <EllipsisIcon />
              </MenuTrigger>
              <MenuPopup align="end">
                <MenuItem onClick={onRename}>Rename…</MenuItem>
                {how === "shipyard" && <MenuItem onClick={again}>Reconnect</MenuItem>}
                <MenuSeparator />
                <MenuItem variant="destructive" onClick={onRemove}>
                  Remove…
                </MenuItem>
              </MenuPopup>
            </Menu>
          )}
        </div>
      </TableCell>
    </TableRow>
  );
}

/** Rename asks for a machine's new name. The box or the address behind it stays what it is. */
function Rename({ machine, onClose }: { machine: Machine | null; onClose: () => void }) {
  // The last machine asked about is kept while the dialog closes, so its words don't blank on the way out.
  const [shown, setShown] = useState<Machine | null>(null);
  const [name, setName] = useState("");
  if (machine && machine !== shown) {
    setShown(machine);
    setName(machine.name);
  }
  const save = (e: FormEvent) => {
    e.preventDefault();
    if (machine) machines.rename(machine.id, name);
    onClose();
  };
  const plain = shown ? shown.box || shown.url.replace("http://", "") : "";
  return (
    <Dialog open={machine !== null} onOpenChange={(o) => !o && onClose()}>
      <DialogPopup className="max-w-md" showCloseButton={false} bottomStickOnMobile={false} onKeyDown={keepTab}>
        <form onSubmit={save}>
          <DialogHeader className="gap-2 p-5 pb-4">
            <DialogTitle className="text-[15px] leading-normal font-medium">Rename {shown?.name}</DialogTitle>
            <DialogDescription className="text-xs">
              What this window calls it, in the switcher and in its sentences. {shown && reach(shown) === "shipyard" ? `It is still the Shipyard box ${shown.box}.` : "Its address stays the same."}
            </DialogDescription>
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder={plain} aria-label="Name" spellCheck={false} autoComplete="off" autoFocus maxLength={64} />
          </DialogHeader>
          <DialogFooter className="px-5 py-3">
            <Button variant="outline" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit">Rename</Button>
          </DialogFooter>
        </form>
      </DialogPopup>
    </Dialog>
  );
}

/** Remove asks before a machine is taken off the list, and says what that does and does not do. */
function Remove({ machine, onClose }: { machine: Machine | null; onClose: () => void }) {
  const [busy, setBusy] = useState(false);
  const [fault, setFault] = useState<Fault | null>(null);
  const [shown, setShown] = useState<Machine | null>(null);
  if (machine && machine !== shown) {
    setShown(machine);
    setFault(null);
  }
  const lines = useMemo(() => (shown ? removing(shown) : []), [shown]);
  const remove = (anyway: boolean) => {
    if (!machine) return;
    setBusy(true);
    void forget(machine, anyway).then((f) => {
      setBusy(false);
      setFault(f);
      if (!f) onClose();
    });
  };
  return (
    <Dialog open={machine !== null} onOpenChange={(o) => !o && !busy && onClose()}>
      <DialogPopup className="max-w-md" showCloseButton={false} bottomStickOnMobile={false} onKeyDown={keepTab}>
        <DialogHeader className="gap-2 p-5 pb-4">
          <DialogTitle className="text-[15px] leading-normal font-medium">Remove {shown?.name}?</DialogTitle>
          <DialogDescription render={<div />} className="flex flex-col gap-1.5 text-xs">
            {lines.map((l) => (
              <p key={l} className="text-pretty">
                {l}
              </p>
            ))}
          </DialogDescription>
          {fault && (
            <div role="alert" className="flex items-start gap-2 rounded-lg border border-destructive/24 bg-destructive/4 px-3 py-2.5 text-xs">
              <CircleAlertIcon aria-hidden className="mt-0.5 size-3.5 shrink-0 text-destructive-foreground" />
              <div className="min-w-0">
                <p className="text-pretty text-destructive-foreground">The forward could not be closed, so {shown?.name} is still on the list. {fault.message}</p>
                <p className="mt-0.5 text-[11.5px] leading-snug text-pretty text-muted-foreground">
                  {fault.hint} Removing it anyway leaves the forward open: <Code>berth unforward {shown ? closing(shown) : ""}</Code> closes it.
                </p>
              </div>
            </div>
          )}
        </DialogHeader>
        <DialogFooter className="px-5 py-3">
          <Button variant="outline" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          {fault && (
            <Button variant="outline" onClick={() => remove(true)} disabled={busy} className="font-normal">
              Remove anyway
            </Button>
          )}
          <Button variant="destructive" onClick={() => remove(false)} loading={busy}>
            {fault ? "Try again" : "Remove"}
          </Button>
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}

// A machine being looked at is asked for its version this often, to keep "last reached" true.
const SEEN_EVERY_MS = 60_000;

/**
 * useSeen keeps a note of when the machine the window is showing last
 * answered, for the Machines view to say once it no longer does. One read
 * of that machine's own daemon a minute, while it is connected; this Mac
 * needs none.
 */
export function useSeen(machine: Machine, connected: boolean): void {
  const { id, url } = machine;
  useEffect(() => {
    if (!connected || id === LOCAL_ID) return;
    let alive = true;
    const note = () => void api.probe(url).then((p) => alive && p.state === "leadline" && machines.seen(id, p.version));
    note();
    const timer = setInterval(note, SEEN_EVERY_MS);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [id, url, connected]);
}

// How long a machine the window has turned to is given to answer before it
// is said not to: a forward's first connection, and a daemon's first
// answer, take a moment.
const GRACE_MS = 1500;

/**
 * Unreachable is the window turned to a machine whose Leadline does not
 * answer. For a Shipyard box the forward is opened again once, by itself:
 * a forward that went away (Shipyard restarted, the laptop slept) is the
 * usual reason, and using the machine is what brings it back. After that,
 * and for an address a person forwarded, it says what is wrong and waits.
 */
export function Unreachable({ machine, onMachines }: { machine: Machine; onMachines: () => void }) {
  const how = reach(machine);
  const linked = useLink(machine.id);
  // Until the grace is over nothing is said, and nothing is asked of Shipyard.
  const [waited, setWaited] = useState(false);
  const tried = useRef(false);
  useEffect(() => {
    const timer = setTimeout(() => setWaited(true), GRACE_MS);
    return () => clearTimeout(timer);
  }, []);
  useEffect(() => {
    if (!waited || tried.current || how !== "shipyard") return;
    tried.current = true;
    void reconnect(machine);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [waited]);

  const here = hereOf(machine);
  const port = portOf(machine);
  const back = (
    <Button size="sm" variant="outline" onClick={() => machines.use(LOCAL_ID)} className="font-normal">
      Back to this Mac
    </Button>
  );
  const manage = (
    <Button size="sm" variant="outline" onClick={onMachines} className="font-normal">
      Manage machines
    </Button>
  );

  if (!waited || linked.state === "trying" || (how === "shipyard" && linked.state === "idle")) {
    // "idle" after a try is a Leadline that answered: the window's own connection follows in a moment.
    const opening = how === "shipyard" && waited && !(tried.current && linked.state === "idle");
    return (
      <Hold title={opening ? `Opening the forward to ${here}…` : `Reaching ${here}…`} media={<Spinner aria-hidden className="size-5 text-muted-foreground" />}>
        {opening ? `Nothing answered on port ${port}, so Leadline is asking Shipyard for the forward to ${machine.box} again.` : "This window fills in by itself once its Leadline answers."}
      </Hold>
    );
  }
  if (linked.state === "failed") {
    return (
      <Hold
        title={`${here} can't be reached`}
        after={
          <div className="flex flex-wrap justify-center gap-2">
            <Button size="sm" onClick={() => void reconnect(machine)}>
              Try again
            </Button>
            {manage}
            {back}
          </div>
        }
      >
        <span role="alert">
          {linked.fault.message} {linked.fault.hint}
        </span>
      </Hold>
    );
  }
  if (how === "shipyard") {
    const probe: Probe = linked.state === "silent" ? linked.probe : { state: "none" };
    return (
      <Hold
        title={probe.state === "other" ? `Something else answers on ${here}` : `Leadline isn't answering on ${here}`}
        after={
          <>
            <div className="flex w-full max-w-xl flex-col gap-1.5 text-left">
              <p className="text-[11.5px] leading-snug text-pretty text-muted-foreground">If Leadline isn't installed on {machine.box}, this installs it there, as a user service on that machine's localhost. Leadline can't install it from this window yet, and until its first release is published the line has nothing to download.</p>
              <CopyLine text={INSTALL_LINE} label="Copy the install command" wrap />
            </div>
            <div className="flex flex-wrap justify-center gap-2">
              <Button size="sm" onClick={() => void reconnect(machine)}>
                Check again
              </Button>
              {manage}
              {back}
            </div>
          </>
        }
      >
        The forward to {machine.box} is open, on port {port}, and {probe.state === "other" ? "what answers through it isn't Leadline. Another program has that machine's port for it." : "nothing answers through it. Its daemon may be stopped, or not installed."}
      </Hold>
    );
  }
  return (
    <Hold
      title={`No answer from ${here}`}
      after={
        <div className="flex flex-wrap justify-center gap-2">
          {manage}
          {back}
        </div>
      }
    >
      Nothing answers at <Code>{machine.url.replace("http://", "")}</Code>. Check that the port you forwarded is still open and that Leadline is running on that machine. This window fills in by itself when it answers.
    </Hold>
  );
}

/** Hold is a whole view with nothing to show yet: what is missing, what to do, and anything more after. */
function Hold({ title, children, media, after }: { title: string; children: ReactNode; media?: ReactNode; after?: ReactNode }) {
  return (
    <Empty className="h-full gap-4 p-8 md:py-8">
      <EmptyHeader className="max-w-md">
        {media && <EmptyMedia className="mb-3">{media}</EmptyMedia>}
        <EmptyTitle role="heading" aria-level={2} className="text-[15px] font-medium">
          {title}
        </EmptyTitle>
        <EmptyDescription>{children}</EmptyDescription>
      </EmptyHeader>
      {after && <EmptyContent className="max-w-2xl gap-3">{after}</EmptyContent>}
    </Empty>
  );
}
