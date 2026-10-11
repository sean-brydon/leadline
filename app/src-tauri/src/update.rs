//! The app updates itself from the latest GitHub release.
//!
//! A release carries `latest.json`, the feed: a version, and for each kind
//! of Mac the address of an archive of the app and that archive's
//! signature. Ten seconds after it opens, and every four hours after, the
//! app reads the feed. A newer version is downloaded, quietly, and checked
//! against the public key the app was built with (plugins.updater.pubkey in
//! tauri.conf.json); an archive signed with any other key is thrown away.
//! Nothing on disk changes until the person presses "Restart to update":
//! then the new app replaces this one and opens in its place. The app never
//! restarts by itself. (Unless it is told to: LEADLINE_UPDATES=install
//! restarts into an update as soon as it is ready, for the test that
//! watches an app update itself, scripts/release-test/updater-test.sh.
//! LEADLINE_UPDATES=off looks for none.)
//!
//! The new app carries a new daemon. Quitting stops the daemon this app
//! started (daemon.rs), and the app that opens starts the one it carries,
//! so after the restart both are the new version. A daemon someone else
//! started is left running, as always, and stays the version it was.
//!
//! All of it happens here, not in the page. The page is told one value,
//! `Status` (the `update_status` command, and an `update` event when it
//! changes), and can ask for one thing, `update_install`. It has no
//! permission to the updater itself (capabilities/default.json).

use serde::Serialize;
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::thread::sleep;
use std::time::Duration;
use tauri::{AppHandle, Emitter, State};
use tauri_plugin_updater::{Update, UpdaterExt};

const EVENT: &str = "update";
/// What tauri.conf.json holds where the public key goes until there is one
/// (AGENTS.md says how to make it). build.rs refuses to build a release
/// with it. An app built to try has it, and doesn't look for updates it
/// could never check.
pub const NO_KEY: &str = "UPDATER-PUBLIC-KEY-NOT-SET";

/// The window and the daemon settle first.
const FIRST_CHECK: Duration = Duration::from_secs(10);
const EVERY: Duration = Duration::from_secs(4 * 60 * 60);
const CHECK_TIMEOUT: Duration = Duration::from_secs(30);
const DOWNLOAD_TIMEOUT: Duration = Duration::from_secs(10 * 60);

/// What the page is told about updates.
#[derive(Clone, Debug, PartialEq, Serialize)]
#[serde(tag = "state", rename_all = "lowercase")]
pub enum Status {
    /// Nothing to offer: not checked yet, up to date, a check that failed
    /// (the next one may not), or a copy that doesn't update itself.
    Idle,
    /// `version` is downloaded and checked, and waits for a restart.
    Ready { version: String },
    Installing { version: String },
    /// Putting `version` in place failed, and `reason` says why. It can be
    /// tried again.
    Failed { version: String, reason: String },
}

struct Shared {
    status: Mutex<Status>,
    /// The update that is ready, and its archive.
    pending: Mutex<Option<(Update, Vec<u8>)>>,
    /// Set once an update is in place: quitting then opens the app again.
    reopen: AtomicBool,
}

/// The app's handle on its updates.
#[derive(Clone)]
pub struct Updates(Arc<Shared>);

impl Updates {
    /// Starts looking for updates, in a copy of the app that can take one.
    pub fn launch(app: &AppHandle) -> Self {
        let me = Self(Arc::new(Shared { status: Mutex::new(Status::Idle), pending: Mutex::new(None), reopen: AtomicBool::new(false) }));
        match why_not(app) {
            Some(why) => note(&format!("not looking for updates: {why}")),
            None => {
                let (app, me) = (app.clone(), me.clone());
                std::thread::spawn(move || me.watch(&app));
            }
        }
        me
    }

    /// Checks now and then until an update is ready. After that there is
    /// nothing to look for: the next check is the new app's.
    fn watch(&self, app: &AppHandle) {
        sleep(FIRST_CHECK);
        loop {
            match tauri::async_runtime::block_on(fetch(app)) {
                Ok(Some((update, archive))) => {
                    let version = update.version.clone();
                    *self.0.pending.lock().unwrap() = Some((update, archive));
                    self.set(app, Status::Ready { version });
                    if std::env::var("LEADLINE_UPDATES").as_deref() == Ok("install") {
                        tauri::async_runtime::block_on(self.install(app));
                    }
                    return;
                }
                Ok(None) => {}
                // Offline, or GitHub is: the next check may fare better,
                // and there is nothing for the person to do about it.
                Err(why) => note(&format!("the check for an update failed: {why}")),
            }
            sleep(EVERY);
        }
    }

    fn set(&self, app: &AppHandle, next: Status) {
        *self.0.status.lock().unwrap() = next.clone();
        let _ = app.emit(EVENT, next);
    }

    /// Puts the update that is ready in place of this app and quits, which
    /// stops the daemon and opens the new app (lib.rs).
    async fn install(&self, app: &AppHandle) {
        let Some((update, archive)) = self.0.pending.lock().unwrap().take() else {
            return;
        };
        let version = update.version.clone();
        self.set(app, Status::Installing { version: version.clone() });
        // Off the main thread: where the app can't be replaced without an
        // administrator's password, the updater asks for it there.
        let installing = update.clone();
        let done = tauri::async_runtime::spawn_blocking(move || {
            let result = installing.install(&archive);
            (result, archive)
        })
        .await;
        match done {
            Ok((Ok(()), _)) => {
                self.0.reopen.store(true, Ordering::Relaxed);
                // On a Mac quitting opens the new app itself (reopen).
                // Elsewhere Tauri's own restart does.
                if cfg!(target_os = "macos") {
                    app.exit(0);
                } else {
                    app.request_restart();
                }
            }
            // Still there to try again.
            Ok((Err(why), archive)) => {
                *self.0.pending.lock().unwrap() = Some((update, archive));
                self.set(app, Status::Failed { version, reason: why.to_string() });
            }
            Err(why) => self.set(app, Status::Failed { version, reason: why.to_string() }),
        }
    }

    /// Opens the app again if an update was just put in place. Called as
    /// the app quits, after its daemon has stopped, so the app that opens
    /// finds the port free and starts the daemon it carries.
    pub fn reopen(&self) {
        if !self.0.reopen.load(Ordering::Relaxed) {
            return;
        }
        // Tauri's own restart opens the bundle the same way, through
        // LaunchServices, so that launchd owns the new app. But an app
        // opened that way gets launchd's environment, not this one's, and a
        // person who opened Leadline with LEADLINE_DATA set would find the
        // updated app showing a different, empty database. So what is
        // Leadline's in the environment goes with it.
        #[cfg(target_os = "macos")]
        if let Some(bundle) = tauri::utils::platform::current_exe().ok().as_deref().and_then(bundle_of) {
            let mut open = std::process::Command::new("/usr/bin/open");
            open.arg("-n").arg(bundle);
            for (name, value) in std::env::vars_os() {
                if name.to_string_lossy().starts_with("LEADLINE_") {
                    let mut pair = name;
                    pair.push("=");
                    pair.push(value);
                    open.arg("--env").arg(pair);
                }
            }
            use std::process::Stdio;
            let _ = open.stdin(Stdio::null()).stdout(Stdio::null()).stderr(Stdio::null()).status();
        }
    }
}

/// Reads the feed, and downloads and checks what it announces if that is
/// newer than this app.
async fn fetch(app: &AppHandle) -> tauri_plugin_updater::Result<Option<(Update, Vec<u8>)>> {
    let Some(mut update) = app.updater_builder().timeout(CHECK_TIMEOUT).build()?.check().await? else {
        return Ok(None);
    };
    update.timeout = Some(DOWNLOAD_TIMEOUT);
    // The signature is checked before this returns.
    let archive = update.download(|_, _| {}, || {}).await?;
    Ok(Some((update, archive)))
}

/// Why this copy of the app doesn't update itself, if it doesn't.
fn why_not(app: &AppHandle) -> Option<&'static str> {
    let key = app.config().plugins.0.get("updater").and_then(|u| u["pubkey"].as_str()).unwrap_or_default();
    let exe = tauri::utils::platform::current_exe().unwrap_or_default();
    reason(cfg!(debug_assertions), key, std::env::var("LEADLINE_UPDATES").ok().as_deref(), read_only(&exe))
}

fn reason(dev: bool, key: &str, wish: Option<&str>, read_only: bool) -> Option<&'static str> {
    if dev {
        // It would offer to replace itself with a release.
        Some("this is a development build")
    } else if key.is_empty() || key == NO_KEY {
        Some("it was built without the updater's public key")
    } else if wish == Some("off") {
        Some("LEADLINE_UPDATES is off")
    } else if read_only {
        // Opened straight from the disk image, or from Downloads before
        // macOS trusts it (it then runs from a read-only copy): there is
        // nowhere to put a new app. It updates once it is in Applications.
        Some("the app is running from a place that can't be written to")
    } else {
        None
    }
}

/// Whether a file is on a volume mounted read-only.
#[cfg(target_os = "macos")]
fn read_only(path: &Path) -> bool {
    use std::os::unix::ffi::OsStrExt;
    let Ok(path) = std::ffi::CString::new(path.as_os_str().as_bytes()) else {
        return false;
    };
    let mut fs = std::mem::MaybeUninit::<libc::statfs>::uninit();
    unsafe { libc::statfs(path.as_ptr(), fs.as_mut_ptr()) == 0 && fs.assume_init().f_flags & libc::MNT_RDONLY as u32 != 0 }
}

#[cfg(not(target_os = "macos"))]
fn read_only(_: &Path) -> bool {
    false
}

/// The bundle an executable is in: Leadline.app for
/// Leadline.app/Contents/MacOS/leadline.
#[cfg_attr(not(target_os = "macos"), allow(dead_code))]
fn bundle_of(exe: &Path) -> Option<PathBuf> {
    let macos = exe.parent()?;
    let contents = macos.parent()?;
    let bundle = contents.parent()?;
    (macos.ends_with("MacOS") && contents.ends_with("Contents") && bundle.extension()? == "app").then(|| bundle.to_path_buf())
}

/// A line for whoever started the app from a terminal. A failure to write
/// it is nothing: opened from Finder the app has nowhere to say it.
fn note(line: &str) {
    let _ = writeln!(std::io::stderr(), "leadline: {line}");
}

#[tauri::command]
pub fn update_status(updates: State<Updates>) -> Status {
    updates.0.status.lock().unwrap().clone()
}

/// Puts the update that is ready in place of this app, then quits; the app
/// opens again as the new version. Does nothing when no update is ready.
#[tauri::command]
pub async fn update_install(app: AppHandle, updates: State<'_, Updates>) -> Result<(), ()> {
    updates.inner().clone().install(&app).await;
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    const KEY: &str = "dW50cnVzdGVkIGNvbW1lbnQ6IGFjbWUK";

    #[test]
    fn only_a_release_in_a_place_it_can_replace_looks_for_updates() {
        assert_eq!(reason(false, KEY, None, false), None);
        assert_eq!(reason(false, KEY, Some("on"), false), None);
        assert!(reason(true, KEY, None, false).unwrap().contains("development"));
        assert!(reason(false, NO_KEY, None, false).unwrap().contains("public key"));
        assert!(reason(false, "", None, false).unwrap().contains("public key"));
        assert!(reason(false, KEY, Some("off"), false).unwrap().contains("LEADLINE_UPDATES"));
        assert!(reason(false, KEY, None, true).unwrap().contains("can't be written"));
    }

    #[test]
    fn finds_the_bundle_around_the_executable() {
        assert_eq!(bundle_of(Path::new("/Applications/Leadline.app/Contents/MacOS/leadline")), Some(PathBuf::from("/Applications/Leadline.app")));
        // Run from a build folder there is no bundle to open again.
        assert_eq!(bundle_of(Path::new("/src/app/src-tauri/target/release/leadline")), None);
        assert_eq!(bundle_of(Path::new("/tmp/Contents/MacOS/leadline")), None);
    }

    #[test]
    fn the_page_reads_a_status_by_its_state() {
        let ready = serde_json::to_value(Status::Ready { version: "0.3.0".into() }).unwrap();
        assert_eq!(ready, serde_json::json!({"state": "ready", "version": "0.3.0"}));
        assert_eq!(serde_json::to_value(Status::Idle).unwrap(), serde_json::json!({"state": "idle"}));
    }
}
