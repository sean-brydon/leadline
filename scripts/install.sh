#!/bin/sh
# Installs Leadline on a Linux machine: the daemon (leadlined) and the
# command line (leadline) into ~/.local/bin, and the daemon as a service of
# your own user that starts at boot. No root is needed. Run it again to
# upgrade; --uninstall takes it all away again.
#
#   curl -fsSL https://github.com/sean-brydon/leadline/releases/latest/download/install.sh | sh
#   sh install.sh --version 0.2.0
#   sh install.sh --from ./leadline_0.2.0_linux_arm64.tar.gz
#   sh install.sh --uninstall
#
# "sh install.sh --help" lists the rest.
#
# Everything below is a function, and the one line that runs them is the
# last in the file: a download cut off halfway does nothing at all.

set -eu

repo=sean-brydon/leadline
unit=leadlined.service
addr=127.0.0.1:4320
otlp=127.0.0.1:4318
# The glibc scripts/build-linux.sh links against: the oldest this runs on.
min_glibc=2.28

usage() {
  cat <<EOF
usage: install.sh [--version X.Y.Z] [--from SOURCE] [--sha256 HEX]
                  [--bin-dir DIR] [--no-service]
       install.sh --uninstall [--purge]

Installs leadline and leadlined into ~/.local/bin and runs the daemon as a
systemd service of your own user, on $addr only, with its data in
~/.leadline. Run it again to upgrade.

  --version X.Y.Z  the release to install (default: the latest, or the one a
                   local tarball is named for)
  --from SOURCE    where the release is, instead of GitHub: a tarball on this
                   machine (leadline_X.Y.Z_linux_<arch>.tar.gz), a folder
                   that holds the release's files, or an http(s) address of
                   either. For machines that can't reach GitHub, and for
                   trying a build.
  --sha256 HEX     the tarball's SHA-256, when there is no checksums.txt
                   beside it. Without one or the other nothing is installed.
  --bin-dir DIR    where the two programs go (default ~/.local/bin)
  --no-service     install the programs only; set up no service
  --uninstall      stop and remove the service and the two programs. Your
                   data in ~/.leadline is kept...
  --purge          ...unless you add this.
EOF
}

say() { printf '%s\n' "$*"; }
warn() { printf '%s\n' "$*" >&2; }
die() {
  warn "install.sh: $*"
  exit 1
}

have() { command -v "$1" >/dev/null 2>&1; }

cleanup() {
  [ -z "${tmp:-}" ] || rm -rf "$tmp"
  [ -z "${stage:-}" ] || rm -rf "$stage"
}

main() {
  version=${LEADLINE_VERSION:-}
  from=${LEADLINE_FROM:-}
  sha256=
  bin_dir=${LEADLINE_BIN_DIR:-}
  service=yes
  uninstall=no
  purge=no
  while [ $# -gt 0 ]; do
    case $1 in
      --version | --from | --sha256 | --bin-dir)
        [ $# -ge 2 ] || die "$1 needs a value"
        option "$1" "$2"
        shift
        ;;
      --version=* | --from=* | --sha256=* | --bin-dir=*) option "${1%%=*}" "${1#*=}" ;;
      --no-service) service=no ;;
      --uninstall) uninstall=yes ;;
      --purge) purge=yes ;;
      -h | --help)
        usage
        exit 0
        ;;
      *)
        usage >&2
        die "don't know the option $1"
        ;;
    esac
    shift
  done

  [ -n "${HOME:-}" ] && [ -d "$HOME" ] || die "HOME is not set to a folder, so there is nowhere to install to"
  [ -n "$bin_dir" ] || bin_dir=$HOME/.local/bin
  case $bin_dir in
    /*) ;;
    *) bin_dir=$(pwd)/$bin_dir ;;
  esac
  # The path goes into the service's unit file, where these mean something.
  case $bin_dir in
    *[\"\\%\$\'\ ]* | *"
"*) die "the folder to install into ($bin_dir) has a space, a quote, a %, a \$ or a backslash in it, which a systemd unit can't carry safely" ;;
  esac
  data_dir=$HOME/.leadline
  unit_dir=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user
  user=$(id -un)
  uid=$(id -u)

  if [ "$purge" = yes ] && [ "$uninstall" = no ]; then
    die "--purge deletes your data and only means something with --uninstall"
  fi
  if [ "$uninstall" = yes ]; then
    do_uninstall
    return
  fi

  os=$(uname -s)
  case $os in
    Linux) ;;
    Darwin) die "this installs Leadline on a Linux machine. On a Mac the Leadline app carries the daemon and runs it; for the command line, run \"make install\" in a checkout of the repository." ;;
    *) die "Leadline has no build for $os (Linux and macOS only)" ;;
  esac
  machine=$(uname -m)
  case $machine in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "Leadline has no build for $machine processors (x86_64 and arm64 only)" ;;
  esac
  have tar || die "tar is not installed, and the release is a tarball"
  have gzip || die "gzip is not installed, and the release is a gzipped tarball"

  tmp=$(mktemp -d "${TMPDIR:-/tmp}/leadline-install.XXXXXX") || die "could not make a temporary folder"
  stage=
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  find_release
  verify
  unpack
  place
  mkdir -p "$data_dir"
  chmod 700 "$data_dir"

  state=none
  why=
  if [ "$service" = yes ]; then
    set_up_service
  fi
  report
  if [ "$state" = failed ]; then
    exit 1
  fi
}

option() {
  case $1 in
    --version) version=$2 ;;
    --from) from=$2 ;;
    --sha256) sha256=$2 ;;
    --bin-dir) bin_dir=$2 ;;
  esac
}

# fetch downloads $1 to the file $2.
fetch() {
  if have curl; then
    curl -fsSL --retry 2 -o "$2" "$1"
  else
    wget -q -O "$2" "$1"
  fi
}

sha256_of() {
  if have sha256sum; then
    sha256sum "$1" | awk '{print $1}'
  elif have shasum; then
    shasum -a 256 "$1" | awk '{print $1}'
  elif have openssl; then
    openssl dgst -sha256 -r "$1" | awk '{print $1}'
  else
    die "none of sha256sum, shasum and openssl is installed, so the download can't be checked. Nothing was installed."
  fi
}

tarball_name() { printf 'leadline_%s_linux_%s.tar.gz\n' "$1" "$arch"; }

# version_in prints the version in a release tarball's name, or nothing when
# the name isn't one of this machine's.
version_in() {
  file=${1##*/}
  case $file in
    leadline_*_linux_"$arch".tar.gz)
      v=${file#leadline_}
      printf '%s\n' "${v%_linux_"$arch".tar.gz}"
      ;;
  esac
}

# for_this_machine stops when a tarball's name says it was built for another
# kind of machine.
for_this_machine() {
  case ${1##*/} in
    leadline_*_linux_"$arch".tar.gz) ;;
    leadline_*_linux_*.tar.gz | leadline_*_darwin_*.tar.gz)
      die "${1##*/} is not for this machine, which is linux_$arch. Nothing was installed."
      ;;
  esac
}

# find_release leaves the tarball at $tarball, on this machine, and the
# release's list of checksums at $sums when there is one.
find_release() {
  version=${version#v}
  case $version in
    '' | [0-9]*.[0-9]*.[0-9]*) ;;
    *) die "$version doesn't look like a version (as in 0.2.0)" ;;
  esac
  case $version in
    *[!0-9A-Za-z.+-]*) die "$version doesn't look like a version (as in 0.2.0)" ;;
  esac
  sums=
  case $from in
    http://* | https://*)
      case $from in
        *.tar.gz)
          for_this_machine "$from"
          [ -n "$version" ] || version=$(version_in "$from")
          download "${from%/*}" "${from##*/}"
          ;;
        *) download "${from%/}" "" ;;
      esac
      ;;
    '')
      if [ -n "$version" ]; then
        download "https://github.com/$repo/releases/download/v$version" ""
      else
        download "https://github.com/$repo/releases/latest/download" ""
      fi
      ;;
    *)
      if [ -d "$from" ]; then
        dir=${from%/}
        [ ! -f "$dir/checksums.txt" ] || sums=$dir/checksums.txt
        if [ -z "$version" ]; then
          version=$(newest_in "$dir")
          [ -n "$version" ] || die "there is no leadline_*_linux_$arch.tar.gz in $dir"
        fi
        tarball=$dir/$(tarball_name "$version")
        [ -f "$tarball" ] || die "there is no $(tarball_name "$version") in $dir"
      elif [ -f "$from" ]; then
        tarball=$from
        for_this_machine "$from"
        [ -n "$version" ] || version=$(version_in "$from")
        dir=$(dirname "$from")
        [ ! -f "$dir/checksums.txt" ] || sums=$dir/checksums.txt
      else
        die "there is no file or folder at $from"
      fi
      ;;
  esac
}

# newest_in prints the highest version a folder has a tarball of for this
# machine.
newest_in() {
  for f in "$1"/leadline_*_linux_"$arch".tar.gz; do
    [ -f "$f" ] || continue
    version_in "$f"
  done | sort -t. -k1,1n -k2,2n -k3,3n | tail -1
}

# download fetches the checksums and the tarball from the address $1. $2 is
# the tarball's name there when the address gave it.
download() {
  base=$1
  have curl || have wget || die "neither curl nor wget is installed, so the release can't be downloaded. Install one, or download the release elsewhere and pass it with --from."
  sums=$tmp/checksums.txt
  if ! fetch "$base/checksums.txt" "$sums" 2>"$tmp/fetch.err"; then
    sums=
    # The list also says which version "latest" is; without it there is no
    # telling what to fetch.
    if [ -z "$version" ] && [ -z "$2" ]; then
      cat "$tmp/fetch.err" >&2
      die "could not download $base/checksums.txt. Is this machine able to reach it? If not, download a release elsewhere and pass it with --from."
    fi
  fi
  name=$2
  if [ -z "$name" ]; then
    if [ -z "$version" ]; then
      version=$(awk '{print $2}' "$sums" | sed 's/^\*//' | while read -r f; do version_in "$f"; done | head -1)
      [ -n "$version" ] || die "$base/checksums.txt lists no leadline_*_linux_$arch.tar.gz: that release has no build for this machine"
    fi
    name=$(tarball_name "$version")
  fi
  tarball=$tmp/$name
  say "Downloading $base/$name"
  fetch "$base/$name" "$tarball" || die "could not download $base/$name. Nothing was installed."
}

# verify stops unless the tarball is byte for byte the one the release
# published: its SHA-256 is the one given with --sha256, or the one the
# release's checksums.txt has for a file of that name.
verify() {
  name=${tarball##*/}
  want=$sha256
  if [ -z "$want" ] && [ -n "$sums" ]; then
    want=$(awk -v name="$name" '$2 == name || $2 == "*" name {print $1; exit}' "$sums")
    [ -n "$want" ] || die "$sums has no checksum for $name, so it can't be checked. Nothing was installed."
  fi
  [ -n "$want" ] || die "there is nothing to check $name against: no checksums.txt beside it, and no --sha256. Nothing was installed. (\"make release\" writes checksums.txt next to the tarballs; keep them together.)"
  want=$(printf '%s' "$want" | tr 'A-F' 'a-f')
  case $want in
    *[!0-9a-f]*) die "the checksum to check $name against ($want) is not a SHA-256. Nothing was installed." ;;
  esac
  [ ${#want} -eq 64 ] || die "the checksum to check $name against ($want) is not a SHA-256. Nothing was installed."
  got=$(sha256_of "$tarball")
  if [ "$got" != "$want" ]; then
    warn "install.sh: $name is not the file the release published."
    warn "  expected SHA-256  $want"
    warn "  this file's       $got"
    die "the download is damaged or has been changed. Nothing was installed."
  fi
  say "Checksum matches ($got)"
}

# unpack takes the two programs out of the tarball, into a folder beside
# where they will live, and makes sure they run on this machine before
# anything already installed is touched.
unpack() {
  mkdir -p "$bin_dir"
  stage=$(mktemp -d "$bin_dir/.leadline-install.XXXXXX") || die "could not write to $bin_dir"
  # Only these two names are taken, whatever else the tarball holds.
  tar -xzf "$tarball" -C "$stage" leadline leadlined 2>"$tmp/tar.err" || {
    cat "$tmp/tar.err" >&2
    die "$tarball is not a Leadline release: it should hold leadline and leadlined. Nothing was installed."
  }
  for b in leadline leadlined; do
    if [ -L "$stage/$b" ] || [ ! -f "$stage/$b" ]; then
      die "$tarball is not a Leadline release: $b in it is not a plain file. Nothing was installed."
    fi
    chmod 755 "$stage/$b"
  done
  if ! line=$("$stage/leadlined" --version 2>"$tmp/run.err"); then
    explain_wont_run
  fi
  # "leadlined 0.2.0 (linux/arm64)": the version is the second word.
  # shellcheck disable=SC2086 # split into words on purpose
  set -- $line
  installed=${2:-unknown}
}

# explain_wont_run says why the daemon would not start on this machine. It
# is nearly always the C library.
explain_wont_run() {
  libc=$(ldd --version 2>&1 | head -1 || true)
  case $libc in
    *musl*)
      die "leadlined does not run here: this machine's C library is musl (Alpine and its kin), and Leadline's Linux build needs glibc $min_glibc or newer. Nothing was installed. On Alpine, run it in a Debian or Ubuntu container instead."
      ;;
  esac
  if [ ! -e /lib/ld-linux-x86-64.so.2 ] && [ ! -e /lib64/ld-linux-x86-64.so.2 ] && [ ! -e /lib/ld-linux-aarch64.so.1 ]; then
    if ls /lib/ld-musl-* >/dev/null 2>&1; then
      die "leadlined does not run here: this machine's C library is musl (Alpine and its kin), and Leadline's Linux build needs glibc $min_glibc or newer. Nothing was installed. On Alpine, run it in a Debian or Ubuntu container instead."
    fi
  fi
  glibc=$(getconf GNU_LIBC_VERSION 2>/dev/null | awk '{print $2}' || true)
  if [ -n "$glibc" ] && [ "$(printf '%s\n%s\n' "$glibc" "$min_glibc" | sort -t. -k1,1n -k2,2n | head -1)" != "$min_glibc" ]; then
    die "leadlined does not run here: this machine has glibc $glibc, and Leadline needs $min_glibc or newer (Debian 10, Ubuntu 18.10, RHEL 8 and later). Nothing was installed."
  fi
  cat "$tmp/run.err" >&2
  die "leadlined from this release does not run on this machine (see above). Nothing was installed."
}

# place moves the two programs into the folder. A rename, so a daemon that
# is running keeps the file it has open until it is restarted.
place() {
  previous=
  if [ -x "$bin_dir/leadlined" ]; then
    # shellcheck disable=SC2046 # split into words on purpose
    set -- $("$bin_dir/leadlined" --version 2>/dev/null || true)
    previous=${2:-an earlier build}
  fi
  mv -f "$stage/leadline" "$bin_dir/leadline"
  mv -f "$stage/leadlined" "$bin_dir/leadlined"
  rmdir "$stage" 2>/dev/null || true
  stage=
}

has_systemd() { [ -d /run/systemd/system ] && have systemctl; }

lingering() { [ -e "/var/lib/systemd/linger/$user" ]; }

# session reports whether this user's own systemd answers. A login over SSH
# has XDG_RUNTIME_DIR; su and sudo don't, though the folder is there
# whenever the user's systemd is.
session() {
  if [ -z "${XDG_RUNTIME_DIR:-}" ] && [ -d "/run/user/$uid" ]; then
    XDG_RUNTIME_DIR=/run/user/$uid
    export XDG_RUNTIME_DIR
  fi
  systemctl --user show-environment >/dev/null 2>&1
}

write_unit() {
  mkdir -p "$unit_dir"
  cat >"$unit_dir/$unit.new" <<EOF
# Written by Leadline's installer (scripts/install.sh), which writes it
# again on every upgrade. Keep changes of your own in a drop-in:
#   systemctl --user edit $unit
[Unit]
Description=Leadline daemon: the traces, queries and logs of the apps on this machine
Documentation=https://github.com/$repo
# Five failed starts in a minute and systemd stops trying: something else
# has the port, or the data folder can't be written.
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
# Both listeners are named here, on loopback, so that nothing in the
# environment can open them to the network.
ExecStart=$bin_dir/leadlined -addr $addr -otlp $otlp -data %h/.leadline
Restart=on-failure
RestartSec=2
# Wrong flags don't get better by trying again.
RestartPreventExitStatus=2
# On SIGTERM the daemon writes what it holds and closes its database.
TimeoutStopSec=30
# What it writes holds SQL and log lines: for this user only.
UMask=0077

# The daemon reads and writes its own folder, listens on loopback and talks
# to the databases you connect. It needs nothing more, so it gets nothing
# more. Only what a user's own systemd can enforce without root is here:
# the options that hide parts of the filesystem (ProtectSystem, ProtectHome,
# PrivateTmp) need user namespaces, which many servers turn off, and with
# them missing the service would not start at all.
NoNewPrivileges=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
RestrictNamespaces=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=default.target
EOF
  mv -f "$unit_dir/$unit.new" "$unit_dir/$unit"
}

# set_up_service leaves $state as one of:
#   running   the service is up and answering
#   waiting   it is installed and will start with this user's systemd,
#             which isn't running now
#   failed    it is installed and would not start
#   byhand    this machine has no systemd
set_up_service() {
  if ! has_systemd; then
    state=byhand
    return
  fi
  write_unit

  # A user's services run while the user is logged in, unless the user
  # "lingers": then systemd starts them at boot and keeps them. Turning
  # that on for yourself is allowed on some machines and needs an
  # administrator on others, so try, and say which it was.
  linger=on
  if ! lingering; then
    loginctl enable-linger "$user" --no-ask-password >/dev/null 2>&1 || true
    if lingering; then
      linger=enabled
    else
      linger=off
    fi
  fi

  # Lingering starts the user's systemd if it wasn't running; give it a
  # moment to answer.
  tries=0
  until session; do
    tries=$((tries + 1))
    if [ "$tries" -ge 10 ] || { [ "$linger" = off ] && [ ! -d "/run/user/$uid" ]; }; then
      # No systemd of this user's to talk to. "enable" is one link, which
      # can be made by hand: the service then starts whenever that systemd
      # does (the next login over SSH, or once lingering is on).
      mkdir -p "$unit_dir/default.target.wants"
      ln -sf "../$unit" "$unit_dir/default.target.wants/$unit"
      state=waiting
      return
    fi
    sleep 1
  done

  systemctl --user daemon-reload
  systemctl --user enable "$unit" >/dev/null 2>&1 || true

  # A daemon someone started by hand has the port, and the service would
  # fail against it over and over.
  if ! systemctl --user is-active --quiet "$unit" && "$bin_dir/leadline" version -daemon -url "http://$addr" >/dev/null 2>&1; then
    state=failed
    why=port
    return
  fi

  # restart, not start: on an upgrade the daemon that is running is the
  # old program.
  systemctl --user reset-failed "$unit" >/dev/null 2>&1 || true
  if ! systemctl --user restart "$unit"; then
    state=failed
    return
  fi
  tries=0
  while [ "$tries" -lt 20 ]; do
    if running=$("$bin_dir/leadline" version -daemon -url "http://$addr" 2>/dev/null); then
      state=running
      return
    fi
    tries=$((tries + 1))
    sleep 1
  done
  state=failed
}

on_path() {
  case :$PATH: in
    *:"$bin_dir":*) return 0 ;;
  esac
  return 1
}

report() {
  say ""
  if [ -n "$previous" ] && [ "$previous" != "$installed" ]; then
    say "Leadline $installed is installed (it was $previous)."
  elif [ -n "$previous" ]; then
    say "Leadline $installed is installed again."
  else
    say "Leadline $installed is installed."
  fi
  say ""
  say "  programs   $bin_dir/leadline, $bin_dir/leadlined"
  say "  data       $data_dir"

  by_hand="  nohup $bin_dir/leadlined >> $data_dir/leadlined.log 2>&1 &"
  case $state in
    running)
      say "  service    $unit, running as $user (leadlined $running)"
      say "  listening  $addr and, for OpenTelemetry's default port, $otlp:"
      say "             this machine only, nothing from the network"
      ;;
    waiting)
      say "  service    $unit, installed but NOT RUNNING YET"
      ;;
    failed)
      say "  service    $unit, installed but IT DID NOT START"
      ;;
    byhand)
      say "  service    none: this machine doesn't run systemd"
      ;;
    none)
      say "  service    none (--no-service)"
      ;;
  esac

  case $state in
    running)
      case $linger in
        on) ;;
        enabled)
          say ""
          say "Lingering was turned on for $user, so the service starts at boot and keeps"
          say "running after you log out. (loginctl disable-linger $user turns that off.)"
          ;;
        off)
          say ""
          say "It will NOT start at boot yet. systemd runs your services only while you"
          say "are logged in, unless your user \"lingers\", and turning that on here needs"
          say "an administrator. Once, as one:"
          say "  sudo loginctl enable-linger $user"
          say "Until then the daemon stops when your last session ends."
          ;;
      esac
      ;;
    waiting)
      say ""
      say "systemd has no session for $user right now (this is su or sudo, not a"
      say "login), so the service could not be started from here. It starts by itself"
      if lingering; then
        say "when $user's systemd does: at the next boot, or the next login over SSH."
      else
        say "the next time $user logs in over SSH. To start it now, and at every boot,"
        say "have an administrator run:"
        say "  sudo loginctl enable-linger $user"
      fi
      ;;
    failed)
      say ""
      if [ "$why" = port ]; then
        say "A Leadline daemon that is not this service is already answering on $addr"
        say "(one started by hand?), and the service can't share the port. Stop that"
        say "one, then start the service:"
        say "  pkill -TERM -x leadlined"
        say "  leadline service start"
      else
        say "What the service wrote:"
        journalctl --user --unit "$unit" --lines 15 --no-pager 2>/dev/null || systemctl --user status "$unit" --no-pager --lines 15 2>/dev/null || true
        say ""
        say "When that is put right: leadline service start"
      fi
      ;;
    byhand | none)
      say ""
      if [ "$state" = byhand ]; then
        say "In a container, or on a distribution without systemd, start the daemon"
        say "yourself and leave it running:"
      else
        say "To run the daemon:"
      fi
      say "$by_hand"
      say "It listens on $addr, this machine only. Stop it with:"
      say "  pkill -TERM -x leadlined"
      ;;
  esac

  if ! on_path; then
    say ""
    say "$bin_dir is not on your PATH, so \"leadline\" won't be found yet. Log out"
    say "and in again (Debian and Ubuntu add ~/.local/bin by themselves), or add"
    say "this line to ~/.profile, or to ~/.bashrc or ~/.zshrc:"
    # shellcheck disable=SC2016 # the person's shell expands it, not this one
    say '  export PATH="$HOME/.local/bin:$PATH"'
  fi

  if [ "$state" = failed ]; then
    return
  fi
  say ""
  say "Next:"
  say "  leadline run -- <your dev command>   show an app's logs, traces and queries"
  if [ "$state" = running ] || [ "$state" = waiting ]; then
    say "  leadline service status              the daemon: also start, stop, restart, logs"
  fi
  say "To look at it from your laptop, forward a port there to this machine:"
  say "  ssh -N -L 4336:$addr $user@<this machine>"
  say "and the daemon is at http://127.0.0.1:4336 on the laptop."
}

do_uninstall() {
  removed=
  if has_systemd; then
    if session; then
      systemctl --user disable --now "$unit" >/dev/null 2>&1 || true
    fi
  fi
  if [ -e "$unit_dir/$unit" ] || [ -L "$unit_dir/default.target.wants/$unit" ]; then
    rm -f "$unit_dir/$unit" "$unit_dir/default.target.wants/$unit"
    rm -rf "$unit_dir/$unit.d"
    removed="$removed the service,"
    if has_systemd && session; then
      systemctl --user daemon-reload >/dev/null 2>&1 || true
      systemctl --user reset-failed "$unit" >/dev/null 2>&1 || true
    fi
  fi
  for b in leadline leadlined; do
    if [ -e "$bin_dir/$b" ]; then
      rm -f "$bin_dir/$b"
      removed="$removed $bin_dir/$b,"
    fi
  done
  if [ -z "$removed" ]; then
    say "Leadline is not installed here: no service, and no leadline or leadlined in $bin_dir."
  else
    say "Removed${removed%,}."
  fi

  if [ -d "$data_dir" ]; then
    if [ "$purge" = yes ]; then
      rm -rf "$data_dir"
      say "Deleted your data in $data_dir."
    else
      say "Your data is still in $data_dir ($(du -sh "$data_dir" 2>/dev/null | awk '{print $1}')). To delete it too:"
      say "  rm -rf $data_dir     (or run this again with --uninstall --purge)"
    fi
  fi
  if has_systemd && lingering; then
    say "Lingering is still on for $user, in case other services of yours need it."
    say "  sudo loginctl disable-linger $user     turns it off"
  fi
  if has_systemd && ! session && [ -n "$removed" ]; then
    say "systemd has no session for $user right now, so a daemon that is running"
    say "was not stopped: pkill -TERM -x leadlined"
  elif ! has_systemd && [ -n "$removed" ]; then
    say "If you started leadlined by hand, it is still running: pkill -TERM -x leadlined"
  fi
}

main "$@"
