# Publishing to the Grafana dashboard directory

The reader's half of this, why the files are already in the right shape and
what a new revision means, is on
<https://jmrp.io/docs/ghchronicle/dashboards/>. This page is the
maintainer's half: the steps, in order.

The five dashboards are already in the shape <https://grafana.com/grafana/dashboards>
requires. Nothing needs converting, because the export format they are
generated in is the format the directory takes:

- `__inputs` declares the datasource the importer must choose. Without it a
  published dashboard carries the author's own datasource uid and renders
  empty for everyone else.
- `__requires` names the Grafana version and the datasource plugin, which is
  what the directory lists as the dashboard's requirements.
- There is no `id` key. The directory assigns one on publication.

## Publishing

1. Sign in at <https://grafana.com> with the GitHub account.
2. Dashboards, then **New dashboard**, then paste the contents of one
    `ghchronicle-<store>.json`.
3. Fill in the listing: name, a description, a screenshot, and the categories.
    The names are worth keeping distinct, because five dashboards with the same
    title are indistinguishable in search results:

    | File | Suggested listing name |
    |---|---|
    | `ghchronicle-influxdb.json` | ghchronicle for InfluxDB |
    | `ghchronicle-prometheus.json` | ghchronicle for Prometheus |
    | `ghchronicle-postgres.json` | ghchronicle for PostgreSQL and TimescaleDB |
    | `ghchronicle-graphite.json` | ghchronicle for Graphite |
    | `ghchronicle-elasticsearch.json` | ghchronicle for Elasticsearch and OpenSearch |

4. Publish. Each gets a numeric id, and a user then imports it by typing that
    id into Grafana's import screen rather than downloading a file.

## Keeping them current

The directory versions a dashboard: publishing again against the same listing
adds a revision rather than replacing the old one, and users are shown that an
update exists. So a regeneration that changes panels is a new revision of the
same five listings, not five new listings.

Two things to check before each revision, both of which the repository's own
tools already answer:

```sh
go run ./cmd/gen_dashboards -check                                # the five files are current
GRAFANA_TOKEN=... go run ./cmd/check_dashboards influxdb <uid>    # every panel still returns data
```

The second has to be pointed at a store holding a real account. The fixture
carries every field of every measurement; an account carries only what has
happened to it, and a column of these stores exists once a point has carried
it, so a panel selecting a field the account has never written is refused and
draws "No data". The same check is the last item of
[RELEASING.md](../.github/RELEASING.md) before the tag, with what its two
summary lines mean.

## Publishing to your own Grafana

The directory takes the files as they are. A running server does not: the
`${DS_*}` placeholder and the `__inputs` block ask an importer to choose a
datasource, which a server cannot do. `cmd/publish_dashboard` builds the
dashboard bound to a concrete datasource uid, drops the two export blocks and
overwrites whatever is at the dashboard's uid:

```sh
GRAFANA_TOKEN=... go run ./cmd/publish_dashboard influxdb <datasource-uid>
GRAFANA_TOKEN=... go run ./cmd/publish_dashboard -loki <loki-uid> influxdb <datasource-uid>
```

`-loki` names a Loki datasource on the same server. The files carry a text
panel, "Where failure output went", where the output of failed jobs would be,
because job logs are text and go to the Loki sink, and an importer may have no
Loki; with the flag the published dashboard reads those lines from Loki in
that panel's place, under the same title and at the same grid position, so
nothing else in the layout moves. The flag is for a server of your own: the
directory listing keeps the text panel, since the directory cannot know
whether an importer has a log store.

## The `uid`

Each file carries a fixed `uid` (`ghchronicle-<store>`). That is deliberate for
a repository import, where a stable uid means a stable URL and a re-import
updates in place rather than duplicating. On a directory import Grafana offers
to change it, and a user importing two of these into one Grafana will be asked
to, since the uids differ per store and cannot collide.
