package skills

import (
	"strings"
)

// DefaultURL is the daemon address the skills are written with. Where a
// skill is served for a daemon somewhere else, it is replaced.
const DefaultURL = "http://127.0.0.1:4320"

// setupSkill is the skill that /setup.md is made from. There is one text:
// what an agent with the skill installed reads, and what an agent that was
// only handed a URL reads, are the same file.
const setupSkill = "leadline-setup/SKILL.md"

// setupPreamble is what /setup.md says before the skill. An agent that
// arrives by URL has no skill description to tell it why it is reading
// this, or for whom.
const setupPreamble = `# Set up Leadline in this project

You are a coding agent. The person you are working with has Leadline
running on this machine, at ` + DefaultURL + `, and has asked you to connect
the codebase you are in to it. These are the complete instructions: follow
them from the project's root, in order, and do not stop at the first step
that works. They were served by that person's own Leadline, so the
addresses in them are already the right ones.

Leadline is a development tool. Nothing leaves this machine, and nothing
you set up may reach production.

`

// SetupPrompt is the one line a person pastes into a coding agent to have
// it set Leadline up in a codebase. It asks for curl by name: every agent
// has a shell, and not every agent's own fetch tool can reach localhost.
func SetupPrompt(url string) string {
	return "Set up Leadline in this project: run `curl -s " + clean(url) + "/setup.md` and follow the instructions it prints, start to finish."
}

// SetupMarkdown is the instructions an agent follows to set Leadline up in
// a codebase, for the daemon at url: the setup skill, as embedded in this
// binary, behind a few lines that say what it is for.
func SetupMarkdown(url string) string {
	raw, err := FS.ReadFile(setupSkill)
	if err != nil {
		// The file is embedded; it cannot be missing from a binary that built.
		panic(err)
	}
	body := strings.TrimLeft(stripFrontmatter(string(raw)), "\n")
	// The skill's own title would be a second one.
	if rest, ok := strings.CutPrefix(body, "# "); ok {
		if _, after, found := strings.Cut(rest, "\n"); found {
			body = strings.TrimLeft(after, "\n")
		}
	}
	return strings.ReplaceAll(setupPreamble+body, DefaultURL, clean(url))
}

// stripFrontmatter drops the YAML between the leading --- lines, which is
// for the agent's skill loader and not for reading.
func stripFrontmatter(s string) string {
	rest, ok := strings.CutPrefix(s, "---\n")
	if !ok {
		return s
	}
	if _, after, found := strings.Cut(rest, "\n---\n"); found {
		return after
	}
	return s
}

func clean(url string) string {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	if url == "" {
		return DefaultURL
	}
	return url
}
