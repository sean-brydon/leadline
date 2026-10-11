fn main() {
    // The app carries the daemon (bundle.externalBin), and tauri-build wants
    // the file there even for a check. Say how to get it rather than only
    // that it is missing.
    let sidecar = format!("binaries/leadlined-{}", std::env::var("TARGET").unwrap_or_default());
    if !std::path::Path::new(&sidecar).is_file() {
        panic!("{sidecar} isn't built yet: run `pnpm sidecar` in app/ (pnpm tauri dev and pnpm tauri build do it themselves)");
    }
    // Naming the app's commands makes each one a permission, so the page can
    // call only what capabilities/default.json lists.
    let commands = tauri_build::AppManifest::new().commands(&["daemon_status", "daemon_start"]);
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(commands)).expect("failed to run tauri-build");
}
