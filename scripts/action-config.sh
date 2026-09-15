#!/usr/bin/env bash
# Writes the configuration the Action runs with when it is given none. It is a
# file of its own, rather than lines inside action.yml, so that a test can run
# exactly what the Action runs.
#
# Every family on, nothing written anywhere: the card is the only output of a
# default run. A user who wants a database supplies a config file of their own.
set -euo pipefail

login=${USER_LOGIN-}
include=${INCLUDE_PRIVATE-}

# A GitHub login, and nothing else. The value is written into a YAML document
# below, where a line break would let it add keys of its own to the
# configuration the Action then runs.
case $login in
  "" | *[!A-Za-z0-9-]* | -* | *-)
    echo "user must be a GitHub login, got '$login'" >&2; exit 2 ;;
esac

# Off unless asked. A card is published, and one that counts private
# repositories names them in a public README.
case $include in
  true | false) ;;
  *) echo "include-private must be true or false, got '$include'" >&2; exit 2 ;;
esac

cat > "$OUT" <<YAML
github:
  token: \${GITHUB_TOKEN}
targets:
  user: ${login}
  include_private: ${include}
sinks: {}
state_file: ${STATE}
YAML
