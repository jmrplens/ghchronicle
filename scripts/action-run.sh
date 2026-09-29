#!/usr/bin/env bash
# Assembles the binary's command line and runs it. A file of its own, rather
# than lines inside action.yml, so that a test can run exactly what the
# Action runs.
set -euo pipefail

args=(-config "$GHC_CONFIG")
case "$MODE" in
  once) args+=(-once) ;;
  backfill) args+=(-backfill); [ -n "$SINCE" ] && args+=(-backfill-since "$SINCE") ;;
  card) ;;
  # The workflow that asks for this mode is the word -migrate -yes wants:
  # somebody chose to run it, and it is dispatched by hand.
  migrate)
    if [ -n "$CARD" ]; then
      echo "mode migrate sweeps nothing, so it draws no card; leave card empty" >&2; exit 2
    fi
    args+=(-migrate -yes) ;;
  *) echo "mode must be once, backfill, card or migrate, got '$MODE'" >&2; exit 2 ;;
esac
if [ -n "$CARD" ]; then
  # The binary refuses to create directories: it sweeps, and then the
  # write fails when the card's directory is not there. Here the path
  # is the workflow author's own input, in their own checkout, and the
  # first run in a fresh profile repository has no generated/ yet, so
  # the Action creates the directory itself rather than fail after
  # spending the sweep.
  mkdir -p -- "$(dirname -- "$CARD")"
  args+=(-card "$CARD" -card-layout "$LAYOUT" -card-theme "$THEME")
  [ -n "$FIELDS" ] && args+=(-card-fields "$FIELDS")
  # Passed only when it is not the default, so a workflow that pins
  # `version` to a release older than the flag keeps working.
  [ "$MOTION" != once ] && args+=(-card-motion "$MOTION")
  [ -n "${WIDTH:-}" ] && [ "$WIDTH" != 0 ] && args+=(-card-width "$WIDTH")
  # The same gate as the width above, against this input's own default rather
  # than zero: 0 is a speed a reader means, the slowest one, and a card drawn
  # at 0.5 is the card the flag's absence draws anyway.
  [ -n "${SPEED:-}" ] && [ "$SPEED" != 0.5 ] && args+=(-card-speed "$SPEED")
  [ "$MODE" = card ] && args+=(-card-only)
elif [ "$MODE" = card ]; then
  echo "mode card needs a card path" >&2; exit 2
fi

# The log goes to the step's output as it always has, and every line that
# says a store was changed, or is left in two shapes, or owes its history,
# becomes an annotation on the run as it is written: a warning in a log nobody
# opens is how a store stays in two shapes for weeks, and a job cancelled or
# timed out while a start read back what it cleared ends before any line
# after it. Standard output and the log share the step's output either way;
# pipefail, set above, makes the status the binary's own.
annotate() {
  local line title
  while IFS= read -r line; do
    printf '%s\n' "$line"
    case $line in
      *'migration pending'*) title='ghchronicle migration pending' ;;
      *'applying a migration before the first sweep'*) title='ghchronicle migration applied at the start' ;;
      *'migration failed'*) title='ghchronicle migration failed' ;;
      *'refill owed'* | *'refill did not finish'* | *'refill stays owed'*) title='ghchronicle refill owed' ;;
      *) continue ;;
    esac
    # A workflow command ends at a line break and reads % as an escape.
    printf '::warning title=%s::%s\n' "$title" "${line//'%'/'%25'}"
  done
}
status=0
ghchronicle "${args[@]}" 2>&1 | annotate || status=$?
exit "$status"
