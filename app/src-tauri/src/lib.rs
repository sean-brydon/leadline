// The shell is a window and the daemon's keeper (daemon.rs). The page talks
// to leadlined over HTTP on localhost, so everything it shows also works in
// a browser tab; from here it only hears how the daemon is doing, and when a
// newer Leadline is ready to restart into (update.rs).
mod daemon;
mod update;

use std::time::Duration;
use tauri::Manager;

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    // The same program, run by the app to watch over its daemon.
    if daemon::watchdog() {
        return;
    }
    tauri::Builder::default()
        // Reads the feed and checks a download's signature. The page can't
        // reach it: update.rs decides when, and the page gets no updater
        // permission.
        .plugin(tauri_plugin_updater::Builder::new().build())
        .setup(|app| {
            let daemon = daemon::Daemon::launch(app.handle());
            // The page loads once this returns. The daemon is up in a
            // fraction of a second; a slow one doesn't hold the window up.
            daemon.settle(Duration::from_millis(1500));
            app.manage(daemon);
            app.manage(update::Updates::launch(app.handle()));
            Ok(())
        })
        .invoke_handler(tauri::generate_handler![
            daemon::daemon_status,
            daemon::daemon_start,
            update::update_status,
            update::update_install
        ])
        .build(tauri::generate_context!())
        .expect("error while running Leadline")
        .run(|app, event| {
            // Every way of quitting ends here; the watchdog covers dying.
            if let tauri::RunEvent::Exit = event {
                app.state::<daemon::Daemon>().stop();
                // After an update: the new app, once the old daemon is gone.
                app.state::<update::Updates>().reopen();
            }
        });
}
