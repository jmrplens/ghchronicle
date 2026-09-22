# Ready-made compose files

Five stacks, each one complete. Pick the file whose name says what you want,
put two lines in a `.env` beside it, and start it:

```sh
printf 'GITHUB_TOKEN=github_pat_...\nGITHUB_USER=your-login\n' > .env
docker compose -f compose.influxdb-grafana.yaml up -d
```

| File                            | What comes up                                  |
| ------------------------------- | ---------------------------------------------- |
| `compose.collector.yaml`        | the collector on its own, writing to a file    |
| `compose.influxdb.yaml`         | the collector and InfluxDB 3                   |
| `compose.influxdb-grafana.yaml` | those two and Grafana, dashboard already in it |
| `compose.postgres.yaml`         | the collector and PostgreSQL                   |
| `compose.postgres-grafana.yaml` | those two and Grafana, dashboard already in it |

Each one carries its own configuration inline, so there is no second file to
write, and each is brought up against the real images before a release. With
Grafana, the collector publishes the dashboard when it starts and points it at
the store beside it: nothing to import, no datasource to fill in.

The [Docker page](https://jmrp.io/docs/ghchronicle/install/docker/) shows the
same files with a picker, which is the easier way in if you are reading the
documentation rather than the repository.

These files are generated. `cmd/gen_compose` writes them and `make
check-compose` fails when what is committed is not what the generator
produces, so edit the generator rather than the YAML.
