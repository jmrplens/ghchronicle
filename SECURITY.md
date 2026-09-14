# Security policy

## Reporting a vulnerability

Do not open a public issue.

Use GitHub's private vulnerability reporting: the **Security** tab of this
repository, then **Report a vulnerability**. It creates a private advisory that
only the maintainer can see, and it is the only channel this project asks you
to use.

Please include the version (`ghchronicle -version`), what an attacker gains,
and the smallest way to reproduce it. If a proof of concept needs a token, do
not send the token.

You can expect an acknowledgement within a week and, once the report is
confirmed, a fix released before the advisory is published. A reporter who
wants credit gets it in the advisory; a reporter who does not is not named.

## Supported versions

The latest release. This is a single binary with no long-term branches, so a
fix ships as a new tag rather than as a patch to an older one.

## What is worth reporting

This process holds a GitHub token with read access to everything an account can
see, which is the thing worth protecting here. Anything that leads to the token
leaving the process, or to it being used for something the configuration did
not ask for, is a vulnerability. So is anything that writes outside the paths
the configuration names.

Concretely, and in rough order of interest:

- The token, or any secret expanded from `${VAR}`, appearing in a log line, in
  an error message, in a point, in the card, or in a request to any host other
  than the configured API.
- A collector or a sink reaching a host the configuration did not name.
- A path written outside `state_file`, `sinks.dedupe_file`, `sinks.file.path`
  and `log.file`.
- Anything in a GitHub response that changes what the process executes, opens
  or writes, rather than only what it records.
- A dependency advisory that this project's use actually reaches.

## What is not a vulnerability

- The token having wide read access. That is what the tool is for; the
  documentation says which scope buys which family so the token can be cut
  down, and the hardened systemd unit and the container image exist to bound
  what the process can do with it.
- Collected data being readable by whoever can read the store or the file sink.
  The file sink is written `0600` inside a `0750` directory for that reason,
  and widening it is the operator's decision.
- The Prometheus exporter serving the numbers it exists to serve. Bind it to
  loopback if they are not for everyone.
- A rate limit, a 403 or a 404. Those are recorded and skipped by design.
