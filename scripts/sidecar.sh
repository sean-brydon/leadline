#!/bin/sh
# Builds leadlined where Tauri looks for the app's sidecar:
# app/src-tauri/binaries/leadlined-<target triple> (bundle.externalBin in
# tauri.conf.json). `pnpm tauri dev` and `pnpm tauri build` run this first
# (pnpm sidecar), and cargo needs the file to be there even for a check.
#
# The triple is this machine's, or the one Tauri is building for
# (TAURI_ENV_TARGET_TRIPLE, set under `tauri build --target`), or the one
# named:
#
#   scripts/sidecar.sh                           this machine's
#   scripts/sidecar.sh universal-apple-darwin    both kinds of Mac in one file
#
# A release's app is universal (scripts/mac-release.sh). Tauri then wants
# three files: one for each architecture, which cargo checks for as it
# compiles each, and the two joined by lipo, which is the one it bundles.
set -eu

# This script by its whole path: it runs itself for each half of a
# universal build, after the cd below.
self=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")
cd "$(dirname "$0")/.."

GO=${GO:-go}
command -v "$GO" >/dev/null || { echo "sidecar: Go is not installed (https://go.dev/dl)" >&2; exit 1; }

host=$(rustc -vV 2>/dev/null | sed -n 's/^host: //p')
triple=${1:-${TAURI_ENV_TARGET_TRIPLE:-$host}}
[ -n "$triple" ] || { echo "sidecar: no target triple; is Rust installed (https://rustup.rs)?" >&2; exit 1; }

if [ "$triple" = universal-apple-darwin ]; then
  "$self" aarch64-apple-darwin
  "$self" x86_64-apple-darwin
  out=app/src-tauri/binaries/leadlined-universal-apple-darwin
  lipo -create -output "$out" app/src-tauri/binaries/leadlined-aarch64-apple-darwin app/src-tauri/binaries/leadlined-x86_64-apple-darwin
  echo "sidecar: $out ($(lipo -archs "$out"))"
  exit 0
fi

case $triple in
  aarch64-apple-darwin) os=darwin arch=arm64 ;;
  x86_64-apple-darwin) os=darwin arch=amd64 ;;
  aarch64-unknown-linux-gnu) os=linux arch=arm64 ;;
  x86_64-unknown-linux-gnu) os=linux arch=amd64 ;;
  *) echo "sidecar: don't know how to build leadlined for $triple" >&2; exit 1 ;;
esac

# The app's daemon says the project's version (the VERSION file), like the
# ones a release ships.
pkg=github.com/sean-brydon/leadline/internal/version
stamp="-X $pkg.Version=$(cat VERSION) -X $pkg.Commit=$(git rev-parse --short --verify HEAD 2>/dev/null || true)"

# On a Mac, clang builds for the macOS it is running on unless told
# otherwise, and the daemon would then refuse to start on anything older
# than the Mac that bundled the app. 13 is the oldest Go itself runs on,
# and what the app says it needs (bundle.macOS.minimumSystemVersion in
# tauri.conf.json), so that an older Mac declines the app instead of
# opening a window whose daemon can't start.
if [ "$os" = darwin ]; then
  min=-mmacosx-version-min=13.0
  export CGO_CFLAGS="-O2 -g $min" CGO_CXXFLAGS="-O2 -g $min" CGO_LDFLAGS="-O2 -g $min"
fi

# DuckDB is C++ linked in through cgo, so this needs a C compiler, and one
# for the target when it isn't this machine.
out=app/src-tauri/binaries/leadlined-$triple
mkdir -p app/src-tauri/binaries
CGO_ENABLED=1 GOOS=$os GOARCH=$arch "$GO" build -trimpath -ldflags="-s -w $stamp" -o "$out" ./cmd/leadlined
echo "sidecar: $out"
