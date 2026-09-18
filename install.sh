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
verify_signature() {
  local dir=$1 version=$2
  command -v cosign >/dev/null 2>&1 || return 0
  curl -fsSL -o "${dir}/checksums.txt.sigstore.json" \
    "${GHCHRONICLE_DOWNLOAD_BASE}/v${version}/checksums.txt.sigstore.json" || {
    say "note: this release publishes no signature bundle, so only the checksum was verified"
    return 0
  }
  cosign verify-blob \
    --bundle "${dir}/checksums.txt.sigstore.json" \
    --certificate-identity-regexp "$COSIGN_IDENTITY" \
    --certificate-oidc-issuer "$COSIGN_ISSUER" \
    "${dir}/checksums.txt" >/dev/null 2>&1 ||
    die "cosign could not verify that checksums.txt came from the release workflow. Do not use what was downloaded."
  say "signature verified with cosign"
}

# target_dir picks somewhere writable rather than reaching for sudo. A script
# read off the network should not be the thing that decides to become root.
target_dir() {
  if [ -n "${BIN_DIR:-}" ]; then
    printf '%s\n' "$BIN_DIR"
  elif [ -w /usr/local/bin ]; then
    printf '%s\n' /usr/local/bin
  else
    printf '%s\n' "${HOME}/.local/bin"
  fi
}

main() {
  local version="${VERSION:-}" dir="" os arch archive tmp
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
  case ":${PATH}:" in
    *":${dir}:"*) "${dir}/${BINARY}" -version ;;
    *) say "note: ${dir} is not on your PATH. Add it, or run ${dir}/${BINARY} by its full path." ;;
  esac
}

main "$@"
