#!/bin/sh
# Builds leadlined where Tauri looks for the app's sidecar:
# app/src-tauri/binaries/leadlined-<target triple> (bundle.externalBin in
# tauri.conf.json). `pnpm tauri dev` and `pnpm tauri build` run this first
# (pnpm sidecar), and cargo needs the file to be there even for a check.
#
# The triple is this machine's, or the one Tauri is building for
# (TAURI_ENV_TARGET_TRIPLE, set under `tauri build --target`).
set -eu

cd "$(dirname "$0")/.."

GO=${GO:-go}
command -v "$GO" >/dev/null || { echo "sidecar: Go is not installed (https://go.dev/dl)" >&2; exit 1; }

host=$(rustc -vV 2>/dev/null | sed -n 's/^host: //p')
triple=${TAURI_ENV_TARGET_TRIPLE:-$host}
[ -n "$triple" ] || { echo "sidecar: no target triple; is Rust installed (https://rustup.rs)?" >&2; exit 1; }

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
# than the Mac that bundled the app. 13 is the oldest Go itself runs on.
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
