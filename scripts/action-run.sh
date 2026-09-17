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
  *) echo "mode must be once, backfill or card, got '$MODE'" >&2; exit 2 ;;
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
ghchronicle "${args[@]}"
