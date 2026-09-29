# Contributing

Issues and pull requests are welcome. This page is what a contributor needs to
know before opening one: where things live, what has to pass, and what a change
owes the documentation.

The full documentation is at <https://jmrp.io/docs/ghchronicle/>. This is
the shorter, repository-side version of it.

## What this is, in one paragraph

One static Go binary with no runtime dependencies. Its direct dependencies are
`gopkg.in/yaml.v3`, the PostgreSQL driver `github.com/jackc/pgx/v5`,
`golang.org/x/term` so that `-setup` can read a secret without echoing it, and
`golang.org/x/sys`, which `x/term` builds on and which takes the lock beside the
state file (`flock` on Unix, `LockFileEx` on Windows); `go.mod` is the list. It sweeps the GitHub
API on a schedule and writes each observation as a point stamped with the date
the thing happened, not the date it was collected. That single rule is what
makes re-collection converge instead of accumulating, and most of the design
follows from it.

## The layout

```text
cmd/ghchronicle     the binary: flags, sinks, runner, the one-shot card
cmd/probe           development aid: runs collectors and prints line protocol
internal/ghapi      REST and GraphQL client, ETag cache, rate state, typed errors
internal/collect    one file per family of metrics
internal/sink       Point, line protocol, the sinks, the Reducer that makes gauges
internal/render     the SVG card
internal/config     YAML with ${VAR} expansion (and ~ in paths), per-family cadences
internal/run        the sweep scheduler, its state file, the cache file beside
                    it, and the turns the slow families take
internal/teardown   what -uninstall and a migration do to each store, found by
                    asking the store
internal/migrate    every change to what a stored row is keyed by, and what
                    -migrate and a start do about it
internal/dashboards the dashboard specification, shared by the generators
                    and by the binary that publishes it
test/e2e            the binary against a fake GitHub, and against real stores
site/               the documentation, from which docs/ is generated
```

## Before you open a pull request

```sh
make build vet test-race   # the end-to-end suite runs in test-race too
make golangci-lint         # the config check, the formatter's diff, then the linters
```

The targets name the packages rather than `./...`, which on a machine with the
git-ignored `plan/` directory would take in a Go package of its own; and
`make golangci-lint` is what CI runs, formatter included, where a plain
`golangci-lint run` skips the formatting gate. `make analyze` runs CI's Go,
Markdown, shell and generated-artifact checks and reports each failure at once;
actionlint, hadolint, the site's lint below and `go vet` for the other
platforms CI type-checks run only in CI.

For a change under `site/`:

```sh
cd site && pnpm install && pnpm run build && pnpm run lint
```

That pair is the gate that holds the two languages together. `pnpm run lint`
checks that every English page has a Spanish twin with the same headings and
the same components, that the published counts match the code, that `docs/` is
still what these pages generate, and that the markdown twin of every page still
builds; `pnpm run build` is where every internal link and anchor is resolved,
by the validator plugin, and an unresolved one fails it.

For a change to the Go code, the six checks of CI's Generated artifacts job,
which regenerates nothing and fails on anything committed that the code no
longer produces:

```sh
make check-dashboards check-gallery check-layouts check-config-options check-compose check-config-cases
```

Each is fixed by the same target without `check-`, and the dashboards by
`go run ./cmd/gen_dashboards`: the five dashboard files, the card pictures
under `site/src/assets/`, `site/src/data/layouts.json`,
`site/src/data/config-options.json`, the compose files under `deploy/` and
`internal/config/testdata/config-cases.json` are generated and never
hand-edited. `config-cases.json` is written from `config-options.json`, so
run `make config-options` before `make config-cases`; a new setting or a
changed cadence moves both.

## What a change owes

**A new collector** means: a file in `internal/collect` that takes a
`collect.Walk`; a place in `internal/run` (`perRepoFamilies` and a case in
`repoFamily` or `familyBatch` for a family that runs per repository, an
`r.family` call in `accountFamilies` for one about the account); an entry in
`config.defaultEvery`, with its group and a measured reason for its cadence (a
cadence missing there is rejected at start-up); the family's row in the
cadence table of `configuration/cadences.mdx`, whose English reason is the
code's word for word, which `internal/config/documented_test.go` checks; its
row in the cost table of `api/cost.mdx`; a rule in `sink.promRules`; a Loki
rendering if it is an event; a fixture and a test; a route in
`test/e2e/fakegh` with an entry in that package's `Measurements`; a panel in
`internal/dashboards`; and a row in the measurements page of the site. All of
it in both languages where it is a page.

**A new measurement in an existing family** means its rule in
`sink.promRules`, a Loki rendering if it is an event, its row on the
measurements page, and the counts the site states in prose:
`site/scripts/gen-stats.mjs` holds each of them to the code, and a count it has
no word for in `NUMBER_WORDS` fails until one is added. Its tag keys go in
`internal/migrate/identity.json`, written by
`go test ./internal/migrate -run TestEveryChangeOfIdentityIsRegistered -update`
once the fake GitHub answers it; the same holds for every measurement of a new
collector.

**A new field on an existing measurement** is a field and never a tag: a new
tag gives every row written after it an identity the rows already stored do
not have, so each item becomes two series, and PostgreSQL keys a table on the
tags it was created with. A value that can change after the row's own date is
a field for the same reason. One the
collector writes only under a condition goes in `conditionalColumns`
(`internal/dashboards/conditional_columns_test.go`) when a SQL panel reads it;
the test fails until it is there.

**A change to a measurement's tag keys**, a tag that becomes a field or one
that is removed, means a new entry in `migrate.Registry`
(`internal/migrate/registry.go`), with an ID of its own: the release, the tags
only the old shape carries, and every family that writes the measurement, so
`-migrate` can find the old shape in a store and say what bringing it along
takes. `TestEveryChangeOfIdentityIsRegistered` sweeps the fake GitHub, compares
every measurement's tag keys with `internal/migrate/identity.json`, and refuses
to rewrite that file with `-update` while a tag that went away has no entry
that is not yet pinned in `internal/migrate/testdata/registry.json`; a pinned
entry, one a release may have shipped, never changes, and `-update` pins every
entry it accepts. `TestEveryMigrationNamesEveryFamilyThatWritesIt` sweeps it
one family at a time and fails on an entry whose families are not the ones
that write it, and `TestEveryFamilyWritesAMeasurementInOneShape` on two
families that write one measurement with different tag keys. A tag renamed is
a tag removed and one added, and a tag added to an existing measurement is
refused outright, for the reason in the paragraph above.

**A panel one store draws differently from the others** means the reason in
that store's own description of the panel, and an entry in `dashboardsDiffer`
(`test/e2e/docker/dashboards_agree_test.go`) quoting those words and naming
only the stores that draw the difference. The containerised suite compares
what every store draws, after the panel's transformations and overrides, and
fails on a difference no description explains, on an entry whose words the
description no longer holds, and on a store an entry names that draws the
panel as the others do. A table heads the columns every store draws in the
order the SQL stores select them, and a column the SQL stores draw that
another store does not is named in that store's own description of the panel;
the suite fails on either, and `TestEveryColumnAStoreLacksIsNamedInItsDescription`
holds every table to the second whether or not the fixture fills it. A
Graphite table says what it drops with `grRows`, which writes the sentence out
of the table itself. A stat value one store cannot draw over a repository or
a range with nothing in it is the same, with its entry in `tilesLeftOverNothing`
(`test/e2e/docker/tiles_over_nothing_test.go`). A chart or a bar chart the SQL
stores fold into other folds in each other store or says it does not
(`TestEveryStoreFoldsTheRestIntoOtherOrSaysItDoesNot`). And a list's
`ORDER BY` names what tells two of its rows apart after the column it sorts
by, the item's own identity where the rows are items, since InfluxDB and
PostgreSQL break a tie each its own way (`TestEverySQLListOrdersItsRowsCompletely`).
The PostgreSQL translation compares every text key by its bytes, as InfluxDB
does, and types each key from its name: a panel that sorts by a field it
cannot type, or takes the least or the greatest of one, stops
`go run ./cmd/gen_dashboards` until the field is named in `textFields` or
`numberFields` (`internal/dashboards/collate.go`).

**A cadence change** means a measured reason in `config.defaultEvery`, the
cadence table in both languages, `make config-options` then
`make config-cases`, the figures (`cd site && pnpm run figures`), `make docs`,
and the table of how long the slow families wait on the cadences page when a
family crosses six hours, which no test holds.

**A new sink** means: a file in `internal/sink` with an httptest-backed test of
its exact wire format, a struct in `config.Sinks` with a validation message
that says what is required, a branch in `buildSinks`, a commented block in
`config.example.yaml`, a page under `site/src/content/docs/sinks/` with its
Spanish twin, and a store in `internal/dashboards/stores.go` if Grafana can
query it. It also means its place in `storesOf` (`internal/migrate/stores.go`):
whether `-migrate` asks the store, follows the state file's record of it, or
has nothing to do there and says why, and the destination, never a
credential, a record of it is kept against. `TestEverySinkIsAStoreThePlannerKnows`
fails on a sink left out. A sink whose store keeps rows needs a way to be
brought along, a `teardown.Clearer` or an entry in `storeWays`
(`cmd/ghchronicle/migrate.go`), and its page a section saying what a migration
does there.

**A step that deletes or sets aside data**, in a migration or in `-uninstall`,
means three tests: one that it touches only what it must, with the same
measurement in another database, schema or prefix, another measurement beside
it and a name that only starts like it, all left as they were; one that the
dry run, `-migrate` without `-yes` or `-uninstall` without it, sends the store
nothing but reads and changes no file; and a run against the real store in
the containerised suite (`test/e2e/docker`), at the version its compose file
pins. The name it acts on is exact, the measurement or the copy it made, and
never a pattern.

**A new setting or Action input** means `config.example.yaml` and the
configuration pages, or `action.yml` and the inputs table of
`install/actions.mdx`, in both languages, then `make config-options` and
`make config-cases`. A test walks the `yaml:` tags and fails on a setting the
example does not name.

**A behaviour change** means a test that fails before it and passes after it.

## The configuration form cannot drift from the parser

This used to be explained on the page the form is on, where it was noise for
the reader trying to write a `config.yaml`. It belongs here.

The controls are not a copy of the settings. `cmd/gen_config` exports
`internal/config`'s own types into a file the page reads, down to the default
each key resolves to, which it measures by validating a probe rather than by
restating a number. `make check-config-options` fails when that file is no
longer what the code produces, in the analysis suite and in CI, so the form
cannot offer a setting the binary does not have and cannot miss one it does.

The other direction is checked too, and its limit is worth saying out loud. The
configurations the form writes for the answer sets in
`internal/config/testdata/config-cases.json` go through the real parser in
`internal/config`'s own tests, including the shapes that have caught people
out: a cadence per family under `every.families`, an `include_private` left
off, a sink whose credentials are `${VAR}` references, a card-only run with no
destination at all, and answers the parser is expected to refuse. Those cases
are generated by the module the page runs, so they cannot be a copy of it, and
every branch of that module that can change what it writes has one. What the
corpus does not prove is behaviour no answer set produces, which is why the
file that owns it lists what it cannot cover and why.

## House rules

- Everything in the repository is in English: code, comments, commit messages,
  pull requests and documentation. The Spanish translation lives only in
  `site/src/content/docs/es/`.
- No em dash or en dash characters anywhere.
- Comments explain why, not what. A comment that restates the code is worse
  than no comment.
- Measurements are measured. A number in a comment or on a page is something
  that was run, not something that was estimated, and it says what it was
  measured against.
- No attribution or co-author lines in commits or pull requests.

## Reporting something

A bug or an idea goes in an issue; the templates ask for the version, the
configuration with the token removed, and the log line. A security
vulnerability does not go in an issue: [SECURITY.md](SECURITY.md) says where it
goes instead. How people are expected to talk to each other in any of those
places is [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md), and reporting a breach of
it is described there.
