# The desktop app carries the daemon and runs it by itself, so `make app` is
# the whole product, and `make dev` the same from source.
GO ?= go
BIN := bin

# The one version of the project (the VERSION file), stamped into both
# programs with the commit they were built from, when there is one.
VERSION := $(shell cat VERSION)
COMMIT := $(shell git rev-parse --short --verify HEAD 2>/dev/null)
VERSION_PKG := github.com/sean-brydon/leadline/internal/version
LDFLAGS := -X $(VERSION_PKG).Version=$(VERSION) -X $(VERSION_PKG).Commit=$(COMMIT)

# Where `make install` puts the two programs: $(PREFIX)/bin.
PREFIX ?= $(HOME)/.local

.PHONY: dev build install check app demo linux release publish release-check rehearse updater-test

# The app from source. It builds the daemon from source too and runs it, as
# the bundled app does, logging to ~/.leadline/leadlined.log. To watch the
# daemon in a terminal instead, start bin/leadlined first: the app uses a
# daemon that is already running and leaves it alone.
dev: app/node_modules
	cd app && pnpm tauri dev

# leadline and leadlined into bin/. DuckDB is linked in through cgo.
build:
	CGO_ENABLED=1 $(GO) build -ldflags "$(LDFLAGS)" -o $(BIN)/ ./cmd/leadline ./cmd/leadlined

# The two programs onto your PATH, so `leadline run -- pnpm dev` works in
# any terminal. It adds those two files and touches nothing else: no
# service, no data. (On a Linux server use scripts/install.sh, which also
# runs the daemon; on a Mac the app runs it.) Each is copied and then
# renamed into place, so one that is running is not written over.
install: build
	mkdir -p "$(PREFIX)/bin"
	for b in leadline leadlined; do \
		cp $(BIN)/$$b "$(PREFIX)/bin/.$$b.new" && mv -f "$(PREFIX)/bin/.$$b.new" "$(PREFIX)/bin/$$b" || exit 1; \
	done
	@echo "installed leadline and leadlined $(VERSION) into $(PREFIX)/bin"
	@case ":$$PATH:" in *":$(PREFIX)/bin:"*) ;; *) \
		echo "$(PREFIX)/bin is not on your PATH. Add this to ~/.zshrc (or ~/.profile):"; \
		echo '  export PATH="$(PREFIX)/bin:$$PATH"' ;; esac

# Every check in AGENTS.md. cargo needs the app's copy of the daemon to
# exist (scripts/sidecar.sh), and its tests start and stop that copy.
check:
	$(GO) vet ./...
	$(GO) test -race ./...
	scripts/install_test.sh
	scripts/version.sh
	scripts/release-test/credentials_test.sh
	scripts/sidecar.sh
	cd app/src-tauri && cargo check && cargo test
	cd app && CI=true pnpm install --frozen-lockfile && pnpm test && pnpm build
	cd packages/node && CI=true pnpm install --frozen-lockfile && pnpm test

# Leadline.app and its dmg, the daemon inside, under
# app/src-tauri/target/release/bundle.
app: app/node_modules
	cd app && pnpm tauri build

# The two programs for a Linux server, both architectures, into
# dist/linux_<arch>/. Built in a container, so this needs Docker.
linux:
	scripts/build-linux.sh

# A release's files into dist/: a tarball of the two programs for Linux and
# macOS on each architecture, and checksums.txt. AGENTS.md has the steps.
# The macOS ones are signed and notarized when the environment says how
# (scripts/release.sh), and say so when they are not.
release:
	scripts/release.sh

# The macOS half of a release from this Mac: the app and the two programs,
# signed with the Developer ID in the keychain, notarized, checked, opened
# once, and put on GitHub as a draft that the tag's workflow completes.
# Credentials come from the environment, else from 1Password
# (scripts/publish.sh). NOTES="What changed" is the release's text.
publish:
	NOTES="$(NOTES)" scripts/publish.sh

# What to be sure of before a tag: every check, then the app out of its dmg,
# opened in a folder and on data of its own (scripts/release-check.sh).
# Quit Leadline first: the app's daemon has one port. DMG=path tries that
# dmg instead of building one.
release-check:
	scripts/release-check.sh $(if $(DMG),--dmg $(DMG))

# A whole release from this Mac with stand-ins for every credential and for
# Apple, and nothing pushed or uploaded (scripts/release-test/rehearse.sh).
rehearse:
	scripts/release-test/rehearse.sh

# An installed app updating itself to the next version, from a feed on this
# machine (scripts/release-test/updater-test.sh). Two builds: it takes a while.
updater-test:
	scripts/release-test/updater-test.sh

# Made-up traces and logs, sent to the daemon that is running.
demo:
	$(GO) run ./cmd/leadline demo

app/node_modules: app/package.json app/pnpm-lock.yaml
	cd app && pnpm install
	@touch $@
