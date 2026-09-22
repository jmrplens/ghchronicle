# Changelog

What changed in each release, why, and what was left unproven.

The release notes on each tag are generated from the commits and say what
landed. This file is for what they cannot say: the reason a thing changed, the
measurement behind it, and the part nobody verified. Where a claim here was
measured, it says on what.

Versions follow [semantic versioning](https://semver.org/). The dates are the
day the tag was pushed.

## 2.4.0 - 2026-09-21

Installing this stopped being a reading exercise.

- **`ghchronicle -setup`**, a guided install that lives in the binary rather
  than in the install script, so it serves whoever arrived through Homebrew,
  Docker or a zip on Windows just as well as whoever piped `install.sh` into a
  shell. It asks for a GitHub token, which account to collect, where to keep the
  data, whether to publish the dashboard, and whether to install a service, and
  it checks every answer against the real thing before writing anything: the
  token against `/user`, the store and Grafana against their own probes. The
  configuration it writes carries only `${VAR}` references; the credentials go
  beside it in a file only the account that ran it can read. It never replaces
  an existing service without asking.
- **Five compose files that come up on their own**, one per combination of
  store and Grafana, each carrying its own configuration inline so there is no
  second file to write, and each reachable from a picker on the Docker page.
  With Grafana in the stack the dashboard is already there: the collector
  publishes it at start-up and points it at the store beside it.
- **PostgreSQL as a connecting sink**, through pgx, beside the SQL file it
  could already write. With it, all five stores can now derive their own
  Grafana datasource.
- **The documentation reorganised by audience**: what somebody using the tool
  needs, what they look up, and what only matters to whoever edits the
  repository. The README became the front door, because it is what a reader
  sees first.

Measured here, on this server: every compose combination brought up against
the real images, with the dashboard drawing real data, not merely a container
that starts.

Not verified, and worth saying plainly: `-setup` has only ever been run
end to end on Linux. The launchd agent and the Windows scheduled task are built
and read by tests that run on all three systems in CI, and the code that writes
them is exercised there, but nobody has installed either on a real machine.

## 2.3.0 - 2026-09-19

- **The binary publishes its own dashboard.** Given a Grafana URL and a token,
  `ghchronicle -publish-dashboard` creates the datasource if it is missing and
  publishes the dashboard for every store it writes to, and the collector does
  the same at start-up. Importing JSON by hand became one of the ways in rather
  than the only one.
- A dashboard left behind by a store that is no longer configured is **named**
  rather than silently overwritten or silently kept.

## 2.2.0 - 2026-09-19

- **The version is stamped at build time** instead of being kept in step by
  hand in several files. A check fails the build when the places that mention
  it disagree.
- The installer says what it does now rather than what it used to do, and
  **warns on all three systems** when the binary it just installed is not the
  one the shell will find first.

## 2.1.0 - 2026-09-19

- **Install in one line**, and the installed binary behaves as a command on
  Linux, macOS and Windows.
- **The backfill can be watched, and goes back on its own.**
- Every HTTP client got **its own connection pool**. Sharing one meant a slow
  store could hold up the collector.

## 2.0.0 - 2026-09-18

The release that made a long backfill survivable.

- **A backfill that a kill interrupts is picked up where it left off.** The
  checkpoint covers the sinks in scope and carries no credentials.
- **What a collector gathered before it failed is kept**, and what failed is
  said, rather than the whole sweep being lost to one bad family.
- **A repository is named the same way in every measurement**, which a
  dashboard cannot work around once it is not.
- Every panel of every dashboard was drawn against a real account; three that
  the pictures showed to be wrong were fixed.

## 1.0.0 - 2026-09-15

First public release. MIT, one initial commit, twenty release artifacts with
SBOMs and keyless cosign signatures, images on ghcr.io and Docker Hub for amd64
and arm64, and the bilingual documentation on GitHub Pages.
