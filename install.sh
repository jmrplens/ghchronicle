#!/usr/bin/env bash
# One command that puts the ghchronicle binary on this machine.
#
#   curl -fsSL https://raw.githubusercontent.com/jmrplens/ghchronicle/main/install.sh | bash
#
# What it does, in order: work out this platform, resolve a version, download
# that archive and the release's checksum file, refuse to go on unless the
# archive's SHA-256 is the one the release published, and only then put the
# binary somewhere on PATH.
#
# The verification is not optional and there is no flag to skip it. A script
# piped into a shell is the least inspectable way to install anything, so the
# one thing it must not do is trust what it just downloaded.
#
# Every statement below is inside a function, and `main` is called on the last
# line. A connection that drops half way through the pipe leaves bash with an
# incomplete file: it may define some of these functions, but it reaches no
# call, so a truncated download does nothing rather than something partial.
set -euo pipefail

REPO="jmrplens/ghchronicle"
BINARY="ghchronicle"

# Overridable so the test suite can point the whole thing at a local server.
# A mirror is the other reason someone would set them.
: "${GHCHRONICLE_DOWNLOAD_BASE:=https://github.com/${REPO}/releases/download}"
: "${GHCHRONICLE_LATEST_URL:=https://api.github.com/repos/${REPO}/releases/latest}"

# The workflow that signs a release, as the certificate records it. Checked by
# running it against the published v2.0.0 bundle, which answers "Verified OK".
COSIGN_IDENTITY='^https://github\.com/jmrplens/ghchronicle/\.github/workflows/release\.yml@refs/tags/v'
COSIGN_ISSUER="https://token.actions.githubusercontent.com"

# The oldest cosign that reads the bundle every release publishes, measured
# against v2.5.0's: 2.4.1 and older answer "bundle does not contain cert for
# verification" and exit 1, exactly as they would for a forged file, so a
# cosign that old tells this script nothing about the signature.
COSIGN_MINIMUM="2.4.2"

# Named once so the advice this prints stays the command the documentation
# gives, rather than a second spelling of it that can drift.
SELF_URL="https://raw.githubusercontent.com/${REPO}/main/install.sh"

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
Usage: install.sh [--version X.Y.Z] [--dir PATH]

  --version   The release to install. Default: the newest one.
  --dir       Where to put the binary. Default: /usr/local/bin when it is
              writable, otherwise ~/.local/bin.
  --help      This.

The same two can be given as the VERSION and BIN_DIR environment variables,
which is how to set them when the script is piped into a shell:

  curl -fsSL .../install.sh | VERSION=2.0.0 bash
USAGE
}

# need reports a missing tool by name rather than letting the failure surface
# as "command not found" from somewhere in the middle of the run.
need() { command -v "$1" >/dev/null 2>&1 || die "this needs $1, which is not on PATH"; }

# platform prints "os arch" in the spelling the release archives use, and
# refuses rather than guessing: a wrong archive is a confusing failure later.
platform() {
  local os arch
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)
  case "$os" in
    linux | darwin) ;;
    mingw* | msys* | cygwin*)
      die "on Windows take the .zip from https://github.com/${REPO}/releases and see the Windows install page" ;;
    *) die "no release is built for $os" ;;
  esac
  case "$arch" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "no release is built for $arch" ;;
  esac
  printf '%s %s\n' "$os" "$arch"
}

# newest_version asks the API for the latest release and reads the tag out of
# it without a JSON parser, because jq is not on a plain machine and this is
# the one field needed.
newest_version() {
  local tag
  tag=$(curl -fsSL "$GHCHRONICLE_LATEST_URL" |
    sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$tag" ] || die "could not read the newest version from $GHCHRONICLE_LATEST_URL. Pass --version, or take the archive from the releases page"
  printf '%s\n' "$tag"
}

# verify_checksum fails on anything but an exact match, including the case
# where the archive is not named in the checksum file at all: a lookup finding
# nothing must not read as nothing wrong.
#
# The name is compared as a whole field and not with grep, because every
# archive's name is a prefix of its SBOM's: a substring match on
# "ghchronicle_2.0.0_linux_amd64.tar.gz" also picks up the line for
# "ghchronicle_2.0.0_linux_amd64.tar.gz.spdx.json", and the check then fails on
# a file this script never downloaded.
verify_checksum() {
  local dir=$1 archive=$2 line
  line=$(awk -v want="$archive" '$2 == want { print; found = 1 } END { exit !found }' \
    "${dir}/checksums.txt" || true)
  [ -n "$line" ] || die "${archive} is not named in the release's checksums.txt"
  (
    cd "$dir"
    if command -v sha256sum >/dev/null 2>&1; then
      printf '%s\n' "$line" | sha256sum --check --status -
    elif command -v shasum >/dev/null 2>&1; then
      printf '%s\n' "$line" | shasum -a 256 --check --status -
    else
      die "this needs sha256sum or shasum to check what it downloaded, and has neither"
    fi
  ) || die "${archive} does not match the checksum the release published. Do not use it."
}

# verify_signature runs only when cosign is already here. The checksum above is
# what every install is held to; this answers the further question of whether
# the checksum file itself came from the release workflow, and it is worth
# doing when the tool is at hand rather than worth installing a tool for.
#
# Each way of not asking is said, so a reader never takes the checksum line for
# the whole of what was checked. install.ps1 does the same, in the same words.
#
# A cosign too old to read the bundle is one of those ways, and is said as one
# rather than refused: it cannot tell a genuine release from a forged one, so
# its failure is no more evidence than a missing cosign. A cosign new enough
# that still fails is refused, with what it said, because "tuf refresh failed"
# from a machine that cannot reach Sigstore is not a forged file either, and
# the reader is the one who can tell the two apart.
verify_signature() {
  local dir=$1 version=$2 have said
  command -v cosign >/dev/null 2>&1 || {
    say "note: cosign is not on PATH, so only the checksum was verified"
    return 0
  }
  curl -fsSL -o "${dir}/checksums.txt.sigstore.json" \
    "${GHCHRONICLE_DOWNLOAD_BASE}/v${version}/checksums.txt.sigstore.json" || {
    say "note: this release publishes no signature bundle, so only the checksum was verified"
    return 0
  }
  have=$(cosign_version)
  if [ -n "$have" ] && older_than "$have" "$COSIGN_MINIMUM"; then
    say "note: cosign ${have} cannot read this release's signature bundle (${COSIGN_MINIMUM} or newer can), so only the checksum was verified"
    return 0
  fi
  said=$(cosign verify-blob \
    --bundle "${dir}/checksums.txt.sigstore.json" \
    --certificate-identity-regexp "$COSIGN_IDENTITY" \
    --certificate-oidc-issuer "$COSIGN_ISSUER" \
    "${dir}/checksums.txt" 2>&1) ||
    die "cosign did not confirm that checksums.txt came from the release workflow, so nothing was installed. What cosign said:
${said}"
  say "signature verified with cosign"
}

# cosign_version is the version the cosign on PATH reports, as X.Y.Z, or
# nothing when it reports none in that form. A build from source says
# "devel", and is taken to be new enough: skipping the check for a version
# nobody can read would be the one way to install past a signature that does
# not verify.
cosign_version() {
  cosign version 2>/dev/null |
    sed -n 's/^GitVersion:[[:space:]]*v\{0,1\}\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p' |
    head -n 1 || true
}

# older_than reports whether version $1 comes before version $2, both X.Y.Z.
older_than() {
  local -a a b
  local i
  IFS=. read -r -a a <<<"$1"
  IFS=. read -r -a b <<<"$2"
  for i in 0 1 2; do
    if [ "${a[i]}" -lt "${b[i]}" ]; then return 0; fi
    if [ "${a[i]}" -gt "${b[i]}" ]; then return 1; fi
  done
  return 1
}

# on_path reports whether a directory is already one the shell searches.
on_path() {
  case ":${PATH}:" in
    *":$1:"*) return 0 ;;
    *) return 1 ;;
  esac
}

# target_dir picks somewhere writable rather than reaching for sudo. A script
# read off the network should not be the thing that decides to become root, so
# running it without one installs for this user and says how to finish.
#
# A directory the shell already searches comes before one that merely exists.
# On a Debian-like system ~/.local/bin is added to PATH by ~/.profile only when
# it already exists, so the first install creates it and the command is not
# found until the next login: a home directory that is already on PATH is worth
# preferring over the conventional one for exactly that reason.
target_dir() {
  local candidate
  if [ -n "${BIN_DIR:-}" ]; then
    printf '%s\n' "$BIN_DIR"
    return
  fi
  if [ -w /usr/local/bin ]; then
    printf '%s\n' /usr/local/bin
    return
  fi
  for candidate in "${HOME}/.local/bin" "${HOME}/bin"; do
    if [ -d "$candidate" ] && on_path "$candidate"; then
      printf '%s\n' "$candidate"
      return
    fi
  done
  printf '%s\n' "${HOME}/.local/bin"
}

# finishing_line is what to do when the binary landed somewhere the shell does
# not search. Whichever way it is fixed, "add this to PATH" on its own is the
# answer that leaves the reader to work out both the line and the file.
finishing_line() {
  local dir=$1 profile="your shell's startup file"
  # Spelled out rather than with a tilde: this is read by a person who is about
  # to open the file, and "~" in a message is one more thing to resolve.
  case "${SHELL:-}" in
    */zsh) profile="${HOME}/.zshrc" ;;
    */bash) profile="${HOME}/.bashrc" ;;
    */fish) profile="${HOME}/.config/fish/config.fish" ;;
  esac
  say ""
  say "It is not on your PATH yet, so the command is not found by name. Either:"
  say ""
  say "  export PATH=\"${dir}:\$PATH\"      # this shell now, and in ${profile} to keep it"
  say ""
  say "or install it for everyone instead, which needs root:"
  say ""
  say "  curl -fsSL ${SELF_URL} | sudo bash"
}

main() {
  local version="${VERSION:-}" dir="" os arch archive tmp running
  while [ $# -gt 0 ]; do
    case "$1" in
      --version) version=${2:-}; shift 2 || die "--version needs a value" ;;
      --version=*) version=${1#*=}; shift ;;
      --dir) dir=${2:-}; shift 2 || die "--dir needs a value" ;;
      --dir=*) dir=${1#*=}; shift ;;
      -h | --help) usage; return 0 ;;
      *) die "unknown option $1. Try --help" ;;
    esac
  done

  need curl
  need tar
  # Two statements and not `read ... <<<"$(platform)"`: a command substitution
  # inside a here-string is not the command `set -e` is watching, so a refusal
  # from platform was printed and then walked straight past, leaving os and
  # arch empty and the run to fail later against a 404 for
  # "ghchronicle_2.0.0__.tar.gz". A plain assignment does exit.
  local detected
  detected=$(platform)
  read -r os arch <<<"$detected"
  [ -n "$version" ] || version=$(newest_version)
  version=${version#v}
  archive="${BINARY}_${version}_${os}_${arch}.tar.gz"

  tmp=$(mktemp -d)
  # shellcheck disable=SC2064  # $tmp is wanted as it is now, not at exit
  trap "rm -rf '$tmp'" EXIT

  say "downloading ${BINARY} ${version} for ${os}/${arch}"
  curl -fsSL -o "${tmp}/${archive}" "${GHCHRONICLE_DOWNLOAD_BASE}/v${version}/${archive}" ||
    die "no archive at ${GHCHRONICLE_DOWNLOAD_BASE}/v${version}/${archive}. Check the version against the releases page."
  curl -fsSL -o "${tmp}/checksums.txt" "${GHCHRONICLE_DOWNLOAD_BASE}/v${version}/checksums.txt" ||
    die "the release publishes no checksums.txt, so what was downloaded cannot be checked"

  verify_checksum "$tmp" "$archive"
  say "checksum verified"
  verify_signature "$tmp" "$version"

  tar -xzf "${tmp}/${archive}" -C "$tmp" "$BINARY" ||
    die "the archive does not contain a ${BINARY} binary"

  [ -n "$dir" ] || dir=$(target_dir)
  mkdir -p "$dir" || die "cannot create $dir. Pass --dir with somewhere writable."
  install -m 0755 "${tmp}/${BINARY}" "${dir}/${BINARY}" ||
    die "cannot write to $dir. Pass --dir with somewhere writable, or run this with sudo."

  say "installed ${dir}/${BINARY}"
  if ! on_path "$dir"; then
    finishing_line "$dir"
    return
  fi
  # On PATH is not the same as the one that runs. An older copy from `go
  # install` in ~/go/bin, or a package manager's, earlier in PATH keeps
  # winning, and nothing about this install would say so: the version printed
  # below comes from the file just written, by its full path, so it looks
  # right while the name resolves elsewhere. Measured the hard way on a machine
  # where a September build shadowed the release for an afternoon.
  running=$(command -v "$BINARY" 2>/dev/null || true)
  if [ -n "$running" ] && [ "$running" != "${dir}/${BINARY}" ]; then
    say ""
    say "warning: ${BINARY} still runs ${running}, which comes earlier in your PATH."
    say "         Remove it, or put ${dir} first, or run ${dir}/${BINARY} by its full path."
    say ""
  fi
  "${dir}/${BINARY}" -version
  offer_setup "${dir}/${BINARY}"
}

# offer_setup points at the guided setup, and runs it when there is somebody
# there to answer.
#
# The asking is done through /dev/tty rather than standard input, because the
# documented way to run this is `curl ... | bash`, where standard input is the
# script itself: a read there would swallow the rest of the script rather than
# wait for a person. When there is no terminal at all the offer is a line to
# read, which is the honest form of the same thing.
offer_setup() {
  local binary=$1 answer
  say ""
  # Opened rather than tested for. `[ -r /dev/tty ]` asks about permissions and
  # says yes on a machine with no controlling terminal, where the open then
  # fails with "No such device or address": a CI job, a container build, a
  # provisioning run. Trying the open is the question actually being asked.
  #
  # Inside a group, with stderr redirected on the group. A command's own
  # redirections are made left to right, so in `: < /dev/tty 2> /dev/null` the
  # open fails before stderr is moved, and bash reports the failure on the
  # stderr the script was given: every such install ended with
  # "/dev/tty: No such device or address".
  if ! { : < /dev/tty; } 2> /dev/null; then
    say "Next: ${BINARY} -setup writes a configuration, asking only what it"
    say "      cannot work out and checking each answer as it goes."
    return
  fi
  printf 'Set it up now? It asks for a token and where to put the data. [Y/n]: ' > /dev/tty
  read -r answer < /dev/tty || answer=n
  case "${answer:-y}" in
    [Yy] | [Yy][Ee][Ss] | "") "$binary" -setup < /dev/tty ;;
    *) say "Run ${BINARY} -setup when you are ready." ;;
  esac
}

main "$@"
