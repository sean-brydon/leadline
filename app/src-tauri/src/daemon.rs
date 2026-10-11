//! The app runs Leadline's daemon, so opening the app is enough.
//!
//! On launch it asks 127.0.0.1:4320 for /api/health. A daemon that answers
//! (one started in a terminal, or by another copy of the app) is used and
//! left alone. If nothing answers, the app starts the leadlined it carries
//! (Tauri's sidecar: beside the app's own executable) and waits for it.
//!
//! A daemon the app started is the app's to stop. Quitting sends it SIGTERM
//! and waits, so DuckDB closes its file properly. If the app dies without
//! quitting (a crash, a force quit, `kill`), a watchdog does the same: a
//! second, tiny run of this program that holds a pipe from the app and acts
//! when the pipe closes. Only killing the watchdog too leaves a daemon
//! behind, and the next launch finds and uses that one.
//!
//! The page learns all this from one value, `Status`: the `daemon_status`
//! command, and a `daemon` event when it changes (src/lib/daemon.ts).

use serde::Serialize;
use std::fs::{File, OpenOptions, TryLockError};
use std::io::{Read, Seek, SeekFrom, Write};
use std::net::{SocketAddr, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, ExitStatus, Stdio};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError, Sender};
use std::sync::{Arc, Mutex};
use std::thread::sleep;
use std::time::{Duration, Instant};
use tauri::{AppHandle, Emitter, Manager, State};

/// Where the daemon listens. The page has the same address built in (BASE
/// in src/lib/api.ts, and connect-src in tauri.conf.json).
const ADDR: &str = "127.0.0.1:4320";
const SIDECAR: &str = "leadlined";
const EVENT: &str = "daemon";
/// The app's executable run with this argument and a pid is the watchdog.
const WATCHDOG_ARG: &str = "--watch-daemon";

/// An empty DuckDB file opens in well under a second.
const START_TIMEOUT: Duration = Duration::from_secs(30);
/// The daemon gives its open connections two seconds, then closes DuckDB.
const STOP_TIMEOUT: Duration = Duration::from_secs(5);
/// A daemon someone else runs is gone after this many unanswered checks.
const MISSES: u32 = 3;
const MAX_LOG: u64 = 1 << 20;

/// What the page is told about the daemon.
#[derive(Clone, Debug, PartialEq, Serialize)]
#[serde(tag = "state", rename_all = "lowercase")]
pub enum Status {
    Starting,
    /// `owned` is whether this app started it, and so will stop it.
    Running { owned: bool },
    /// `reason` is what happened and `hint` what to do, both as sentences;
    /// `detail` is the end of the daemon's log, and `log` that file's path.
    /// Either may be empty.
    Failed { reason: String, hint: String, detail: String, log: String },
}

/// What the supervisor needs to know; the tests give it another port.
struct Config {
    addr: String,
    sidecar: PathBuf,
    args: Vec<String>,
    log: PathBuf,
    /// The program to run as the watchdog: the app itself.
    watchdog: Option<PathBuf>,
}

#[derive(Debug, PartialEq)]
enum Probe {
    /// A Leadline daemon answered.
    Leadline,
    /// Something has the port, and it isn't Leadline.
    Other,
    Nothing,
}

fn probe(addr: &str) -> Probe {
    let Ok(sock) = addr.parse::<SocketAddr>() else {
        return Probe::Nothing;
    };
    let Ok(mut conn) = TcpStream::connect_timeout(&sock, Duration::from_millis(500)) else {
        return Probe::Nothing;
    };
    let _ = conn.set_read_timeout(Some(Duration::from_secs(2)));
    let _ = conn.set_write_timeout(Some(Duration::from_secs(2)));
    // HTTP/1.0: the answer is never chunked, and ends with the connection.
    let request = format!("GET /api/health HTTP/1.0\r\nHost: {addr}\r\n\r\n");
    let mut answer = String::new();
    if conn.write_all(request.as_bytes()).is_ok() {
        let _ = conn.take(4096).read_to_string(&mut answer);
    }
    if is_leadline(&answer) {
        Probe::Leadline
    } else {
        Probe::Other
    }
}

/// Whether an HTTP answer is the daemon's health: 200 and its name.
fn is_leadline(answer: &str) -> bool {
    let Some((head, body)) = answer.split_once("\r\n\r\n") else {
        return false;
    };
    let ok = head.lines().next().and_then(|l| l.split_whitespace().nth(1)) == Some("200");
    ok && serde_json::from_str::<serde_json::Value>(body).is_ok_and(|v| v["name"] == "leadline" && v["ok"] == true)
}

/// A daemon this app started.
struct Owned {
    child: Child,
    watchdog: Option<Child>,
    log: PathBuf,
    /// Where this run's lines begin in the log.
    log_from: u64,
}

impl Owned {
    fn exited(&mut self) -> Option<ExitStatus> {
        self.child.try_wait().ok().flatten()
    }

    fn stop(&mut self) {
        terminate(&mut self.child);
        self.release();
    }

    /// Tells the watchdog the daemon is dealt with, so it never signals a
    /// pid that has since become some other process's.
    fn release(&mut self) {
        if let Some(mut watchdog) = self.watchdog.take() {
            if let Some(mut pipe) = watchdog.stdin.take() {
                let _ = pipe.write_all(b"q");
            }
            let _ = watchdog.wait();
        }
    }

    /// The last lines this run of the daemon logged.
    fn tail(&self) -> String {
        let mut text = String::new();
        if let Ok(mut f) = File::open(&self.log) {
            let len = f.metadata().map(|m| m.len()).unwrap_or(0);
            let from = self.log_from.max(len.saturating_sub(4096));
            if f.seek(SeekFrom::Start(from)).is_ok() {
                let mut bytes = Vec::new();
                let _ = f.read_to_end(&mut bytes);
                text = String::from_utf8_lossy(&bytes).into_owned();
            }
        }
        let lines: Vec<&str> = text.lines().filter(|l| !l.trim().is_empty()).collect();
        lines[lines.len().saturating_sub(8)..].join("\n")
    }
}

/// Asks a process to finish and waits; one that won't is killed.
fn terminate(child: &mut Child) {
    if matches!(child.try_wait(), Ok(Some(_))) {
        return;
    }
    #[cfg(unix)]
    {
        // Not yet waited for, so the pid is still this child's.
        unsafe { libc::kill(child.id() as libc::pid_t, libc::SIGTERM) };
        let until = Instant::now() + STOP_TIMEOUT;
        while Instant::now() < until {
            if !matches!(child.try_wait(), Ok(None)) {
                return;
            }
            sleep(Duration::from_millis(20));
        }
    }
    let _ = child.kill();
    let _ = child.wait();
}

enum Phase {
    /// Nothing of ours runs: not started yet, or failed.
    Idle,
    /// Ours, and not answering yet. The lock is held until it does.
    Starting { daemon: Owned, since: Instant, lock: Option<File> },
    Ours(Owned),
    /// Someone else's daemon, and how many checks in a row it has missed.
    Theirs { misses: u32 },
}

struct Supervisor {
    cfg: Config,
    phase: Phase,
    report: Box<dyn Fn(Status) + Send>,
}

impl Supervisor {
    fn new(cfg: Config, report: impl Fn(Status) + Send + 'static) -> Self {
        Self { cfg, phase: Phase::Idle, report: Box::new(report) }
    }

    /// Finds a daemon or starts one. Does nothing while there is one.
    fn start(&mut self) {
        if !matches!(self.phase, Phase::Idle) {
            return;
        }
        (self.report)(Status::Starting);
        let lock = start_lock(&self.cfg.addr);
        match probe(&self.cfg.addr) {
            Probe::Leadline => self.theirs(),
            Probe::Other => {
                let port = self.cfg.addr.rsplit(':').next().unwrap_or_default();
                self.fail(
                    format!("Another program is using port {port}."),
                    format!("Leadline's daemon needs that port. Quit the other program (lsof -i :{port} shows which), then start the daemon."),
                    None,
                )
            }
            Probe::Nothing => match self.spawn() {
                Ok(daemon) => self.phase = Phase::Starting { daemon, since: Instant::now(), lock },
                Err(why) => self.fail(
                    "Leadline couldn't start its daemon.".into(),
                    format!("{why} You can also run leadlined yourself, and this window will use it."),
                    None,
                ),
            },
        }
    }

    fn spawn(&self) -> Result<Owned, String> {
        let bin = &self.cfg.sidecar;
        if !bin.is_file() {
            return Err(format!("It should be at {}, and isn't.", bin.display()));
        }
        let (log, log_from) =
            open_log(&self.cfg.log).map_err(|e| format!("Its log, {}, can't be written: {e}.", self.cfg.log.display()))?;
        let err = log.try_clone().map_err(|e| format!("{e}."))?;
        let mut command = Command::new(bin);
        command.args(&self.cfg.args).stdin(Stdio::null()).stdout(log).stderr(err);
        // A process group of its own: Ctrl-C in the terminal the app was
        // started from (pnpm tauri dev) then reaches the app, not the
        // daemon, which gets its one SIGTERM from the app or the watchdog.
        // A second signal could land while DuckDB is closing.
        #[cfg(unix)]
        std::os::unix::process::CommandExt::process_group(&mut command, 0);
        let child = command
            .spawn()
            .map_err(|e| format!("{} didn't run: {e}.", bin.display()))?;
        let watchdog = self.cfg.watchdog.as_ref().and_then(|exe| {
            Command::new(exe)
                .arg(WATCHDOG_ARG)
                .arg(child.id().to_string())
                .stdin(Stdio::piped())
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .spawn()
                .inspect_err(|e| eprintln!("leadline: no watchdog for the daemon: {e}"))
                .ok()
        });
        Ok(Owned { child, watchdog, log: self.cfg.log.clone(), log_from })
    }

    /// Looks at the daemon again; called a few times a second.
    fn poll(&mut self) {
        match std::mem::replace(&mut self.phase, Phase::Idle) {
            // Failed, but a daemon started by hand since then is as good.
            Phase::Idle => {
                if probe(&self.cfg.addr) == Probe::Leadline {
                    self.theirs();
                }
            }
            Phase::Starting { mut daemon, since, lock } => {
                if let Some(status) = daemon.exited() {
                    self.gone(daemon, format!("Leadline's daemon stopped as it was starting ({status})."));
                } else if probe(&self.cfg.addr) == Probe::Leadline {
                    self.phase = Phase::Ours(daemon);
                    (self.report)(Status::Running { owned: true });
                } else if since.elapsed() > START_TIMEOUT {
                    daemon.stop();
                    self.fail(
                        format!("Leadline's daemon didn't answer on {} within {} seconds.", self.cfg.addr, START_TIMEOUT.as_secs()),
                        "Start it again. If it keeps happening, its log says where it got to.".into(),
                        Some(&daemon),
                    );
                } else {
                    self.phase = Phase::Starting { daemon, since, lock };
                }
            }
            Phase::Ours(mut daemon) => match daemon.exited() {
                Some(status) => self.gone(daemon, format!("Leadline's daemon stopped ({status}).")),
                None => self.phase = Phase::Ours(daemon),
            },
            Phase::Theirs { misses } => {
                if probe(&self.cfg.addr) == Probe::Leadline {
                    self.phase = Phase::Theirs { misses: 0 };
                } else if misses + 1 < MISSES {
                    self.phase = Phase::Theirs { misses: misses + 1 };
                } else {
                    self.fail(
                        "The daemon this window was using has stopped.".into(),
                        "It was started outside the app. Start it again and this window reconnects by itself, or let Leadline run its own.".into(),
                        None,
                    );
                }
            }
        }
    }

    /// Our daemon exited by itself.
    fn gone(&mut self, mut daemon: Owned, reason: String) {
        daemon.release();
        // Two daemons can't share the port or the DuckDB file, so ours
        // loses to one started at the same moment. That one will do.
        if probe(&self.cfg.addr) == Probe::Leadline {
            self.theirs();
        } else {
            self.fail(reason, "Start it again. The end of its log is below.".into(), Some(&daemon));
        }
    }

    fn theirs(&mut self) {
        self.phase = Phase::Theirs { misses: 0 };
        (self.report)(Status::Running { owned: false });
    }

    fn fail(&mut self, reason: String, hint: String, daemon: Option<&Owned>) {
        self.phase = Phase::Idle;
        (self.report)(Status::Failed {
            reason,
            hint,
            detail: daemon.map(Owned::tail).unwrap_or_default(),
            log: daemon.map(|d| d.log.display().to_string()).unwrap_or_default(),
        });
    }

    /// Stops the daemon if it is ours; someone else's is left running.
    fn stop(&mut self) {
        if !matches!(self.phase, Phase::Starting { .. } | Phase::Ours(_)) {
            return;
        }
        if let Phase::Starting { mut daemon, .. } | Phase::Ours(mut daemon) = std::mem::replace(&mut self.phase, Phase::Idle) {
            daemon.stop();
        }
    }

    fn tick(&self) -> Duration {
        match self.phase {
            Phase::Starting { .. } => Duration::from_millis(50),
            _ => Duration::from_secs(1),
        }
    }
}

/// Two apps opened at the same moment would both find no daemon and both
/// start one. Whoever holds this file's lock checks and starts; the other
/// waits here, and then finds the first one's daemon answering.
fn start_lock(addr: &str) -> Option<File> {
    let port = addr.rsplit(':').next().unwrap_or_default();
    let path = std::env::temp_dir().join(format!("leadline-{port}.lock"));
    let file = OpenOptions::new().create(true).truncate(false).write(true).open(path).ok()?;
    let until = Instant::now() + START_TIMEOUT + STOP_TIMEOUT;
    loop {
        match file.try_lock() {
            Ok(()) => return Some(file),
            Err(TryLockError::WouldBlock) if Instant::now() < until => sleep(Duration::from_millis(50)),
            Err(_) => return None,
        }
    }
}

/// Opens the daemon's log to append to, and says where this run begins.
/// The log starts over once it is large.
fn open_log(path: &Path) -> std::io::Result<(File, u64)> {
    if let Some(dir) = path.parent() {
        let mut builder = std::fs::DirBuilder::new();
        builder.recursive(true);
        // The data directory, private as the daemon makes it.
        #[cfg(unix)]
        std::os::unix::fs::DirBuilderExt::mode(&mut builder, 0o700);
        builder.create(dir)?;
    }
    let file = OpenOptions::new().create(true).append(true).open(path)?;
    let mut len = file.metadata()?.len();
    if len > MAX_LOG {
        file.set_len(0)?;
        len = 0;
    }
    Ok((file, len))
}

/// The watchdog's whole life. It reads from the app: a byte says the app
/// has dealt with the daemon itself; the end of input says the app is gone,
/// and then the daemon is stopped the way the app would have stopped it.
#[cfg(unix)]
fn watch(pid: libc::pid_t, mut app: impl Read) {
    let mut byte = [0u8; 1];
    loop {
        match app.read(&mut byte) {
            Ok(0) => break,
            Ok(_) => return,
            Err(e) if e.kind() == std::io::ErrorKind::Interrupted => {}
            Err(_) => break,
        }
    }
    let alive = || unsafe { libc::kill(pid, 0) } == 0;
    unsafe { libc::kill(pid, libc::SIGTERM) };
    let until = Instant::now() + STOP_TIMEOUT;
    while alive() && Instant::now() < until {
        sleep(Duration::from_millis(20));
    }
    if alive() {
        unsafe { libc::kill(pid, libc::SIGKILL) };
    }
}

/// Runs the watchdog if that is what this process was started as, and says
/// whether it was. Called first thing, before there is any app.
pub fn watchdog() -> bool {
    let mut args = std::env::args().skip(1);
    if args.next().as_deref() != Some(WATCHDOG_ARG) {
        return false;
    }
    #[cfg(unix)]
    if let Some(pid) = args.next().and_then(|p| p.parse::<libc::pid_t>().ok()).filter(|p| *p > 1) {
        // Whatever takes the app down must not take this down with it:
        // Ctrl-C in a terminal reaches every process started from it.
        for signal in [libc::SIGINT, libc::SIGTERM, libc::SIGHUP] {
            unsafe { libc::signal(signal, libc::SIG_IGN) };
        }
        watch(pid, std::io::stdin().lock());
    }
    true
}

enum Msg {
    Start,
    Stop(Sender<()>),
}

/// The app's handle on its supervisor, which runs on a thread of its own.
pub struct Daemon {
    status: Arc<Mutex<Status>>,
    tx: Sender<Msg>,
}

impl Daemon {
    /// Starts looking for a daemon, and starting one if there is none.
    pub fn launch(app: &AppHandle) -> Self {
        let status = Arc::new(Mutex::new(Status::Starting));
        let (tx, rx) = mpsc::channel();
        let report = {
            let (status, app) = (status.clone(), app.clone());
            move |next: Status| {
                let mut current = status.lock().unwrap();
                if *current != next {
                    *current = next.clone();
                    drop(current);
                    let _ = app.emit(EVENT, next);
                }
            }
        };
        let cfg = config(app);
        std::thread::spawn(move || supervise(Supervisor::new(cfg, report), rx));
        Self { status, tx }
    }

    /// Waits, briefly, for the first answer. The page opens its connection
    /// to the daemon as it loads and retries only seconds later, so a
    /// daemon that is up first makes the window fill in at once.
    pub fn settle(&self, within: Duration) {
        let until = Instant::now() + within;
        while *self.status.lock().unwrap() == Status::Starting && Instant::now() < until {
            sleep(Duration::from_millis(10));
        }
    }

    /// Stops the daemon if this app started it, and returns once it has
    /// exited. For when the app quits.
    pub fn stop(&self) {
        let (done, stopped) = mpsc::channel();
        if self.tx.send(Msg::Stop(done)).is_ok() {
            let _ = stopped.recv_timeout(STOP_TIMEOUT * 2);
        }
    }
}

fn supervise(mut sup: Supervisor, rx: Receiver<Msg>) {
    sup.start();
    loop {
        match rx.recv_timeout(sup.tick()) {
            Ok(Msg::Start) => sup.start(),
            Ok(Msg::Stop(done)) => {
                sup.stop();
                let _ = done.send(());
                return;
            }
            Err(RecvTimeoutError::Timeout) => sup.poll(),
            Err(RecvTimeoutError::Disconnected) => return sup.stop(),
        }
    }
}

fn config(app: &AppHandle) -> Config {
    // The daemon's own rule (cmd/leadlined): LEADLINE_DATA, or ~/.leadline.
    let data = std::env::var_os("LEADLINE_DATA")
        .filter(|v| !v.is_empty())
        .map(PathBuf::from)
        .or_else(|| app.path().home_dir().ok().map(|home| home.join(".leadline")))
        .unwrap_or_else(|| PathBuf::from(".leadline"));
    // Its log lives with its data, unless there is nowhere to keep data.
    let log = if data.as_os_str() == ":memory:" { std::env::temp_dir() } else { data.clone() }.join("leadlined.log");
    let exe = tauri::utils::platform::current_exe().ok();
    Config {
        addr: ADDR.into(),
        // Only the copy the app carries, never one found on PATH.
        sidecar: exe.as_deref().and_then(Path::parent).unwrap_or(Path::new(".")).join(SIDECAR),
        // -addr and -data are spelled out so the daemon is where the page
        // and the log expect it, whatever else is in the environment.
        args: vec!["-addr".into(), ADDR.into(), "-data".into(), data.display().to_string()],
        log,
        watchdog: exe.filter(|_| cfg!(unix)),
    }
}

#[tauri::command]
pub fn daemon_status(daemon: State<Daemon>) -> Status {
    daemon.status.lock().unwrap().clone()
}

/// Tries again after a failure; does nothing while a daemon is running.
#[tauri::command]
pub fn daemon_start(daemon: State<Daemon>) {
    let _ = daemon.tx.send(Msg::Start);
}

#[cfg(all(test, unix))]
mod tests {
    use super::*;
    use std::net::TcpListener;

    fn free_addr() -> String {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        listener.local_addr().unwrap().to_string()
    }

    /// A supervisor for the real leadlined on a port of its own, keeping
    /// nothing on disk, and the statuses it has reported.
    fn supervisor(name: &str) -> Option<(Supervisor, Arc<Mutex<Vec<Status>>>)> {
        let sidecar = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("binaries")
            .join(format!("{SIDECAR}-{}", env!("TAURI_ENV_TARGET_TRIPLE")));
        if !sidecar.is_file() {
            eprintln!("skipped: no {} (pnpm sidecar builds it)", sidecar.display());
            return None;
        }
        let addr = free_addr();
        let seen = Arc::new(Mutex::new(Vec::new()));
        let cfg = Config {
            args: ["-addr", &addr, "-data", ":memory:", "-otlp", ""].map(String::from).to_vec(),
            log: std::env::temp_dir().join(format!("leadline-test-{}-{name}.log", std::process::id())),
            addr,
            sidecar,
            watchdog: None,
        };
        let report = {
            let seen = seen.clone();
            move |s| seen.lock().unwrap().push(s)
        };
        Some((Supervisor::new(cfg, report), seen))
    }

    /// Removes what a test left in the temp directory.
    fn tidy(sup: &Supervisor) {
        let port = sup.cfg.addr.rsplit(':').next().unwrap();
        let _ = std::fs::remove_file(std::env::temp_dir().join(format!("leadline-{port}.lock")));
        let _ = std::fs::remove_file(&sup.cfg.log);
    }

    fn poll_until(sup: &mut Supervisor, seen: &Arc<Mutex<Vec<Status>>>, done: impl Fn(&Status) -> bool) -> Status {
        let until = Instant::now() + Duration::from_secs(20);
        loop {
            let last = seen.lock().unwrap().last().cloned().unwrap();
            if done(&last) {
                return last;
            }
            assert!(Instant::now() < until, "still {last:?}");
            sleep(Duration::from_millis(20));
            sup.poll();
        }
    }

    #[test]
    fn knows_the_daemons_health_from_anything_else() {
        let ok = "HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\n{\"name\":\"leadline\",\"ok\":true}\n";
        assert!(is_leadline(ok));
        assert!(!is_leadline("HTTP/1.0 200 OK\r\n\r\n{\"name\":\"grafana\",\"ok\":true}"));
        assert!(!is_leadline("HTTP/1.0 403 Forbidden\r\n\r\n{\"name\":\"leadline\",\"ok\":true}"));
        assert!(!is_leadline("HTTP/1.0 200 OK\r\n\r\n<html>"));
        assert!(!is_leadline(""));
    }

    #[test]
    fn probes_a_free_port_and_a_strangers() {
        assert_eq!(probe(&free_addr()), Probe::Nothing);
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = listener.local_addr().unwrap().to_string();
        let server = std::thread::spawn(move || {
            let (mut conn, _) = listener.accept().unwrap();
            let _ = conn.write_all(b"HTTP/1.0 404 Not Found\r\n\r\n");
        });
        assert_eq!(probe(&addr), Probe::Other);
        server.join().unwrap();
    }

    #[test]
    fn starts_the_daemon_and_stops_it_cleanly() {
        let Some((mut sup, seen)) = supervisor("own") else { return };
        sup.start();
        assert_eq!(poll_until(&mut sup, &seen, |s| *s != Status::Starting), Status::Running { owned: true });
        assert_eq!(probe(&sup.cfg.addr), Probe::Leadline);
        // A second start while it runs starts nothing.
        sup.start();
        assert!(matches!(sup.phase, Phase::Ours(_)));

        let Phase::Ours(mut daemon) = std::mem::replace(&mut sup.phase, Phase::Idle) else { unreachable!() };
        terminate(&mut daemon.child);
        // SIGTERM is a clean exit for the daemon, not a death by signal.
        assert_eq!(daemon.child.wait().unwrap().code(), Some(0));
        assert_eq!(probe(&sup.cfg.addr), Probe::Nothing);
        tidy(&sup);
    }

    #[test]
    fn uses_a_running_daemon_and_leaves_it_running() {
        let Some((mut theirs, _)) = supervisor("theirs") else { return };
        theirs.start();
        let Phase::Starting { daemon: mut other, .. } = std::mem::replace(&mut theirs.phase, Phase::Idle) else {
            panic!("the first daemon didn't start")
        };
        while probe(&theirs.cfg.addr) != Probe::Leadline {
            assert!(other.exited().is_none(), "{}", other.tail());
            sleep(Duration::from_millis(20));
        }

        let Some((mut sup, seen)) = supervisor("second") else { return };
        sup.cfg.addr = theirs.cfg.addr.clone();
        sup.start();
        assert_eq!(seen.lock().unwrap().last(), Some(&Status::Running { owned: false }));
        sup.stop();
        assert_eq!(probe(&sup.cfg.addr), Probe::Leadline);

        // And says so when that daemon goes away.
        other.stop();
        let failed = poll_until(&mut sup, &seen, |s| matches!(s, Status::Failed { .. }));
        assert!(matches!(failed, Status::Failed { ref reason, .. } if reason.contains("has stopped")), "{failed:?}");
        tidy(&theirs);
    }

    #[test]
    fn says_why_when_the_daemon_dies() {
        let Some((mut sup, seen)) = supervisor("dies") else { return };
        sup.start();
        poll_until(&mut sup, &seen, |s| *s == Status::Running { owned: true });
        let Phase::Ours(daemon) = &sup.phase else { unreachable!() };
        unsafe { libc::kill(daemon.child.id() as libc::pid_t, libc::SIGKILL) };
        let failed = poll_until(&mut sup, &seen, |s| matches!(s, Status::Failed { .. }));
        let Status::Failed { reason, detail, log, .. } = failed else { unreachable!() };
        assert!(reason.contains("signal: 9"), "{reason}");
        assert!(detail.contains("listening"), "{detail}");
        assert_eq!(log, sup.cfg.log.display().to_string());

        // Starting again works.
        sup.start();
        assert_eq!(poll_until(&mut sup, &seen, |s| *s != Status::Starting), Status::Running { owned: true });
        sup.stop();
        assert_eq!(probe(&sup.cfg.addr), Probe::Nothing);
        tidy(&sup);
    }

    #[test]
    fn says_what_to_do_when_the_daemon_is_missing() {
        let Some((mut sup, seen)) = supervisor("missing") else { return };
        sup.cfg.sidecar = PathBuf::from("/nonexistent/leadlined");
        sup.start();
        let last = seen.lock().unwrap().last().cloned().unwrap();
        assert!(matches!(last, Status::Failed { ref hint, .. } if hint.contains("/nonexistent/leadlined")), "{last:?}");
        tidy(&sup);
    }

    #[test]
    fn the_watchdog_stops_the_daemon_only_when_the_app_is_gone() {
        let mut child = Command::new("sleep").arg("60").spawn().unwrap();
        let pid = child.id() as libc::pid_t;
        // The app said it has dealt with the daemon: hands off.
        watch(pid, &b"q"[..]);
        assert!(child.try_wait().unwrap().is_none());
        // The app is gone. (Someone has to wait for the child, as launchd
        // does for an orphan, or it stays around as a zombie.)
        let waiter = std::thread::spawn(move || child.wait().unwrap());
        watch(pid, std::io::empty());
        let status = waiter.join().unwrap();
        assert_eq!(std::os::unix::process::ExitStatusExt::signal(&status), Some(libc::SIGTERM));
    }
}
