#!/bin/sh
# Builds leadline and leadlined for Linux into dist/linux_<arch>/, on a Mac
# or on Linux, the same way in both: inside a container of AlmaLinux 8
# (scripts/linux-builder.Dockerfile says why that one), on the architecture
# it is building for. The binaries need glibc 2.28 or newer and nothing
# else of the machine.
#
#   scripts/build-linux.sh              both architectures
#   scripts/build-linux.sh arm64        one
#
# The architecture that isn't this machine's runs under emulation, which
# Docker has to have turned on (the script says how when it isn't). Under
# qemu on an Apple-silicon Mac the amd64 build takes a minute or two, and
# its image four minutes the first time, against seconds for arm64. CI uses
# a runner of each architecture instead.
#
# DOCKER names another docker (podman). Go's build cache is kept in
# dist/.cache between runs, and modules come from this machine's module
# cache when it has Go.
set -eu

cd "$(dirname "$0")/.."

docker=${DOCKER:-docker}
command -v "$docker" >/dev/null || { echo "build-linux: $docker is not installed; the Linux binaries are built in a container" >&2; exit 1; }

[ $# -gt 0 ] || set -- amd64 arm64
for arch; do
  case $arch in
    amd64 | arm64) ;;
    *) echo "build-linux: don't know the architecture $arch (amd64 or arm64)" >&2; exit 2 ;;
  esac
done

version=$(cat VERSION)
# The repository may have no commit yet, or no git at all (a source tarball).
commit=$(git rev-parse --short --verify HEAD 2>/dev/null || true)
pkg=github.com/sean-brydon/leadline/internal/version

# Modules are the same files on every platform, so this machine's cache
# serves; the build cache is per architecture.
modcache=$(go env GOMODCACHE 2>/dev/null || true)
[ -n "$modcache" ] || modcache=$PWD/dist/.cache/gomod
mkdir -p "$modcache"

native=$("$docker" version --format '{{.Server.Arch}}' 2>/dev/null || true)

for arch; do
  image=leadline-linux-builder:$arch
  out=dist/linux_$arch
  cache=dist/.cache/gobuild_$arch
  mkdir -p "$out" "$cache"

  # Without emulation the other architecture fails deep inside the image
  # build with "exec format error". Say what is missing instead.
  if [ "$arch" != "$native" ] && ! "$docker" run --rm --name "leadline-l-probe-$arch" --platform "linux/$arch" almalinux:8.10 true >/dev/null 2>&1; then
    cat >&2 <<EOF
build-linux: this machine's Docker can't run linux/$arch programs, so it
can't build for $arch. Turn emulation on, then run this again:
  $docker run --privileged --rm tonistiigi/binfmt --install $arch
(it lasts until Docker restarts; Docker Desktop has emulation on unless you
turned it off, and OrbStack has a Rosetta setting for it). Or build only
this machine's own: scripts/build-linux.sh $native
The release workflow builds each on a runner of its own kind.
EOF
    exit 1
  fi

  echo "build-linux: $arch: the builder image" >&2
  "$docker" build --quiet --platform "linux/$arch" -t "$image" - < scripts/linux-builder.Dockerfile >/dev/null

  echo "build-linux: $arch: leadline and leadlined $version" >&2
  # The repository is mounted read-only and at the same path every time,
  # and -trimpath and -buildvcs=false keep this machine out of the result:
  # the same source gives the same two files anywhere.
  "$docker" run --rm --name "leadline-l-build-$arch" --platform "linux/$arch" \
    --user "$(id -u):$(id -g)" -e HOME=/tmp \
    -v "$PWD":/src:ro -w /src \
    -v "$PWD/$out":/out \
    -v "$PWD/$cache":/cache -e GOCACHE=/cache \
    -v "$modcache":/gomod -e GOMODCACHE=/gomod -e GOFLAGS=-modcacherw \
    "$image" \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X $pkg.Version=$version -X $pkg.Commit=$commit" \
      -o /out/ ./cmd/leadline ./cmd/leadlined

  # A release must not need more of a machine than this says, and must
  # start. Fail here, not on someone's server. (The stack limit is for
  # emulation only: qemu hands an x86-64 program addresses no x86-64
  # processor would, which DuckDB's allocator can't hold; an unlimited
  # stack makes the kernel lay memory out the old way, lower down.)
  # shellcheck disable=SC2016 # the container's shell expands these
  "$docker" run --rm --name "leadline-l-check-$arch" --platform "linux/$arch" --ulimit stack=-1:-1 \
    -e WANT="$version" -v "$PWD/$out":/out:ro "$image" sh -c '
    for b in leadline leadlined; do
      needed=$(objdump -p "/out/$b" | awk "/NEEDED/ {print \$2}" | tr "\n" " ")
      case $needed in *libstdc++* | *libgcc_s*)
        echo "build-linux: $b needs $needed: the C++ runtime was not linked in" >&2; exit 1 ;;
      esac
      newest=$(objdump -T "/out/$b" | grep -o "GLIBC_[0-9.]*" | sort -uV | tail -1)
      [ "$(printf "%s\nGLIBC_2.28\n" "$newest" | sort -V | tail -1)" = GLIBC_2.28 ] || {
        echo "build-linux: $b needs $newest, newer than the glibc 2.28 it is meant to run on" >&2; exit 1; }
    done
    said=$(/out/leadlined --version) || { echo "build-linux: leadlined does not start" >&2; exit 1; }
    case $said in "leadlined $WANT "*) ;; *)
      echo "build-linux: leadlined says \"$said\", not version $WANT" >&2; exit 1 ;;
    esac'
  ls -l "$out/leadline" "$out/leadlined" >&2
done
