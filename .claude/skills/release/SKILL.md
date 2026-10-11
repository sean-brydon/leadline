---
name: release
description: Cut a Leadline release (vX.Y.Z) - version bump, the checks the owner expects before a tag, tagging or publishing from the Mac, and checking the published app, tarballs and update feed. Use when asked to release, retag, rehearse a release, or check one.
---

# Releasing

The tag is the release. Pushing `vX.Y.Z` runs `.github/workflows/release.yml`:

1. **preflight** (a minute): the tag is `VERSION`'s, the four places the version is written agree, and the signing secrets are set. A release that would fail on any of those fails here.
2. **checks:** all of `ci.yml`. Nothing is published unless it passes.
3. **linux:** the two programs for amd64 and arm64, installed and asked.
4. **macos:** the two programs for both kinds of Mac, signed and notarized; then the universal app, signed, notarized, stapled and checked (`scripts/mac-release.sh`). About 30 minutes.
5. **publish:** `checksums.txt` over the four tarballs, everything onto a **draft** release, `latest.json` last, and only then the release is published as Latest.

`AGENTS.md`, "Versions and releases", says what each file is, how signing works, and what has to exist once (the updater key, the 1Password items, the repository's secrets). Read it before a first release.

An agent usually can't push a tag or reach 1Password. Prepare everything, run the checks, and give the user the command.

## Prepare

```sh
V=0.3.0                      # no leading v
scripts/version.sh "$V"      # VERSION, the app's three, Cargo.lock
git commit -am "release: v$V"
```

Then, on that commit:

```sh
make check                   # everything CI runs
shellcheck scripts/*.sh scripts/release-test/*.sh scripts/release-test/standins/*
make release-check           # with Leadline quit: see below
```

`make release-check` is the smoke test the owner expects before any tag. It builds the app, takes it out of its dmg into a folder of its own, and opens it on a data folder of its own: the daemon must start, be the one inside the app, answer as this version, and stop when the app quits, and again when the app is killed. It needs port 4320, so **Leadline must not be running**; if it is, the check says so and stops. Ask the user to quit it rather than skip the check. The window appears twice for a few seconds.

When the release scripts, the workflow, the updater or the Tauri version changed, also rehearse (neither sends anything anywhere, and neither needs a credential):

```sh
make rehearse                # a whole release from this Mac with stand-ins; ~10 minutes
make updater-test            # an installed app updating itself to the next version; two builds
```

And run the workflow by hand (Actions, release, Run workflow) on the release commit: it builds, signs and notarizes for real and publishes nothing.

## Release

From CI, which is the usual way. The user runs:

```sh
git push origin main && git tag -a "v$V" -m "Leadline v$V" && git push origin "v$V"
```

Or the macOS half from the user's Mac, signed with the Developer ID in its keychain and the rest read from 1Password (`scripts/publish.sh`):

```sh
make publish NOTES="What changed"     # Leadline quit, a clean main at the release commit
```

It builds and checks the same things as the workflow's macos job, opens the app it built, uploads a draft release, and pushes the tag itself. The workflow then finds the draft's `latest.json` announcing the version, builds nothing for macOS, adds the Linux half and publishes.

## After a failure

Nothing is public until the publish job's last step, so a failed run leaves at most a draft.

```sh
gh run view <run-id> --log-failed
gh run rerun <run-id> --failed          # when the cause was outside the commit (Apple, a runner)
```

When the commit has to change, the tag moves:

```sh
gh release delete "v$V" --yes || true   # only a draft exists
git push origin ":refs/tags/v$V" && git tag -f -a "v$V" -m "Leadline v$V" <sha> && git push origin "v$V"
```

Where it usually breaks, and what the message means:

- **"needs repository secrets that are not set"**: `scripts/ci-secrets.sh` (the user runs it; it reads 1Password).
- **"the updater has no public key yet"**: `scripts/updater-key.sh` was never run. It is run once, ever; never to "fix" a key that is already in `tauri.conf.json`.
- **"Apple did not accept …"**: Apple's log is printed above it and names the file. A program inside the app without the hardened runtime or a timestamp is the usual cause.
- **"the updater signature does not match the public key"**: `TAURI_SIGNING_PRIVATE_KEY` is not the private half of the key in `tauri.conf.json`. Do not change the public key to make it pass: installed apps trust the one they have.
- **"was signed with entitlements"**: something added an entitlements file. Leadline needs none; `AGENTS.md` says why and what would change that.

## Check it's live

```sh
gh run watch <run-id> --exit-status
gh release list --limit 2                                  # v$V marked Latest
curl -fsSL https://github.com/sean-brydon/leadline/releases/latest/download/latest.json | jq .version
curl -fsSL https://github.com/sean-brydon/leadline/releases/latest/download/checksums.txt   # four lines

D=$(mktemp -d)
gh release download "v$V" -D "$D" -p 'Leadline-macos-universal.dmg' -p "leadline_${V}_darwin_arm64.tar.gz" -p checksums.txt
(cd "$D" && shasum -a 256 -c --ignore-missing checksums.txt)
hdiutil attach -nobrowse -mountpoint "$D/mnt" "$D/Leadline-macos-universal.dmg"
spctl -a -vv "$D/mnt/Leadline.app"                         # expect: source=Notarized Developer ID
xcrun stapler validate "$D/mnt/Leadline.app"
hdiutil detach "$D/mnt"
tar -xzf "$D/leadline_${V}_darwin_arm64.tar.gz" -C "$D"
codesign --verify --strict -R=notarized --check-notarization "$D/leadline" "$D/leadlined" && echo notarized
"$D/leadlined" --version
```

An installed app offers the update within four hours of `latest.json` being up, or ten seconds after it is next opened. A daemon the app did not start (`leadlined` in a terminal, the systemd service on a server) keeps its version until it is restarted: say so in the release notes when a feature needs the new daemon. Servers upgrade by running the install command again.
