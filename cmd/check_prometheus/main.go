// Command check_prometheus checks the Prometheus dashboard two ways.
//
// Names first: every metric a panel asks for must appear in a live dump of the
// exporter's own /metrics. A typo in a metric name is not a syntax error, so
// PromQL will happily parse it and return nothing forever.
//
// Then syntax: each expression is posted to Grafana's Prometheus datasource,
// which hands it to Prometheus to parse. An empty result is expected when
// nothing has been scraped yet; an error is not.
//
//	go run ./cmd/check_prometheus <metrics-dump> [datasource-uid]
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/cmd/internal/dashboards"
	"github.com/jmrplens/ghchronicle/internal/grafana"
)

// name is every metric this project exports: the exporter prefixes all of them.
var name = regexp.MustCompile(`\bgithub_[a-z0-9_]+`)

func main() {
	// Nothing here cancels the run, but every request is posted under one
	// context so that a caller that wanted to could.
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

const usage = "usage: check_prometheus <metrics-dump> [datasource-uid]"

// run is the whole command, with the arguments, the two streams and the exit
// status passed in and handed back rather than taken from the process, so a
// test can drive every way it ends.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 1
	}
	dump, uid := args[0], ""
	if len(args) > 1 {
		uid = args[1]
	}

	exported, err := exportedNames(dump)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	store, ok := dashboards.ByName("prometheus")
	if !ok {
		fmt.Fprintln(stderr, "there is no prometheus dashboard to check")
		return 1
	}

	// Without a uid there is nothing to ask Grafana, so the dashboard keeps the
	// placeholder datasource an export carries and only the names are checked.
	var ds any = store.DS
	client := grafana.New()
	if uid != "" {
		ds = map[string]any{"type": "prometheus", "uid": uid}
		if client.Token == "" {
			fmt.Fprintln(stderr, "GRAFANA_TOKEN is not set, so no expression can be posted")
			return 1
		}
	}

	missing, bad := 0, 0
	for _, panel := range grafana.Walk(store.Build(ds)["panels"]) {
		expr, _ := panel.Target["expr"].(string)
		for _, metric := range uniqueNames(expr) {
			if !exported[metric] {
				missing++
				fmt.Fprintf(stdout, "MISSING %s: %s is not exported\n", panel.Title, metric)
			}
		}
		if uid == "" {
			continue
		}
		if refusal := refused(ctx, client, ds, panel.Target, expr); refusal != "" {
			bad++
			fmt.Fprintf(stdout, "FAIL %s: %s\n", panel.Title, grafana.Trim(refusal, 200))
		}
	}
	fmt.Fprintf(stdout, "\n%d unknown metric names, %d rejected expressions\n", missing, bad)
	if missing > 0 || bad > 0 {
		return 1
	}
	return 0
}

// refused posts one panel target to Grafana and answers with why the request
// failed or Prometheus rejected the expression, and with nothing when it was
// accepted.
func refused(ctx context.Context, client grafana.Client, ds any, target map[string]any, expr string) string {
	query := map[string]any{}
	maps.Copy(query, target)
	query["datasource"] = ds
	// The dashboard variable is not expanded here, so the matcher it feeds
	// has to become one that matches every repository instead.
	query["expr"] = strings.ReplaceAll(expr, `repo=~"$repo"`, `repo=~".*"`)
	query["intervalMs"] = 300000
	query["maxDataPoints"] = 200

	ref, _ := query["refId"].(string)
	res, err := client.Query(ctx, "now-6h", "now", query, 60*time.Second)
	if err != nil {
		return err.Error()
	}
	_, errText := grafana.Frames(res, ref)
	return errText
}

// exportedNames reads the metric names a dump of /metrics declares. Only the
// TYPE lines are authoritative: a sample line carries labels and a value too.
func exportedNames(path string) (map[string]bool, error) {
	// The dump is named on the command line and read as given; cleaning it
	// first means the path opened is the path printed in any error.
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]bool{}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "# TYPE ") {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 2 {
			out[fields[2]] = true
		}
	}
	return out, scan.Err()
}

// uniqueNames is every metric name an expression mentions, once each, in the
// order it mentions them, so a repeated name is reported once.
func uniqueNames(expr string) []string {
	seen := map[string]bool{}
	var out []string
	for _, match := range name.FindAllString(expr, -1) {
		if seen[match] {
			continue
		}
		seen[match] = true
		out = append(out, match)
	}
	return out
}
