# Changelog

What changed in each release, why, and what was left unproven.

The release notes on each tag are generated from the commits and say what
landed. This file is for what they cannot say: the reason a thing changed, the
measurement behind it, and the part nobody verified. Where a claim here was
measured, it says on what.

Versions follow [semantic versioning](https://semver.org/). The dates are the
day the tag was pushed.

## 2.5.0 - 2026-09-25

`go install` reaches this release, the first one it can, and the stars of a
repository whose stargazers GitHub will not list are counted all the same.

- **The module path is `github.com/jmrplens/ghchronicle/v2`.** Every v2 tag
  from 2.0.0 to 2.4.0 carried a `go.mod` that said
  `github.com/jmrplens/ghchronicle`, and Go refuses a v2 or later tag whose
  path does not end in its major. The module mirror listed v1.0.0 and nothing
  else, and answered v2.4.0 with "module path must match major version", so
  the `go install ...@latest` on the home page, in the README and on every
  install page installed 1.0.0, the first public release, and said nothing.
  The command is now
  `go install github.com/jmrplens/ghchronicle/v2/cmd/ghchronicle@latest`.
  The old path still resolves, to 1.0.0, because a tag already pushed keeps
  the `go.mod` it was cut from; nothing can move it.
- **The release preflight holds the tag's major to the module path**, beside
  the VERSION check it already made, so a v3 tag with a v2 path is refused
  before anything is built.
- **`gh_star_day`, every repository's stars by day.** A new measurement in the
  `stars` family, read from `/repos/{owner}/{repo}/stargazers/history` for
  every repository a sweep collects: one row per repository and day, tagged
  `full_name`, `owner` and `repo`, with that day's stars in `stars`. Every
  repository, because since July 2026 GitHub serves the stargazer list only to
  a repository's admins and collaborators. Anyone else gets a 404 from REST and
  an empty list from GraphQL, which counts 46,395 stars on cli/cli and lists
  none of them, so a repository where the account is neither has written no
  new `gh_star` row since, and the two panels that count stars over time left
  it out. The history is served to anyone who can read the repository, so
  reading it for all of them needs nothing detected and nothing configured. It
  went unread until now because it was taken to hold only the last thirty
  weeks: that is its page size, and it pages back to the week the repository
  was created. `gh_star` stays, as the record of who starred and when to the
  second, wherever GitHub still lists them.
- **A day is GitHub's calendar day in Los Angeles.** GitHub labels each week
  Sunday 00:00 UTC but buckets its days by the calendar in
  America/Los_Angeles, daylight saving included. Each row is stamped at 00:00
  UTC of that date, the convention `gh_contribution_day` already follows, and
  anchored to GitHub's week rather than the clock, so a Tuesday sweep and a
  Friday one land on the same rows. No day after the sweep is written: GitHub
  fills the rest of the current week with zeros, and those are left out.
- **An unstar changes the past.** The history is the stargazers GitHub lists
  today, each counted on the day they starred, so an unstar takes a star off
  the day it was given, not the day it was withdrawn. Page one, the newest
  thirty weeks, is what every sweep reads, and it is written whole, zero days
  included, so an unstar there is applied. Older pages are read on a first
  walk and in a backfill and write only the days that have stars, so an older
  day that loses its only star keeps it, the way `gh_star` keeps every star it
  ever saw. Zeros stop at page one because InfluxDB 3 Core writes a file for
  every day a write touches and never compacts them, and zeros back to each
  repository's creation would have been thousands of files a backfill.
- **Two panels change meaning.** Stars gained over time counts `gh_star_day`
  in every store but Prometheus, and Stars over time climbs from it in
  InfluxDB and PostgreSQL; in Graphite and Elasticsearch that curve stays on
  the `gh_repo` snapshots it already drew. Where they read the history now,
  they counted `gh_star` rows before, and what they show moves in three ways.
  The bars are Pacific days, so a star given on a European morning can sit a
  day before the date Recent stars shows for it. The curve counts the
  stargazers GitHub lists when the history is read, so it has lost every star
  taken back before that first read, which the `gh_star` curve kept. It
  usually sits at or a little below the `gh_repo` count, which also counts
  accounts GitHub no longer lists: 25 against 26 on A-Lab, 7 against 8 on
  LoVE-BASS, 12 against 13 on mastodon_official_profiles. A star given more
  than thirty weeks ago and taken back after that read stays in it: only a
  backfill that reaches back to its day lowers that day, and only while the
  day holds another star, so one that was its day's only star stays for good
  and the curve can also sit above the `gh_repo` count. And both panels
  now hold the repositories whose list is hidden, which have stars there and
  no names under Recent stars, since names exist only in `gh_star`. No panel
  counts from both measurements, so no star is counted twice. Prometheus is
  unchanged: `gh_star_day` is not reduced for it, because a per-day history
  has no current value and an unstar lowering a past day would read as a
  counter reset, so its Stars gained still counts only the lists the token can
  read. Graphite's stand-in for Recent stars, a count per repository because
  Graphite keeps no names, reads the history as well.
- **It costs a request per repository a sweep, and usually nothing.** A sweep
  reads page one, which GitHub answers 304 unless a day of the last thirty
  weeks gained or lost a star or a new week began, and a 304 is not charged.
  The first sweep after upgrading reads each repository's history whole, once:
  a page per thirty weeks of its life, 180 pages for the 19 starred
  repositories of this account. The state file records that under
  `history_read`, set only after a walk has reached the end of the history, so
  a walk cut short is read again by the next sweep rather than left partial
  until a backfill: by a 502, and also by a later page answering 403 or 404,
  which is a secondary limit sent without its headers or a repository gone
  mid-walk, not the end of the history. What it does cost is time: a REST
  round trip per repository every sweep, which the GraphQL batch had removed
  for the lists.
- **GitHub Enterprise Server has no star history.** Its REST API, checked
  against 3.21 and 3.22, serves the stargazer list and not
  `stargazers/history`, so against it every repository answers 404, which is
  read as nothing to collect, and `gh_star_day` stays empty. The panels that
  count stars from it are empty with it: Stars gained over time and Stars over
  time in InfluxDB and the SQL stores, Stars gained over time in Graphite and
  Elasticsearch, and Graphite's Recent stars, all of which drew from `gh_star`
  before and showed stars there. `gh_star` is still collected, Recent stars
  still lists names from it in the other stores, and Prometheus still counts
  from it. A history that answered 404 is not recorded as read, so a server
  that starts serving it has each repository's history read whole on the
  next sweep.
- **A job whose steps GitHub no longer serves carries no `steps`.** GitHub
  keeps a run's jobs much longer than their steps. Every job of a run created
  before about 12 April came back with its times, its runner and its
  conclusion over an empty step list, and every job of a later run with its
  steps, so those jobs were written with `steps` at 0, a count nobody made, as
  far back as a backfill reached. A job that ended as success, failure or
  timed out and lists no steps is now written without the field: it ran, so
  it had steps, and in InfluxDB, the SQL stores and Elasticsearch anything that
  averages steps per job now averages the jobs that reported them. The
  Prometheus and OTLP `github_workflow_jobs_steps_mean` still divides by every
  job in the batch, as it already does for `queued_seconds`, so a batch that
  mixes old and new runs reads low there as it did before. A backfill that
  re-reads a job it saw when the run was new no longer writes a 0 over the
  count it wrote then. A skipped job keeps
  its 0, because it runs no steps, and so does a cancelled one, which may have
  stopped before its first. The zeros already stored stay: InfluxDB merges
  the fields of a point written again and the SQL upsert sets only the
  columns a point carries, so collecting a job again leaves its old 0, which
  in PostgreSQL
  `UPDATE gh_workflow_job SET steps = NULL WHERE steps = 0 AND conclusion IN ('success', 'failure', 'timed_out')`
  clears. Elasticsearch replaces a document whole, so there a job collected
  again loses its 0, and with it any count it was first written with.
- **A walk whose first page is answered from the ETag cache reads its
  second.** GitHub sends no Link header with a 304, and the client handed that
  missing header back as it came. Three walks find their next page there, the
  stargazer list by page number and the activity log and the Dependabot alerts
  by cursor, so any of them whose first page came from the cache ended after
  it, without an error. The cache now keeps the Link header beside the body and
  replays it with the 304. An ordinary sweep lost nothing to it: it reads the
  stargazer list only on first sight, one page of Dependabot alerts, and a
  second page of the activity log that has nothing new when the first has not
  changed. What it could cut short was a second request for the same first
  page in one process, which is what a `-backfill-retry` pass makes when it
  goes back for a walk that failed partway: that walk stopped at page one and
  was recorded as done.

Measured on a copy of the repository served to Go as its origin, with three
tags: the old path resolves `@latest` to v1.0.0 and refuses v2.98.0 with the
mirror's own error, and the new one resolves `@latest` to v2.99.0, builds, and
the binary reports that number.

Measured on this account on 24 and 25 September, against the full REST
stargazer list of each of its 19 starred repositories, 440 stars: the history
sums to the length of the list in all 19. Bucketed by Los Angeles days the two
agree on every star. Bucketed by UTC days, 38 of the 127 days compared on
phonometry disagree, and by a fixed UTC-7 one winter star does, which is what
says daylight saving is part of it. On the same endpoint a page is thirty
weeks, `per_page`'s default and its cap (a smaller one is honoured), the 200
for page one carries `rel="next"` and
`rel="last"`, and the 304 for it carries no Link at all and is not charged:
`x-ratelimit-used` read 297 before and after. The steps cutoff was read on 24
September across this account's runs, and a skipped job was measured listing
none.

Not verified, and worth saying plainly:

- The public mirror and pkg.go.dev picking up the first `/v2` tag, which the
  copy could not show. RELEASING.md now says how to check both after tagging.
- The day bucketing is measured, not documented. GitHub documents neither the
  Pacific days nor a week label that does not match them. If it moves to UTC
  days, a star shifts by at most a day and nothing breaks.
- Nobody has watched an unstar arrive. That it rewrites the day the star was
  given is read from the history always summing to the list GitHub serves
  today; none of these repositories lost a star while they were measured, and
  starring then unstarring to watch would have been a write.
- The steps cutoff is one snapshot. It cannot say whether GitHub keeps steps
  for a window, about five and a half months that day, that moves with the
  calendar, or dropped them before a fixed date; a second reading some weeks
  later would.
- The backfill the missing Link could cut short is read from the code. No
  store was searched for a walk it had stopped.
- That GitHub Enterprise Server has no star history is read from its REST
  documentation and its OpenAPI descriptions for 3.21 and 3.22, not from a
  running server.

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
