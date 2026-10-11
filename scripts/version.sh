#!/bin/sh
# The project's version is in four places that must say the same: the
# VERSION file, which the two programs are stamped from, and the desktop
# app's three (app/package.json, app/src-tauri/tauri.conf.json and
# app/src-tauri/Cargo.toml, with Cargo.lock following). The app's updater
# compares the app's own with the feed's, so an app that says one version
# and carries a daemon that says another would be a release nobody can
# reason about.
#
#   scripts/version.sh            print the version, and fail if the four differ
#   scripts/version.sh 0.3.0      set all four (and Cargo.lock)
#
# Setting it commits nothing: `release: v0.3.0` is yours to commit.
set -eu

cd "$(dirname "$0")/.."

die() {
  echo "version: $*" >&2
  exit 1
}

# json_version FILE: the top-level "version" of a JSON file. The first one
# in the file, which in these two is the top-level one.
json_version() {
  sed -n 's/^  "version": "\([^"]*\)",\{0,1\}$/\1/p' "$1" | head -1
}

# The [package] version: the first `version = ` line of Cargo.toml.
cargo_version() {
  sed -n 's/^version = "\([^"]*\)"$/\1/p' app/src-tauri/Cargo.toml | head -1
}

if [ $# -eq 0 ]; then
  version=$(cat VERSION)
  case $version in
    [0-9]*.[0-9]*.[0-9]*) ;;
    *) die "VERSION holds \"$version\", which is not a version (as in 0.2.0)" ;;
  esac
  differ=""
  for at in "app/package.json $(json_version app/package.json)" \
    "app/src-tauri/tauri.conf.json $(json_version app/src-tauri/tauri.conf.json)" \
    "app/src-tauri/Cargo.toml $(cargo_version)"; do
    [ "${at#* }" = "$version" ] || differ="$differ
  ${at%% *} says \"${at#* }\""
  done
  [ -z "$differ" ] || die "VERSION says $version, but:$differ
Set them all with: scripts/version.sh $version"
  echo "$version"
  exit 0
fi

version=${1#v}
# Three numbers and nothing else: the updater compares versions as semver,
# and the installer finds a tarball by this exact text.
echo "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' ||
  die "\"$1\" is not a version: three numbers, as in 0.3.0"

echo "$version" >VERSION
for f in app/package.json app/src-tauri/tauri.conf.json; do
  sed "s/^  \"version\": \"[^\"]*\"\(,\{0,1\}\)\$/  \"version\": \"$version\"\1/" "$f" >"$f.new" && mv -f "$f.new" "$f"
  [ "$(json_version "$f")" = "$version" ] || die "could not set the version in $f: it has no top-level \"version\" line to change"
done
f=app/src-tauri/Cargo.toml
# Only the first: a dependency could have a `version = ` line of its own.
awk -v v="$version" '!done && /^version = "/ { print "version = \"" v "\""; done = 1; next } { print }' "$f" >"$f.new" && mv -f "$f.new" "$f"
[ "$(cargo_version)" = "$version" ] || die "could not set the version in $f"

# Cargo.lock names the app's own version too, and `cargo --locked` (CI)
# refuses a lock that disagrees. Without cargo it is the same one line.
lock=app/src-tauri/Cargo.lock
awk -v v="$version" '
  /^name = "leadline"$/ { print; mine = 1; next }
  mine && /^version = "/ { print "version = \"" v "\""; mine = 0; next }
  { mine = 0; print }' "$lock" >"$lock.new" && mv -f "$lock.new" "$lock"

echo "version: $version in VERSION, app/package.json, app/src-tauri/tauri.conf.json, app/src-tauri/Cargo.toml and Cargo.lock" >&2
echo "version: nothing is committed; when the checks pass: git commit -am \"release: v$version\"" >&2
