fn main() {
    // The app carries the daemon (bundle.externalBin), and tauri-build wants
    // the file there even for a check. Say how to get it rather than only
    // that it is missing.
    let sidecar = format!("binaries/leadlined-{}", std::env::var("TARGET").unwrap_or_default());
    if !std::path::Path::new(&sidecar).is_file() {
        panic!("{sidecar} isn't built yet: run `pnpm sidecar` in app/ (pnpm tauri dev and pnpm tauri build do it themselves)");
    }
    release_has_its_key();
    // A build for a test names another address for the app's daemon
    // (src/daemon.rs).
    println!("cargo:rerun-if-env-changed=LEADLINE_APP_ADDR");
    // Naming the app's commands makes each one a permission, so the page can
    // call only what capabilities/default.json lists.
    let commands = tauri_build::AppManifest::new().commands(&["daemon_status", "daemon_start", "update_status", "update_install"]);
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(commands)).expect("failed to run tauri-build");
}

/// A release is the build that makes the updater's archive
/// (bundle.createUpdaterArtifacts, which tauri.updater.conf.json turns on).
/// Its public key must be a real one: an app shipped with the placeholder
/// could never take an update, and no later release could fix that.
fn release_has_its_key() {
    println!("cargo:rerun-if-changed=tauri.conf.json");
    println!("cargo:rerun-if-env-changed=TAURI_CONFIG");
    let read = |text: String| serde_json::from_str::<serde_json::Value>(&text).ok();
    let file = std::fs::read_to_string("tauri.conf.json").ok().and_then(read);
    // What `tauri build --config` adds reaches a build script here.
    let given = std::env::var("TAURI_CONFIG").ok().and_then(read);
    let setting = |pointer: &str| given.as_ref().and_then(|c| c.pointer(pointer)).or_else(|| file.as_ref().and_then(|c| c.pointer(pointer))).cloned();
    let release = setting("/bundle/createUpdaterArtifacts").is_some_and(|v| v != false);
    let key = setting("/plugins/updater/pubkey").and_then(|v| v.as_str().map(str::to_owned)).unwrap_or_default();
    // The same words as NO_KEY in src/update.rs.
    if release && (key.is_empty() || key == "UPDATER-PUBLIC-KEY-NOT-SET") {
        panic!(
            "this is a release build, and tauri.conf.json has no updater public key yet (plugins.updater.pubkey is the placeholder). \
             Make the key pair once with `scripts/updater-key.sh`, which says where each half goes; AGENTS.md, \"Versions and releases\", has the rest"
        );
    }
}
