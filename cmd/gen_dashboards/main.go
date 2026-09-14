// Command gen_dashboards writes the five Grafana dashboards.
//
// The panels are defined once in cmd/internal/dashboards; this picks each
// store's query set and wraps them in Grafana's shareable export format, where
// the datasource is a `${DS_*}` placeholder and the `__inputs` block asks the
// importer to pick their own.
//
// It also proves the promise the specification makes: the five files carry the
// same panels, in the same order, at the same places, under the same titles.
// Any difference in that layout is a bug in the specification rather than a
// legitimate difference between stores, so it fails the run.
//
//	go run ./cmd/gen_dashboards            # write the files into dashboards/
//	go run ./cmd/gen_dashboards -check     # write nothing, fail if they differ
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/jmrplens/ghchronicle/cmd/internal/dashboards"
)

func main() {
	os.Exit(run(os.Args, os.Stdout, os.Stderr))
}

// run is the whole command, with the arguments, the two streams and the exit
// status passed in and handed back rather than taken from the process, so a
// test can drive every way it ends. args[0] is the program name, as in
// os.Args.
func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "dashboards", "where the dashboard files live")
	check := flags.Bool("check", false, "compare with what is on disk instead of writing")
	// The statuses a flag.ExitOnError set would exit with: a request for the
	// usage is not a failure, a flag it does not know is.
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	var reference [][2]any
	status := 0
	stores := dashboards.AllStores()
	for i := range stores {
		store := &stores[i]
		doc := store.Build(nil)
		layout, err := layoutOf(doc)
		if err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", store.Name, err)
			return 1
		}
		var retitled map[int]bool
		if store.Name == "prometheus" {
			retitled = dashboards.PromRetitled()
		}
		if i == 0 {
			reference = layout
		} else if diff := compare(reference, layout, retitled); diff != "" {
			fmt.Fprintf(stderr, "%s: %s\n", store.Name, diff)
			status = 1
		}

		body, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		body = append(body, '\n')
		path := filepath.Join(*dir, store.File)
		if *check {
			if err = checkUpToDate(path, body); err != nil {
				fmt.Fprintln(stderr, err)
				status = 1
			}
			continue
		}
		if err = os.WriteFile(filepath.Clean(path), body, 0o600); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "wrote %s, %d panels\n", path, dashboards.Count())
	}
	if status == 0 && !*check {
		fmt.Fprintf(stdout, "%d dashboards, identical layouts\n", len(stores))
	}
	return status
}

// checkUpToDate reports why the file at path does not hold the dashboard in
// body, and nothing when it does.
func checkUpToDate(path string, body []byte) error {
	old, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !sameJSON(old, body) {
		return fmt.Errorf("%s is out of date", path)
	}
	return nil
}

// layoutOf is the title and place of every panel, rows included, flattened in
// the order Grafana reads them.
func layoutOf(doc map[string]any) ([][2]any, error) {
	panels, ok := doc["panels"].([]map[string]any)
	if !ok {
		return nil, fmt.Errorf("the panels are a %T, not a list of panels", doc["panels"])
	}
	return appendLayout(nil, panels)
}

// appendLayout adds each panel's title and place to out, and after a collapsed
// row the panels it carries inside itself.
func appendLayout(out [][2]any, panels []map[string]any) ([][2]any, error) {
	for _, p := range panels {
		g, ok := p["gridPos"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("panel %q has no place on the grid", p["title"])
		}
		out = append(out, [2]any{p["title"], fmt.Sprint(g["x"], g["y"], g["w"], g["h"])})
		// Only a collapsed row has panels of its own; for anything else this
		// is nil and adds nothing.
		inner, _ := p["panels"].([]map[string]any)
		var err error
		if out, err = appendLayout(out, inner); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// compare is the first difference between two layouts. A position in
// `retitled` may differ in title alone: that is the Prometheus title a panel
// declares on purpose, and its place still has to match.
func compare(want, got [][2]any, retitled map[int]bool) string {
	if len(want) != len(got) {
		return fmt.Sprintf("%d panels against the first dashboard's %d", len(got), len(want))
	}
	for i := range want {
		if want[i] == got[i] || (retitled[i] && want[i][1] == got[i][1]) {
			continue
		}
		return fmt.Sprintf("panel %d is %v, the first dashboard has %v", i, got[i], want[i])
	}
	return ""
}

func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return fmt.Sprint(x) == fmt.Sprint(y)
}
