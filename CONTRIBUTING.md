# Contributing

Issues and pull requests are welcome. This page is what a contributor needs to
know before opening one: where things live, what has to pass, and what a change
owes the documentation.

The full documentation is at <https://jmrp.io/docs/ghchronicle/>. This is
the shorter, repository-side version of it.

## What this is, in one paragraph

One Go binary with no dependencies beyond a YAML parser. It sweeps the GitHub
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
internal/config     YAML with ${VAR} expansion, per-family cadences
internal/run        the sweep scheduler and its state file
cmd/internal        the dashboard specification the generators share
test/e2e            the binary against a fake GitHub, and against real stores
site/               the documentation, from which docs/ is generated
```

## Before you open a pull request

```sh
go build ./... && go vet ./... && go test -race ./...
golangci-lint run ./...
```

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

For a change to the dashboards:

```sh
go run ./cmd/gen_dashboards          # writes the five files
go run ./cmd/gen_dashboards -check   # writes nothing, fails if they are stale
```

The five JSON files are generated and never hand-edited.

For a change to a card layout:

```sh
make gallery         # regenerates the pictures under site/src/assets/
make check-gallery   # writes nothing, fails if they no longer match the renderer
```

The card pictures are generated and never hand-edited.

## What a change owes

**A new collector** means: a file in `internal/collect` that takes a
`collect.Walk`, a call in `internal/run`, an entry in `config.defaultEvery` (a
cadence missing there is rejected at start-up), a rule in `sink.promRules`, a
Loki rendering if it is an event, a fixture and a test, a route in
`test/e2e/fakegh` with an entry in that package's `Measurements`, a panel in
`cmd/internal/dashboards`, and a row in the measurements page of the site.

**A new sink** means: a file in `internal/sink` with an httptest-backed test of
its exact wire format, a struct in `config.Sinks` with a validation message
that says what is required, a branch in `buildSinks`, a commented block in
`config.example.yaml`, a page under `site/src/content/docs/sinks/` with its
Spanish twin, and a store in `cmd/internal/dashboards/stores.go` if Grafana can
query it.

**A new setting** means `config.example.yaml` and the configuration pages. A
test walks the `yaml:` tags and fails on a setting the example does not name.

**A behaviour change** means a test that fails before it and passes after it.

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
