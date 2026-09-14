// Command check_dashboards runs every panel query of one dashboard through
// Grafana's own query path.
//
// The raw database API accepts things the Grafana plugin then fails to render,
// so checking the SQL directly is not enough: this posts each panel to
// /api/ds/query exactly as a render does, and reports what came back, one line
// per panel and a count at the end.
//
//	GRAFANA_TOKEN=... go run ./cmd/check_dashboards <store> <datasource-uid> [range]
//
// The run itself lives in internal/grafana, because the containerized
// end-to-end suite runs the same panels against real stores in Docker and the
// two must not drift into asking different questions.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/cmd/internal/dashboards"
	"github.com/jmrplens/ghchronicle/internal/grafana"
)

// usage names the store as required. It could not be optional ahead of the
// uid: a uid is whatever string Grafana was given, "influxdb" included, so
// `check_dashboards a b` would be a store and a uid or a uid and a range, and
// nothing in the arguments would say which. `make check-dashboards-live`
// supplies influxdb when STORE is not set.
const usage = "usage: GRAFANA_TOKEN=... check_dashboards <store> <datasource-uid> [range]"

func main() {
	// Nothing here cancels the run, but every request is posted under one
	// context so that a caller that wanted to could.
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// run is the whole command, with the arguments, the two streams and the exit
// status passed in and handed back rather than taken from the process, so a
// test can drive every way it ends.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help") {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	passed, err := check(ctx, args, stdout)
	if err != nil {
		// The reason on standard error and a failing status, the way the
		// Python original stopped.
		fmt.Fprintln(stderr, err)
		return 1
	}
	// A failing panel has already been reported with the others, so it
	// only sets the status.
	if !passed {
		return 1
	}
	return 0
}

// check runs every panel of the dashboard the arguments name and prints one
// line per panel and a count at the end. It answers whether every panel
// answered, and with an error when the run could not be made at all.
func check(ctx context.Context, args []string, stdout io.Writer) (passed bool, err error) {
	if len(args) < 2 {
		return false, errors.New(usage)
	}
	name, uid := args[0], args[1]
	rng := "now-90d"
	if len(args) > 2 {
		rng = args[2]
	}

	store, ok := dashboards.ByName(name)
	if !ok {
		return false, fmt.Errorf("%s is not one of the dashboards: %s",
			name, strings.Join(dashboards.Names(), ", "))
	}

	client := grafana.New()
	if client.Token == "" {
		return false, errors.New("GRAFANA_TOKEN is not set")
	}

	// Grafana wants the plugin behind the uid, and only the two query
	// languages are asked for here, so everything that is not InfluxDB is
	// posted as Prometheus.
	kind := "prometheus"
	if name == "influxdb" {
		kind = "influxdb"
	}
	ds := map[string]any{"type": kind, "uid": uid}

	substitution, err := vars(ctx, client, store, ds, name, rng)
	if err != nil {
		return false, err
	}
	panels := grafana.Panels(store.Build(ds)["panels"])
	results := client.CheckPanels(ctx, rng, "now", panels, substitution, grafana.Options{
		Timeout:       180 * time.Second,
		Workers:       4,
		IntervalMs:    3600000,
		MaxDataPoints: 500,
	})

	bad, empty := 0, 0
	for _, r := range results {
		switch {
		case r.Err != "":
			bad++
			fmt.Fprintf(stdout, "FAIL %s: %s\n", r.Panel.Title, grafana.Trim(r.Err, 240))
		case r.Rows == 0:
			empty++
			fmt.Fprintf(stdout, "EMPTY %s\n", r.Panel.Title)
		default:
			fmt.Fprintf(stdout, "ok   %s: %d\n", r.Panel.Title, r.Rows)
		}
	}
	fmt.Fprintf(stdout, "\n%d failing, %d empty\n", bad, empty)
	return bad == 0, nil
}

// vars is the substitution a dashboard render would perform and /api/ds/query
// does not: the repository variable as All would expand it, and the time macro
// InfluxDB has no plugin support for.
func vars(ctx context.Context, client grafana.Client, store *dashboards.Store,
	ds any, name, rng string,
) (grafana.Vars, error) {
	v := grafana.Vars{Datasource: ds}
	// A variable with an allValue expands to that string and nothing is asked
	// of the database. The two SQL dashboards have none, so for them All is
	// every repository the database knows about.
	if all, ok := store.Variable["allValue"].(string); ok && all != "" {
		v.AllValue = all
	} else {
		var err error
		if v.Repos, err = repos(ctx, client, ds); err != nil {
			return v, err
		}
	}
	// The macro expands to a literal interval, and the range is a Grafana
	// relative time such as now-90d, whose duration is what SQL wants. Only
	// InfluxDB needs it: the PostgreSQL plugin expands its own SQL macros.
	if name == "influxdb" && len(rng) > 4 {
		v.TimeFilterFormat = "time >= now() - INTERVAL '%s'"
		v.TimeFilter = fmt.Sprintf(v.TimeFilterFormat, rng[4:])
	}
	return v, nil
}

// repos answers with every repository in the database, which is what the
// repository variable expands to when All is selected and the variable has no
// allValue of its own.
func repos(ctx context.Context, client grafana.Client, ds any) ([]string, error) {
	sql := "SELECT DISTINCT repo FROM gh_repo"
	res, err := client.Query(ctx, "now-90d", "now", map[string]any{
		"datasource": ds, "format": "table", "rawQuery": true,
		"rawSql": sql, "query": sql, "refId": "A",
	}, 60*time.Second)
	if err != nil {
		return nil, err
	}
	names, err := grafana.Column(res, "A", 0)
	if err != nil {
		return nil, fmt.Errorf("the repository list came back unreadable: %w", err)
	}
	return names, nil
}
