# Changelog

What changed in each release, why, and what was left unproven.

Each section says what changed, the reason it changed, the measurement behind
it, and the part nobody verified. Where a claim here was measured, it says on
what. From 2.5.1 on, a release's section is its notes on GitHub word for word,
so every link in one is absolute; the release page adds the pull and
verification commands and a link to the commits.

Versions follow [semantic versioning](https://semver.org/). The dates are the
day the tag was pushed, with one exception: 2.5.2 has a section and no tag. It
was never released on its own, and its changes shipped in 2.6.0.

## 2.6.1 - 2026-09-28

An audit of the documentation against the code of 2.6.0, every page in both
languages read beside what it describes, and a second pass that followed each
fault it found in a dashboard to every panel of its kind. Most of what it found
was prose the release had left behind, and the pages now say what the code
does. Some of it was the code, and the image most of all: a Docker volume
mounted where the example configuration keeps the state belonged to root, so
the collector saved neither its state nor its cache in it and every restart
started from nothing. Beyond that, a path setting took `~` and `${VAR}` as the
characters they are, whether a discussion comment is the accepted answer was
part of its row's identity although a maintainer decides it days later, the
repository list could outlive its hour by a tick, the example configuration
pinned every cadence of its release for whoever copied it, the card and the
dashboards, in their sums and in every table and chart per repository, counted
two owners' repositories of one name as one, several dashboard tables read the largest
value of the range where they meant the newest, or the other way round, or
counted an item once per row a store held of it, or one cell of a grid where
they meant them all, some Elasticsearch panels read every repository whatever
the picker held, ranked by a measure they did not name or failed outright, the
Graphite charts were labelled with the function that consolidated them, and
every one-shot run reported at Info, as news, how it always runs.

- **A Docker state volume belongs to the collector.** Up to 2.6.0 neither
  image had `/var/lib/ghchronicle`, where the example configuration keeps the
  state and every compose stack in `deploy/` mounts its `state` volume.
  Docker fills a new named volume
  from the directory it is mounted over, owner included, so with no directory
  there the volume was created owned by root and the collector, which runs as
  uid 65532, could not write to it; with nothing mounted there, it could not
  make the directory. Whoever ran either image that way without handing the
  volume to uid 65532 themselves kept nothing a sweep learns: every sweep
  warned `state not saved`, and from 2.6.0 `cache file not saved` too, and
  every restart, or every run of a Docker one-shot, which is a new container
  each time, started from nothing. It collected every family whatever its
  cadence, walked the stargazer list, the whole star history and the
  co-authored pull requests again, and had no ETag to be answered 304 with.
  Both images now carry the directory, owned by uid 65532, made in an empty
  stage since distroless has no shell to make it with. Measured with images
  built here from both Dockerfiles, each run once with `-once` against the
  fake GitHub: before, a new named volume stayed `0:0` and both warnings came
  back; after, it was `65532:65532` and held `state.json` and
  `state-cache.bin` with no warning. The collector-only stack, brought up with
  compose and restarted, read its cache back. An empty volume a 2.6.0
  container left to root is handed over the first time a container of 2.6.1 is
  created on it, which `docker compose pull` followed by `docker compose up -d`
  does, as the
  [upgrade page](https://jmrp.io/docs/ghchronicle/install/upgrading/#a-docker-state-volume-needs-no-chown)
  says. A host directory mounted there is left as it is and still needs its
  `chown`, and a named volume mounted anywhere else is still root's, as
  [what has to be writable](https://jmrp.io/docs/ghchronicle/install/docker/#what-has-to-be-writable)
  says. CI's image job and the release workflow now create a new volume on
  every image they build and read its owner from the host, so an image that
  lost the directory fails CI, and a release whose image lost it is published
  without becoming `latest`.
- **A path setting means the path it spells.** `state_file`,
  `sinks.dedupe_file`, `log.file`, `sinks.file.path` and `sinks.sql.path` were
  taken as written, where credentials and addresses were expanded, so
  `state_file: ~/.ghchronicle/state.json` was a directory called `~` under
  wherever the process started: `-backfill-status` printed the checkpoint as
  `~/.ghchronicle/state-progress.json`, the cache recipe on the Actions page
  cached nothing, and the Windows page's `${LOCALAPPDATA}` paths were folders
  named after the reference. The five now take a leading `~` as the home
  directory and `${VAR}` from the environment, and the state file is expanded
  before the ledger, the checkpoint and the cache file are derived from it, so
  they land beside it. `~name` is left as written. Two spellings that would
  put files where nobody looks stop the start, naming the key: a `~` with no
  home directory, and a `${VAR}` that is unset or empty, which expanded to
  nothing made `state_file: ${STATE_DIRECTORY}/state.json` the root's
  `/state.json`. The configuration file's own path is a command-line argument
  and is not expanded, and the header `-setup` writes now says which values
  are. A configuration that used either spelling kept its state in the oddly
  named directory, where 2.6.1 does not look: move the files across before the
  first start if the walks they save are worth keeping, as the
  [upgrade page](https://jmrp.io/docs/ghchronicle/install/upgrading/#a-path-setting-is-expanded)
  says.
- **Whether a comment is the accepted answer is a field.**
  `gh_discussion_comment` carried `is_answer` as a tag, and a maintainer
  accepts an answer days after the comment was written, or takes it back, so
  the value moved after the row's own date, which is what the measurements page
  says makes a field. As a tag, a comment read before it was accepted and again
  after was two rows at the same instant for ever, and in the exporter an item
  of two series, counted in both. 2.6.1 writes no `is_answer`: the `answers`
  field every row already carried, 1 for the accepted answer and 0 for any
  other comment, says the same thing, so no store needs a column, and InfluxDB
  3, which fixed `is_answer` as a tag column, has nothing to refuse. The
  exporter keeps `user`, `own` and the repository as labels, and not the
  answer. A store written before 2.6.1 and after it holds the measurement in
  two shapes: a comment an earlier release read gains one more row, without
  the tag, when a 2.6.1 sweep reads it again.
  The dashboards read both shapes, one row per comment in every store, accepted
  when any of its rows says so, so an answer accepted before the upgrade and
  taken back since still reads accepted. What makes it one shape again is
  dropping the measurement and running a `-backfill`: the table in InfluxDB 3
  and in PostgreSQL, the `github.discussion_comment` paths in Graphite, the
  `ghchronicle-gh_discussion_comment` index in Elasticsearch, as the
  [measurements page](https://jmrp.io/docs/ghchronicle/collectors/measurements/#how-to-read-the-tables)
  says; Prometheus holds nothing across a restart and needs nothing. A tool
  that makes the change is planned for a later release, in
  [#96](https://github.com/jmrplens/ghchronicle/issues/96).
- **The PostgreSQL sink conflicts on the key its table has.** The connecting
  sink built its `ON CONFLICT` from the tags of the points in hand, the key
  this release would declare, and a table an earlier release made keeps the
  key it was made with. Once `is_answer` stopped being written the two
  differed: measured against PostgreSQL 18.6, with a `gh_discussion_comment`
  table made with `is_answer` in its key, a point without the tag was refused
  with "there is no unique or exclusion constraint matching the ON CONFLICT
  specification", and the `gh_discussion` row of the same batch was rolled
  back with it. The sink now reads a table's primary key from the catalog when
  it reads its columns, once per table per process, and conflicts on that; a
  key column the point no longer carries takes the empty string every tag
  column defaults to, so the new rows sit beside the old ones until the table
  is dropped. Measured the same way, both rows were written, and the batch's
  other row with them. The SQL file sink cannot read a catalog: replayed into
  such a table, its `gh_discussion_comment` statements are refused one by one
  and the rest of the file loads.
- **The repository list is rebuilt half a tick before its hour.** Discovery
  kept the list while it was an hour old or less, to the millisecond, and the
  sweep an hour after the listing reads its clock a few milliseconds either
  side of the hour. When it read a hair short, the list outlived that sweep
  and a repository created in between waited a tick more: an hour and a
  quarter at the quarter-hour tick. It is the lateness 2.5.2 took out of when
  a family is due, and discovery takes the same half-tick margin.
- **The example configuration shows the cadences without setting them.**
  `config.example.yaml` set all thirty-four families under `every.families`,
  each to its release's built-in value, so a configuration copied from it
  pinned every cadence of that release. One copied from 2.5.1, compared line by
  line against the tagged example, keeps twelve families slower than 2.6.0
  runs them: `account`, `outbound` and `totals` at `12h`, `achievements` at
  `24h`, `stars`, `billing` and `analyses` at `6h`, `discussions` at `2h`,
  `deployments` at `1h`, and `activity`, `events` and `notifs` at `30m`. Nothing
  warns, since the warning at start-up is for a cadence four times shorter than
  the built-in one. The block is now commented out, every line with its
  built-in value and the reason for it, and a test fails when the example sets
  any cadence. Deleting those lines from a copied configuration is what picks
  up the current cadences; a line kept is a cadence chosen. The reason given
  for `actions`, "the only family with a per-minute rhythm", was wrong beside
  `events`, `notifs`, `activity` and `ratelimit`, which run at the same quarter
  hour, and it now says what the cadence is for.
- **"Security features" and "Artifact storage counted" read the newest row.**
  "Security features" took `MAX()` of `enabled` and `open_alerts` over the
  range, so a feature switched off inside the range read as on and an alert
  fixed inside it still counted at its peak. Every store now reads the newest
  reading of each repository's feature: InfluxDB and PostgreSQL number each
  full name and feature's rows newest first and keep the first, Elasticsearch
  buckets them by their newest timestamp, Graphite reads the last value of each
  series, consolidated by the last value, and Prometheus asked an instant query
  already. "Artifact storage counted" put `MAX()` of each of its four counts
  side by side, so the live size was the largest the range had held and
  Walked came from whichever sweep read furthest, which hid the short Walked a
  walk cut short by a failed page writes on purpose; the SQL stores now take
  all four from each repository's newest row. In the containerised suite,
  against InfluxDB 3 with a newer reading that finds a feature switched off
  and an older artifact total that walked further written beside a sweep, the
  two tables as they were read Enabled 1, Open alerts 1, Walked 9 and a live
  size of 205,800 bytes, where the newest rows say 0, 0, 2 and 204,800; they now
  read the newest rows.
- **Every other table of current state reads its newest reading.** The same
  fault was looked for in every panel of every store. In InfluxDB and
  PostgreSQL, "Account keys" took the least `days_since_use` of the range and
  "Environments" the least `days_since_change`, each of which climbs by one a
  day, so both read the range's first day, a month short over thirty days;
  "Account keys" also took the most `never_used`, which kept a key reading
  never used after its first use; "Repository settings" took each column's
  largest, so a setting switched off or a CODEOWNERS file mended inside the
  range read as it had been; and "Cache against the ceiling" kept a repository
  at a size its evictions had brought down, beside a tile and a table that
  read the newest. Each now keeps its series' newest row.
  Elasticsearch took the largest reading of the range, and said so, in
  seventeen tables where a `top_metrics` could not be used, since it hands a
  boolean back as text and appends nothing for an absent field: the security
  settings, code scanning setup, token permissions, branch protection, ruleset
  bypasses, deploy keys, stale branches, community profile, repository
  settings, policy files, sponsorship tiers, pinned items, profile flags, star
  lists, Dependabot ecosystems and both punch cards. Each series is now
  narrowed to its newest document before it is read, and a tag that can
  change, a status or a path, is one row as in SQL. Measured through Grafana
  13.2.1 and Elasticsearch 9.5.3, a branch protection whose second reading
  switched signatures off and dropped its review count read signatures 1 and
  reviews 2 before, and 0 and empty now. Two Elasticsearch tables failed
  outright there, "frame has different field lengths": "Account keys", whose
  `top_metrics` asked for three fields no key carries all of, and "Achievement
  progress" whenever a badge's page disagrees with its count, since such a
  badge carries no `percent` and no `next_threshold`. Both read the newest
  document the same way now, and the disagreeing badge is a row with empty
  progress. What still takes an extreme of the range does so because the
  extreme is what the panel means: a peak, an event rather than a reading, or
  a value that cannot fall.
- **The punch cards add up every cell of each repository's grid.** A punch
  card is a grid, a point per weekday and hour, and one read stamps every cell
  with the same instant. InfluxDB and PostgreSQL kept the newest row per
  repository and hour, or per repository and weekday, which is one arbitrary
  cell of those tied on that instant, and Elasticsearch the largest cell of
  each repository's hour or weekday. Through Grafana 13.2.1 in the
  containerised suite, with a cell added so that an hour holds two weekdays,
  Monday (3 at 09:00 and 5 at 10:00) read 3 in InfluxDB and PostgreSQL and 5
  in Elasticsearch, and 09:00 (3 on Monday and 6 on Tuesday) read 3 and 6;
  only Graphite, which sums every cell, read 8 and 9. The three now add up
  every cell of each repository's newest grid in the range. Graphite keeps
  each cell as a series of its own, so a cell a rewritten history emptied
  keeps its last count there, as both panels now say.
- **"Every bucket" gives the extremes of the range in every store.** The SQL
  stores put the most any reading in the range had used and the least any had
  left under Most used and Lowest remaining. Graphite and Elasticsearch put
  the newest reading under those names, and Prometheus the value as it stands,
  so a bucket spent to its last request an hour ago read as untouched in three
  of the five dashboards once it had refilled. Graphite now keeps the lowest
  remaining, consolidated by the minimum, Elasticsearch takes the largest
  limit and used and the smallest remaining, and Prometheus asks
  `max_over_time` and `min_over_time` over the range.
- **"Work elsewhere" lists each item once in Elasticsearch.** It listed the
  newest forty documents of `gh_external_contribution`, and an open item is a
  document for every day it is seen open, so one pull request filled a row a
  day and pushed older items off the table. Each item is now a bucket holding
  its newest document.
- **"Discussion answers" counts each comment once.** It counted rows in
  InfluxDB and PostgreSQL and documents in Elasticsearch, so a comment read
  before and after it was accepted was two comments and two accepted answers,
  and in Graphite it was one row for the whole account under a Repository
  column. It now reads one row per comment in every store, as "Answers
  elsewhere" does, and Graphite gives a row per repository.
- **Five Elasticsearch figures follow the repository picker.** The "Open
  alerts" tile of the Security row, in both its Dependabot and its code
  scanning count, the artifact and cache bytes of "Runs in range" and the
  total of "Downloads" read the newest value of every repository in the store,
  whatever the picker held, where their SQL twins and every other
  Elasticsearch panel of their rows read the picked repositories. The query
  the five share now names the picked repositories, as its SQL twin always
  has.
- **Four Elasticsearch panels rank by the measure they name.** "Events by
  type", "Events by repository", "Notifications" and "Topics" keep their top
  values by a metric, and named it by an id no metric of their query carried.
  Grafana then leaves the buckets in Elasticsearch's own order, by document
  count, so each kept the values with the most documents rather than the
  largest sum or the most repositories, and said nothing. Measured through
  Grafana 13.2.1 and Elasticsearch 9.5.3, with one event type of three
  one-event documents and another of one ten-event document, "Events by type"
  cut to one value kept the three events before and keeps the ten now. The
  punch cards and "Contributions by year" ordered by `_term`, the name from
  before Elasticsearch 6.0, which Elasticsearch 9.5.3 answers with "Cannot
  find aggregation named [_term]"; they worked only because Grafana 13.2.1
  rewrites it, and they now say `_key`.
- **The newest-reading sums keep two owners' repositories apart, and "Open the
  longest" lists the longest open.** "Open alerts", the artifact and cache bytes
  of "Runs in range" and "Downloads" add up each repository's newest reading,
  and InfluxDB, PostgreSQL and Elasticsearch took that reading per short name,
  so `alice/dotfiles` and `acme/dotfiles` were one series and whichever was read
  later stood for both. The exporter kept the five measurements behind them by
  `repo` alone, so Prometheus held one series for the two before any query ran;
  Graphite keeps the full name in every path and never merged them. The queries
  now take each series by `full_name`, still narrowed to the picked
  repositories, and the exporter keeps `owner` and `full_name` beside `repo` on
  `gh_dependabot_alert`, `gh_code_scanning_alert`, `gh_artifact_total`,
  `gh_actions_cache` and `gh_release`. "Alerts by severity", "Alerts by
  ecosystem", "Open alerts over time" and the count of releases beside the
  downloads read the same series, so their bars still add up to the tile; in
  Elasticsearch that count is still of distinct tags, as its description says.
  Measured through Grafana 13.2.1 with `alice/dotfiles` at 2 open Dependabot
  alerts and `acme/dotfiles`, read an hour later, at 5, the Dependabot count
  read 5 before and 7 now in InfluxDB 3.11.2 and PostgreSQL 18.6, and
  Elasticsearch 9.5.3 answered one row of 5 before and a row for each
  repository, 5 and 2, now, which the tile adds up; the exporter's reducer and
  the Prometheus tile, evaluated over the same two, read 5 and 7. "Open the
  longest" and "Open issues the longest" kept, in Elasticsearch, the twenty-five
  pull requests or issues of each repository with the most documents, for up to
  fifty repositories, where the SQL stores and Graphite list the twenty-five
  open longest, so a repository with more open items than that showed whichever
  had the most rows in the range. The repository and number buckets now keep
  their values by the largest open time, which puts the twenty-five open longest
  among the rows, and the table sorts them by it and keeps twenty-five. Measured
  the same way, with thirty pull requests in one repository open one to thirty
  days and the younger ones written more often, the query answered the
  twenty-five youngest before and the twenty-five oldest now. In Prometheus,
  where a row of "Open the longest" is a repository and its description now says
  so, the comments beside the open time were not capped, so the table listed
  every repository with an open pull request, those past the twenty-fifth with
  an empty Open for; they now keep to the rows the open time ranks. Both faults
  were reported in [#97](https://github.com/jmrplens/ghchronicle/issues/97).
- **Every panel keeps two owners' repositories of one name apart.** The sums
  above were the first of a class: about fifty panels in each store still
  partitioned, grouped, joined, deduplicated, ranked or bucketed by the short
  `repo` alone. "Open the longest" read `alice/x#5` and `acme/x#5` as one pull
  request, the configuration tables kept the newer reading of the two, "Commits
  behind a red branch", "Release assets" and the workflow tables joined one
  repository's rows to the other's, and every chart and bar per repository drew
  the two as one. InfluxDB and PostgreSQL now key 55 panels by `full_name`,
  Elasticsearch buckets 57 by it with the short name inside, Graphite groups 30
  by the full name's node and names the row by the short one, and Prometheus
  lists `full_name` in every `by` and `on` of 51. A table still shows the short
  name and the picker still filters by it; a chart in the two SQL stores names
  a series in full only when two in the range share its short name, as the
  [panels page](https://jmrp.io/docs/ghchronicle/dashboards/panels/) says. The
  exporter kept 46 measurements by `repo` alone, so Prometheus had merged the
  two before any query ran: every rule that keeps `repo` now keeps `owner` and
  `full_name` beside it, and the issue and discussion comment counts, which
  kept no repository and drew all of them as one row in "Comments left" and
  "Discussion answers", keep all three. Seven Graphite panels named their rows
  after a `sumSeries` that had already added every series into one, which
  graphite-web answers with one row named after the first series: measured
  against `graphiteapp/graphite-statsd:1.1.10-5`, three series grouped that way
  came back as one row holding 3. "Workflows that keep failing", "Steps that
  fail", "Comments left", "Clone amplification", "Checks that are not
  Actions", "Languages starred" and "Transitions over time" now group the
  series themselves. Four Prometheus tables that show the full name, "Comments
  left", "Discussion answers", "Commits by repository" and "Work elsewhere",
  dropped that column before renaming it and now show it. Measured through the
  containerised stack with `alice/twin#5` open ten days and `acme/twin#5` three,
  "Open the longest" answered one row before and two now in InfluxDB 3.11.2,
  PostgreSQL 18.6, Graphite and, through Grafana 13.2.1, Elasticsearch 9.5.3;
  "Views over time" in InfluxDB drew one series, `twin`, of 24 views before and
  `acme/twin` at 15 and `alice/twin` at 9 now.
- **Graphite tables that add their points up are consolidated by sum.** A
  table sends no `maxDataPoints`, so Grafana asks for the panel's width and
  graphite-web averages neighbouring points to fit it before the table adds
  them up: measured against `graphiteapp/graphite-statsd:1.1.10-5` with the
  repository's storage schema, three discussion comments over the last thirty
  days read 1.5 at 500 points and 3 with none. The twenty tables that add
  their points up now ask for the sum.
- **The Graphite charts name their series again.** graphite-web renames every
  series it consolidates, as `consolidateBy(name,"sum")`, and Grafana draws
  that name as it is. Measured against the same image and Grafana 13.2.1,
  "Views over time" drew its two repositories as `consolidateBy(r1,"sum")` and
  `consolidateBy(r2,"sum")`, "Open alerts over time" its severity as
  `consolidateBy(high,"max")`, which its colours by severity then did not
  match, and "Artifact storage over time" its repository wrapped in two
  functions. Every consolidated chart, twenty-four targets, now gives its
  series back the names they came with and keeps the consolidation: the three
  read `r1` and `r2`, `high`, and `r1`.
- **Panel descriptions say what the panels read.** "Oldest open alerts" called
  itself the rows the two counts are made of, where since 2.6.0 the counts
  come from a read of the open alerts alone and the rows from the newest
  hundred in every state. The Overview's archived repositories hung on a range
  Prometheus does not take. "Achievement progress" had Pair Extraordinaire
  walked every hour, where the state file keeps a tally the hourly pass adds
  to and the whole history is walked once a week. "Work elsewhere" said the
  event feed forgets an item in three days, where it keeps its last three
  hundred events of thirty. The artifact tables blamed the five-page cap alone
  for a live size that is a floor, and a failed page does it too. "Every
  family" said its Repositories column was how many repositories a family was
  asked about, where it is the most one sweep of the range asked it about, and
  for `commits`, `issueevents` and `issues` it counts the repositories the
  movement query found nothing new in and the family left unread, so a family
  that read three can stand at thirty. Seventeen Graphite tables that take no
  median said the medians were over what the storage kept, two said a boolean
  is not a metric in Graphite, which keeps it as 1 or 0, and the note on
  failed job output told readers to set `every.joblogs`, which the loader
  refuses, where the key is `every.families.joblogs`.
- **The card counts two owners' repositories of one name as two.** The
  accumulator behind the SVG card keyed `gh_repo` and `gh_repo_language` by the
  short `repo` tag, so a user's `.github` and an organisation's, in a sweep
  that targets both, were one repository: the later reading replaced the
  other's stars, forks and language bytes, and the card's totals lost them,
  where the Overview, which keys by full name, counted both. The card now keys
  both measurements by `full_name`, and by the short name where a point
  carries none. The top repositories still show the short name, so they can
  show one twice, and a tie between two such rows is broken by the full name
  rather than by the order of a map. Fed two owners' `dotfiles`, a later
  reading of one of them, their languages and a repository with no full name,
  the card read 9 stars, 1 fork and 41 bytes of Shell before, and 16, 3 and
  141 after.
- **A one-shot run no longer reports at Info how it always runs.** "no write
  ledger remembers what the stores hold, listing the jobs of the runs the cache
  file remembers again" was printed at every `-once` run after the first,
  which is every cron job, every Action run and every Docker one-shot, and at
  every start of a service whose sinks keep no ledger (Loki, OTLP, the
  exporter, stdout, the file). It is now said at Debug, as
  `not every store keeps a write ledger, ...`, and at Info only when a ledger
  the configuration keeps reads empty, which is what deleting it to fill a
  wiped store looks like, as `the write ledger remembers nothing, ...`. What
  the runner does is unchanged: in none of these cases does it recall the runs
  the cache file remembers. The two `achievements` warnings that said "no
  progress rows today" say "no progress rows this pass", since the family runs
  every hour and the next pass writes the day's rows. A filter on the old
  texts stops matching.
- **The documentation says what 2.6.0 does, in both languages.** The collector
  and measurement pages: `achievements` hourly, the two measurements whose url
  no table links, `gh_account_total` as one GraphQL query and one search, a
  cache walk that loses an entry still writing the total, what
  `gh_collector_family.repos` counts for the three families that ask first
  what moved, and the types of the fields 2.6.0 added. The cost pages: those
  three families and the movement query at a point per twenty-five
  repositories, the half hour `notifs` asks for, the starred list `outbound`
  reads, what makes a card pay cold, the co-authored walk of a first sweep,
  and a backfill that resumes where it stopped. How a sweep and a backfill
  run: one slow family a sweep, when a primed sweep runs every family, and
  what a backfill reaches that a sweep does not. Configuration: which keys are
  expanded and which are not, when the cache file is saved, the seven Debug
  lines, the lines a 2.6 sweep prints routinely, and the families whose cost
  follows activity. Installation: the Docker state directories that still need
  a `chown`, a state file mounted on its own, which is never saved, an Actions
  cache recipe that caches, the slow families arriving over two and a half
  hours after a service's first start, the five quarter-hour families an
  hourly schedule slows, and a new
  [upgrade page](https://jmrp.io/docs/ghchronicle/install/upgrading/) for
  2.5.x to 2.6.x and for 2.6.1. The start and reference pages: the log lines
  2.6 added, a 502 or 504 in `collector failed` that has already been asked
  twice, a dropped PostgreSQL table declared again only after a restart, the
  flat cadence keys the parser refuses, and a `state not saved` that names
  each way the rename into place fails. The sink pages: what both PostgreSQL
  sinks do to a table an earlier release made, Loki's one measured
  figure for the old release lines, its line formats and its lookback, and the
  joins and second windows Graphite and Elasticsearch cannot ask for. The
  dashboard and card pages: every panel paragraph against the generated files,
  the measurements each section reads derived from them, the archived
  repositories the Overview counts and the card does not, and how the card
  counts a repository. The landing page's calendar runs every hour. README,
  CLAUDE.md, CONTRIBUTING, SECURITY, RELEASING, the Action's notes, which
  promised a card without a token, and the pull request and bug report
  templates say what the code and CI do, and the stacks' README says the
  collector-only stack writes to its own log, not to a file. The code comments
  that still called `deployments` hourly and `achievements` daily, or had the
  event feed forget work in three days, say what 2.6.0 does.
  `make analyze` runs `check-compose` as CI does, and the site's stats check
  every row of the dashboards README's panel counts, four of which said 152
  where all five files hold 154.

Each change in behaviour carries a test shown to fail against the code before
it, run there again for these notes: the five path settings with `~` and with
`${VAR}`, a `~` with no home and an unset variable, each refused; the list
rebuilt a millisecond short of the hour; the refill said at Debug where no
ledger is kept; the example setting any cadence; a comment accepted between
two reads, in both walks that write it; the upsert on a table made with
`is_answer` in its key; the two comment tables read per comment in every
store; the newest reading of "Security features" and "Artifact storage
counted"; the extremes of "Every bucket"; "Work elsewhere" in Elasticsearch;
the twenty Graphite tables that add their points up; every description
above; the two `achievements` warnings; both Dockerfiles shipping the state
directory owned by the user they run as; the four tables of the SQL stores
read from their newest row and the seventeen of Elasticsearch from their newest
document; an extreme of the range read by any panel without a listed reason; a
`top_metrics` over a field the collectors do not always write; the punch cards
in every store; the five Elasticsearch figures without the picker their SQL
twins name; an Elasticsearch ranking by an id its query lacks; a consolidated
Graphite target under the name `consolidateBy` gives it; the card's totals,
the newest-reading sums in every store and the exporter's five gauges over two
owners' repositories of one name; the two open-longest tables in every store;
and every query of every store, and every exporter rule that names a
repository, over two owners' repositories of one name. Where such a test calls something the old code lacks, it was run there
with that call stubbed. The binary
against the fake GitHub holds that a one-shot run says its refill at
Debug and never at Info, and that no comment carries `is_answer`. The
containerised suite holds the two newest-reading tables against InfluxDB 3, as
above, and every bar of both punch cards against what the sweep's own points
add up to, in the four stores that draw them; against the dashboards before
it, that test failed in InfluxDB, PostgreSQL and Elasticsearch. CI's image job
and the release workflow read the owner of a new state volume on every image
they build, a check that fails on the images of 2.6.0.

Not verified, and worth saying plainly:

- None of it has run in production, which runs 2.6.0. The upgrade that puts
  the second shape of `gh_discussion_comment` beside the first is the first
  reading of it in a store.
- The dashboards over two shapes were checked on the statements and queries
  each store is sent, read offline. No suite writes a row of the old shape
  into a real store, so the Graphite join across the two depths of path and
  the Elasticsearch fold per comment have not read a store holding both. The
  PostgreSQL conflict on the old key was measured by hand against PostgreSQL
  18.6, and is held by a test with the catalog stubbed.
- The newest-reading tables were checked for their values against InfluxDB 3,
  and in Elasticsearch on one branch protection and two achievement badges
  read by hand; the punch cards are read bar by bar in the four stores that
  draw them. Beyond those, in the other four stores the containerised suite
  sends every panel through Grafana and holds that it is accepted and answers,
  not what it answers.
- The Graphite sum was measured on one table, the discussion comments, and
  the other nineteen follow from the same wrapper. The Graphite names were
  read on three of the twenty-four consolidated chart targets, and the ranking
  of the Elasticsearch panels on "Events by type" alone; the rest follow from
  the same change.
- The five Elasticsearch figures that now follow the picker are held by a test
  of their queries; no store was read with a repository picked.
- The state directory in the image was measured with images built here for
  linux/amd64, with Docker. The arm64 images, Podman and Docker Desktop were
  not run, and the check that CI and the release workflow now make was run by
  hand, against the published 2.6.0 image and one built here from the root
  Dockerfile, not yet on a runner.
- The card's count by full name is held by the accumulator's test. No account
  with two owners' repositories of one name was drawn.
- The keying by full name of the other panels was measured on "Open the
  longest" in four stores and "Views over time" in InfluxDB. The rest follow
  from the same helpers and are held by a test that reads every query of every
  store and the exporter's rules; the containerised suite sends every panel
  through Grafana and holds that it is accepted and answers. No Prometheus was
  read with two owners' repositories of one name.
- The newest-reading sums by full name were measured on the Dependabot count, in
  the three stores above; the other figures follow from the same two helpers and
  the same exporter rule, and are held by a test of their queries, which
  evaluates the Prometheus and Graphite ones over the two repositories as each
  store holds them. The Elasticsearch open-longest order was measured on one
  repository's pull requests. The cut to twenty-five across repositories, and
  the sum of the Elasticsearch tile's rows, are Grafana transformations and
  reductions, which run in the browser and which no measurement here reached.
- The path expansion was run on Linux. The `${LOCALAPPDATA}` paths of the
  Windows page and the cache recipe of the Actions page follow from it and
  were not run on Windows or on a runner.
- The discovery margin is held by the loop's tests, not seen in production.
- Left out rather than unproven: a tool that drops and fills
  `gh_discussion_comment` again, which is
  [#96](https://github.com/jmrplens/ghchronicle/issues/96), and a warning for
  a cadence slower than the built-in one: a slower cadence is a configuration's
  to choose, and the example no longer chooses one for whoever copies it.

## 2.6.0 - 2026-09-27

Built on 2.5.2, which made every family run at the cadence it states, this
release runs ten of them at the cadences their measured cost allows and adds
what a reader of the store asked for and could not get: how large a
contribution elsewhere was and whether its repository is private, the moment
each release was published, an answer accepted after its comment left the
newest hundred, and an open security alert however old it is. A release now
reaches Loki once, at its publication, where every release was sent again
every hour, and a PostgreSQL database an earlier release made takes the new
fields, which it would have refused. And it stops paying for what an audit of
the production service's request log, 30.9 hours of 2.5.1, found bought
nothing: a fork's community profile and a second read of each named fork, 14
per cent of the charged `core` requests; a query for every repository where
nothing had moved, 43 per cent of the GraphQL points; the whole merged pull
request history read every day for one count, 31 per cent of the bytes; every
ETag the process held, forgotten at each restart; and the daily families run
in one sweep for ever, which made the first hour of each UTC day eight times
the median. The same audit found two things the store said wrong, fixed here
too: the entries of one Actions cache on one ref overwrote each other, and a
502 from a slow listing cost a repository its artifact storage row.

2.5.2 was never released on its own. Its changes, the Loki sentences, the
outbound searches past a hundred, the archived repositories in the totals, the
threads commented elsewhere and a scheduler that ran families a tick late,
ship in this release: they are in the
[2.5.2 section of the changelog](https://github.com/jmrplens/ghchronicle/blob/v2.6.0/CHANGELOG.md#252---2026-09-26).

- **Outbound rows say how large the change was and whether the repository
  is private.** `gh_external_contribution` said where a contribution went and
  what became of it, and nothing about the change or the repository, although
  the search it comes from returns both. A page that renders this work for
  others had to ask the API again for every pull request, and had no way to
  leave out an organisation's private repositories: the searches run with the
  account's token, and those come back beside the public ones. Each row now
  carries `private`, from the repository's `isPrivate`, and a pull request
  carries `additions`, `deletions` and `changed_files`, the names
  `gh_pull_request` uses; an issue carries none of the three.
  `gh_issue_comment` and `gh_discussion_comment` carry `private` as well, so
  every outbound measurement can leave private work out. They are fields and not tags, so no row already stored gains a second
  identity, and none of them moves once an item is closed, which is what lets
  them sit on a row dated when it closed. Every open item has them from the
  first sweep after the upgrade; an item closed before it has them once a
  backfill reads it again.
  ([#79](https://github.com/jmrplens/ghchronicle/issues/79))
- **The upstream repository's stars are a row of their own,
  `gh_upstream_repo`.** They were asked for on each contribution, as
  `repo_stars`, and are not there because that row is dated when the item
  closed: a count that moves nearly every day would rewrite a row of the past
  each time it moved, and in InfluxDB 3 an old partition with it. The new
  measurement is one row per repository the searches reached in the pass,
  stamped at the sweep, tagged `full_name`, `owner` and `repo`, with `stars`,
  `forks`, `private`, the primary `language` when GitHub detects one, and the
  repository's page. The Prometheus exporter keeps its newest reading per
  repository, as it does for `gh_pinned_item`. The "Work elsewhere" table
  gains a Stars column, the newest of these rows inside the range, joined onto
  each item in InfluxDB and PostgreSQL and merged by full name in Prometheus;
  Graphite and Elasticsearch cannot join two measurements in one table and say
  so in the panel. Until the first `outbound` pass of this release writes the
  measurement, which is within the hour, InfluxDB and PostgreSQL refuse that
  table rather than leave the column empty, as the
  [troubleshooting page](https://jmrp.io/docs/ghchronicle/reference/troubleshooting/)
  now says. A repository whose items are all closed, and further back than the
  page a sweep reads, keeps the row of the last sweep that read one of them, or
  of the last backfill.
- **Every release is dated at its publication, in `gh_release_published`.**
  `gh_release` is stamped at the sweep, because its downloads move, and
  carried the release's age as whole days counted back from it. The
  publication itself was stored nowhere, and the floored age cannot give it
  back: the date it reconstructs is a day late for any release published later
  in the day than the sweep ran, so jmrplens/libgen-mcp v2.0.1, published
  2026-09-23T18:28Z, read as the 24th. The new measurement is one row per
  release at its `published_at`, tagged `tag`, with `published` at 1,
  `prerelease` and the release's page, so counting releases per month is a sum
  and the latest stable release is the newest row with `prerelease` false. It
  costs no request, since the release list already carries the date.
  `prerelease` is a field here where it is a tag on `gh_release`: a
  pre-release is promoted by unticking the box on the published release, and
  as a tag the promotion would have been a second row at the same instant,
  which the sum counts twice. The exporter skips the measurement as history,
  as it does `gh_package_version`, since `gh_release` already serves a series
  per release. The first sweep after the upgrade dates the releases on the
  page it reads, and a
  [backfill](https://jmrp.io/docs/ghchronicle/how/backfill/) dates the rest.
  ([#80](https://github.com/jmrplens/ghchronicle/issues/80))
- **A draft carries no `age_days`.** GitHub sends a draft with `published_at`
  null, and the age measured from Go's zero time saturated at 106751 days on
  every draft row, beside a real maximum of 271 for the 159 published releases
  of the account measured, which skewed anything that averaged or ordered the
  field without leaving drafts out first. A draft has not been published, so
  it writes no `gh_release_published` row either. No panel read the field.
- **Loki gets each release once, at its publication, instead of every
  release every hour.** The `release` stream was rendered from `gh_release`,
  stamped at the sweep, so every `repo` pass pushed every release again as
  `release TAG of OWNER/REPO, N downloads`. Measured on 2.5.1 in production,
  over 30.9 hours and 27 `repo` passes, that was 4,313 of the 10,467 lines the
  sink sent, 41 per cent and about 3,355 a day: 160 releases 27 times each, in
  428 distinct texts, for the 2 releases actually published in those hours.
  The line now comes from `gh_release_published`, as
  `published release TAG of OWNER/REPO`, or `published prerelease`, at the
  second the release was published, which on that window is 2 lines. The
  stream keeps `kind="release"`, so a query written against it still reads
  it, and the download counts stay in the metrics store, where a gauge
  belongs. Lines already in Loki keep what they said.
- **The release stream looks back a `repo` cadence.** Dated at the
  publication, a release is first seen by the `repo` pass after it, so one
  published just after a pass read its repository is a whole cadence old when
  the next pass writes, plus however late that pass runs, and `max_age`, an
  hour by default like the cadence, would have left it out for good. That
  stream alone now looks back the `repo` cadence plus `max_age`: two hours at
  the defaults, seven with `repo: 6h`, and never more than six days, a day
  short of the week Loki's `reject_old_samples_max_age` allows. Loki refuses
  an old line only for being behind a newer one in its stream, which the sink
  still checks against what it sent before. A release published in the hour
  before a pass is sent by that pass and again by the next, the same line at
  the same instant, which Loki keeps once. Under `-once` the cadence that
  matters is the schedule that runs the binary, so `every.families.repo`
  should say it. The sink also stopped judging a push against the push's own
  newest entry: it sends each stream oldest first, which Loki takes whole, and
  that rule would have dropped the older of two releases one pass carried.
- **Loki's out-of-order window is an hour, and the pages say so.** The Loki
  and troubleshooting pages, the configuration's comment and
  `config.example.yaml` gave it as about two hours, and told whoever raised
  `max_age` to raise `out_of_order_time_window` with it, which is a Prometheus
  setting. The window is half of the ingester's `max_chunk_age`, an hour by
  default, and that is the setting they now name. `max_age` keeps its default
  of an hour, which is that window.
- **An answer accepted late is read.** A sweep reads the newest hundred
  discussion comments the account wrote, and a comment's `is_answer` was
  refreshed only while it was inside that window, which is measured in
  comments and not in time: an answer accepted after its comment had left it
  was written as an answer only by a backfill. On 2026-09-26 the account had
  108 comments and 15 accepted answers, one accepted fourteen days after it
  was written, and the newest hundred no longer held one of the 15. The
  `outbound` family now also reads
  `viewer.repositoryDiscussionComments(onlyAnswers: true)`, which lists the
  comments that are their discussion's accepted answer whatever their age,
  walked back from the newest end: five pages on a sweep and every page in a
  backfill, a GraphQL point a page. An answer both reads return is written
  once, because the write ledger does not dedupe within a batch and the
  exporter would count it, and weigh its upvotes, twice. The comment walks
  also hand up the rows they read before an error now, as the searches
  already did, so a failed answer walk does not cost the newest hundred.
  ([#81](https://github.com/jmrplens/ghchronicle/issues/81))
- **An open security alert is counted however old it is.** A sweep reads the
  newest page of each alert list, a hundred alerts in every state, and took
  `open_alerts` on `gh_security_feature` and the per-severity `open` of
  `gh_dependabot_alert` and `gh_code_scanning_alert` from that page. An alert
  still open behind a hundred newer ones that were fixed was not counted, so
  such a repository read 0 open in the Security panels of all five stores,
  the number a reader is most likely to repeat. When a walk stops with alerts
  still behind it, a full page on a sweep or the date bound of a backfill, the
  list is now read again with `state=open`, to its end, Dependabot by cursor
  and code scanning by page, and the counts come from that. The item rows are
  unchanged. The extra requests are conditional like every other page, and a
  refusal of them is taken as being about the walk rather than the
  repository: the counts fall back to the page, and the feature is not
  recorded as switched off nor the refusal remembered for a day. A list
  shorter than a page, and a backfill that walked the whole list, ask nothing
  more, so on the account measured two lists pay for it, code scanning on
  jmrplens/Cloudflare-DNS-Updater and Dependabot on jmrplens/jmrp.io.
  ([#82](https://github.com/jmrplens/ghchronicle/issues/82))
- **Code scanning's `alerts` is the repository's total.** When the page comes
  back full, it is read from the last page GitHub declares for a page of one
  alert: 1,393 on jmrplens/Cloudflare-DNS-Updater, where a sweep wrote 100.
  A 304 carries no Link header, and the client replays the one its 200 came
  with. Dependabot's list pages by cursor and declares no last page, so there
  `alerts` stays the rows read, and the measurements page now says that 100
  on a sweep means a hundred or more; a backfill that walks the whole list
  writes the total.
- **PostgreSQL takes a field an earlier release never wrote.** The SQL file
  sink and the PostgreSQL sink declared a table the first time a process, or a
  rotated file, met its measurement, as a `CREATE TABLE IF NOT EXISTS` with
  every column of that batch, and that statement does nothing at all to a
  table an earlier release made. A field the earlier release did not write
  reached the `INSERT` with no column, and PostgreSQL refused the statement
  and the batch around it, on every write after it, since the sink took the
  table as declared. This release adds fields to three measurements that have
  tables already. The `CREATE TABLE` now declares the time and the tags, which
  are the key, and each field follows as
  `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`, which means the same thing to a
  new table and to one any earlier release made. The PostgreSQL sink reads
  the catalog the first time its process meets a table and adds only the
  columns it lacks, because PostgreSQL takes an `ALTER TABLE`'s exclusive
  lock before it checks `IF NOT EXISTS`, so one per field on every restart
  would wait behind each Grafana query reading the table and hold up every
  query after it. A statement the server refuses is sent again on the next
  write rather than taken as done. The file says every field, since a
  replayed file cannot ask, and psql prints a notice for each column a table
  already has.
- **The entries of one Actions cache on one ref are one row, and it holds
  them all.** `gh_actions_cache_entry` is stamped at the start of the UTC day
  and tagged with the repository, the ref and `cache`, the key cut before its
  content hash, and every CodeQL overlay cache on a branch cuts to the same
  tag, as several other caches do. The entries of one cache on one ref were so
  many writes of one row, and the store kept whichever came last: on
  2026-09-26 the fifteen CodeQL caches on main of jmrplens/jmrplens, 57.9 MB
  between them, were stored as one of 3.8 MB, and on 2026-09-27 the account's
  737 entries came to 380 rows. The write ledger holds one value a row, so it
  sent the rest of each group again on every pass: an `actions` pass with
  nothing new wrote 345 rows, about 270 of them these. The collector now sums
  them into the row: `caches` is how many entries there are, `size_bytes`
  their total, `days_since_use` and `key` those of the most recently used, and
  `age_days` that of the oldest. The fields are the ones the measurement had,
  so no store needs a column, and the rows of the days before the upgrade keep
  the one entry the store kept. "Cache entries by key" reads the sums in all
  five stores: the SQL stores add up `caches` where they counted rows,
  Prometheus gains an Entries column, Graphite adds each ref's last value, and
  Elasticsearch takes each ref's newest document where it summed every
  document in the range.
  ([#91](https://github.com/jmrplens/ghchronicle/issues/91))
- **The cache listing is read past its first hundred, in an order a cache hit
  does not change.** It stopped at the hundred entries used most recently, and
  on 2026-09-27 jmrplens/ghchronicle listed 118 and jmrplens/mikroscope 232.
  It is now read a hundred a page, up to ten pages, newest created first.
  GitHub's default order is by last use, which every cache hit changes: an
  entry restored between two pages moved to the front, the one that ended a
  page was read twice and one further down never was. By creation a hit moves
  nothing, and a new entry lands on a page already read. A deletion between
  two pages still loses one, so a walk is taken as whole only when it read as
  many distinct entries as the first page's `total_count` said; otherwise the
  pass writes the totals and no entry row, reports no failure, and the day's
  next pass writes them. A page past the first that fails leaves the rows out
  rather than write part of a cache over the whole of it. The extra pages are
  conditional, and the account's listings answered 304 to 3,059 of 3,256
  requests from 2026-09-25 12:58Z to 2026-09-27 01:39Z.
- **A REST request the gateway gives up on is asked once more.** GitHub's
  artifact listing turned slow on the two repositories of the account with
  the longest artifact history, and a request past the gateway's ten seconds
  comes back 502: in the audit's 27 `artifacts` passes, jmrplens/jmrp.io's
  walk ended in one 12 times and jmrplens/phonometry's 3. Over the whole log,
  2026-09-11 to 2026-09-27, 36 of 397,455 REST GETs answered 502 or 504, 25
  of them after 10.4 to 10.8 seconds, and a page of one took as long as a
  page of a hundred. The client now asks a 502 or a 504 once more, two
  seconds later, before any collector sees it, so every REST family has it.
  The retry is charged like the request it repeats, so it passes the brake
  again; it carries the validator the first attempt carried; and a
  cancellation during the pause sends nothing more. A 500 and a 503 are not
  asked again, and neither is GraphQL, whose gateway error means a query too
  large, which the collectors already ask again with a smaller page.
  ([#89](https://github.com/jmrplens/ghchronicle/issues/89))
- **An artifact walk cut short still writes the repository's total.**
  `gh_artifact_total` was appended after the walk, so a failed page cost the
  repository its storage row for the pass, as it did jmrplens/jmrp.io in 12
  passes of 27. It is now written from the pages walked when a later one
  fails, with `walked` below `count` marking the live figures as a floor, as
  it already did for a walk the five-page cap stopped. A failed first page
  still writes nothing, because a row of zeros would read as every artifact
  gone.
- **Ten families run more often.** `events`, `notifs` and `activity` every
  quarter of an hour instead of every half, `deployments` every half hour
  instead of every hour, `stars`, `billing` and `analyses` every hour instead
  of every six, `account` and `totals` every hour instead of every twelve, and
  `discussions` every hour instead of every two. Their extra passes are
  answered almost entirely by 304s, which are free, or cost a handful of
  GraphQL points, and several of them change far more often than they were
  read: the event feed had moved in 43 of 46 half hours, and the billing
  report's month in progress answered 200 to all 46 six-hourly conditional
  reads since 2026-09-13. Costed from the request log of the production
  process from 2026-09-25 12:58Z to 2026-09-26 19:50Z, over 37 repositories
  and with each family's first pass left out so that every pass counted had a
  warm ETag cache, they add about 250 billable `core` requests and 500 GraphQL
  points a day, 0.2 and 0.4 per cent of what the two hourly budgets of 5,000
  allow in a day, and 22 search requests. The points are mostly `deployments`,
  eight a pass, and `totals`, six; the `core` requests mostly `activity`,
  `events` and `analyses`, which are charged only for what moved. With the
  half tick of 2.5.2 these are the cadences the families really run at, and
  the shortest is still a quarter of an hour, so the tick does not move. A
  configuration that names one of them under `every.families` keeps what it
  names. The Overview's archived stars, which a range shorter than the
  `totals` cadence can leave out, now need a range of an hour rather than
  twelve.
  ([#93](https://github.com/jmrplens/ghchronicle/issues/93))
- **The reasons the start-up warning quotes are the ones measured.** Each
  family's `why`, which the warning about a cadence set far shorter than the
  built-in one quotes, now says what a pass costs and how fast the data moves.
  Five were not so, and are corrected whether or not their cadence changed:
  `commits` said a request per commit, where it is a GraphQL point per
  repository, and since the change below only per repository whose default
  branch moved, 38 of 999 answers measured, with one more per twenty-five
  repositories to ask which; `issues` said a request per item, where it is one
  or two points per repository whose items moved, and up to about nine once a
  day for a whole page; `repo` said one request, where it is three REST
  requests per repository and two points per ten; `rulesets` said its
  requests answer 304 until somebody edits one, which held only in a process
  that lived a day, none of the 43 after a restart being conditional, and now
  holds across a restart, since the ETag cache is kept beside the state file;
  and `billing` said a few times a day at most, where it had changed at every
  read. `issueevents` and `achievements` say what they cost after the changes
  below. No family ships at two hours any more, and that rung stays on the
  ladder the warning's factor of four is worked out from, since it is where
  one step down from six hours lands.
- **The families of six hours or more take turns.** A family that runs is
  marked with its sweep's instant, so families that once ran in one sweep came
  due together at every cadence for ever, and the state file carried them
  across restarts. In production the eight daily families ran in the first
  sweep of each UTC day and the twelve-hour ones together 45 minutes later: on
  2026-09-26 that first sweep took 294 charged `core` requests and 24.3 MB,
  where the median sweep took 17.5 requests, and its hour was eight times the
  median hour in `core` requests. The service now starts at most one family
  of six hours or more in a sweep and leaves the others due for the next
  tick, the one overdue longest first, counted from the later of its last run
  and the last time it was let start, so a family whose every pass fails does
  not take every turn. Once two have run in different sweeps they stay apart,
  so the wait is paid once, and at worst it is a tick for each other slow
  family: 2h30m for the last of the eleven at the built-in cadences, 3h with
  `deps` and `history` on at a day. A configuration with more slow
  families than one a tick can start within their cadences starts the fewest
  that fit. `-once` still runs every family that is due, having no next tick
  to leave one for, and a backfill, a card and the sweep that primes the
  Prometheus exporter run every family; the primed sweep now marks as run
  only those that were due, where marking them all put the group back
  together at every restart. The log names who starts and who waits, under
  `slow families due together take turns`. Every family keeps its cadence and
  its cost; only the sweep it lands in moves.
  ([#87](https://github.com/jmrplens/ghchronicle/issues/87))
- **Commits, issues and issue events ask first what moved.** The three read
  only what moved since a window of their own, one GraphQL query per
  repository on every pass, and GitHub charges a query for the page it asks
  for, not for what comes back. In the audit's 30.9 hours, 961 of the 999
  `commits` answers held no commit, and 468 of the 968 incremental `issues`
  answers and 486 of the 1,005 `issueevents` answers held no item: 2,077
  points for nothing, 43 per cent of the process's GraphQL spend. A sweep that
  runs any of the three now first asks every repository, twenty-five to an
  aliased query at a point each, when the head of its default branch was
  committed and when its newest issue and pull request were updated, and each
  family leaves unread a repository where nothing moved since the start of
  its own window. For `commits` that is exactly when the read comes back
  empty, since `history(since:)` filters on the committed date. The query is
  asked once a sweep, by the first of the three to reach a repository. A
  repository it did not answer for, a failed query, the daily whole page of
  `issues` and every backfill are read as before, and the log says how many
  repositories each family left unread. On this account it costs two points
  a sweep and would have saved 2,023 points in those hours, about 66 an hour,
  and more than that, since a repository whose items all predate the window
  answered a page anyway and is skipped too. Twenty-five is the batch because
  fifty took up to 7.8 of the gateway's ten seconds on busy repositories.
  ([#92](https://github.com/jmrplens/ghchronicle/issues/92))
- **The co-authored pull request count is kept, and each day adds to it.**
  `achievements` counted Pair Extraordinaire by walking every public merged
  pull request of the account, with the message of every commit in it, on
  every daily pass, though a merged pull request never changes: 34 or 35
  queries and 18 to 24 MB a day for one number, on an account with 2,315 such
  pull requests, and 22.0 of the 70.9 MB the service transferred in the
  audit's 30.9 hours. The state file now keeps the count, the last UTC day it
  covers, the version of the rule it was counted by and the day the whole
  history was last walked, and each pass walks only the days since and adds
  what it finds. Today is counted but not settled, since pull requests are
  still being merged into it, and the next pass walks it again; a `merged:`
  range is of UTC days, which is what lets a pass settle a day by the clock.
  The whole history is walked again once a week, when the rule changes, on a
  backfill and when the tally is missing or dated today or later, because the
  count can go down: a repository made private or deleted takes its pull
  requests out of `is:public`. A walk that fails leaves the tally as it was.
  Run live, a pass over the last one to four days read 54 KB to 874 KB in two
  to three seconds, where the whole walk is 35 queries, 23.7 MB and 94
  seconds. The first pass after the upgrade finds no tally and walks the whole
  history once.
  ([#90](https://github.com/jmrplens/ghchronicle/issues/90))
- **A fork's community profile is asked once a day, and a named repository is
  not read twice.** GitHub serves no community profile for a fork, and its 404
  carries no ETag, so it was charged in full on every `repo` pass: 404 of them
  in the audit's 30.9 hours for the fifteen forks the account names in
  `targets.repos`. Discovery also read each named repository again for the
  four flags the owned listing had just returned, and a fork's body embeds
  its parent's counters, which move, so 212 of those 405 reads were charged.
  Together that was 616 of the 4,426 charged `core` requests, 14 per cent.
  The community profile now goes through the refusals the runner remembers
  for the `repo` family, as security, analyses, inventory and deps already
  do, so a repository GitHub refuses it for, fork or not, is asked once a
  day, 15 a day here where the hourly cadence made it 360, and one it starts
  answering for is noticed within the day; a backfill still asks. Discovery
  takes a named repository from the listing that returned it, matched without
  regard to case and keeping the configuration's spelling, and reads only a
  name no listing returned, such as another owner's repository.
  ([#86](https://github.com/jmrplens/ghchronicle/issues/86))
- **A restart keeps what the process learned about GitHub.** The ETag cache,
  the workflow runs whose jobs were written, the refusals remembered for a
  day and the page sizes `totals` sets for the pull request query lived in
  the process, so every restart paid for all of them again, and a daily
  family in a process that did not live a day never got a 304: the first 38
  minutes after the restart of 2026-09-26 spent 1,092 charged `core` requests
  on passes that cost about 66 warm. The runner now keeps all four in a file
  beside the state file, `<name>-cache.bin`, read at the first sweep, written
  at most every five minutes and once more on the way out, with mode 600 like
  the state file, since it holds what GitHub answered about private
  repositories too. It has no setting and follows the state file, so the
  Docker volume, the systemd `ReadWritePaths` or the Action's cached
  directory that holds the state file holds it as well; the
  [configuration page](https://jmrp.io/docs/ghchronicle/configuration/#the-cache-beside-it)
  says what is in it. An answer is kept under its URL and a digest of what
  its collector decodes, so an upgrade that adds a field to one asks those
  URLs again rather than answer a 304 with a body stored without it. The file
  keeps what was asked for within twice the longest cadence, never less than
  a day, leaves out any answer over a megabyte and stops at 64 MB, least
  recently asked for first. A file cut short, damaged or of another format is
  set aside with a warning, which costs what deleting it costs: one pass of
  each family at a cold cache's price, and nothing else. A card-only run and
  a backfill read it and do not write it, the backfill because its deep pages
  would crowd the sweeps' answers out.
  ([#88](https://github.com/jmrplens/ghchronicle/issues/88))
- **A remembered run is recalled only where every store holds its jobs.** A
  run in the cache file is a claim that its jobs were written, so a start
  recalls none where nothing says what the stores hold: with no write ledger,
  as when the ledger is deleted to fill a wiped store again, with
  `dedupe_file: off` or a store's own `dedupe: false`, or in a run that ends
  with its sweep; and where a destination was added since the file was
  written. A pass a store refused forgets its runs as well. Their jobs are
  then listed and offered again with every other point.
- **A first sweep with no page sizes runs `totals` first.** The pull request
  query is sized per repository from the counts `totals` reads, and a restart
  was meant to run `totals` first; only the sweep that primes the Prometheus
  exporter did. On 2026-09-26 the daily whole page of `issues` cost 328 points
  where it had cost 139 on each day before, a page of fifty for 37 of its 38
  requests. A first sweep that finds no page sizes, in the process or in the
  cache file, now runs `totals` before the pull requests, whatever its
  cadence says.

These were found by checking this release, deployed, against GitHub panel by
panel, and are fixed before the tag:

- **The Overview counts an archived repository only while the collector
  reads it.** With All selected, the stars and forks let every archived
  `gh_repo_total` row in the range through, however old, where a live
  repository counts only while the picker lists it, which is while a sweep has
  written its `gh_repo` in the last seven days. Measured on 2026-09-27 against
  the production store over the default thirty days:
  jmrplens/portainer-mcp-enhanced, an archived fork this configuration does
  not collect, with `include_forks` off, had one row, from a backfill on
  2026-09-18 under an earlier configuration, and added 8 stars and 3 forks,
  so the tile read 393 and 106 where GitHub gives 385 and 103 for what the
  sweeps collect: 305 and 79 for the 37 live repositories and 80 and 24 for
  the 17 archived ones set aside. An archived row now counts only for a
  repository with a `gh_repo_total` row in those same seven days, which every
  `totals` sweep writes for one set aside, an hour apart by default. InfluxDB
  and PostgreSQL ask that of the repository, as the picker does, so a range
  that ended a month ago still counts one the collector reads today; the query
  now gives 385 and 103 against production. Elasticsearch and Graphite cannot
  ask one window about another, so they keep the archived rows of the last
  seven days, and a range that ended more than a week ago leaves the archived
  repositories out there, which their panels say. Prometheus needs nothing: an
  instant query sees what the running collector pushed in the last five
  minutes. ([#78](https://github.com/jmrplens/ghchronicle/issues/78))
- **"Every repository, ever" is each repository's newest row.** It took
  `MAX()` of every column over the range, grouped by the short name, and
  stars, branches, tags, releases and open issues go down: on the same day
  jmrplens/FFT2octave read 4 stars, from that backfill's row of 2026-09-18,
  where GitHub and its newest row said 3, and it listed
  portainer-mcp-enhanced as well. The SQL stores now keep each repository's
  newest row in the range, one per `full_name`, so two owners' repositories
  of one name are two rows, with the Overview's rule for archived ones; commits
  and merges only grow, so they read as before, and the link is the newest
  row's. InfluxDB 3.11.5 refused the row numbering and the `IN` of that rule
  in one `SELECT` over `gh_repo_total` ("Window schema has wrong number of
  fields") and answers with the filter a level below, so that is where it
  sits.
  Against production the table lists 54 repositories, FFT2octave at 3 stars
  and portainer-mcp-enhanced not at all. Elasticsearch buckets by the full
  name, and hides the column, where it bucketed by the short name, and keeps
  the archived documents of the last seven days; Graphite keeps the archived
  points of the last seven days; Prometheus already read each series' current
  value.
- **"Cache entries by key" is what each repository holds now.** It added up
  each ref's newest row anywhere in the range, and a ref whose caches GitHub
  evicted keeps its last row: jmrplens/gitlab-mcp-server's `golangci-lint`
  read 128 entries and 9.46 GiB over 108 refs, the oldest row from
  2026-09-18, more than `gh_actions_cache`, GitHub's own total, gave the whole
  repository, 87 entries and 9.96 GiB. The rows are stamped at the start of
  the UTC day and rewritten through it, and the panel now reads only those of
  the last day in the range the collector read a repository's caches: in
  InfluxDB and PostgreSQL the rows at each repository's newest timestamp, in
  Elasticsearch a terms bucket on the timestamp under each repository that
  keeps the newest, where it read each ref's newest document. Against
  production the query gives `golangci-lint` 44 entries and 3,312,061,861
  bytes over 24 refs, byte for byte what GitHub lists for its
  `golangci-lint-` keys; `?key=golangci-lint` matches by prefix and also
  lists the 20 entries of `golangci-lint.cache-Linux-2959` and `-2960`, 64
  and 3.94 GiB in all, which the table shows as caches of their own. Graphite
  cannot find each repository's newest day, so it reads the last UTC day of
  the range, and its table is empty from midnight UTC until the day's first
  `actions` pass; it used to carry each evicted ref to the end of the range.
  Prometheus holds the value last pushed for each ref, so an evicted one
  counts there until the exporter drops it a day later or, pushed over OTLP,
  until the process restarts, and the panel says so there.
  ([#91](https://github.com/jmrplens/ghchronicle/issues/91))
- **`-list` says what a sweep writes for an archived repository set aside.**
  It printed `(archived: the archive date only; a backfill collects it)`
  beside each, which stopped being true when 2.5.2 began writing its
  `gh_repo_total` row on every `totals` sweep, as the first production run of
  this release showed. It now prints
  `(archived: its archive date and lifetime totals; a backfill collects the rest)`.
- **The achievements are read every hour.** The family ran once a day because
  every pass walked the whole merged history, 18 to 24 MB. Since the
  co-authored count is kept, a pass is the profile page, 36 KB and off the
  budget, and two GraphQL points for the pull requests merged since the day
  before, about 48 points a day at the hour. At a day, a tier reached in the
  morning waited for the next night: on 2026-09-27 the account had 16 accepted
  answers and GitHub showed Galaxy Brain silver, while the dashboard showed
  bronze from the 15 of the night's pass. The whole history is still walked
  once a week, and the rows are still one a day, rewritten until it ends. Out
  of the families of six hours or more, it no longer takes turns, which is
  what brings the longest wait above to 2h30m.
  ([#90](https://github.com/jmrplens/ghchronicle/issues/90))

Each of the three panel fixes carries a test shown to fail against the
queries before it, in every store it holds: the statement or query each store
is sent, read offline, and for the archived rule an evaluation of the Graphite
targets over an account holding a repository nothing has written for nine
days. The containerised suite writes the three kinds of stale row into
InfluxDB 3.11.2 beside a sweep, and against the dashboards before the fixes it
read 91 stars for 83, listed the repository nobody collects, gave the set-aside
one 8 stars for 3 and a cache 9 entries for 4; after them, none of these.

Measured on 2026-09-26 against the live API with this account's token: the
five outbound searches cost a point a page with every new field and without
them, over 102 items in 51 repositories, the answer growing from 35.9 KB to
52.9 KB, and the three comment walks a point a page with `isPrivate` and
without it. `onlyAnswers` exists on github.com and in the schema of GHES 3.17,
the oldest published, lists oldest first like the unfiltered connection, and
costs a point a page, and the 14 answers inside the newest hundred came back
byte for byte the same from both queries. On jmrplens/Cloudflare-DNS-Updater
the `rel="last"` page of `per_page=1` reads 1,393 code scanning alerts, the
1,393 distinct alerts a backfill wrote; run live, two of the account's alert
lists fill their first page, both of their open lists are empty, and a second
read answered every extra request 304. In the proxy log the cadences were
costed from, a `core` request answered 304 left `x-ratelimit-used` where the
request before it in the same rate window had put it 11,692 times out of
11,771, and the star histories answered 304 to 184 of 185 conditional reads
and `activity` to 2,320 of 2,356. Against Loki 3.7.7 with its default limits,
a stream holding an entry five minutes old took one 55 minutes old and
refused one 75 minutes old; an entry two hours old into a stream with nothing
newer was taken, one six days old was taken and one eight days old refused
with "timestamp too old"; a line sent twice at the same instant came back
once, again after its chunk was flushed; and one push of entries from 23
hours, 12 hours and a minute ago into an empty stream was taken whole.
Against PostgreSQL 18.6: a `gh_external_contribution` table written by one
sink, then a second sink process writing two more fields, answered
`column "additions" of relation "gh_external_contribution" does not exist` and
wrote nothing, and with each field declared as a column of its own the same
sequence wrote the row. With a reader holding a table,
`CREATE TABLE IF NOT EXISTS` returned in 0.4 ms and the catalog read in 15 ms,
while an `ADD COLUMN IF NOT EXISTS` for a column already there waited until a
one second `lock_timeout` refused it and a `SELECT` behind it waited 3
seconds; a restarted sink with a `lock_timeout` of 500 ms wrote in 32 ms.

The audit counted the log of the proxy in front of the production service,
one line per GitHub request with its status, rate headers, GraphQL cost and
size, over one process of 2.5.1 from 2026-09-25 12:58Z to 2026-09-26 19:50Z:
25,857 requests, 4,426 of the 21,828 `core` ones charged, 4,786 GraphQL
points and 70.9 MB on the wire; the rows written are the journal's. Measured
live on 2026-09-27: all 28 of the account's forks answered their community
profile 404 and its 39 other repositories 200, and the owned listing and a
read of each repository agreed on the four flags for all 67; page 3 of
jmrplens/phonometry's artifact listing answered 502 after 10.5 seconds and
the same request two seconds later 200 in 1.6; jmrplens/mikroscope's cache
listing honoured `sort=created_at` and its direction, over 240 entries with
distinct creation times, and after the change jmrplens/jmrplens wrote two
cache rows, the CodeQL one with `caches` 15 and 57,894,432 bytes, and
mikroscope 116 over 232 entries with no identity shared. Against golang/go,
whose head was committed at 22:27:19Z and authored three days before,
`history(since:)` held the head from that second and nothing from the next.
The movement query took 5.2 to 7.8 seconds for fifty of the hundred most
recently updated repositories with more than twenty thousand stars and 2.9
to 4.8 for twenty-five, and a hundred cost 2 points and came back with
`RESOURCE_LIMITS_EXCEEDED` past the sixty-fifth alias. A `merged:` range held
a pull request merged at 00:05Z and one at 23:18Z each in its own date only.
One sweep of every family kept 1,065 answers in the cache file, 10.3 MB of
bodies and 1.6 MB of file, none over 404 KB, and the process before the
audited one, which lived five and a half days, held 6,695 URLs, about 99 MB,
of which the 2,517 asked for in its last 48 hours come to about 31 MB.

Each change in behaviour carries a test shown to fail against the code before
it: the draft's age, the promoted release, the Loki line and its lookback, the
answer past the newest hundred, the column an earlier process never wrote, the
exporter's reading of an upstream repository, the open alert behind a hundred
fixed ones, a day of ticks at the built-in cadences, which fails on all ten
against the old table, the entries of one cache on one ref and a listing that
changes while it is read, a 502 answered on its second asking and the
artifact total kept past one that is not, a fork asked for its community
profile once a day, six slow families due in one sweep, which the old loop
started together, the co-authored count added to across a restart, and a
repository nothing moved in, which the old runner asked all three families
about. Where such a test's file calls something the old code lacks, it was
run there with that call stubbed, or with the tests that need it set aside.
The binary against the fake GitHub holds the rest: a second process asks
with the validators the first one stored and is answered 304, a named fork
is asked nothing the listing answered, a second sweep leaves unread what did
not move, and the cache rows add up to the totals the fake declares with no
identity shared, each of which fails against the code before it. The
containerised suite now runs "Cache entries by key" against every store with
rows in it, InfluxDB and PostgreSQL no longer excused. The new fields are
held by the outbound golden file, and the release lookback by a test of the
binary against the fake GitHub as well as the sink's own.

Not verified, and worth saying plainly:

- Production has run it for hours, not days: long enough for the panels to be
  checked against GitHub, as above. The cost of the new cadences is projected
  from 2.5.1's request log, not read from 2.6.0 running them, and so is what
  the audit's fixes save: every saving above is that log with the waste
  counted out, and the first day after the upgrade is the first reading of
  both. The same is true of the release lines in Loki, the accepted answers
  and `gh_upstream_repo`.
- The upgrade is the one start with no cache file, so it pays for the first
  pass of each family in full once, and the first restart after it is the
  first reading of the file in production. Its bounds were measured on this
  account's 37 repositories; an account with more or larger answers was not
  tried, and one past 64 MB keeps the answers asked for most recently.
- One retry was seen to turn a 502 into a 200 once, by hand. How many of the
  gateway errors production meets a second asking two seconds later clears
  is for 2.6.0's own request log to say.
- The turns were seen in the loop's tests and in a simulated week, not in
  production, where the first day after the upgrade spreads the daily
  families over up to 3h. Whether they stay apart after that is read from
  the state file of the days that follow.
- A co-authored count that goes down, because a repository was made private
  or deleted, and a cache listing that loses an entry while it is read have
  been held to fixtures only; neither was seen live, and the weekly walk that
  notices the first has not run against GitHub.
- The release lookback was measured against Loki 3.7.7 with its default
  limits. A Loki with another `max_chunk_age`, or a
  `reject_old_samples_max_age` shorter than a week, was not tried, and the
  six-day cap assumes the week.
- An open alert behind a hundred fixed ones has been counted against the
  collector's fixtures only; the fake GitHub's alert lists are shorter than a
  page. On this account neither list that fills its page has an open alert,
  so the live reading is 0 either way.
- `private` true on a contribution or an upstream repository has been seen in
  fixtures only: none of the 51 repositories this account's searches reach is
  private, although one of its newest hundred issue comments sits in a private
  one.
- Whether GitHub gives a release taken back to a draft a new `published_at`
  when it is published again was not seen. If it does, the release has two
  rows and the sum counts it twice; the distinct tags of a repository are the
  exact count.
- A sweep reads the newest five hundred accepted answers. On an account with
  more, an older comment accepted late is still found only by a backfill; no
  such account was tried.
- Left out rather than unproven: Dependabot's `alerts` stays what a sweep
  read, since a total would take a walk of the whole list, and no panel reads
  `gh_release_published` yet, because a releases-per-month panel renumbers
  every later panel the containerised suite names by position. An `actions`
  pass with nothing new still writes the two rows per repository stamped at
  the sweep, 74 on this account, because keying a current state in the write
  ledger without its timestamp trades against panels whose range is shorter
  than the interval it would then be sent again at. And the daily whole page
  of `issues` stays on the first sweep of the UTC day rather than spread over
  the day by repository, as the audit offered: a day the family stopped
  before a repository's hour would lose rows of that day no later read can
  write, since a read stamps the day it happens on.

## 2.5.2 - 2026-09-26

Four things the store said that GitHub did not, each found by reading one
beside the other on the account this collects: a Loki line that called every
contribution merged, a count of threads commented elsewhere that was mostly
at home, five searches that stopped at a hundred, and archived repositories
whose stars froze at the last backfill. And the outbound family, which
costs next to nothing, now runs every hour, and every family runs at the
cadence it states, which about half the time it did not.

- **A Loki line says what happened to the contribution.** The rendering of
  `gh_external_contribution` read the user, the repository and the number and
  nothing else, so every line said `USER merged OWNER/REPO#N`: open issues,
  open pull requests and pull requests closed without merging included. An
  item still open is stamped at the start of each day it is seen open, so
  each of them added a false "merged" line a day, and production held 36 in
  26 hours, 23 of them for issues. The sentence now follows the `kind` and
  `state` tags and the `merged` field: `USER's pull request OWNER/REPO#N` is
  open, was merged or was closed without merging, and
  `USER's issue OWNER/REPO#N` is open or was closed. Merging and closing are
  said of the item rather than put in the account's name, because the row
  does not say who did either, and in someone else's repository it is
  usually a maintainer. An open item says it is open, not that it was
  opened, since its line comes back every day it stays open: still one a
  day, because the sink keeps no state. A combination where the state and
  the field disagree, which the five searches do not produce, reads
  `USER's contribution OWNER/REPO#N` and leaves the rest to the logfmt tail.
  Lines already in Loki keep what they said.
- **`commented_elsewhere` leaves out the account's own repositories, and
  drops on upgrade.** It was searched as `commenter:LOGIN -author:LOGIN`,
  which leaves out the threads the account opened but not the repositories
  it owns, so every issue it answered and every Dependabot pull request it
  commented on at home counted as work in other people's. Its two siblings,
  `pulls_merged_elsewhere` and `issues_elsewhere`, already meant outside the
  account's repositories, with `-user:LOGIN`, and the Lifetime panel
  described all three that way. The query is now
  `commenter:LOGIN -user:LOGIN`, the same rule. The field keeps its name, so
  in every store the series drops by the difference on the first `totals`
  sweep after the upgrade: from 125 to 55 on the account it was measured on,
  where 103 of the 125 were in its own repositories and 54 of those were
  Dependabot's. Search counts an issue or a pull request once however many
  comments the account left on it, so the Lifetime tile now reads "Threads
  commented elsewhere", and the panel's description says what the two
  outside numbers count.
- **The outbound searches read past their first hundred.** The five searches
  behind `gh_external_contribution` asked for one page of a hundred and never
  read a cursor, on a sweep or on a backfill. An account past a hundred items
  in a state kept the newest hundred by creation date, an old pull request
  merged later never got its merged row because it was no longer among them,
  and `gh_account_total.pulls_merged_elsewhere`, which is GitHub's own count,
  disagreed with the table beside it. Nothing failed, so nothing said so. The
  two open states are now read to the end on every sweep, newest created
  first, since each open item gets a row for every day it stays open. The
  three closed states are ordered by what moved last, `sort:updated-desc`,
  because a merge or a close moves an item to the top however old it is: a
  sweep reads each back to a cadence before the previous outbound sweep, and
  a backfill until the pages run out or reach `backfill.since`. A page count
  would not have done for a sweep, since a hundred later updates, a bot
  locking old threads or a relabel, carry the item that closed onto a page
  nobody reads. GitHub serves a thousand results of any search and no more,
  so an account past a thousand in one state keeps the thousand that moved
  most recently, and the log now says so at warning, once per count, with
  the kind, the state, the count and what was read. A search that fails part
  way keeps the pages that answered. It costs a GraphQL point a page, so a
  sweep still spends eight until an account has more than a hundred open of
  a kind or more than a hundred move between two sweeps. Open items come
  back on the first sweep after the upgrade; an item closed before it that
  2.5.1 never read takes a
  [backfill](https://jmrp.io/docs/ghchronicle/how/backfill/), since a sweep
  reads back only to the one before it.
- **`outbound` runs every hour, not every twelve.** A pass is about nine
  GraphQL points and no REST: 8 measured in production on 2026-09-26 (the
  starred list, the five searches, the two comment walks, about 126 KB), so
  the hour is some 200 points a day out of 5,000 an hour, where the busiest
  hour of that day spent 1,813. The rows it rewrites are skipped by the
  write ledger unless they changed, and an open item is stamped at the start
  of its day, so the extra passes write almost nothing. What they buy is
  time: on that day four answers were accepted after the one pass of the
  morning, and the dashboard showed 11 accepted answers for the rest of the
  day where GitHub showed 15. A configuration that already names
  `outbound` under `every.families` keeps what it names.
- **A family runs at its cadence, not a tick after it about half the time.**
  A family was due when its interval had elapsed to the nanosecond, and the
  loop reads the sweep's clock a few milliseconds after its ticker wakes it,
  by an amount that changes from tick to tick. Whenever one sweep was less
  late than the one before, the two were a hair less than a tick apart and
  every family whose cadence is a whole number of ticks waited a whole tick
  more. In production, over 30.85 hours and 122 sweeps before this release,
  the quarter hour families ran every 24 minutes on average, 41 of 77 gaps
  being 30 minutes, the half hour ones every 39 and the hourly ones every 69;
  `actions` ran 56 times in the 22 hours of 2026-09-26 it was watched where
  88 were due. A family is now due to within half a tick, which absorbs any
  such lateness and cannot let it run a tick early, and the inbox's daily
  full read takes the same margin. Running at the stated cadences costs what
  the cost tables already say, which is more than the service spent: about
  450 GraphQL points and 400 to 750 core requests a day more on the account
  measured, and some 20,000 InfluxDB rows a day more, most of them the rows
  `actions` stamps at every pass.
  ([#85](https://github.com/jmrplens/ghchronicle/issues/85))
- **Archived repositories' stars and forks reach the account's totals on
  every sweep.** With `include_archived` off, the default, a sweep sets
  archived repositories aside and wrote only their `gh_repo_archived` row.
  Their `gh_repo` and `gh_repo_total` came from a backfill, once, stamped at
  the backfill, so their stars and forks froze there and left every panel
  once that instant left the range, and an install that never ran a backfill
  never had them. It rested on the premise that nothing about an archived
  repository moves, and people still star, unstar and fork them: on
  2026-09-26 jmrplens/FFT2octave's only row, from the backfill of 2026-09-18,
  said 4 stars where GitHub said 3, and the seventeen archived repositories
  the default filter sets aside on that account held 80 stars and 24 forks
  the Overview left out. The query every `totals` sweep already sent for
  their archive dates now also asks for the fields `gh_repo_total` is made
  of, and each gets that row beside its `gh_repo_archived`: the tags and
  fields a collected repository's row has, `archived` true, stamped at the
  sweep. Their history is still not walked, and a sweep writes them no
  `gh_repo` and no `gh_repo_policy`. The counts cost the gateway time, so the
  query carries twenty five repositories rather than fifty: a GraphQL point
  per twenty five archived repositories per `totals` sweep.
- **The Overview and "Every repository, ever" count them, in all five
  stores.** The Overview's stars and forks read a live repository's newest
  `gh_repo`, as before, and an archived one's newest `gh_repo_total`, one row
  per full name, so two owners' repositories of the same name are two, and
  one archived while the collector runs is counted once rather than under
  both its live row and its archived one. Under All the sums include the
  archived repositories the picker does not list, so on upgrade the tiles
  rise by what those hold, 80 stars and 24 forks on the account above; with
  repositories picked, those alone are counted, and a range shorter than the
  `totals` cadence, twelve hours by default, can leave the archived rows out.
  "Every repository, ever" lists them under All with their current counts,
  and a repository archived inside the range is one row flagged archived in
  every store, where Elasticsearch, Graphite and Prometheus drew it twice.
  The picker still lists live repositories only, because it reads `gh_repo`;
  since the SQL stores' All expands to that list, their filter lets the
  archived rows through when the variable's text is "All". In
  Elasticsearch the two sums also gained the repository filter they lacked.

Measured on 2026-09-26. The 36 Loki lines are production's stream over the
26 hours before the fix, and the five searches on this account answered 33
merged pull requests, all with `mergedAt`, and 69 items in the other four
states, none with it. `commented_elsewhere` read 125 through REST and through
the GraphQL alias the collector sends, and 55 with `-user:`; of the 102
threads the account had opened elsewhere, 33 count, because the opening post
is not a comment. On the searches: GitHub's default order is newest created
first, the same hundred in the same order as `sort:created-desc`, and
`sort:updated-desc` is honoured; 53 of the first hundred of one account's
closed issues by `updated-desc` had been opened before the oldest of the
first hundred by creation and closed after it, one opened in 2014 and closed
on 1 September 2026; 2,860 merged pull requests answered ten pages of a
hundred, with no item twice, and then no next page; `closedAt` was not after
`updatedAt` on any of 1,000 merged pull requests, and was on one of 373
closed unmerged, by a day, in 2011. The index's copy of `updatedAt` can lag
the item's own by years, facebook/flow pull requests updated in 2019 sorting
among 2017, which only makes the bound read a page more. Against the fifty
most starred archived repositories of google and of microsoft, at cost 1
every time, the lifetime row of fifty at once answered once in 9.2 seconds
and was refused twice with the gateway's 502 after 10.7 and 11.1, twenty five
answered in 5.6 to 6.7, this account's eighteen archived repositories, its
one archived fork among them, in 3.6 to 4.1, and the same fifty asked for the
scalars and the watchers alone in about a second.

Every fix carries a test shown to fail against the code before it. The
containerised suite gained a case against InfluxDB 3: a sweep sets an
archived repository aside, and the Overview's own SQL counts its stars under
All and not with the live repositories picked, and "Every repository, ever"
lists it, archived, with its stars.

Not verified, and worth saying plainly:

- The cadences at the half tick, in production. The test sweeps a day of
  ticks whose lateness alternates by two milliseconds and counts the passes;
  the gaps the journal shows after the upgrade are the first reading of it.
- None of it has run in production. The Loki sentences, the step down in
  `commented_elsewhere` and the archived rows are what the tests and the
  readings above say they will be; the first sweep after this upgrade is the
  first reading of them in a store.
- The walk past a hundred, by the collector against GitHub. This account has
  33, 15, 11, 24 and 19 items in the five states, so its sweeps still read a
  page each. The pages were read with `gh` on other accounts, and the walk is
  held to them by unit tests and by the binary against the fake GitHub.
- The thousand is reported, not worked around. Splitting a search by
  `created:` ranges would reach past it and was not done, and the warning has
  fired against the fake GitHub only.
- An open state is walked by an offset cursor, so an item that leaves it
  while a walk of more than one page is under way moves every later item up
  by one, and one of them can miss that sweep and be read by the next. That
  is read from the cursor, not seen.
- That Grafana renders `${repo:text}` as "All" when All is selected, which the
  SQL stores' filter rests on, is read from `@grafana/scenes` and from
  `templateSrv` before it, not seen in a browser: the checkers and the
  containerised suite render the variables themselves. A Grafana that
  answered anything else would leave the archived rows out under All, as
  2.5.1 did, and count nothing twice.
- The Prometheus and Graphite sums were checked by evaluators of their query
  languages written for the test, over a made-up account with two owners'
  `.github`, a repository archived while the collector ran and one set aside
  after a backfill. The containerised suite runs them against the real
  stores, but only InfluxDB has counted an archived repository's stars
  there.
- Twenty five archived repositories to a query was measured on the heaviest
  of two organisations. A heavier batch meets the gateway's 502, which halves
  it and retries, so it costs a query and never a repository, but no account
  has been seen needing it.

## 2.5.1 - 2026-09-25

What releasing and deploying 2.5.0 turned up, fixed the same day: a publish
that gave up over one panel, release notes nobody could read, images nobody
had signed, and a handful of smaller things that said less than they should.

- **Publishing no longer gives up over the Loki datasource.** Since 2.4.0, a
  Grafana token that may publish dashboards and may not create datasources,
  the Editor the documentation recommends, abandoned every dashboard when a
  Loki sink was configured without `grafana.datasource.loki_uid`. The
  publisher looked for its own `ghchronicle-loki`, found none, was refused
  making it, and returned that refusal as the run's error, although Loki
  feeds one panel, the failed job output. In production the run said "could
  not publish the dashboard, carrying on without it" and published nothing.
  It now warns once, naming `ghchronicle-loki`, the permission the token
  lacks, `grafana.datasource.loki_uid`, and up to three Loki datasources
  Grafana already has with their addresses, and publishes every dashboard
  with that panel left as its note. The warning speaks for that panel alone,
  since it is printed before any store is tried. A `ghchronicle-loki` an
  earlier run made, which the token may read and may not rewrite, is read as
  it is, with a warning naming `datasources:write` and `loki_uid`. A sink
  with a `tenant_id` asks for that rewrite on every start, and so does a
  datasource whose address was changed by hand, so an Admin run followed by
  a token scoped down to an Editor met the refusal on every start after it.
- **A Loki datasource Grafana already has at the sink's address is adopted.**
  Before making `ghchronicle-loki`, the publisher lists what Grafana has and
  adopts a Loki datasource at the address it works out from the sink, which
  needs only `datasources:read` and writes nothing. Never for a sink with a
  `tenant_id`: the tenant travels in a secret Grafana does not hand back, so
  a datasource there could be reading another tenant. Store datasources are
  still made rather than adopted by address, because one InfluxDB, PostgreSQL
  or Elasticsearch serves many databases under credentials Grafana never
  shows, and an address does not say which of them a datasource can see. A
  health probe the token may not make now names `datasources:query` instead
  of advising `grafana.datasource.url`, a refused store datasource names the
  permission and `grafana.datasource.uid`, and a warning from a publish on
  start reaches the journal as a warning.
- **`-uninstall dashboard` removes `ghchronicle-loki`.** It removed only the
  datasources named after a store, so the Loki one stayed while the
  dashboards page said a datasource it created goes. One named in
  `loki_uid`, and one it adopted, are left, like any datasource it did not
  make.
- **The release page carries this file's section for the version** instead
  of GoReleaser's list of commits. That list filed all ten 2.5.0 commits
  under "Other", sorted alphabetically, each behind a forty-character SHA,
  because its groups expected conventional prefixes this repository does not
  use, and the section written for the release never reached its page. The
  preflight job now cuts the `## X.Y.Z - YYYY-MM-DD` section out and refuses
  a tag without one, with an empty one, or with a relative link, written
  inline, as a reference definition or in an HTML `href` or `src`, which the
  release page would resolve against `/releases/tag/`. GoReleaser publishes
  it above the pull lines, the verification commands and the compare link,
  which is where the commits still are.
- **The container images are signed, and `latest` waits for the check.** On
  ghcr.io and on Docker Hub, with cosign, keylessly, by the same release
  workflow identity as the checksum file, and the release job verifies each
  image against that identity at its exact tag before it runs it, so an
  unsigned image now fails the release. GoReleaser pushes only the version
  tag. `latest`, which every documented `docker run` pulls, is moved onto the
  same digest by the release job once both checks pass, so a release that
  fails leaves it on the previous one rather than on an image nobody
  verified; pushed with the version tag, it moved before the signing. Every
  image up to 2.5.0 is unsigned.
  [Verify the image](https://jmrp.io/docs/ghchronicle/install/docker/#verify-the-image)
  has the command, which needs cosign 3: cosign 2.6.1 finds the signature
  only with `--new-bundle-format`, cosign 2.5.0 not even then, and the
  release page's footer now says so too.
- **The Action takes `version` with or without its v.** `version: 2.5.0` went
  into the download URL as typed and failed with a 404, while `v2.5.0`
  worked. Both now install the same release. The major tag `v2` is refused by
  name before anything is fetched, since it is what `uses:` takes and no
  binaries hang from it, and a release that does not exist is reported as
  missing rather than as a gzip error.
- **`install.ps1` checks the signature too.** It verified the checksum and
  stopped, while `install.sh` also verifies `checksums.txt` with cosign when
  cosign is on PATH. It now does the same, with the identity and issuer
  `install.sh` uses, which a test holds the two scripts to, and both
  installers say when only the checksum was verified. Neither takes an old
  or unreachable cosign for a forged file any more. A cosign older than
  2.4.2 cannot read the bundle every release publishes and fails exactly as
  it would on a forged file, which `install.sh` reported as "Do not use what
  was downloaded"; both installers now say that cosign is too old and go on
  on the checksum, as they do without one. A newer cosign that fails still
  stops the install, and the refusal now quotes it, so "tuf refresh failed"
  from a machine that cannot reach Sigstore reads as what it is.
- **`install.sh` no longer ends with "/dev/tty: No such device or address"**
  when it runs without a controlling terminal: a CI job, a container build, a
  provisioning run. The probe's stderr redirection was made after the open it
  was meant to silence; it now covers the open.
- **A `go install` build says what it is.** It printed
  `ghchronicle 2.5.0 (commit unknown, built unknown)`, because a module
  download carries no checkout and so no commit or date. It now prints the
  module version the go command fetched and the Go release that compiled it,
  `ghchronicle 2.5.1 (module v2.5.1, built with go1.27.1)`. Every other
  build prints the line it printed before, byte for byte.
- **A Prometheus or OTLP mean is over the items that carried the field.** The
  count reduction divided every summed field by the number of points in the
  series, so a field a collector leaves out when it has no honest value
  counted as a zero there. `steps` on a job GitHub no longer serves steps
  for, which 2.5.0 stopped writing, and `queued_seconds` on a job with no
  start time pulled `github_workflow_jobs_steps_mean` and
  `github_workflow_jobs_queued_seconds_mean` down, as the 2.5.0 entry said,
  and so did every other field written only when it has a value, the wait
  for a first human review among them. Averaging over the items that carried
  the field is what AVG does with a null in the SQL stores. Three
  fields whose absence is itself the answer are still averaged over every
  item, so their means stay shares: `merged` on an outside contribution,
  `advanced` on a fork and `pull_requests` on a workflow run.
- **A first stargazer walk that fails is walked whole again, and a backfill
  walks every list whole.** The state file recorded a repository under
  `first_saw` before its one full walk of the stargazer list, so a first walk
  a 502 cut short retired the repository all the same: every later sweep read
  only its newest hundred, through the batch, and the older stars stayed
  unread for good, because a backfill read a recorded list by page one and
  the last page too. It is recorded now only once the walk came back without
  an error, as `history_read` already was, and a backfill walks every
  stargazer list whole, recorded or not, so running one recovers a repository
  an earlier release recorded after a walk that failed.
- **Every HTTP client has a connection pool of its own.** The Grafana client,
  the uninstall's store calls, the guided setup's probe and the containerised
  suite's harness sent through `http.DefaultClient`, whose pool a parallel
  test closing its httptest server empties under a request another test has
  in flight. The source test that holds every `http.Client` literal to a
  transport of its own now also refuses `http.DefaultClient` and the
  `http.Get`, `Head`, `Post` and `PostForm` that send through it, which is
  how these four went unnoticed.
- **Two checks of the containerised suite no longer depend on the clock.** The
  InfluxDB check that a second sweep adds no dated row counted `gh_star_day`
  whole, and a second sweep after a UTC midnight writes the new day by
  design; it now counts the days before the first sweep's own. The same
  midnight also moved the other four tables it counts: each sweep had a fake
  GitHub of its own on the live clock, so the second one dated every traffic
  day, run, commit and star in the fixtures a day later, onto timestamps the
  first never wrote. The second sweep's fake is now frozen at the first
  sweep's start. The PostgreSQL star-day check loads one sweep and compares
  exact sets, which a midnight cannot move, and now says so.
- **The release run ends by linking the Marketplace box.** Listing a
  release in the Marketplace's version menu takes a tick on its edit page,
  and no token can give it: GitHub asks for a 2FA confirmation only the web
  page performs, and the releases API has no parameter for it. 2.5.0 stayed
  out of the menu until it was ticked by hand, so the last job of the release
  run now leaves a notice and a line in the run's summary with that page's
  address.
- **Two things this file and the site said that were not so.** The 2.4.0
  entry had `-setup` serving whoever arrived through Homebrew, and there is
  no Homebrew channel; it now says a tarball. The stars screenshot's
  description on the panels page described the picture it replaced.
  `.github/ACTION.md` now also answers why the Marketplace listing offered
  only `v1`: no setting names a major version, and the listing's version
  menu is the releases ticked for the Marketplace one by one, which no
  numbered release had been.

Measured against a throwaway Grafana 13.2.1, with an Editor service account, a
Prometheus datasource named in `grafana.datasource.uid`, and a Loki sink at an
address Grafana has no datasource for: 2.5.0 ended with "Permissions needed:
datasources:create" and exit 1, and this release warned, named the Loki
datasource Grafana already had and its address, published the dashboard with
the panel as text, and exited 0. With the sink at the address of that
datasource it adopted it, the panel read it, and no `ghchronicle-loki` was
made. A service account with no role is refused `datasources:read` on the list
and `datasources:query` on the probe, and the run now ends naming the second.
A new case of the containerised suite mints an Editor on its own Grafana and
holds the publish to three outcomes: the note, the adoption, and a
`ghchronicle-loki` an Admin made with a tenant, read as it is. On the same
Grafana 13.2.1, a `ghchronicle-loki` an Admin token had made, for a sink with
a `tenant_id` and then again with its address changed by hand in Grafana, was
read as it was by an Editor: one warning naming `datasources:write`, exit 0,
and the panel drawing logs from it. With an Editor, a Loki sink and an
InfluxDB sink but no `grafana.datasource.uid`, the Loki warning is followed by
the store's refusal and exit 1, and no longer promises every dashboard first.

Measured with GoReleaser 2.18.1, in a full release of the 2.5.0 tree against a
local registry and a stand-in for the GitHub API: the body posted was the
2.5.0 section byte for byte above the footer, with no list of commits, and the
images were signed by digest after the push and before the release was
created, two signatures per digest while `latest` was still one of
GoReleaser's tags, which cosign 3.1.3 verifies and cosign 2.6.1 finds only
with `--new-bundle-format`. Moving `latest` from the version tag with
`docker buildx imagetools create`, as the release job now does, was measured
with buildx 0.37.0 against a local registry on a two-platform OCI index carrying
index annotations, as GoReleaser pushes one: `latest` came out under the same
digest, which is what lets the version tag's signature cover it.
`changelog.disable: true`, the tidier-looking setting, published the footer
and nothing above it, which is why the configuration keeps it false. The
preflight's cut of the 2.5.0 section matches its lines in this file under mawk
and gawk, and it refuses a missing section, an empty one and a relative link.
Under GNU grep 3.11 it refuses a relative inline link, reference definition,
`href` and `src`, and a bare `#fragment`, which points into this file and not
at the release page, and it lets through absolute and `mailto:` links and a
footnote definition. Against the published 2.5.0 release with cosign 3.1.3,
both installers answered "signature verified with cosign", and both refused a
copy whose `checksums.txt` had one line added, installing nothing. With cosign
2.4.0, which exits 1 on that bundle where 2.4.2 verifies it, both installed
2.5.0 on the checksum and said the cosign was too old to read the signature,
where `install.sh` had refused it; with cosign 3.1.3 and Sigstore's TUF CDN
unreachable, `install.sh` refused and quoted "tuf refresh failed". The
Action's install step, run as written against GitHub, installed 2.5.0 from
`2.5.0`, `v2.5.0` and `latest`, refused `v2`, and said there is no archive for
`2.9.9`. `go install` of v2.5.0 through the public proxy printed the unknowns,
a file proxy serving this tree as v2.5.1 printed the module line, and a
GoReleaser-stamped build, `make build`, the Dockerfile with and without its
build arguments and an unstamped build in a checkout print what they printed
before.

Not verified, and worth saying plainly:

- The keyless signing of an image, and the move of `latest` inside the
  release job. The first needs the Actions OIDC token, so the local release
  signed with a key; the first proof of both is this release's own job,
  which verifies each image before it runs it and checks the digest `latest`
  lands on.
- The production Grafana. It was fixed with `loki_uid` before any of this
  was written, and the warning and the adoption were measured on a throwaway
  one.
- A token that may create datasources still makes `ghchronicle-loki` at the
  sink's push address with the path dropped, which, where the collector
  pushes to a published port and Grafana reaches Loki on a container network,
  is an address Grafana may not reach, and nothing asks the Loki datasource
  whether it answers. `loki_uid` is the answer there.
- `install.ps1` ran under PowerShell 7.6.6 on Linux, not under Windows
  PowerShell 5.1, so its handling there of what cosign writes to stderr is
  reasoned rather than seen.
- The Marketplace. That ticking the box on a numbered release puts it in the
  listing's version menu is GitHub's documented behaviour; nobody has ticked
  one yet.
- How far the new divisor moves the Prometheus panels that read these means
  on this account. The unit tests pin the divisor; no exporter was read
  before and after.
- A backfill walking a recorded stargazer list whole, against GitHub. A unit
  test holds it to every page of a five-page list, where it read two; no
  backfill has run with it, and its cost is one request per hundred stars,
  the same as a repository's first sweep.
- The containerised InfluxDB check across a real UTC midnight. It passed with
  the second sweep's fake frozen, and the dates a frozen fake resolves do not
  depend on the hour, but no run has straddled one.

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
  than in the install script, so it serves whoever arrived through a tarball,
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
