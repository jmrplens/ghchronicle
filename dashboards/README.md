# Dashboards

Five dashboards, all in English, all in Grafana's shareable export format:
the datasource is a `${DS_...}` placeholder and the `__inputs` block asks the
importer to choose their own.

| File | Panels | Store |
|---|---|---|
| `ghchronicle-influxdb.json` | 152 | InfluxDB 3, queried with SQL |
| `ghchronicle-prometheus.json` | 152 | Prometheus |
| `ghchronicle-postgres.json` | 152 | PostgreSQL or TimescaleDB, from the SQL sink |
| `ghchronicle-graphite.json` | 152 | Graphite, from the Graphite sink |
| `ghchronicle-elasticsearch.json` | 152 | Elasticsearch or OpenSearch, from the Elasticsearch sink |

## Importing

Grafana: Dashboards, then New, then Import, then upload the file. Grafana will
ask which datasource to use:

- InfluxDB: the InfluxDB 3 datasource for the database the sink writes to, in
  SQL mode.
- Prometheus: the Prometheus that scrapes ghchronicle's exporter.
- PostgreSQL: the PostgreSQL datasource for the database the SQL sink's
  statements were piped into. TimescaleDB is the same datasource with the
  TimescaleDB switch on; the queries do not change.
- Graphite: the Graphite the sink writes to. The paths assume the default
  prefix, `github`, and the functions need Graphite 1.1 or later.
- Elasticsearch: an Elasticsearch datasource whose index pattern is
  `ghchronicle-*` and whose time field is `@timestamp`. One datasource serves
  every panel, because each target names its own index in the query.
  OpenSearch works through the same plugin.

Or with the API, naming the input the file declares:

```sh
curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d "{\"dashboard\": $(cat ghchronicle-influxdb.json), \"inputs\": [
        {\"name\":\"DS_INFLUXDB\",\"type\":\"datasource\",\"pluginId\":\"influxdb\",\"value\":\"<uid>\"}],
      \"overwrite\": true}" \
  "$GRAFANA/api/dashboards/import"
```

The inputs are `DS_INFLUXDB` (`influxdb`), `DS_PROMETHEUS` (`prometheus`),
`DS_POSTGRES` (`grafana-postgresql-datasource`), `DS_GRAPHITE` (`graphite`)
and `DS_ELASTICSEARCH` (`elasticsearch`).

## One dashboard, five stores

They are the same dashboard. `cmd/internal/dashboards` holds one ordered list of
sections and panels, and every panel carries one query set per store. The
generator picks one set and emits the JSON, so every file has the same panels in
the same places with the same titles, and a user who chose any store gets the
whole dashboard rather than a smaller cousin. `cmd/gen_dashboards` checks that
promise on every run and fails rather than write files that have drifted apart.

The stores cannot answer identical questions, and the panels say so rather
than pretend. InfluxDB and PostgreSQL hold a row per fact, dated when the fact
happened, so they can draw the traffic of a particular Tuesday, the star curve
since 2018 and the merge time of a pull request closed in July; the PostgreSQL
set is the InfluxDB SQL translated by `toPG` in `query.go`, because the SQL
sink writes the same facts as tables. Prometheus stamps every sample at scrape
time, so the exporter reduces the per-item rows to current values plus, for the
counted measurements, a monotonic `_total` of distinct items seen since the
exporter started; `increase()` over that is how "per day" and "over the range"
panels are answered. Graphite keeps the dated points but has no rows: a table
there is one number per series reduced over the range, so a table that needs
several fields of one row keeps the column it is sorted by and says which it
dropped, and a boolean is not a metric there at all. Elasticsearch keeps the
dated documents, so a per-item table is the newest documents themselves and
everything else is a bucket aggregation; the sink writes no mapping, so the
aggregations use the `.keyword` sub-field of each tag. Each panel whose twin
in another store is richer says so in one sentence of its description.

Twenty-four panels have no Prometheus answer at all, because the exporter
skips the measurement, drops the identity the panel is about, or the panel is
a join between two measurements: top paths, the contribution calendar, the two
commit punch cards, the per-item tables (the largest pull requests, pull
requests by author, the latest discussions, the answers left elsewhere, the
latest notifications and their kin), the slowest steps, release assets,
container tags and events by repository among them. They are still emitted,
as text panels with the same title saying what they would show and why the
store cannot, so the layouts stay identical. The other three stores answer
all but a handful, some of them in a reduced form the description
names.

## Regenerating

The JSON is generated, not hand-edited.

```sh
go run ./cmd/gen_dashboards          # writes all five files
go run ./cmd/gen_dashboards -check   # writes nothing, fails if they are stale
```

`cmd/internal/dashboards/sections_*.go` is where a panel is added or changed;
`panels.go` holds the panel constructors and `query.go` the query helpers for
each store. `stores.go` only chooses a query set and a datasource. A new tag on
a collector changes the Graphite path depth of its measurement, which the `tags`
table in `tags.go` mirrors. `go test ./cmd/internal/dashboards` fails when the
committed JSON no longer matches the specification.

## Checking

No builder is trusted without running the queries. The raw database API
accepts things the Grafana plugin then fails to render, so the checkers go
through Grafana's own query path where a datasource exists.

```sh
GRAFANA_TOKEN=... go run ./cmd/check_dashboards influxdb <datasource-uid>
GRAFANA_TOKEN=... go run ./cmd/check_prometheus <metrics-dump> <datasource-uid>
go run ./cmd/check_postgres <schema.json>            # EXPLAIN every query
go run ./cmd/publish_dashboard influxdb <uid>        # publish for eyeballing
go run ./cmd/publish_dashboard -loki <loki-uid> influxdb <uid>   # and read the job logs
```

`check_dashboards` reports every panel as ok, empty or failing.
`check_prometheus` additionally checks each metric name against a live dump of
the exporter's own `/metrics`, because a typo in a metric name is not a syntax
error: PromQL parses it happily and returns nothing forever.

`check_postgres` needs no data: it declares the SQL sink's schema in a
scratch database inside a transaction it rolls back, and asks PostgreSQL to
EXPLAIN each panel query with Grafana's macros replaced by literals. A column
that does not exist or a reserved word left unquoted fails there. The schema
comes from an InfluxDB that has seen every measurement; `--dump <uid>` fetches
it through Grafana.

The Graphite and Elasticsearch files have no parser to hand. They are checked
by importing them into a Grafana and by the shape of each target: every
Graphite path has the depth of its measurement's tag set with `$repo` at the
repository node, and every Elasticsearch target names its index, aggregates on
a `.keyword` field, and puts the date histogram last.

One thing the exported files do not show: the output of failed jobs. That
is text, so the InfluxDB sink excludes it by default and it goes to a log
store instead, and an importer may have no log store, so the file carries a
text panel, "Where failure output went", saying that the query is
`{job="ghchronicle", kind="job_log"}`. A Grafana that does have a Loki gets
the lines themselves: `publish_dashboard -loki <loki-uid>` draws them in that
panel's place, same title and same place in the grid, filtered by the
dashboard's repository variable where the store's variable can be read as a
regular expression (InfluxDB, PostgreSQL and Prometheus; the Graphite and
Elasticsearch variables name the glob star as their All value, so there the
panel shows every repository and says so).
