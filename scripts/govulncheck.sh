#!/usr/bin/env bash
# Run govulncheck, tolerating a small, documented allowlist of accepted
# advisories that have no available fix.
#
# govulncheck has no native ignore mechanism. This wrapper runs it and, when it
# reports vulnerabilities, passes only if EVERY reported advisory ID is on the
# ALLOWLIST below. Any advisory not on the list fails the build, so newly
# introduced (fixable) vulnerabilities are never silently ignored.
#
# What this gates on, precisely: whether OUR CODE CALLS a vulnerable symbol.
# That is what govulncheck's own exit status reports, and this wrapper defers to
# it rather than re-deriving it from the printed text. The distinction is not
# academic. govulncheck also reports advisories against modules that are merely
# in the build graph, and it prints those only at higher -show levels, so a
# wrapper that scraped every advisory ID out of the output would pass or fail
# depending on a flag its caller happened to pass: `-show verbose ./...` would
# fail where the same scan without the flag passed.
#
# The scanner is the release go.mod names in its `tool` directive, which is
# where `go install golang.org/x/vuln/cmd/govulncheck` (no @version) takes it
# from. A scanner that moved on its own would change what "no known
# vulnerability" meant last week, so it moves only in a commit that runs
# `go get -tool golang.org/x/vuln/cmd/govulncheck@<version>`. Dependabot will
# not open that commit: go.mod records a tool's module as `// indirect`, its
# version updates skip every indirect requirement, and it reads no `tool`
# directive (dependabot/dependabot-core#12050, still open).
#
# Accepted advisories:
#   None, and none has ever been needed.
#
#   Keep the list empty. An entry here is a vulnerability shipped on purpose in
#   code we actually call. One is added only with its advisory ID (GO-YYYY-NNNN)
#   and a written reason beside it, in this comment: why no version bump can fix
#   it, and why the reachable path is inert. An ID without that argument is not
#   an acceptance, it is a silenced alarm.
#
# Usage: scripts/govulncheck.sh [-tags <tags>] [packages...]
set -uo pipefail

# Space-separated OSV IDs accepted with documented justification above.
ALLOWLIST=""

echo "=== govulncheck ==="
out="$(govulncheck "$@" 2>&1)"
status=$?
printf '%s\n' "$out"

if [ "$status" -eq 0 ]; then
  # Nothing our code calls is vulnerable. Advisories against modules that are
  # only in the build graph may still appear above, at -show levels that print
  # them; they are informational and do not gate, because a module we require
  # for one package does not become a risk through a package we never import.
  exit 0
fi

# Past here our code is affected, or govulncheck itself failed. Advisory IDs
# govulncheck reported (deduplicated).
ids="$(printf '%s\n' "$out" | grep -oE 'GO-[0-9]{4}-[0-9]+' | sort -u)"

if [ -z "$ids" ]; then
  # A non-zero exit with no advisory parsed is a tool or build error. Propagate
  # it verbatim rather than reporting a clean scan.
  exit "$status"
fi

unaccepted=""
for id in $ids; do
  case " $ALLOWLIST " in
  *" $id "*) ;;
  *) unaccepted="$unaccepted $id" ;;
  esac
done

echo ""
if [ -n "$unaccepted" ]; then
  echo "govulncheck: FAIL: unaccepted advisories:$unaccepted"
  exit 1
fi

ids_oneline="$(printf '%s\n' "$ids" | paste -sd' ' -)"
echo "govulncheck: PASS: only accepted advisories present: ${ids_oneline} (see scripts/govulncheck.sh)"
exit 0
