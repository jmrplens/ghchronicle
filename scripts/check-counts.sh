#!/bin/sh
# The measurement and family counts appear in prose in more than one file, and
# had drifted in two of them: the README said thirty-two families and the
# metrics reference said forty-seven measurements when the source held
# fifty-three. Counting the source is the only way prose stays true.
#
# The prose spells the number out, so this spells it too: either the English
# word for the current count or the digits satisfies the check. Spelling it
# here rather than pinning one word is what stopped this passing for months
# after the count had moved on.
set -eu
cd "$(dirname "$0")/.."
measurements=$(grep -rhoE 'Measurement: "[a-z_]+"' internal/collect/*.go | sort -u | wc -l | tr -d ' ')
families=$(awk '/^var defaultEvery/,/^}/' internal/config/config.go | grep -cE '^[[:space:]]+"[a-z]+":' || true)

ONES="zero one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen"
TENS="x x twenty thirty forty fifty sixty seventy eighty ninety"

# The English spelling of a number below a hundred, which is as far as this
# ever needs to count.
spell() {
  n=$1
  if [ "$n" -lt 20 ]; then
    echo "$ONES" | cut -d' ' -f$((n + 1))
    return
  fi
  ten=$(echo "$TENS" | cut -d' ' -f$((n / 10 + 1)))
  unit=$((n % 10))
  if [ "$unit" -eq 0 ]; then
    echo "$ten"
  else
    echo "$ten-$(echo "$ONES" | cut -d' ' -f$((unit + 1)))"
  fi
}

words=$(spell "$measurements")
status=0
for f in README.md docs/metrics.md; do
  if ! grep -qiE "$words|\\b$measurements measurements" "$f"; then
    echo "$f does not state the measurement count ($measurements, \"$words\")" >&2
    status=1
  fi
done
echo "measurements: $measurements, families: $families"
exit $status
