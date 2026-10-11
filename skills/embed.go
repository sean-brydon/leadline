// Package skills holds the agent skills Leadline ships, so the command
// line can install them into a project (leadline skills).
package skills

import "embed"

//go:embed */SKILL.md
var FS embed.FS
