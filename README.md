<p align="center">
  <img src="brand/banner.png" alt="ghchronicle" width="100%">
</p>

# ghchronicle

Collects everything GitHub will tell you about an account, and keeps it with
the date it happened.

GitHub answers most questions about the present and almost none about the past.
The traffic API serves fourteen days and forgets. The activity feed keeps three
hundred events. Read notifications disappear. The star list will tell you when
each star was given, but only if you ask before the list gets long. None of it
is archived anywhere unless you archive it.

`ghchronicle` sweeps those surfaces on a schedule and writes every observation
as a dated point, so a year from now the question "how fast were we merging in
July" still has an answer.

```sh
ghchronicle -config config.yaml
```

## What it draws

Five Grafana dashboards, one per store that can answer with dated points,
generated from a single specification and shipped in the repository. This is
one section of the eighteen, drawn from a demonstration account:

![The contributions section of the InfluxDB dashboard: contributions over
time, a contribution calendar, the commit mix, commits per week, per hour of
day, per weekday and per repository, the yearly totals, and the commits a
profile hides](site/src/assets/dashboards/contributions.png)

And the headline section, which every dashboard opens with:

![The overview section: repositories, stars and forks; views, unique visitors
and clones in range; followers, following, sponsors and sponsoring; and the
account's own figures](site/src/assets/dashboards/overview.png)

## What it renders

A profile card, from the same sweep, in ten layouts and two families. The
three animated ones animate where the reader's browser lets them. Each card
here is drawn twice, one per palette, and GitHub shows the one that matches
the theme you read it in:

<picture><source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/jmrplens/ghchronicle/main/site/src/assets/card-animated-counters_dark.svg"><img src="https://raw.githubusercontent.com/jmrplens/ghchronicle/main/site/src/assets/card-animated-counters.svg" alt="The animated-counters layout: a grid of large numbers over a contribution sparkline"></picture>

<picture><source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/jmrplens/ghchronicle/main/site/src/assets/card-github-stats_dark.svg"><img src="https://raw.githubusercontent.com/jmrplens/ghchronicle/main/site/src/assets/card-github-stats.svg" alt="The github-stats layout: a header band, rows of four monospace numbers and a language share bar with its legend"></picture>

<picture><source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/jmrplens/ghchronicle/main/site/src/assets/card-badge-row_dark.svg"><img src="https://raw.githubusercontent.com/jmrplens/ghchronicle/main/site/src/assets/card-badge-row.svg" alt="The badge-row layout: a horizontal row of small pill badges, each with a label and a number"></picture>

```sh
ghchronicle -config config.yaml -card card.svg -card-layout animated-counters -card-theme light
ghchronicle -config config.yaml -card card_dark.svg -card-layout animated-counters -card-theme dark
```

[The ten layouts](https://jmrp.io/docs/ghchronicle/card/layouts/), with what
each one draws and how to put one in a profile README.

## Documentation

The full documentation is at
**<https://jmrp.io/docs/ghchronicle/>**, in English and Spanish:
[quickstart](https://jmrp.io/docs/ghchronicle/start/quickstart/),
[the 91 measurements](https://jmrp.io/docs/ghchronicle/collectors/measurements/),
[choosing a store](https://jmrp.io/docs/ghchronicle/sinks/),
[the cost of a sweep](https://jmrp.io/docs/ghchronicle/api/cost/) and
[troubleshooting](https://jmrp.io/docs/ghchronicle/reference/troubleshooting/).
Every page also serves itself as markdown at the same path with `.md` on the
end, and [llms.txt](https://jmrp.io/docs/ghchronicle/llms.txt) indexes the
lot. The copies under [docs/](docs/README.md) are generated from those pages.

## What it collects

Ninety-one measurements across thirty-four families, covering every surface
a personal or organisation account exposes.

| Area | What is kept |
|---|---|
| Traffic | Views, unique visitors and clones per day, referrers and paths. GitHub's window is 14 days; this rewrites it whole on every sweep, so a collector that was down for a day repairs itself on the next run |
| Stars | One point per star, dated when it was given. The full stargazer walk happens once per repository; after that the newest hundred ride in one GraphQL query per ten repositories |
| Repositories | Stars, forks, watchers, open issues, size, age, idle days, licence, visibility, languages by bytes, topics, community profile score |
| Releases | Downloads per release and per asset, asset sizes, draft and prerelease state |
| Pull requests | Per item: time to first review, time to merge, lines added and deleted, files changed, review rounds, comments, commits |
| Issues | Per item: time to close, comments, reactions, label count |
| Actions | Runs with duration and queue time, jobs, individual steps, workflows and their state, artifacts and their expiry, cache usage |
| Security | Dependabot and code scanning alerts by severity, plus an explicit record of which features are switched on, so no data is distinguishable from no alerts |
| Contributions | The whole profile calendar, one point per day at that day's date, plus totals and the per-repository commit breakdown |
| Activity | The event feed and the notification inbox, both of which GitHub discards quickly |
| Billing | Usage per day, product, SKU and repository, with gross, discount and net |
| Account | Followers, following, packages, gists, social accounts, sponsors |

## Where it writes

Ten destinations, and more than one at a time is the normal arrangement. Everything
is pushed: nothing here needs to be scraped, so the collector runs wherever it
can reach its databases.

| Store | Keeps | Good for |
|---|---|---|
| InfluxDB | the dated history | "how fast were we merging in July" |
| PostgreSQL / TimescaleDB | the dated history, as SQL you pipe into `psql` | a Grafana user who has a Postgres and no InfluxDB |
| Graphite | the dated history | an existing Graphite |
| Elasticsearch / OpenSearch | the dated history, as documents | search across everything collected |
| Prometheus | the current value | alerting, and a number on a wall |
| OpenTelemetry | either, depending on the backend | an existing collector pipeline |
| Loki | the events, as log lines | "what happened, in order" |
| Telegraf | whatever Telegraf can reach | Kafka, Graphite, Datadog, anything with a Telegraf output |
| File and stdout | line protocol or JSON | a shipper you already run, and a durable buffer |

The difference that decides which to use is dating. InfluxDB keys a point by
measurement, tag set and timestamp, so replaying the same fourteen-day traffic
window every six hours converges on the right answer instead of accumulating
copies; the whole backfill design rests on that. Prometheus cannot: it stamps a
sample at scrape time and rejects meaningfully older ones. Measured against
Prometheus 3.14 with the OTLP receiver enabled and a thirty-minute out-of-order
window, a sample dated two days back comes back as HTTP 400. So the reduction
to current values happens before Prometheus ever sees the data, which is also
what stops it being served one series per star.

## Dashboards

One dashboard, rendered once per store, in `dashboards/`. Same sections, same
panels, same positions, whichever database you chose; where a store cannot
answer a panel honestly the panel is still there and says why. All in English
and in Grafana's shareable export format, so importing asks you to pick your
own datasource.

Import from the Grafana UI (Dashboards, New, Import) or with the API. They are
generated from one specification by the scripts beside them; edit those rather
than the JSON.

## Install

```sh
go install github.com/jmrplens/ghchronicle/cmd/ghchronicle@latest
```

or take a binary from the releases page, or run the container:

```sh
docker run -v $PWD/config.yaml:/config.yaml -e GITHUB_TOKEN ghcr.io/jmrplens/ghchronicle -config /config.yaml
```

## Configure

Two decisions are enough to start, and none of the three installs above leaves
a file behind to copy:

```yaml
github:
  token: ${GITHUB_TOKEN}
targets:
  user: your-login
sinks:
  stdout: true
```

Every `${VAR}` is read from the environment, so the file can be committed while
the secrets stay out of it. `config.example.yaml` in this repository is the
documented version, with a comment on every option there is. It collects every metric it knows how to unless
`groups` names the groups you want, in which case the rest are neither read nor
written; `ghchronicle -groups` lists them.

```sh
ghchronicle -list          # the repositories that would be collected
ghchronicle -once          # one sweep, then exit
ghchronicle                # run on the configured schedule
```

The token needs read access. Traffic additionally needs push access to the
repository, Dependabot alerts need `security_events`, and the `keys` family
needs `read:public_key` and `read:gpg_key`, which no other scope implies.
Anything the token
cannot see is recorded as unavailable and skipped, not treated as a failure: a
repository with a feature switched off must not stop the sweep for the other
forty.

## What GitHub will not give you

Written down so nobody spends an afternoon rediscovering it. On a personal
account, `stats/code_frequency` and `stats/contributors` answer 202 with an
empty body indefinitely. The three per-product billing endpoints are 410 Gone
and only the `/users/{login}/` form of the usage report works. Custom
repository properties, classic projects, cost centres and the audit log are
organisation or enterprise only. `workflows/{id}/timing` returns 200 with an
always-empty `billable`. GraphQL reports zero packages while REST lists them,
so packages come from REST.

## Rate limit

The collector never spends the last `reserve_rate` calls of any bucket, so
whatever else uses the same token keeps working. GitHub runs fifteen
independent budgets and names the one it charged in a header; the reserve is
tracked per bucket and scaled to each, because search allows thirty requests a
minute against core's five thousand. Responses are cached by ETag, and a 304
costs no quota at all, which is what makes short cadences affordable.

Each family has its own cadence because they move at very different speeds:
workflow runs every fifteen minutes, the contribution calendar every twelve
hours. The cheapest thing here by far is GraphQL: one query returns the full
366-day contribution calendar, every contribution total, the per-repository
commit breakdown and the social counts, for one point of a five thousand point
budget.

A backfill is the opposite intention and says so: `-backfill` walks every
surface to the end, bounded by a date you choose or by nothing at all, and when
a bucket runs out it waits for the window to reset rather than giving up.

Three things cannot be backfilled at any price, and the documentation says so
rather than letting you find out: the event feed keeps three hundred events,
traffic is fourteen days, and job logs are deleted after ninety.

## A card for a profile README

A side feature, not the point of the project. The point is the ingestion above.

```sh
ghchronicle -config config.yaml -card profile.svg -card-only
```

One sweep, one self-contained SVG: no webfont, no external stylesheet, no
script, and byte-identical output for the same input so a scheduled job that
commits it does not produce a diff on every run. Ten layouts in two visual
families, one of them GitHub's own look, three of them animated, all with a
choosable set of fields. The repository ships as a composite Action:

```yaml
- uses: jmrplens/ghchronicle@v1
  with:
    token: ${{ secrets.GHCHRONICLE_TOKEN }}
    mode: card
    card: generated/github-stats.svg
    card-layout: github-stats
```

See [docs/card.md](docs/card.md), and
[.github/ACTION.md](.github/ACTION.md) for how the Action is published and
which token it needs.

## Contributing

Issues and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md) says
how the repository is laid out, what has to pass before a pull request can be
merged, and what a change to the collectors owes the documentation.
[SECURITY.md](SECURITY.md) is where a vulnerability goes, and it is not a
public issue.

## Licence

MIT. See [LICENSE](LICENSE).
