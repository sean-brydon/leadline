// "Send to Shipyard": from a problem Leadline shows (a query, an endpoint,
// a trace, a log line), start a coding agent on it in Shipyard, with what
// Leadline recorded as its prompt.
//
// The prompt carries real SQL and log lines into an agent's context, so the
// dialog shows exactly the text that will go, editable, with the lines that
// were copied from the app marked. Nothing is asked of Shipyard until the
// dialog opens, and nothing is sent until Send is pressed.
//
// Two daemons can be involved (lib/machines.ts). The brief is written by
// the daemon the window is showing, since it holds the data. The targets
// and the send go to this computer's own daemon, since Shipyard's command
// line is here.

import { createContext, useCallback, useContext, useEffect, useId, useMemo, useRef, useState, type ReactNode, type RefObject } from "react";
import { CheckIcon, CircleAlertIcon, CopyIcon, ShipIcon } from "lucide-react";
import { api, ApiError, showingLocal, type Brief, type Fault, type Scope, type ShipyardAgent, type ShipyardTargets, type ShipyardTask } from "@/lib/api";
import { askKey, baseChoices, byteLength, countMarked, kb, markedLines, PROJECT_CHECKOUT, promptFault, recordedTexts, taskNameFault, tidyTaskName, type BriefAsk } from "@/lib/brief";
import { useMachine } from "@/lib/live";
import { cn } from "@/lib/utils";
import { Code, CopyLine, copyText, Tip } from "@/components/ui";
import { Button } from "@/components/ui/button";
import { Dialog, DialogDescription, DialogPopup, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";

// The dialog is one for the whole window. A view asks for it through this:
// null where there is no window around the view to hold it.
const SendContext = createContext<((ask: BriefAsk) => void) | null>(null);

/** SendProvider holds the dialog, and gives every view under it the way to open it. `scope` is the window's: the brief is asked for over what the views show. */
export function SendProvider({ scope, children }: { scope: () => Scope; children: ReactNode }) {
  const [ask, setAsk] = useState<BriefAsk | null>(null);
  const open = useCallback((a: BriefAsk) => setAsk(a), []);
  return (
    <SendContext.Provider value={open}>
      {children}
      <SendToShipyard ask={ask} scope={scope} onClose={() => setAsk(null)} />
    </SendContext.Provider>
  );
}

/**
 * SendButton opens the dialog for one problem. `label` says which problem,
 * to a screen reader and in the tooltip. With `words` it is a button that
 * says "Send to Shipyard", down to its icon alone where the pane it is in
 * is narrow (the pane is a container); without, it is the icon, for a row.
 */
export function SendButton({ ask, label, words, className }: { ask: BriefAsk; label: string; words?: boolean; className?: string }) {
  const open = useContext(SendContext);
  if (!open) return null;
  // A row that opens something may hold this button: the press is the button's alone.
  const press = (e: { stopPropagation: () => void }) => {
    e.stopPropagation();
    open(ask);
  };
  return (
    <Tip label={label}>
      {words ? (
        <Button variant="outline" size="xs" onClick={press} onKeyDown={(e) => e.stopPropagation()} aria-label={label} aria-haspopup="dialog" className={cn("shrink-0 font-normal", className)}>
          <ShipIcon />
          <span className="hidden @lg:inline">Send to Shipyard</span>
        </Button>
      ) : (
        <Button
          variant="ghost"
          size="icon-xs"
          onClick={press}
          onKeyDown={(e) => e.stopPropagation()}
          aria-label={label}
          aria-haspopup="dialog"
          className={cn("size-5 shrink-0 text-foreground/60 hover:text-foreground sm:size-5", className)}
        >
          <ShipIcon className="size-3.5 sm:size-3.5" />
        </Button>
      )}
    </Tip>
  );
}

const faultOf = (e: unknown): Fault =>
  e instanceof ApiError ? { code: e.code, message: e.message, hint: e.hint || undefined } : { code: "", message: e instanceof Error ? e.message : String(e) };

/** SendToShipyard is the dialog. `ask` null is closed. */
export function SendToShipyard({ ask, scope, onClose }: { ask: BriefAsk | null; scope: () => Scope; onClose: () => void }) {
  // What was asked stays drawn while the dialog closes.
  const last = useRef(ask);
  if (ask) last.current = ask;
  // A send under way is not walked away from: its answer is the only word of whether a task now exists.
  const busy = useRef(false);
  return (
    <Dialog open={ask !== null} onOpenChange={(o) => !o && !busy.current && onClose()}>
      <DialogPopup className="max-w-[920px] overflow-hidden" bottomStickOnMobile={false}>
        {last.current && <Panel key={askKey(last.current)} ask={last.current} scope={scope} onClose={onClose} busy={busy} />}
      </DialogPopup>
    </Dialog>
  );
}

// The dialog's inside: one height for every state, so nothing jumps as the
// answers arrive, and it gives way in a window shorter than that.
const FRAME = "flex min-h-0 w-full flex-[0_1_640px] flex-col";

type Targets = { state: "asking" } | { state: "none"; why: Fault } | { state: "ready"; data: ShipyardTargets };

function Panel({ ask, scope, onClose, busy }: { ask: BriefAsk; scope: () => Scope; onClose: () => void; busy: { current: boolean } }) {
  const id = useId();
  const machine = useMachine();
  // The window the views showed when the dialog opened: it does not slide on while a person reads.
  const [at] = useState(scope);

  // The brief, from the daemon the window is showing.
  const [example, setExample] = useState(false);
  const [brief, setBrief] = useState<Brief | null>(null);
  const [briefFault, setBriefFault] = useState<Fault | null>(null);
  const [text, setText] = useState("");
  // The prompt as the daemon wrote it: what Reset goes back to, and what "edited" is measured against.
  const [written, setWritten] = useState("");
  // The switch was turned while the prompt held edits: rebuilding it would drop them, so it asks first.
  const [pending, setPending] = useState<boolean | null>(null);
  const edited = text !== written;

  const load = useCallback(
    (withExample: boolean) => {
      let wanted = true;
      api.brief(ask, at, withExample).then(
        (b) => {
          if (!wanted) return;
          setBrief(b);
          setBriefFault(null);
          setText(b.prompt);
          setWritten(b.prompt);
          setExample(withExample);
        },
        (e: unknown) => {
          if (!wanted) return;
          const f = faultOf(e);
          // A daemon without the route answers 404 in plain text.
          setBriefFault(e instanceof ApiError && !e.code && (e.status === 404 || e.status === 405) ? { code: "old", message: `The Leadline on ${machine.name} is too old to write this up. Update it, then try again.` } : f);
        },
      );
      return () => {
        wanted = false;
      };
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [ask, at],
  );
  useEffect(() => load(false), [load]);

  const turnExample = (on: boolean) => {
    if (edited) setPending(on);
    else load(on);
  };

  // Where it could go, from this computer's own daemon. Asked once the
  // brief says which service the problem is in.
  const [targets, setTargets] = useState<Targets>({ state: "asking" });
  const [box, setBox] = useState("");
  const [project, setProject] = useState("");
  const [base, setBase] = useState(PROJECT_CHECKOUT);
  const [agent, setAgent] = useState("");
  const [name, setName] = useState("");
  const source = brief?.source;
  const asked = useRef(false);

  const askTargets = useCallback(
    (forBox?: string) => {
      let wanted = true;
      setTargets((was) => (was.state === "ready" && forBox !== undefined ? was : { state: "asking" }));
      api.shipyardTargets(source, forBox).then(
        (t) => {
          if (!wanted) return;
          if (!t) {
            setTargets({ state: "none", why: { code: "old", message: "This computer's Leadline is too old to send to Shipyard. Update it, then try again." } });
            return;
          }
          setTargets({ state: "ready", data: t });
          setBox(t.box);
          if (forBox === undefined) {
            // The first answer: Leadline's own pick, when it has one.
            setProject(t.suggested?.project ?? "");
            setBase(t.suggested?.base ?? PROJECT_CHECKOUT);
          } else {
            // Another box: keep the project if that box has it too.
            setProject((was) => (t.projects.some((p) => p.name === was && p.repo) ? was : ""));
            setBase(PROJECT_CHECKOUT);
          }
        },
        (e: unknown) => {
          if (!wanted) return;
          const f = faultOf(e);
          setTargets({ state: "none", why: e instanceof ApiError ? f : { code: "unreachable", message: "This computer's Leadline isn't answering, so nothing can be sent to Shipyard from here." } });
        },
      );
      return () => {
        wanted = false;
      };
    },
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [source?.service, source?.project, source?.worktree, source?.path],
  );
  useEffect(() => {
    if (!brief || asked.current) return;
    asked.current = true;
    setName(brief.task_name);
    return askTargets();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [brief !== null]);

  const data = targets.state === "ready" ? targets.data : null;
  const shipyard = data?.available ?? false;
  const chosen = data?.projects.find((p) => p.name === project);
  // The box's own agents, then the ones the project's config adds.
  const agents = useMemo(() => {
    const out: ShipyardAgent[] = [...(data?.agents ?? [])];
    for (const a of chosen?.agents ?? []) if (!out.some((o) => o.id === a.id)) out.push(a);
    return out;
  }, [data, chosen]);
  useEffect(() => {
    if (agents.some((a) => a.id === agent)) return;
    setAgent((agents.find((a) => a.id === "claude") ?? agents[0])?.id ?? "");
  }, [agents, agent]);
  const bases = baseChoices(chosen, data?.suggested?.project === project ? data.suggested.base : "");
  useEffect(() => {
    if (!bases.some((b) => b.value === base)) setBase(PROJECT_CHECKOUT);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [project, base]);

  // Sending.
  const [sending, setSending] = useState(false);
  const [sent, setSent] = useState<ShipyardTask | null>(null);
  const [sendFault, setSendFault] = useState<Fault | null>(null);
  const nameFault = taskNameFault(name, chosen);
  const textFault = promptFault(text);
  const canSend = shipyard && !sending && brief !== null && !textFault && !nameFault && box !== "" && project !== "" && agent !== "" && !data?.box_problem;
  const send = () => {
    if (!canSend) return;
    setSending(true);
    setSendFault(null);
    busy.current = true;
    api.shipyardSend({ box, project, name, prompt: text, agent, base: base || undefined }).then(
      (task) => {
        busy.current = false;
        setSending(false);
        setSent(task);
      },
      (e: unknown) => {
        busy.current = false;
        setSending(false);
        setSendFault(e instanceof ApiError ? faultOf(e) : { code: "unreachable", message: "This computer's Leadline didn't answer.", hint: "The task may have started all the same: look in Shipyard before sending it again." });
      },
    );
  };

  const [copied, setCopied] = useState(false);
  const copy = async () => {
    if (!(await copyText(text))) return;
    setCopied(true);
    setTimeout(() => setCopied(false), 1600);
  };

  const recorded = useMemo(() => (brief ? recordedTexts(brief.prompt, brief.recorded) : new Set<string>()), [brief]);
  const marked = countMarked(text, recorded);
  // "Show" scrolls the prompt to its marked lines, one run of them at a time, and round again.
  const editor = useRef<HTMLDivElement>(null);
  const shown = useRef(-1);
  const showMarked = () => {
    const box = editor.current;
    if (!box) return;
    const runs = [...box.querySelectorAll<HTMLElement>("[data-recorded]")].filter((el) => !(el.previousElementSibling as HTMLElement | null)?.hasAttribute("data-recorded"));
    if (runs.length === 0) return;
    shown.current = (shown.current + 1) % runs.length;
    box.scrollTop = Math.max(0, runs[shown.current].offsetTop - box.clientHeight / 3);
  };
  const noShipyard = targets.state === "none" || (data !== null && !data.available);

  if (sent) return <Sent task={sent} agents={agents} onClose={onClose} />;

  return (
    <div className={cn(FRAME, "text-xs")}>
      <header className="flex shrink-0 flex-col gap-1 border-b py-3 pr-12 pl-5">
        <DialogTitle className="text-[15px] leading-normal font-medium">Send to Shipyard</DialogTitle>
        <DialogDescription className="line-clamp-2 min-h-4 text-xs text-pretty">
          {brief ? <Words text={brief.summary} /> : briefFault ? "Leadline could not write this problem up." : "Writing up what Leadline recorded…"}
        </DialogDescription>
      </header>

      <div className="flex min-h-0 flex-1">
        {/* What will be sent. */}
        <section aria-labelledby={`${id}-what`} className="flex min-w-0 flex-1 flex-col gap-2 px-5 py-3">
          <div className="flex shrink-0 items-baseline gap-2">
            <h3 id={`${id}-what`} className="shrink-0 font-medium">
              Exactly what will be sent
            </h3>
            {brief && (
              <span id={`${id}-size`} className="min-w-0 truncate text-[11px] text-muted-foreground tabular-nums">
                {kb(byteLength(text))}
                {edited ? ", edited" : ""}
              </span>
            )}
            {edited && (
              <Button variant="link" size="xs" onClick={() => setText(written)} className="ml-auto h-4 shrink-0 px-0 text-[11px] font-normal text-muted-foreground sm:h-4 sm:text-[11px]">
                Undo my edits
              </Button>
            )}
          </div>
          {brief ? (
            <PromptEditor boxRef={editor} value={text} onChange={setText} recorded={recorded} disabled={sending} label="The prompt that will be sent" describedBy={`${id}-legend ${id}-size`} />
          ) : (
            <div className={cn("flex min-h-0 flex-1 items-center justify-center gap-2 rounded-lg border px-6 text-center", briefFault ? "text-destructive-foreground" : "text-muted-foreground")} role={briefFault ? "alert" : "status"}>
              {briefFault ? (
                briefFault.message
              ) : (
                <>
                  <Spinner aria-hidden className="size-3.5" />
                  Writing it up…
                </>
              )}
            </div>
          )}
          <div className="flex shrink-0 flex-col gap-2">
            <div className="flex items-start gap-2 text-[11.5px] leading-snug text-muted-foreground">
              <span aria-hidden className="mt-[3px] h-2.5 w-1 shrink-0 rounded-[1px] bg-blue" />
              <p id={`${id}-legend`} className="min-w-0 flex-1 text-pretty">
                {brief ? (marked === 0 ? "No line of this is copied from what your app sent." : `${marked === 1 ? "1 marked line is" : `${marked} marked lines are`} copied from what your app sent. The rest is Leadline's own words and numbers.`) : "Lines copied from what your app sent will be marked."}
              </p>
              {marked > 0 && (
                <Button variant="link" size="xs" onClick={showMarked} aria-label="Scroll the prompt to its marked lines" className="h-4 shrink-0 px-0 text-[11px] font-normal text-muted-foreground sm:h-4 sm:text-[11px]">
                  Show
                </Button>
              )}
            </div>
            {pending !== null ? (
              <div role="alert" className="flex items-center gap-2 rounded-lg border px-3 py-1.5">
                <span className="min-w-0 flex-1 text-pretty">{pending ? "Putting real values in" : "Taking real values out"} writes the prompt afresh, without your edits.</span>
                <Button
                  variant="outline"
                  size="xs"
                  className="shrink-0 font-normal"
                  onClick={() => {
                    load(pending);
                    setPending(null);
                  }}
                >
                  Write it afresh
                </Button>
                <Button variant="ghost" size="xs" className="shrink-0 font-normal" onClick={() => setPending(null)}>
                  Keep my edits
                </Button>
              </div>
            ) : (
              <div className="flex items-start gap-2.5">
                <span className="flex h-[18px] items-center">
                  <Switch
                    id={`${id}-example`}
                    checked={example}
                    disabled={!brief || sending || (!example && !brief.example.available)}
                    onCheckedChange={turnExample}
                    aria-describedby={`${id}-example-note`}
                  />
                </span>
                <div className="min-w-0">
                  <label htmlFor={`${id}-example`}>Include real values</label>
                  <p id={`${id}-example-note`} className="text-[11.5px] leading-snug text-pretty text-muted-foreground">
                    {brief && !example && !brief.example.available
                      ? "There is nothing more to include: what is shown has no values in it."
                      : example
                        ? "On: one statement and the log lines are as they ran, with the values your app used."
                        : "Off: statements and log lines are reduced to their shape, with ? where a value was. On adds one real statement, and log lines as they were written."}
                  </p>
                </div>
              </div>
            )}
          </div>
        </section>

        {/* Where it goes. */}
        <section aria-labelledby={`${id}-where`} className="flex w-[304px] shrink-0 flex-col gap-3 overflow-y-auto border-l bg-muted/40 px-4 py-3">
          <h3 id={`${id}-where`} className="shrink-0 font-medium">
            Where it goes
          </h3>
          {targets.state === "asking" || !brief ? (
            <p role="status" className="flex items-center gap-2 text-muted-foreground">
              {!briefFault && <Spinner aria-hidden className="size-3.5" />}
              {briefFault ? "Nowhere, until there is something to send." : brief ? "Asking Shipyard for your boxes…" : "Shipyard is asked once the prompt is written."}
            </p>
          ) : noShipyard ? (
            <NoShipyard why={targets.state === "none" ? targets.why : data?.problem} installed={data?.installed ?? false} onRetry={() => askTargets()} />
          ) : data && data.boxes.length === 0 ? (
            <div className="flex flex-col gap-2">
              <p>Shipyard has no boxes yet.</p>
              <p className="text-[11.5px] leading-snug text-pretty text-muted-foreground">Pair a box in Shipyard, then try again. Until then the prompt works with any agent: copy it and paste it in.</p>
              <Button variant="outline" size="xs" className="self-start font-normal" onClick={() => askTargets()}>
                Try again
              </Button>
            </div>
          ) : data ? (
            <>
              <Field label="Box" id={`${id}-box`}>
                <Pick
                  id={`${id}-box`}
                  value={box}
                  disabled={sending}
                  onChange={(v) => {
                    setBox(v);
                    setSendFault(null);
                    askTargets(v);
                  }}
                  options={data.boxes.map((b) => ({ value: b.name, label: `${b.name}${b.local ? " (this computer)" : ""}${b.state === "online" ? "" : `, ${b.state}`}` }))}
                  placeholder="Pick a box"
                />
                {data.box_problem && (
                  <Note tone="err">
                    {data.box_problem.message} {data.box_problem.hint}
                  </Note>
                )}
              </Field>
              <Field label="Project" id={`${id}-project`}>
                <Pick
                  id={`${id}-project`}
                  value={project}
                  disabled={sending || data.projects.length === 0}
                  onChange={(v) => {
                    setProject(v);
                    setSendFault(null);
                  }}
                  options={data.projects.map((p) => ({ value: p.name, label: p.repo ? p.name : `${p.name}, not a git repository`, disabled: !p.repo }))}
                  placeholder={data.box_problem ? "None to pick" : data.projects.length === 0 ? "This box has no projects" : "Pick a project"}
                />
              </Field>
              <Field label="Starts from" id={`${id}-base`}>
                <Pick id={`${id}-base`} value={base} disabled={sending || !chosen} onChange={setBase} options={bases.map((b) => ({ value: b.value, label: b.label }))} placeholder="Pick a project first" />
              </Field>
              {agents.length > 1 ? (
                <Field label="Agent" id={`${id}-agent`}>
                  <Pick id={`${id}-agent`} value={agent} disabled={sending} onChange={setAgent} options={agents.map((a) => ({ value: a.id, label: a.name }))} placeholder="Pick an agent" />
                </Field>
              ) : agents.length === 1 ? (
                <Field label="Agent">
                  <p className="selectable">{agents[0].name}</p>
                </Field>
              ) : (
                !data.box_problem && (
                  <Field label="Agent">
                    <Note tone="err">Shipyard found no agent it can start on {box}. Install Claude Code or Codex there, or copy the prompt and give it to an agent yourself.</Note>
                  </Field>
                )
              )}
              <Field label="Task name" id={`${id}-name`}>
                <Input
                  id={`${id}-name`}
                  size="sm"
                  value={name}
                  disabled={sending}
                  onChange={(e) => {
                    setName(tidyTaskName(e.target.value));
                    setSendFault(null);
                  }}
                  spellCheck={false}
                  autoComplete="off"
                  aria-invalid={nameFault ? true : undefined}
                  aria-describedby={`${id}-name-note`}
                  className="font-mono text-[11.5px] sm:text-[11.5px]"
                />
                <Note id={`${id}-name-note`} tone={nameFault ? "err" : undefined}>
                  {nameFault ?? "Its worktree and its branch get this name."}
                </Note>
              </Field>
              {/* Last, so a short window shows every control before the reasons for them. */}
              <Why data={data} box={box} project={project} base={base} service={brief.service} />
              {!showingLocal() && <Note>This was written up by {machine.name}. It is sent from this computer, where Shipyard is.</Note>}
            </>
          ) : null}
        </section>
      </div>

      {sendFault && (
        <div role="alert" className="flex shrink-0 items-start gap-2 border-t bg-destructive/4 px-5 py-2">
          <CircleAlertIcon aria-hidden className="mt-0.5 size-3.5 shrink-0 text-destructive-foreground" />
          <p className="selectable min-w-0 text-pretty">
            <span className="text-destructive-foreground">{sendFault.message}</span> {sendFault.hint && <span className="text-muted-foreground">{sendFault.hint}</span>}
          </p>
        </div>
      )}
      <footer className="flex shrink-0 items-center gap-2 border-t bg-muted/72 px-5 py-3">
        <p className="mr-auto min-w-0 truncate text-[11.5px] text-muted-foreground">
          {textFault && brief ? <span className="text-destructive-foreground">{textFault}</span> : noShipyard ? "It works with any coding agent: paste it in." : sending ? "Starting the task…" : "Nothing is sent until you press Send."}
        </p>
        <Button variant="ghost" size="sm" onClick={onClose} disabled={sending} className="font-normal">
          Cancel
        </Button>
        <Button variant={noShipyard ? "default" : "outline"} size="sm" onClick={copy} disabled={!brief || text.trim() === ""} className={noShipyard ? undefined : "font-normal"}>
          {copied ? <CheckIcon className={noShipyard ? undefined : "text-success-foreground"} /> : <CopyIcon />}
          <span aria-live="polite">{copied ? "Copied" : "Copy prompt"}</span>
        </Button>
        {!noShipyard && (
          <Button size="sm" onClick={send} disabled={!canSend} loading={sending}>
            <ShipIcon />
            Send to Shipyard
          </Button>
        )}
      </footer>
    </div>
  );
}

/**
 * PromptEditor is the prompt, editable, with the lines that are recorded
 * data marked. A textarea can't colour its own lines, so the marks are
 * drawn on a copy of the text that lies under it, set in the same type and
 * wrapped the same way; the textarea is as tall as that copy, and the box
 * around both is what scrolls.
 */
function PromptEditor({ boxRef, value, onChange, recorded, disabled, label, describedBy }: { boxRef?: RefObject<HTMLDivElement | null>; value: string; onChange: (v: string) => void; recorded: Set<string>; disabled?: boolean; label: string; describedBy?: string }) {
  const lines = value.split("\n");
  const marks = markedLines(value, recorded);
  const TYPE = "px-3 py-2 font-mono text-[11.5px] leading-[18px] break-words whitespace-pre-wrap";
  return (
    <div
      ref={boxRef}
      data-prompt
      className="relative min-h-0 flex-1 overflow-y-auto rounded-lg border border-input bg-background ring-ring/24 has-focus-visible:border-ring has-focus-visible:ring-[3px] dark:bg-input/32"
    >
      <div className="relative min-h-full">
        <div aria-hidden className={cn(TYPE, "pointer-events-none text-transparent")}>
          {lines.map((line, i) => (
            // The mark is the line's own box, bled to the edges; its padding keeps the words where the textarea has them.
            <div key={i} data-recorded={marks[i] ? "" : undefined} className={marks[i] ? "-mx-3 border-l-2 border-blue bg-blue/12 pr-3 pl-[10px]" : undefined}>
              {line === "" ? "​" : line}
            </div>
          ))}
        </div>
        <textarea
          value={value}
          onChange={(e) => onChange(e.target.value)}
          disabled={disabled}
          aria-label={label}
          aria-describedby={describedBy}
          spellCheck={false}
          autoCapitalize="off"
          autoCorrect="off"
          className={cn(TYPE, "selectable absolute inset-0 block size-full resize-none overflow-hidden bg-transparent text-foreground outline-none disabled:opacity-64")}
        />
      </div>
    </div>
  );
}

/** Words draws a sentence of the brief: what it has in backticks is set as code. */
function Words({ text }: { text: string }) {
  return (
    <>
      {text.split("`").map((part, i) =>
        i % 2 === 1 ? (
          <code key={i} className="rounded-sm bg-muted px-1 py-px font-mono text-[11px] text-foreground/90">
            {part}
          </code>
        ) : (
          part
        ),
      )}
    </>
  );
}

function Field({ label, id, children }: { label: string; id?: string; children: ReactNode }) {
  return (
    <div className="flex shrink-0 flex-col gap-1">
      {id ? (
        <label htmlFor={id} className="text-[11px] text-muted-foreground">
          {label}
        </label>
      ) : (
        <div className="text-[11px] text-muted-foreground">{label}</div>
      )}
      {children}
    </div>
  );
}

function Note({ id, tone, children }: { id?: string; tone?: "err"; children: ReactNode }) {
  return (
    <p id={id} role={tone === "err" ? "alert" : undefined} className={cn("text-[11.5px] leading-snug text-pretty", tone === "err" ? "text-destructive-foreground" : "text-muted-foreground")}>
      {children}
    </p>
  );
}

// Base UI's Select takes "" for "nothing picked", and "" is also the base
// that means the project's own checkout: it travels through as CHECKOUT.
const CHECKOUT = "__checkout__";
const toItem = (v: string) => (v === PROJECT_CHECKOUT ? CHECKOUT : v);
const fromItem = (v: string) => (v === CHECKOUT ? PROJECT_CHECKOUT : v);

/** Pick is a coss Select at the size of the dialog's other controls. `id` goes on its button, so a label can name it. */
function Pick({ id, value, onChange, options, placeholder, disabled }: { id: string; value: string; onChange: (v: string) => void; options: { value: string; label: string; disabled?: boolean }[]; placeholder: string; disabled?: boolean }) {
  const has = options.some((o) => o.value === value);
  const items = options.map((o) => ({ value: toItem(o.value), label: o.label }));
  return (
    <Select items={items} value={has ? toItem(value) : null} onValueChange={(v) => v !== null && onChange(fromItem(v as string))} disabled={disabled}>
      <SelectTrigger id={id} size="sm" className="min-w-0 text-xs sm:text-xs">
        <SelectValue placeholder={placeholder} />
      </SelectTrigger>
      <SelectPopup>
        {options.map((o) => (
          <SelectItem key={toItem(o.value)} value={toItem(o.value)} disabled={o.disabled} className="text-xs sm:text-xs">
            {o.label}
          </SelectItem>
        ))}
      </SelectPopup>
    </Select>
  );
}

/** Why says why this box, project and branch, while they are still the ones Leadline picked; and that it could not pick, when it could not. */
function Why({ data, box, project, base, service }: { data: ShipyardTargets; box: string; project: string; base: string; service: string }) {
  const s = data.suggested;
  if (!s) {
    if (project) return null;
    return <Note>{service ? `Leadline can't tell which project ${service} is: no Shipyard worktree attached it, and its folder is not a Shipyard project's.` : "Leadline can't tell which project this is."} Pick where the task should start.</Note>;
  }
  if (s.box !== box || s.project !== project) return null;
  return (
    <Note>
      {s.why} {s.base === base ? s.base_why : ""}
    </Note>
  );
}

/** NoShipyard is the "where it goes" of a computer that can't send: why, and that the prompt is still good for any agent. */
function NoShipyard({ why, installed, onRetry }: { why?: Fault; installed: boolean; onRetry: () => void }) {
  const missing = !installed && (!why || why.code === "shipyard_not_found");
  return (
    <div className="flex flex-col gap-2">
      <p role="status">{missing ? "Shipyard isn't installed on this computer." : (why?.message ?? "Shipyard isn't answering.")}</p>
      <p className="text-[11.5px] leading-snug text-pretty text-muted-foreground">
        {missing
          ? "Shipyard runs coding agents on your own machines, each task in a worktree of its own. Without it the prompt is still the whole brief: copy it and give it to any agent."
          : `${why?.hint ? `${why.hint} ` : ""}The prompt is still the whole brief: copy it and give it to any agent.`}
      </p>
      {!missing && (
        <Button variant="outline" size="xs" className="self-start font-normal" onClick={onRetry}>
          Try again
        </Button>
      )}
    </div>
  );
}

/** Sent is the dialog after a task started: what Shipyard made, and what to do next. */
function Sent({ task, agents, onClose }: { task: ShipyardTask; agents: ShipyardAgent[]; onClose: () => void }) {
  const agent = agents.find((a) => a.id === task.agent)?.name ?? task.agent;
  return (
    <div className={cn(FRAME, "text-xs")}>
      <header className="flex shrink-0 flex-col gap-1 border-b py-3 pr-12 pl-5">
        <DialogTitle className="text-[15px] leading-normal font-medium">Sent to Shipyard</DialogTitle>
        <DialogDescription className="text-xs">An agent has the prompt and a worktree of its own to work in.</DialogDescription>
      </header>
      <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-5 py-4">
        <div role="status" className="flex items-start gap-2">
          <CheckIcon aria-hidden className="mt-0.5 size-3.5 shrink-0 text-success-foreground" />
          <p>
            Started <Code>{task.name}</Code> in {task.project} on {task.box}.
          </p>
        </div>
        <dl className="grid max-w-xl grid-cols-[92px_minmax(0,1fr)] gap-x-3 gap-y-1.5 rounded-lg border px-3 py-2.5">
          <Row label="Worktree">{task.ref}</Row>
          <Row label="Branch">
            {task.branch ?? task.name}
            {task.base ? `, from ${task.base}` : ", from what the project has checked out"}
          </Row>
          <Row label="Agent">{agent}</Row>
          {task.session && <Row label="Session">{task.session}</Row>}
          <Row label="Prompt">{kb(task.prompt_bytes)}, as you saw it</Row>
        </dl>
        <section className="flex max-w-xl flex-col gap-2">
          <h3 className="font-medium">What happens next</h3>
          <p className="text-pretty text-muted-foreground">
            <span className="text-foreground">It is in Shipyard now.</span> Open Shipyard to watch the agent work, answer it when it asks, and review what it changes before any of it is merged.
          </p>
          <p className="text-pretty text-muted-foreground">
            <span className="text-foreground">It will show up here.</span> When that worktree's app starts sending telemetry, it appears in Leadline's sidebar as <Code>{task.service}</Code>. Its numbers there are the after to this problem's before.
          </p>
          {task.session && (
            <div className="flex flex-col gap-1 pt-1">
              <span className="text-[11px] text-muted-foreground">To watch it in a terminal</span>
              <CopyLine text={`berth attach ${task.box}/${task.session}`} label="Copy the command that attaches a terminal to the agent" />
            </div>
          )}
        </section>
      </div>
      <footer className="flex shrink-0 items-center justify-end gap-2 border-t bg-muted/72 px-5 py-3">
        <Button size="sm" onClick={onClose}>
          Done
        </Button>
      </footer>
    </div>
  );
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="selectable min-w-0 font-mono text-[11.5px] break-words">{children}</dd>
    </>
  );
}
