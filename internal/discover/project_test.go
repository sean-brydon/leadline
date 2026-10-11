package discover

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func frameworksOf(p *Project) string {
	var out []string
	for _, fw := range p.Frameworks {
		out = append(out, strings.TrimSuffix(fw.ID+"@"+fw.Version, "@"))
	}
	return strings.Join(out, " ")
}

func TestDetectProjects(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string

		wantName, stack, label, frameworks, manager, dev string
		otel                                             bool
		because                                          string
		// The advice: its kind, and its command.
		kind, command string
	}{
		{
			name: "next with pnpm",
			files: map[string]string{
				"package.json":   `{"name":"acme","scripts":{"dev":"next dev --turbopack","build":"next build"},"dependencies":{"next":"^16.0.1","react":"19.2.0"}}`,
				"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
				"next.config.ts": "export default {};\n",
				// Installed, as npm lays it out: the exact version.
				"node_modules/next/package.json": `{"name":"next","version":"16.0.3"}`,
			},
			wantName: "acme", stack: "node", label: "Next.js 16", frameworks: "next@16.0.3", manager: "pnpm", dev: "pnpm dev",
			kind: "next-plugin", command: "pnpm add -D @leadline/next",
		},
		{
			name: "next with the plugin wired",
			files: map[string]string{
				"package.json":    `{"name":"acme-web","scripts":{"dev":"next dev"},"dependencies":{"next":"15.5.2"},"devDependencies":{"@leadline/next":"^0.1.0"}}`,
				"next.config.mjs": "import { withLeadline } from \"@leadline/next\";\nexport default withLeadline({});\n",
				"yarn.lock":       "",
			},
			wantName: "acme-web", stack: "node", label: "Next.js 15", frameworks: "next@15.5.2", manager: "yarn", dev: "yarn dev",
			because: "plugin", kind: "already",
		},
		{
			name: "next with the plugin installed and not wired",
			files: map[string]string{
				"package.json":   `{"name":"acme-web","scripts":{"dev":"next dev"},"dependencies":{"next":"15.5.2","@leadline/next":"^0.1.0"}}`,
				"next.config.js": "module.exports = {};\n",
			},
			wantName: "acme-web", stack: "node", label: "Next.js 15", frameworks: "next@15.5.2", manager: "npm", dev: "npm run dev",
			kind: "next-plugin", command: "",
		},
		{
			name: "next that has its own opentelemetry",
			files: map[string]string{
				"package.json": `{"name":"acme-otel","scripts":{"dev":"next dev"},"dependencies":{"next":"16.0.0","@vercel/otel":"^1.0.0"}}`,
				"bun.lock":     "",
			},
			wantName: "acme-otel", stack: "node", label: "Next.js 16", frameworks: "next@16.0.0", manager: "bun", dev: "bun run dev",
			otel: true, kind: "otel-env", command: "leadline run -- bun run dev",
		},
		{
			name: "vite and express",
			files: map[string]string{
				"package.json":      `{"name":"acme-admin","scripts":{"dev":"vite","start":"node server.js"},"dependencies":{"express":"^5.1.0"},"devDependencies":{"vite":"~7.1.4"}}`,
				"package-lock.json": "{}",
			},
			wantName: "acme-admin", stack: "node", label: "Vite + Express", frameworks: "vite@7.1.4 express@5.1.0", manager: "npm", dev: "npm run dev",
			kind: "run-wrap", command: "leadline run -- npm run dev",
		},
		{
			name: "express alone, started with start",
			files: map[string]string{
				"package.json": `{"name":"acme-api","packageManager":"pnpm@10.4.0","scripts":{"start":"node index.js","test":"vitest"},"dependencies":{"express":"4.21.2"}}`,
			},
			wantName: "acme-api", stack: "node", label: "Express 4", frameworks: "express@4.21.2", manager: "pnpm", dev: "pnpm start",
			kind: "run-wrap", command: "leadline run -- pnpm start",
		},
		{
			name: "a dev script that already uses leadline run",
			files: map[string]string{
				"package.json": `{"name":"acme-run","scripts":{"dev":"leadline run -- vite"},"devDependencies":{"vite":"latest","@leadline/node":"^0.1.0"}}`,
			},
			wantName: "acme-run", stack: "node", label: "Vite", frameworks: "vite", manager: "npm", dev: "npm run dev",
			because: "dev-script provider", kind: "already",
		},
		{
			name: "the provider installed and nothing loading it",
			files: map[string]string{
				"package.json": `{"name":"acme-node","scripts":{"dev":"node --watch server.js"},"devDependencies":{"@leadline/node":"^0.1.0","fastify":"5.2.0"}}`,
			},
			wantName: "acme-node", stack: "node", label: "Fastify 5", frameworks: "fastify@5.2.0", manager: "npm", dev: "npm run dev",
			because: "provider", kind: "run-wrap", command: "leadline run -- npm run dev",
		},
		{
			name: "a package.json nobody can read",
			files: map[string]string{
				"package.json": `{ this is not json`,
			},
			stack: "node", label: "Node.js", manager: "", kind: "run-wrap", command: "leadline run -- <your dev command>",
		},
		{
			name: "go with a framework and opentelemetry",
			files: map[string]string{
				"go.mod":              "module github.com/acme/billing-service/v2\n\ngo 1.27.0\n\nrequire (\n\tgithub.com/go-chi/chi/v5 v5.2.1\n\tgo.opentelemetry.io/otel v1.38.0\n)\n",
				"cmd/billing/main.go": "package main\nfunc main() {}\n",
			},
			wantName: "billing-service", stack: "go", label: "Go 1.27 + chi", frameworks: "go@1.27 chi@5.2.1", manager: "go", dev: "go run ./cmd/billing",
			otel: true, kind: "otel-env", command: "leadline run -- go run ./cmd/billing",
		},
		{
			name: "go with main.go at the root",
			files: map[string]string{
				"go.mod":  "module acme.test/tool\n\ngo 1.25\n\nrequire github.com/gin-gonic/gin v1.10.0\n",
				"main.go": "package main\nfunc main() {}\n",
			},
			wantName: "tool", stack: "go", label: "Go 1.25 + Gin", frameworks: "go@1.25 gin@1.10.0", manager: "go", dev: "go run .",
			kind: "run-wrap", command: "leadline run -- go run .",
		},
		{
			name: "go with several commands",
			files: map[string]string{
				"go.mod":             "module acme.test/many\n\ngo 1.27\n",
				"cmd/api/main.go":    "package main\n",
				"cmd/worker/main.go": "package main\n",
			},
			wantName: "many", stack: "go", label: "Go 1.27", frameworks: "go@1.27", manager: "go", dev: "",
			kind: "run-wrap", command: "leadline run -- <your dev command>",
		},
		{
			name: "django with uv",
			files: map[string]string{
				"pyproject.toml": "[project]\nname = \"acme-shop\"\ndependencies = [\n  \"django>=5.1\",\n  \"django-environ==0.11.2\",\n  \"psycopg[binary]>=3.2\",\n]\n",
				"uv.lock":        "",
				"manage.py":      "#!/usr/bin/env python\n",
			},
			wantName: "acme-shop", stack: "python", label: "Django 5.1", frameworks: "django@5.1", manager: "uv", dev: "uv run python manage.py runserver",
			kind: "run-wrap", command: "leadline run -- uv run python manage.py runserver",
		},
		{
			name: "fastapi with poetry",
			files: map[string]string{
				"pyproject.toml": "[tool.poetry]\nname = \"acme-api\"\n\n[tool.poetry.dependencies]\npython = \"^3.13\"\nfastapi = \"^0.115.6\"\nopentelemetry-sdk = \"^1.29\"\n",
				"main.py":        "from fastapi import FastAPI\n",
			},
			wantName: "acme-api", stack: "python", label: "FastAPI 0.115", frameworks: "fastapi@0.115.6", manager: "poetry", dev: "poetry run fastapi dev main.py",
			otel: true, kind: "otel-env", command: "leadline run -- poetry run fastapi dev main.py",
		},
		{
			name: "flask from requirements.txt, in a git checkout",
			files: map[string]string{
				".git/HEAD":        "ref: refs/heads/main\n",
				"requirements.txt": "Flask==3.1.0\ngunicorn\n",
				"app.py":           "from flask import Flask\n",
				"manage.py.bak":    "",
			},
			stack: "python", label: "Flask 3.1", frameworks: "flask@3.1.0", manager: "pip", dev: "flask run --debug",
			kind: "run-wrap", command: "leadline run -- flask run --debug",
		},
		{
			name: "a git checkout of nothing known",
			files: map[string]string{
				".git/HEAD": "ref: refs/heads/main\n",
				"README.md": "# notes\n",
			},
			stack: "unknown", label: "Folder", kind: "run-wrap", command: "leadline run -- <your dev command>",
		},
		{
			name: "a pyproject that only configures a linter",
			files: map[string]string{
				"pyproject.toml": "[tool.ruff]\nline-length = 100\n\n[[tool.uv.index]]\nname = \"acme-internal\"\n",
			},
			stack: "python", label: "Python", manager: "pip", kind: "run-wrap", command: "leadline run -- <your dev command>",
		},
		{
			name: "rails",
			files: map[string]string{
				"Gemfile":      "source \"https://rubygems.org\"\n\ngem \"rails\", \"~> 8.0.1\"\ngem \"pg\"\n",
				"Gemfile.lock": "GEM\n  specs:\n    rails (8.0.2)\n      actioncable (= 8.0.2)\n",
				"bin/dev":      "#!/usr/bin/env sh\n",
				"package.json": `{"name":"app","private":true,"scripts":{"build":"esbuild"}}`,
			},
			wantName: "app", stack: "ruby", label: "Rails 8.0", frameworks: "rails@8.0.2", manager: "bundler", dev: "bin/dev",
			kind: "run-wrap", command: "leadline run -- bin/dev",
		},
		{
			name: "rust",
			files: map[string]string{
				"Cargo.toml": "[package]\nname = \"acme-edge\"\nversion = \"0.1.0\"\n\n[dependencies]\naxum = { version = \"0.8.1\", features = [\"macros\"] }\ntokio = \"1\"\n",
			},
			stack: "rust", label: "Axum 0.8", frameworks: "axum@0.8.1", manager: "cargo", dev: "cargo run",
			kind: "run-wrap", command: "leadline run -- cargo run",
		},
	}
	for i, c := range cases {
		// The folder's name is the project's when no file names it.
		dir := filepath.Join(t.TempDir(), fmt.Sprintf("folder-%d", i))
		writeFiles(t, dir, c.files)
		p, ok := osFiles().detect(dir, dir)
		if !ok {
			t.Errorf("%s: not taken for a project", c.name)
			continue
		}
		wantName := c.wantName
		if wantName == "" {
			wantName = filepath.Base(dir)
		}
		p.Sends.Already = len(p.Sends.Because) > 0
		advice := advise(p, "http://127.0.0.1:4320")
		got := fmt.Sprintf("name=%s stack=%s label=%q frameworks=%q manager=%s dev=%q otel=%v because=%q kind=%s command=%q",
			p.Name, p.Stack, p.Label, frameworksOf(p), p.PackageManager, p.DevCommand, p.OpenTelemetry, strings.Join(p.Sends.Because, " "), advice.Kind, advice.Command)
		want := fmt.Sprintf("name=%s stack=%s label=%q frameworks=%q manager=%s dev=%q otel=%v because=%q kind=%s command=%q",
			wantName, c.stack, c.label, c.frameworks, c.manager, c.dev, c.otel, c.because, c.kind, c.command)
		if got != want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, want)
		}
		if advice.Summary == "" {
			t.Errorf("%s: the advice has no sentence", c.name)
		}
	}
}

func TestDetectIsNotFooledByAnEmptyFolder(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"README.md": "# acme\n", "src/index.ts": "", "notes/package.json/x": ""})
	if p, ok := osFiles().detect(dir, dir); ok {
		t.Errorf("a folder with no project file was taken for a project: %+v", p)
	}
	// A directory that happens to be called package.json is not one.
	if p, ok := osFiles().detect(filepath.Join(dir, "notes"), dir); ok {
		t.Errorf("a directory named package.json made a project: %+v", p)
	}
}

func TestAdviceLines(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acme")
	writeFiles(t, dir, map[string]string{
		"package.json":    `{"name":"acme","scripts":{"dev":"next dev"},"dependencies":{"next":"16.0.1"}}`,
		"next.config.mjs": "export default {};\n",
	})
	p, _ := osFiles().detect(dir, dir)
	got := advise(p, "http://127.0.0.1:4320")
	if got.File != "next.config.mjs" || len(got.Lines) != 2 || !strings.Contains(got.Lines[0], `from "@leadline/next"`) || !strings.Contains(got.Summary, "next.config.mjs") {
		t.Errorf("next advice = %+v", got)
	}

	dir = filepath.Join(t.TempDir(), "billing")
	writeFiles(t, dir, map[string]string{"go.mod": "module acme.test/billing\n\ngo 1.27\n\nrequire go.opentelemetry.io/otel v1.38.0\n"})
	p, _ = osFiles().detect(dir, dir)
	got = advise(p, trimDaemon("http://127.0.0.1:4342/"))
	want := []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4342", "OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf"}
	if got.Kind != "otel-env" || got.File != ".env" || strings.Join(got.Lines, "|") != strings.Join(want, "|") {
		t.Errorf("otel advice = %+v", got)
	}

	// A daemon that is not at the usual address is named, in every kind
	// of advice that would otherwise send to nobody.
	const elsewhere = "http://127.0.0.1:4342"
	if got.Command != "" {
		t.Errorf("a project with no dev command got one: %q", got.Command)
	}
	for _, c := range []struct {
		files                    map[string]string
		kind, command            string
		envFile, envLine, traces string
	}{
		{map[string]string{"package.json": `{"name":"a","scripts":{"dev":"next dev"},"dependencies":{"next":"16.0.1"}}`},
			"next-plugin", "npm install --save-dev @leadline/next", ".env.local", "LEADLINE_URL=" + elsewhere, ""},
		{map[string]string{"package.json": `{"name":"b","scripts":{"dev":"vite"},"devDependencies":{"vite":"7.0.0"}}`},
			"run-wrap", "leadline run --url " + elsewhere + " -- npm run dev", "", "", "npm install --save-dev @leadline/node"},
		{map[string]string{"package.json": `{"name":"c","scripts":{"dev":"node x.js"},"devDependencies":{"@leadline/node":"0.1.0"}}`},
			"run-wrap", "leadline run --url " + elsewhere + " -- npm run dev", "", "", ""},
		{map[string]string{"go.mod": "module acme.test/d\n\ngo 1.27\n\nrequire go.opentelemetry.io/otel v1.38.0\n", "main.go": "package main\n"},
			"otel-env", "leadline run --url " + elsewhere + " -- go run .", "", "", ""},
		{map[string]string{"Cargo.toml": "[workspace]\n"},
			"run-wrap", "leadline run --url " + elsewhere + " -- <your dev command>", "", "", ""},
	} {
		dir := filepath.Join(t.TempDir(), "x")
		writeFiles(t, dir, c.files)
		p, _ := osFiles().detect(dir, dir)
		there := advise(p, elsewhere)
		envLine, traces := strings.Join(there.EnvLines, "|"), ""
		if there.Traces != nil {
			traces = there.Traces.Command
		}
		if there.Kind != c.kind || there.Command != c.command || there.EnvFile != c.envFile || envLine != c.envLine || traces != c.traces {
			t.Errorf("%s elsewhere = %+v (traces %q)", c.kind, there, traces)
		}
		// At the usual address, by either name, nothing is added.
		for _, usual := range []string{"http://127.0.0.1:4320", "http://127.0.0.1:4320/", "http://localhost:4320", ""} {
			here := advise(p, trimDaemon(usual))
			if strings.Contains(here.Command, "--url") || here.EnvFile != "" || len(here.EnvLines) != 0 {
				t.Errorf("%s at %q names the daemon: %+v", c.kind, usual, here)
			}
		}
	}

	// Data already arriving outranks everything.
	p.Sends = Sends{Already: true, Because: []string{"service"}, Service: "billing"}
	if got = advise(p, ""); got.Kind != "already" || !strings.Contains(got.Summary, "billing") {
		t.Errorf("already advice = %+v", got)
	}
}

func TestGit(t *testing.T) {
	base := t.TempDir()
	main := filepath.Join(base, "acme")
	wt := filepath.Join(base, "acme-fix-orders-n1")
	writeFiles(t, base, map[string]string{
		"acme/.git/HEAD": "ref: refs/heads/main\n",
		"acme/.git/worktrees/acme-fix-orders-n1/HEAD":   "ref: refs/heads/fix-orders-n1\n",
		"acme/.git/worktrees/acme-fix-orders-n1/gitdir": wt + "/.git\n",
		"acme/.git/worktrees/gone/HEAD":                 "0123456789abcdef0123456789abcdef01234567\n",
		"acme/.git/worktrees/gone/gitdir":               filepath.Join(base, "gone") + "/.git\n",
		"acme-fix-orders-n1/.git":                       "gitdir: " + main + "/.git/worktrees/acme-fix-orders-n1\n",
		"submodule/.git":                                "gitdir: ../acme/.git/modules/submodule\n",
		"notgit/README.md":                              "",
	})
	f := osFiles()
	g := f.gitAt(main)
	if g == nil || g.Root != main || g.Branch != "main" || g.Worktree || g.MainRepo != "" {
		t.Fatalf("main checkout = %+v", g)
	}
	if len(g.Worktrees) != 2 || g.Worktrees[0] != (GitWorktree{Path: wt, Branch: "fix-orders-n1"}) ||
		g.Worktrees[1] != (GitWorktree{Path: filepath.Join(base, "gone"), Branch: "0123456"}) {
		t.Errorf("worktrees = %+v", g.Worktrees)
	}
	g = f.gitAt(wt)
	if g == nil || !g.Worktree || g.MainRepo != main || g.Branch != "fix-orders-n1" || g.Root != wt {
		t.Errorf("linked worktree = %+v", g)
	}
	if g = f.gitAt(filepath.Join(base, "submodule")); g == nil || g.Worktree || g.Branch != "" {
		t.Errorf("submodule = %+v", g)
	}
	if g = f.gitAt(filepath.Join(base, "notgit")); g != nil {
		t.Errorf("a folder with no .git = %+v", g)
	}
}
