# CLAUDE.md

Context for AI agents working in this repository.

## What this is

`ghchronicle` collects every metric GitHub exposes about an account and writes
each observation as a point dated when the thing happened. Go, single static
binary. Its direct dependencies are `gopkg.in/yaml.v3`, the PostgreSQL driver
`github.com/jackc/pgx/v5`, and `golang.org/x/term` (over `golang.org/x/sys`)
so that `-setup` can read a secret without echoing it; `go.mod` is the list.

The reason it exists: GitHub keeps almost nothing. Traffic is a rolling
fourteen days, the event feed is the last three hundred events of the past
thirty days, inbox notifications go after three months unless saved, job logs
after the repository's retention period (ninety days by default), and from
1 October 2026 workflow runs follow that same retention. If it is not
collected while it is there, it is gone.

## Layout

```text
cmd/ghchronicle     the binary: flags, sinks, runner, the one-shot card
cmd/probe           development aid, runs collectors and prints line protocol, writes nothing
internal/ghapi      REST and GraphQL client, ETag cache, per-bucket rate state, typed errors
internal/collect    one file per family of metrics, and Walk (the pagination bound)
internal/sink       Point, line protocol, eleven sink implementations, the
                    Reducer that makes gauges. The published count is ten:
                    stdout in two formats is one destination to configure,
                    which is what site/scripts/gen-stats.mjs counts
internal/render     the SVG card: thirteen layouts in two families, and the accumulator sink
internal/config     YAML with ${VAR} expansion, per-family cadences, backfill bound
internal/run        the sweep scheduler, its state file, the backfill cooldown,
                    and the checkpoint that makes a backfill resumable
internal/grafana    the little Grafana client, and the panel run both the
                    dashboard checker and the containerised suite use
test/e2e            builds the binary and runs it against a fake GitHub
test/e2e/docker     the same binary against the real stores in Docker, and the
                    five dashboards against those; behind the dockere2e tag
cmd/                the development utilities: the dashboard generator, its
                    checkers, the publisher and the brand generator
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

**Prometheus cannot hold that.** Measured, not assumed: against Prometheus 3.14
with `--web.enable-otlp-receiver` and `out_of_order_time_window: 30m`, a sample
dated two days back is rejected with HTTP 400. That is why `sink.Summarize`
exists and why the exporter reduces per-item rows to current values. Do not
"fix" the exporter by giving it timestamps.

**A tag is a series, a field is a value.** Anything unbounded goes in a field.
The Actions runner name looks like a good tag until you notice a hosted runner
is named uniquely per run ("GitHub Actions 1000163135"), which would create a
series per job ever run. It is a field.

**Unavailable is not a failure.** `ghapi.UnavailableError` (403/404: the
feature is switched off) and `ghapi.NotReadyError` (202: GitHub is still
computing) mean "there is nothing here". `collect.isSkippable` recognises both.
A repository with Dependabot switched off must not stop the sweep for the other
forty.

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

**A `url` field is absolute or it is absent.** Around fifty measurements carry
one, and every table selects it as a column called Link. The column itself is
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

**Push, never scrape.** InfluxDB, Loki, OTLP, Telegraf, Graphite and
Elasticsearch are all pushed to. The Prometheus exporter still exists for the
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

## Working here

```sh
go build ./... && go vet ./... && go test -race ./...    # includes test/e2e, ~10 s
golangci-lint run ./...
go run ./cmd/probe owner/name          # try collectors against one repository
GHC_DUMP=<family> go run ./cmd/probe   # print that family's line protocol
go run ./cmd/ghchronicle -config config.yaml -list
go run ./cmd/ghchronicle -config config.yaml -once
```

`config.yaml` is git-ignored. `config.example.yaml` is the documented one; keep
them in step when adding a setting.

Adding a collector means: a file in `internal/collect` that takes a `Walk`, a
case in `run.repoFamily` or a `r.family` call in `run.Once`, an entry in
`config.defaultEvery` (a cadence for a name missing there is rejected at
start-up, which has bitten twice), a rule in `sink.promRules` (leaving it out
means the exporter skips it, which is the safe default), a Loki rendering if
it is an event, a fixture and a test in `internal/collect`, and a panel in
`internal/dashboards` with a query set per store.

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

Adding a sink means: a file in `internal/sink` with an httptest-backed test of
its exact wire format, a struct in `config.Sinks` with a validation message
that says what is required, a branch in `buildSinks`, a commented block in
`config.example.yaml`, a page under `site/src/content/docs/sinks/` with its
Spanish twin beside it, and a store in `internal/dashboards/stores.go` if it
can be queried by Grafana.

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
- No em dash characters anywhere.
- Comments explain why, not what. A comment that restates the code is worse
  than no comment.
- No attribution lines in commits or pull requests.
