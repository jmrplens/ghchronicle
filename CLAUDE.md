# CLAUDE.md

Context for AI agents working in this repository.

## What this is

`ghchronicle` collects every metric GitHub exposes about an account and writes
each observation as a point dated when the thing happened. Go, single static
binary. Its direct dependencies are `gopkg.in/yaml.v3`, the PostgreSQL driver
`github.com/jackc/pgx/v5`, `golang.org/x/term` so that `-setup` can read a
secret without echoing it, and `golang.org/x/sys`, which `x/term` builds on and
which takes the lock beside the state file (`flock` on Unix, `LockFileEx` on
Windows); `go.mod` is the list.

The reason it exists: GitHub keeps almost nothing. Traffic is a rolling
fourteen days, the event feed is the last three hundred events of the past
thirty days, inbox notifications go after three months unless saved, job logs
after the repository's retention period (ninety days by default), and from
1 October 2026 workflow runs follow that same retention. If it is not
collected while it is there, it is gone.

## Layout

```text
cmd/ghchronicle     the binary: flags, sinks, runner, the one-shot card, -setup, -uninstall
cmd/probe           development aid, runs collectors and prints line protocol, writes nothing
internal/ghapi      REST and GraphQL client, ETag cache, per-bucket rate state, typed
                    errors, and the one retry of a REST 500, 502, 503 or 504,
                    of a GraphQL failure that is not GitHub's timeout, and of
                    a job log's storage
internal/collect    one file per family of metrics, Walk (the pagination bound),
                    Refusals (the memory of 403 and 404), Movements (the gate)
internal/sink       Point, line protocol, twelve sink types, the Unchanged wrapper
                    that holds the write ledger, and the Reducer that makes
                    gauges. The published count is eleven: stdout in two formats
                    is one destination to configure, which is what
                    site/scripts/gen-stats.mjs counts
internal/render     the SVG card: thirteen layouts in two families, and the accumulator sink
internal/config     YAML, ${VAR} in credentials and addresses and ~ as well in
                    path settings, per-family cadences, backfill bound
internal/run        the sweep scheduler, its state file, the cache file that
                    makes a restart cheap, the turns the slow families take,
                    the backfill cooldown, and the checkpoint that makes a
                    backfill resumable
internal/grafana    the little Grafana client, and the panel run both the
                    dashboard checker and the containerised suite use
internal/httpx      a connection pool per client, never http.DefaultTransport's
internal/teardown   what -uninstall drops from each store, and what a migration
                    asks it and sets aside there, found by asking the store.
                    Destructive by design, and the caller confirms
internal/migrate    the registry of every change to what a stored row is keyed
                    by, the plan -migrate prints, what a start applies on its
                    own, the refill, and the purge of what was set aside
test/e2e            builds the binary and runs it against a fake GitHub
test/e2e/docker     the same binary against the real stores in Docker, and the
                    five dashboards against those; behind the dockere2e tag
test/live           a few points pushed at a real Loki or OTLP endpoint named in
                    GHC_LIVE_LOKI or GHC_LIVE_OTLP; skipped without them
cmd/                the development utilities: gen_dashboards and its checkers
                    (check_dashboards, check_prometheus, check_postgres),
                    publish_dashboard, gen_config (the configuration page's
                    data), gen_compose (the Docker page's compose files),
                    gen_layouts and gen_brand
internal/dashboards the dashboard specification, built in memory: what the
                    generators write out and what the binary publishes
dashboards/         one specification rendered into a Grafana dashboard per store
site/               the documentation site: the English pages and their Spanish
                    twins, and the scripts that generate what is derived from them
docs/               those English pages as plain Markdown, written by
                    site/scripts/gen-docs.mjs. Output, never edited by hand
action.yml          the composite Action that wraps all of it
plan/               working notes, NOT versioned
```

## The rules that are not obvious

**A point carries the date the thing happened, not the date it was collected.**
A workflow run is stamped when it finished, a star when it was given, a
traffic day at that day's own date, a closed pull request when it closed. This
is what makes re-collection idempotent: InfluxDB keys a point by measurement,
tag set and timestamp, so rewriting the same fourteen-day window every six
hours converges instead of accumulating. Anything that is genuinely a current
state (cache size, open alerts, inventory) is stamped now, deliberately.
Between the two sits a third: what has no date of its own and must not become a
row per sweep is a daily snapshot, stamped at 00:00 UTC and rewritten through
the day (an open pull request, issue or outbound item until it closes,
`gh_actions_cache_entry`, a branch). A current state never sits on a row dated
in the past, where each move would rewrite history: the stars of the
repository an outbound item went to are `gh_upstream_repo`, stamped now, beside
`gh_external_contribution`, dated when the item closed; a release's downloads
are `gh_release`, stamped now, and its publication `gh_release_published`.

**Prometheus cannot hold that.** Measured, not assumed: against Prometheus 3.14
with `--web.enable-otlp-receiver` and `out_of_order_time_window: 30m`, a sample
dated two days back is rejected with HTTP 400. That is why `sink.Summarize`
exists and why the exporter reduces per-item rows to current values. Do not
"fix" the exporter by giving it timestamps.

**A tag is a series, a field is a value.** Anything unbounded goes in a field.
The Actions runner name looks like a good tag until you notice a hosted runner
is named uniquely per run ("GitHub Actions 1000163135"), which would create a
series per job ever run. It is a field.

A value that can change after the row's own date is a field too, never a tag:
as a tag the change opens a second series at the same instant and the stale
row stays for ever. `state` on the alert rows became the field `alert_state`,
`prerelease` is a field on `gh_release_published` because a pre-release is
promoted in place, and since 2.6.1 whether a discussion comment is the
accepted answer is only the `answers` field, where `is_answer` was also a
tag. A tag moved to a field takes a new name or an existing field that says
the same, because a store that holds the tag column refuses a field of that
name. And an existing measurement gains fields, not tags: a tag changes every
row's identity, and PostgreSQL keys each table on the tags it was created
with. A tag a later release adds to a table an earlier release made gets no
column from either PostgreSQL sink, so every insert of that measurement is
refused until the table is dropped or altered by hand; only a tag first seen
within one process or one file becomes a plain column outside the key. A tag
dropped stays in the key, holding ''. Dropping `is_answer` left the stores with
`gh_discussion_comment` in two shapes until they are brought along (the next
rule); the planning panels read one row per comment across both, for a store
nobody has brought along yet.

**A change to what a row is keyed by is a migration.** A point's identity is
its measurement, its tags and its time, so a release that removes a tag, or
moves one into the fields, leaves every row already stored beside the new ones
for ever, in every store that keeps rows. A tag renamed, or added, is refused
outright: the registry can name a tag that went away and has no way yet to find
the rows that lack one. A removal owes, in the same change, a new entry
in `migrate.Registry` (`internal/migrate/registry.go`): an ID
`<release>/<measurement>/<what>`, which state files keep and which is never
renamed or reused; the first release that writes the new shape; the tags only
the old shape carries (`OldTags`, what a store is asked about and what makes
Graphite's old paths one node deeper); every family that writes the
measurement; the tag that says whose rows they are; the tag that says who wrote
a row (`Author`) where an account-wide family writes the measurement beside a
per-repository one; the tags that name one item; and whether GitHub still
serves the whole history (`Whole`, which lets a
store be cleared and read again) or today's state only (`Current`, a note that
changes nothing). A value that changed with the identity kept is `Value`, a
note too. Three tests hold it. `TestEveryChangeOfIdentityIsRegistered` sweeps
the fake GitHub and compares every measurement's tag keys with
`internal/migrate/identity.json`: a tag that went away with no entry fails, and
`-update` refuses to rewrite the file until there is one; a tag added to an
existing measurement fails outright. Only an entry not yet pinned in
`internal/migrate/testdata/registry.json` explains a removal, and a pinned one
never changes: a state file that recorded its ID settled it for good, so a
second tag added to a shipped entry's `OldTags` would never be asked about.
`-update` pins every entry it accepts. `TestEveryMigrationNamesEveryFamilyThatWritesIt`
sweeps it one family at a time and fails on an entry whose families are not
exactly the writers, which is what the manual refill of 2.6.1 got wrong: it
read `outbound` and not `discussions`; `TestEveryFamilyWritesAMeasurementInOneShape`
fails when two families write one measurement with different tag keys, which
the union in `identity.json` cannot see. `TestTheRegistryHoldsTogether` holds
each entry to its own rules. Nothing else is owed: `-migrate` plans every entry
against every store and changes nothing, `-migrate -yes` applies it, and a
start under `migrate: auto`, the default, applies on its own only what loses
nothing, which is GitHub serving the whole history, the old rows set aside for
at least 24 hours (InfluxDB 3's own soft delete, 72 hours by default, a rename
in PostgreSQL, a clone in Elasticsearch) and every row in the store this
configuration's, which the store is asked, repositories as well as accounts
whenever a family reads per repository. A one-shot run on a new state file
applies nothing on its own: the Action's state file goes with its runner, and
only the state file says a refill is still owed. The refill is recorded as owed
before a store is touched, and taken back only when the store says a failure
left it as it was. A store that
can be asked decides whether it holds the old shape, and the state file's
`stores` record decides for the SQL file, Graphite and Telegraf. Every
destructive step names exactly one measurement, or the copy it made, in the
database, bucket, schema or prefix the sink writes to, never a pattern, and
has a test that it touches nothing else, one that the dry run changes nothing,
and a run against the real store in the containerised suite. The 2.0.0
renaming of the repository tags is not in the registry, on purpose: a 1.x
store is recreated, and the registry starts at what a 2.x store can hold.

**Unavailable is not a failure.** `ghapi.UnavailableError` (403/404: the
feature is switched off) and `ghapi.NotReadyError` (202: GitHub is still
computing) mean "there is nothing here". `collect.isSkippable` recognises both.
A repository with Dependabot switched off must not stop the sweep for the other
forty. A 403 or 404 carries no ETag, so an endpoint that refuses by design is
charged in full every time it is asked: it goes through `collect.Refusals`,
handed out by `r.refusalsFor(family)`, which answers a path refused within the
day without asking, keeps each refusal across restarts in the cache file, and
is nil in a backfill, which asks everything. A fork's community profile is the
newest case.

**The activity feeds end with a 422.** Past their ceiling GitHub answers 422
"pagination is limited for this resource". `isPaginationLimit` treats that as
the end of the data, not a failure. Measured: the event feed serves three pages
of 100 and refuses the fourth.

**A sweep is an increment; a backfill is a walk.** Every collector takes a
`collect.Walk{Pages, Since}`: zero pages means its own small default, a
negative count means until the API runs out, and `Since` stops the walk once
a newest-first list has gone past it. The runner hands `Walk{}` to a sweep and
`Walk{Pages: -1, Since: BackfillSince}` to a backfill. Two endpoints needed
their own handling, found by running it: Dependabot refuses `page=` and pages
by cursor, and the GraphQL gateway answers a hundred pull requests with their
reviews with an HTML 502 after ten seconds (`ghapi.TooLargeError`), so `Pulls`
halves its page and retries on the same cursor. The star history is handed
`collect.Unbounded` instead until the state file records `history_read` for
the repository, which only a walk that reached the end of the history sets
(`StarHistory.Read` says whether it did), so the first sweep after upgrading
and any walk cut short, by an error or by a 403 or 404 past page one, read the
whole history once. A 404 on page one, which is every repository on GitHub
Enterprise Server, leaves it unset too.

**A gateway error means one thing in REST and another in GraphQL.** A REST 502
or 504 is the gateway giving up after about ten seconds, intermittently and
whatever the page size, a 500 the application giving up on the same slow
listing a step further in, and a 503 a server that could not take the request
just then, so `ghapi` asks a GET once more two seconds later, through the brake
and with the same `If-None-Match`, and a `collector failed` naming one has
already failed twice. A job log's object storage answering one of the four is
asked again on its own, without the brake, since it spends no budget. A 4xx, a
501 or 505, and a request that got no answer at all are never asked again. A
collector must not add a retry of its own. In GraphQL only a 502 or 504 that
took GitHub's documented ten seconds (`ghapi.GraphQLTimeoutWindow`; all 51 in
the production proxy's log came after 10.45 to 11.23 s) is the query being too
large (`TooLargeError`), which the same query would only time out on again, so
`Pulls` and the co-authored walk halve their page on the same cursor, an
aliased batch (`aliasBatch`) halves the batch, and the other walks keep what
they read and stop. Any other 500, 502, 503 or 504 to a query is asked once
more like a GET, on the same page: the log's three 503s came after 0.70 to
1.08 s, and each, read as too large, ended a commit walk as though the history
had run out and reported success. A query that fails twice, or an answer that
is not JSON and not that timeout, is a `StatusError`, a failure. Do not port
either remedy to the other.

**Three families read only what moved, and the gate is exact, not a guess.**
`commits`, `issueevents` and the incremental pass of `issues` leave unread a
repository that `collect.Movements`, one aliased query per 25 repositories
asked once a sweep, says has not moved since the family's window
(`stayedPut`). That loses no row only because the gate asks the condition that
empties the read: the default branch head's `committedDate` is what
`history(since:)` filters on, and the newest `updatedAt` of issues and pull
requests is what the other two order by and stop at. Change a collector's
window or filter and change the gate with it. Never gated: a backfill, the
daily whole page of `issues`, and a repository the query did not answer for.

**The slow families take turns, in the loop only.** The running service starts
one family of six hours or more a sweep at the built-in cadences (`takeTurns`),
because families marked in one sweep come due together for ever;
`turnsPerSweep` starts more when one a sweep could not keep every cadence, as
under `every.default: 6h`. `-once` runs every family that is due, and a primed
sweep, a card and a backfill run every family, none of them in turns. The
worst-wait table on the cadences page (eleven slow families at the built-in
cadences, 2h30m) is prose that no test pins: a cadence that crosses six hours
changes it.

**The cache file only makes a pass cheaper.** `<state>-cache.bin`, beside the
state file, keeps the client's conditional answers, the workflow runs whose
jobs were written, the standing refusals and the page sizes `totals` set.
Losing it costs one full-price pass of each family and nothing else, and it
has to stay that way: anything whose loss loses data, which is `last_head`
today, belongs in the state file, never here. An answer is keyed by its URL
and a digest of the shape of the type it was decoded into (`decodingName`), so
a struct that gains a field asks again unconditionally rather than replaying a
body stored without it. Three versions are raised by hand, since nothing
derives them: `cacheMagic` for the file's layout, `ledgerMagic` for the write
ledger's record, and `coauthoredRule` for what the Pair Extraordinaire tally
counts, raised with any change to its search, its trailer or the commits it
reads. A file or tally of another version is ignored and rebuilt once. A
card-only run and a backfill read the file and never write it: the card
delivered no jobs to a store, and a backfill's pages would push the sweeps'
own out.

**A batch carries each row once.** Two points with one identity in one batch
both reach every sink: the write ledger reserves the whole batch before it
records any of it, so it does not dedupe within one; the store keeps whichever
came last; and the Reducer's count and sum rules add both. So a collector
merges before it returns: the entries of one Actions cache on one ref are
summed into their row, and an answer both comment walks read is rendered once.

**A `url` field is absolute or it is absent.** 66 of the 95 measurements
carry one (the measurements page says which), and a table listing the rows of
one selects it as a column called Link. The column itself is
hidden: the row's link hangs on the table's first column, `linkOn()`, reading
the url through `${__data.fields.Link}`, because on a phone the Link column at
the far right was reached in two tables of twenty-eight. That hidden column
carries no value mapping on purpose: Grafana hands `${__data.fields.X}` the
cell's display text, so a mapping to the word Open would make the link open
the word. A value that is not a url is a link to the wrong place, and a
relative one sends the reader to the Grafana host. Most of these urls are appended to something GitHub returned,
and GitHub does not always return it, so the concatenation lives in
`internal/collect/urls.go` (`pageURL`, `githubPage`, `githubRootedPage`), each
of which answers with the empty string when a part is missing, and callers pair
that with `setNonEmpty` or `withURL` so the point carries no url at all. Do not
build one with `+` in a collector, and do not build one in a panel either: a
table links from the url column its query returns, and `panel()` keeps a link
override only in the stores whose query returns that column, saying in the
others why it is absent. `cmd/check_dashboards` holds every link column of a
rendered dashboard to the rule: the column comes back, and holds nothing but
absolute urls and empty cells, a null from the SQL stores or the empty string
Elasticsearch buckets a missing url under.

**Push, never scrape.** InfluxDB, PostgreSQL, Loki, OTLP, Telegraf, Graphite
and Elasticsearch are all pushed to. The Prometheus exporter still exists for the
dashboard checker, but production feeds Prometheus through its OTLP receiver.
A gauge pushed once is invisible to an instant query five minutes later, so
the OTLP sink republishes the current state on `repeat`. The
Reducer also publishes `total`, a running distinct-item count per series,
which is what lets a Prometheus dashboard say "per day" through `increase()`.

**Tags Prometheus reserves.** A tag called `job` (or `instance`) collides with
the scrape labels and the OTLP receiver overwrites it with the service name.
Workflow jobs are tagged `job_name`.

**Weekly rows are anchored to the week, not to today.** `gh_commits_week` is
stamped at the Sunday that starts each week. A sweep on Tuesday and one on
Friday have to land on the same row, or every re-read writes a second copy of
the year. `gh_star_day` is anchored the same way, to the `week` the API
returns plus the day's index, never to the clock.

**A panel reads what can go down from its newest row.** Open alerts, whether
a feature is on, stars: each series' newest row inside the range, never
`MAX()` over the range, which counts a fixed alert at its peak and a feature
switched off as on. Archived repositories are the other trap. One set aside by
the filter gets two rows from a sweep and no `gh_repo`: `gh_repo_total` dated
now and `gh_repo_archived` dated at its archive, read with no repository
filter. The SQL stores let the first through with `RFA`, under All and only
when it has a row inside the picker's seven days (`setAsideCollected`), and
Elasticsearch and Graphite hold the same seven days (`esCollectedWindow`,
`grCollected`), so a store's leftovers from a repository nobody collects any
more count nowhere.

**A PostgreSQL table is declared in two steps.** `CREATE TABLE IF NOT EXISTS`
declares `time` and the tags, which are the primary key and the one thing a
CREATE can settle; every field follows as `ADD COLUMN IF NOT EXISTS`, because
a table an earlier release made lacks the new ones. The file sink writes every
ALTER, and psql prints a NOTICE for each column already there. The connecting
sink reads `information_schema` once per table per process and adds only what
is missing, because PostgreSQL takes an ACCESS EXCLUSIVE lock before it checks
IF NOT EXISTS, and one ALTER per field queued every restart behind whatever
Grafana was reading. Do not simplify it back. Its upsert conflicts on the key
the table has, read from the catalog, not on the one this release would
declare. A table dropped or renamed behind it answers 42P01, and the sink then
forgets the tables of that batch, declares them again and sends the batch once
more: without that, every write of the measurement failed until a restart and
took the rest of its batch along (measured on 18.6), which a migration setting
the table aside would meet every time.

## What GitHub will not give a personal account

Verified, so nobody spends an afternoon on it again:

- `stats/code_frequency` and `stats/contributors`: 202 with an empty body,
  forever. `stats/participation` and `stats/punch_card` do work.
- `/settings/billing/{actions,packages,shared-storage}`: 410 Gone.
  `/user/settings/billing/usage`: 404. Only `/users/{login}/settings/billing/usage`
  works, and it returns full RFC 3339 timestamps in a field documented as a date.
- Custom repository properties, classic projects, cost centres, the audit log:
  organisation or enterprise only.
- `/user/installations`: 403 without a GitHub App.
- `workflows/{id}/timing`: 200 with an always-empty `billable`.
- GraphQL reports zero packages while REST lists them. Packages come from REST.
- The stargazer list, since July 2026, to anyone but a repository's admins and
  collaborators: REST answers 404 and GraphQL's `stargazers` answers an empty
  list with `totalCount` 0, while `stargazerCount` still gives the real number
  (measured on cli/cli and octocat/Hello-World with a token holding every
  scope). So `gh_star`, who starred and when to the second, exists only where
  the token has that access, which it always has on the account's own
  repositories and on those of an organisation the account administers. The
  dated star counts come from `stargazers/history` below, which every
  repository gets.
- `stargazers/history` is not limited to the last thirty weeks; thirty weeks
  is its page size, which is both the default and the most `per_page` allows
  (a smaller `per_page` is honoured, a larger one is cut to thirty), so the
  walk sends none and a shorter page is the last one. It answers anyone who can see
  the repository, unauthenticated too, with stars per day grouped by week,
  and pages back to the repository's first week (cli/cli: 13 pages, back to
  2019). It names no stargazers. ghchronicle reads it for every repository as
  `gh_star_day`, and in every store that keeps rows (InfluxDB, PostgreSQL,
  Graphite, Elasticsearch) the per-day star panels, and the InfluxDB and
  PostgreSQL star curve, read that and not `gh_star`. Prometheus cannot: its
  `promRules` entry for `gh_star_day` is skip, so its Stars gained still
  counts `gh_star` through `github_stars_gained_total`, on purpose. `gh_star`
  also still supplies the names in Recent stars. No panel counts from both,
  so a repository with both cannot be counted twice. The days are
  America/Los_Angeles calendar days under a `week` labelled Sunday 00:00 UTC
  (measured against the lists of nineteen repositories, 440 stars: no
  mismatch by Pacific day, while by UTC day 38 of one repository's 127 days
  disagree), and each is stamped at 00:00 UTC of its date. It counts today's stargazers
  by the day each one starred, so an unstar rewrites a past day: page 1, the
  thirty weeks a sweep re-reads, is written with its zero days so the drop is
  applied, and older pages write only days with stars, because zero rows back
  to each repository's creation multiply InfluxDB 3 Core's file count in a
  backfill. A 304 carries no Link header, which is why ghapi's cache keeps
  the Link of the 200 beside its body and replays it, and why this walk reads
  no Link at all and stops on a short or empty page instead.
- The steps of old workflow runs. `runs/{id}/jobs` keeps listing every job
  for as long as GitHub holds the run, but each job's `steps` comes back
  empty for runs created before 12 April 2026 (measured 2026-09-24, about 166
  days back; one snapshot, so whether it is a rolling window is not known).
  A job that completed as success, failure or timed out ran at least one
  step, so the collector writes it with no `steps` field when that list is
  empty, and no `gh_workflow_step` rows. Any other job keeps `steps` as the
  list's length: a skipped job really has 0.
- The traffic window for a repository with no traffic is stale, not empty:
  GitHub keeps returning the last fourteen days that had data.
- A fork's `community/profile`: 404, for all 28 forks of this account and none
  of its 39 other repositories (2026-09-27), with no ETag, so it is charged
  every time. It is remembered as a refusal for a day.
- The REST gateway's 502 or 504 is not the page size: `per_page=1` took as
  long as `per_page=100`. It is rare: of 457,098 GETs in the production proxy
  log from 2026-09-11 to 2026-09-29, GitHub answered 50 with a 502 after 10.4
  to 11.0 seconds, 2 with a 504 and 9 with a 500, 7 of those the artifact
  listing of jmrplens/jmrp.io after 8.3 to 8.5 seconds. The proxy's own 502s,
  answered when this client canceled a request, are not GitHub's and are not
  counted. Of the 21 502s and 504s asked again two seconds later since 2.6.0,
  20 were answered.
- GraphQL aliased batches are bounded by the gateway's ten seconds, not by
  points. A hundred aliases cost 2 points and answered
  `RESOURCE_LIMITS_EXCEEDED` for every alias past the sixty-fifth; fifty
  movement aliases took up to 7.8 s on busy repositories, and fifty archived
  lifetime rows met the 502 twice at 10.7 and 11.1 s. Both batch 25
  (2026-09-26 and 27).
- The Dependabot alert list pages by cursor and declares no last page, so no
  single request says how long it is. Code scanning's pages by number, and the
  `rel="last"` page at `per_page=1` is the total: 1,393 on
  jmrplens/Cloudflare-DNS-Updater, the count a backfill wrote.
- The Actions cache listing orders by last use by default, which every cache
  hit changes, so a walk across pages reads one entry twice and another never.
  `sort=created_at` is honoured, and a hit moves nothing in that order.
- Search serves 1,000 results of any query and no more, without an error. Its
  default order is newest created first, `sort:updated-desc` is honoured, a
  `merged:` range is of UTC days, and the index's `updatedAt` can lag the
  item's own by years (facebook/flow pull requests updated in 2019 sort among
  2017), never ahead of it.
- `repositoryDiscussionComments` takes no `orderBy` and lists oldest first;
  `onlyAnswers: true` exists on github.com and in the schema of GHES 3.17, lists
  the same way and costs a point a page.
- A draft release has `published_at` null.
- `/users/{login}` all but never answers 304: 1 of 48 conditional reads in the
  production proxy log from 2026-09-12 to 2026-09-27. The billing month in
  progress answered 200 to all 56 conditional reads there, and the month
  before it 304 to all 56.

## Working here

```sh
make build vet test-race     # the race suite includes test/e2e; 42 s on the maintainer's machine
make golangci-lint           # config verify, fmt --diff, then run: CI's lint job
make analyze                 # CI's Go, Markdown, shell and generated-artifact checks, each failure reported at once
go run ./cmd/probe owner/name          # try collectors against one repository
GHC_DUMP=<family> go run ./cmd/probe   # print that family's line protocol
go run ./cmd/ghchronicle -config config.yaml -list
go run ./cmd/ghchronicle -config config.yaml -once
go run ./cmd/ghchronicle -config config.yaml -migrate   # what an upgrade left in the stores; changes nothing
```

The targets name their packages (`PKGS` in the Makefile) rather than `./...`,
because the git-ignored `plan/` holds a Go package of its own, and `./...` on
the maintainer's machine is not `./...` in CI. The containerised suite is
behind a tag: `go vet -tags dockere2e ./test/...` is what compiles it.

A Go change is not done until CI would pass on what is committed, and CI only
checks; it regenerates nothing. The first line is its Generated artifacts job,
the other two what the documentation is held to:

```sh
make check-dashboards check-gallery check-layouts check-config-options check-compose check-config-cases
make check-docs mdlint check-doc-links
cd site && pnpm run build && pnpm run lint
```

Each of the first six is regenerated by the same target without `check-`, the
dashboards by `go run ./cmd/gen_dashboards`. `config-cases` is derived from
`config-options`, so regenerate them in that order; a cadence or a setting
moves both. `make docs` rewrites `docs/`. In `site/`, `pnpm run lint` needs the
build before it, and its `stats:check` and `figures:check` hold the prose and
the figures to the code (`pnpm run stats` and `pnpm run figures` regenerate
them). A new measurement or family moves `site/src/data/stats.json` and every
count `site/scripts/gen-stats.mjs` pins in prose in both languages, and a count
with no word in its `NUMBER_WORDS` fails until it has one.

`config.yaml` is git-ignored. `config.example.yaml` is the documented one; keep
them in step when adding a setting. Its `every.families` block shows each
built-in cadence commented out and never sets one, because a copied file that
set them would pin them past the release that changes one;
`internal/config/documented_test.go` refuses a line that sets one. A setting is
expanded only where `config.go` calls `expandEnv` (credentials and addresses)
or `expandPath` (paths, which take a leading `~` as well); a new one without
the call is taken literally, as every path setting was before 2.6.1.

Adding a collector means: a file in `internal/collect` that takes a `Walk`;
for a per-repository family, its name in `perRepoFamilies` and a case in
`(*Runner).repoFamily`, or in `familyBatch` for a part that asks about every
repository in one query (and `batchOnlyFamilies` when that is all of it); for
an account family, an `r.family` call in `accountFamilies`; an entry in
`config.defaultEvery` with its group and a measured `why` (a cadence for a name
missing there is rejected at start-up, which has bitten twice), which the
cadence table of `configuration/cadences.mdx` repeats, the English `why`
verbatim, pinned by `documented_test.go`; a row in the cost table of
`api/cost.mdx`; a rule in `sink.promRules` (leaving it out means the exporter
skips it, which is the safe default); a Loki rendering if it is an event; a
fixture and a test in `internal/collect`; a row on the measurements page; a
panel in `internal/dashboards` with a query set per store; and the tag keys of
each measurement it writes in `internal/migrate/identity.json`, written by
`go test ./internal/migrate -run TestEveryChangeOfIdentityIsRegistered -update`
once the fake answers it.

It also means an end-to-end fixture, a route for it in `test/e2e/fakegh`, and
an entry in that package's `Measurements`. The family list both suites schedule
is derived from `config.Families()`, so a new family is collected the moment it
has a cadence; what does not derive is whether the fake answers anything it
asks for. A family whose routes are missing runs, four-oh-fours, files that as
"this account has none" and writes no point, and the only symptom is a table
the stores never create, which surfaces three screens later as a handful of
dashboard panels rejected for naming something that does not exist.
`TestTheSweepCollectedEveryFamily` and `assertEveryFamilyIsRepresented` are
what turn that into one failure naming the family.

A new measurement in an existing family owes the same `promRules` entry, Loki
rendering, measurements row and line in `identity.json`. A new field on an existing measurement is a
field, never a tag (see above), and one written only under a condition goes in
`conditionalColumns` (`internal/dashboards/conditional_columns_test.go`) if a
SQL panel reads it.

Adding a sink means: a file in `internal/sink` with an httptest-backed test of
its exact wire format, a struct in `config.Sinks` with a validation message
that says what is required, a branch in `buildSinks`, a commented block in
`config.example.yaml`, a page under `site/src/content/docs/sinks/` with its
Spanish twin beside it, and a store in `internal/dashboards/stores.go` if it
can be queried by Grafana. It also means its place in `storesOf`
(`internal/migrate/stores.go`): whether a migration asks the store, follows
the state file's record of it, or has nothing to do there and says why, and
the destination, never a credential, that a record is kept against;
`TestEverySinkIsAStoreThePlannerKnows` fails on a sink left out. A store that
keeps rows needs its way to be brought along too, a `teardown.Clearer` or an
entry in `storeWays` (`cmd/ghchronicle/migrate.go`), and a section on its page
saying what a migration does there.

`docs/` is generated, never hand-edited either. The English pages of the site
are the source, `site/scripts/gen-docs.mjs` writes every file under `docs/` from
them, `make docs` runs it and `make check-docs` fails when what is committed no
longer matches the pages. There used to be two complete sets of documentation
maintained by hand, and they drifted in both directions; the twin the site
serves at each page's own URL and the file under `docs/` are now the same
reduction of the same source. A page added to the site fails `make check-docs`
until it is claimed by an entry of that script's manifest, which is what keeps
`docs/` complete without anyone remembering it exists.

The dashboards are generated, never hand-edited: `internal/dashboards` is
the one source, `cmd/gen_dashboards` writes the five files and refuses to write
them when their layouts have drifted apart, and `cmd/check_dashboards` and
`cmd/check_prometheus` run every panel through Grafana's own query path. A panel
a store cannot answer becomes a text panel with the same title, so every store
gets the same layout.

The figures are generated too, for the same reason and by the same kind of
gate. `site/scripts/gen-figures.mjs` reads `defaultEvery`, `perRepoFamilies`,
`accountFamilies`, `config.Sinks` and the five dashboard files, and writes an
inline SVG per figure, locale and layout under `site/src/data/figures/`, the
markdown each figure reduces to in the twins, and the contents of the mermaid
fence in `how/index.mdx`. `pnpm run figures:check` fails when any of them no
longer matches. Nothing a figure states is typed into it: the one figure this
site drew by hand had aged past thirteen families, said sixteen event
renderings where there are twenty-two, and left one configured destination out
of the list it claimed to be. A figure lives in the site's own `--rb-*` tokens,
so it follows the theme toggle and `check-contrast.mjs` can gate it; a raw hex
inside an SVG is invisible to that gate.

## Style

- Everything in the repository is in English: code, comments, commits, pull
  requests, documentation.
- No em dash or en dash characters anywhere.
- Comments explain why, not what. A comment that restates the code is worse
  than no comment.
- No attribution lines in commits or pull requests.
