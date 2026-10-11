package discover

import (
	"strings"

	"github.com/sean-brydon/leadline/internal/runner"
)

// installDev is the command that adds a development dependency with a
// project's own package manager.
func installDev(manager, pkg string) string {
	switch manager {
	case "pnpm":
		return "pnpm add -D " + pkg
	case "yarn":
		return "yarn add -D " + pkg
	case "bun":
		return "bun add -d " + pkg
	}
	return "npm install --save-dev " + pkg
}

func hasFramework(p *Project, id string) bool {
	for _, fw := range p.Frameworks {
		if fw.ID == id {
			return true
		}
	}
	return false
}

func because(p *Project, why string) bool {
	for _, b := range p.Sends.Because {
		if b == why {
			return true
		}
	}
	return false
}

// advise says how to connect a project, as data: the kind, one sentence,
// and the exact command or lines. It changes nothing. daemon is the
// daemon's own address: the advice is followed on the daemon's machine.
func advise(p *Project, daemon string) Connect {
	// A daemon that is not where `leadline run` and the providers look
	// has to be named, or what is advised would send to nobody.
	elsewhere := !usualDaemon(daemon)
	run := "leadline run -- "
	if elsewhere {
		run = "leadline run --url " + daemon + " -- "
	}
	wrap := run + "<your dev command>"
	if p.DevCommand != "" {
		wrap = run + p.DevCommand
	}

	// Data is arriving, or will the next time it starts: nothing to do.
	switch {
	case because(p, "service"):
		return Connect{Kind: "already", Summary: "Already sends to Leadline as " + p.Sends.Service + "."}
	case because(p, "run"):
		return Connect{Kind: "already", Summary: "Already runs under leadline run."}
	case because(p, "dev-script"):
		return Connect{Kind: "already", Summary: "Its dev script already uses leadline run: it reports when it is started."}
	case because(p, "plugin"):
		return Connect{Kind: "already", Summary: "The Next.js plugin is in place: it reports when the dev server runs."}
	}

	// The provider is installed and nothing loads it yet.
	if because(p, "provider") {
		return Connect{Kind: "run-wrap", Command: wrap,
			Summary: "@leadline/node is installed: start the dev command with leadline run, which loads it."}
	}

	// Two OpenTelemetry setups in one process fight, so a project that
	// has one is pointed at Leadline, never given a second.
	if p.OpenTelemetry {
		file := ".env"
		if p.Stack == "node" {
			file = ".env.local"
		}
		c := Connect{Kind: "otel-env", File: file,
			Summary: "It already has OpenTelemetry: point its exporter at Leadline.",
			Lines: []string{
				"OTEL_EXPORTER_OTLP_ENDPOINT=" + daemon,
				"OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf",
			}}
		if p.DevCommand != "" {
			// leadline run sets the same two itself.
			c.Command = wrap
		}
		return c
	}

	if hasFramework(p, "next") {
		file := p.nextConfig
		if file == "" {
			file = "next.config.ts"
		}
		c := Connect{Kind: "next-plugin", File: file,
			Summary: "Add the Next.js plugin: one development dependency and one line in " + file + ".",
			Command: installDev(p.PackageManager, "@leadline/next"),
			Lines: []string{
				`import { withLeadline } from "@leadline/next";`,
				"export default withLeadline(nextConfig);",
			}}
		if p.hasPlugin {
			// Installed, and next.config does not load it yet.
			c.Command = ""
			c.Summary = "@leadline/next is installed but " + file + " does not load it: wrap the config with withLeadline."
		}
		if elsewhere {
			// The plugin sends to LEADLINE_URL, else to the usual
			// address.
			c.EnvFile, c.EnvLines = ".env.local", []string{"LEADLINE_URL=" + daemon}
		}
		return c
	}

	c := Connect{Kind: "run-wrap", Command: wrap,
		Summary: "Put leadline run in front of the dev command: its output becomes logs and its Postgres queries are recorded."}
	if p.Stack == "node" {
		c.Traces = &ConnectStep{
			Summary: "For traces too, add the Node.js provider. leadline run loads it.",
			Command: installDev(p.PackageManager, "@leadline/node"),
		}
	}
	return c
}

// trimDaemon is the daemon's address without a trailing slash; the usual
// one when none was given.
func trimDaemon(url string) string {
	if url == "" {
		return runner.DefaultURL
	}
	return strings.TrimRight(url, "/")
}

// usualDaemon reports whether an address is the one `leadline run` and
// the providers use when told nothing.
func usualDaemon(url string) bool {
	url = strings.TrimRight(url, "/")
	return url == strings.TrimRight(runner.DefaultURL, "/") || url == "http://localhost:4320"
}
