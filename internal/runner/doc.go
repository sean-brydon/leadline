// Package runner is `leadline run`: it runs a dev command as if it had been
// run directly, and beside that turns what the command prints into log
// records, sets the command up to send telemetry, and tells the daemon the
// run exists.
//
// The order of concern is the command first, Leadline second. The bytes go
// from the command to the terminal on one path; capture is handed copies
// through a channel that drops when it is full, so nothing Leadline does,
// and nothing the daemon fails to do, can hold the command up.
package runner
