// Package discover looks around a machine for what Leadline could watch,
// so onboarding can offer it instead of asking for a URL to be pasted:
// what is listening now, the projects behind it, the database and Redis
// URLs in those projects' env files, and the owner's Shipyard boxes.
//
// It only reads, and it reads little. The rules, which API.md states for
// the people who rely on them and the tests hold the code to:
//
//   - Nothing is crawled. Projects come from the working directories of
//     listening dev servers, from `leadline run` commands the daemon has
//     seen, and from folders a person names, which are walked to a small
//     fixed depth. The home directory itself is refused.
//   - A scan opens no env file. It says which env files a project has,
//     from the folder's listing. Reading them is a second step, ReadEnv,
//     which takes the projects to read by name and reads no other's.
//   - An env file is read for connection URLs only. Every other value is
//     dropped as it is parsed and only counted; the file's bytes are wiped
//     when the parse ends.
//   - A URL's password stays in this package and in internal/integrations,
//     which saves it. What leaves is a masked URL and an opaque id; no
//     exported type has a field that could carry the URL as written.
//   - No project is modified. How to connect one is advice, as data.
//   - Shipyard is asked with read-only commands, and only when someone
//     asks about Shipyard. Two things change anything there, each from one
//     place and only when a person asks for it: starting a task (task.go)
//     and opening or closing a forward to a box's Leadline (forward.go).
//
// The HTTP API over this package is described in API.md.
package discover
