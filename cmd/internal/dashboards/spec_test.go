package dashboards

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jmrplens/ghchronicle/internal/grafana"
)

// dashboardDir is where the exported files live, relative to this package.
const dashboardDir = "../../../dashboards"

// TestEveryStoreHasTheSameLayout is the promise the specification makes: a
// user who picks Graphite gets the same dashboard as one who picks InfluxDB,
// not a smaller cousin. A store that cannot answer a panel gets a text panel
// in its place, which keeps the title and the grid position.
//
// The one licensed difference is a title the Prometheus dashboard is given on
// purpose (Panel.PromTitle), where its number under the shared title was a
// different quantity; the place and size still have to match there too.
func TestEveryStoreHasTheSameLayout(t *testing.T) {
	t.Parallel()
	var want []string
	var first string
	retitled := PromRetitled()
	for _, store := range AllStores() {
		got := layout(store.Build(nil))
		if want == nil {
			want, first = got, store.Name
			continue
		}
		if len(got) != len(want) {
			t.Fatalf("%s has %d panels, %s has %d", store.Name, len(got), first, len(want))
		}
		for i := range want {
			if got[i] == want[i] {
				continue
			}
			if store.Name == "prometheus" && retitled[i] && place(got[i]) == place(want[i]) {
				continue
			}
			t.Errorf("%s panel %d is %q, %s has %q", store.Name, i, got[i], first, want[i])
		}
	}
	if len(retitled) == 0 {
		t.Error("no panel declares a Prometheus title, so the allowance above checks nothing")
	}
}

// TestPromTitleRendersOnPrometheusAlone pins which dashboard a PromTitle
// reaches: the Prometheus one, and none of the other four.
func TestPromTitleRendersOnPrometheusAlone(t *testing.T) {
	t.Parallel()
	var spec []Panel
	b := &builder{}
	for _, sec := range Sections {
		spec = append(spec, sec.Build(b)...)
	}
	for _, store := range AllStores() {
		for _, got := range grafana.Panels(store.Build(nil)["panels"]) {
			p := &spec[got.Index]
			want := p.Title
			if store.Name == "prometheus" && p.PromTitle != "" {
				want = p.PromTitle
			}
			if got.Title != want {
				t.Errorf("%s panel %d is titled %q, the specification says %q", store.Name, got.Index, got.Title, want)
			}
		}
	}
}

// place is a layout entry without its title.
func place(entry string) string {
	return entry[strings.LastIndex(entry, " @ "):]
}

// TestPanelTitlesAreUniqueInASection catches the copy that keeps the title of
// the panel it was copied from. Across sections a repeat is deliberate: the
// overview names a tile after the table that details it, and reading "Forks"
// twice on one page would be the bug instead.
func TestPanelTitlesAreUniqueInASection(t *testing.T) {
	t.Parallel()
	total := 0
	b := &builder{}
	for _, sec := range Sections {
		seen := map[string]bool{}
		for _, p := range sec.Build(b) {
			total++
			// The brand header is the one panel drawn without a title: a
			// title bar over the mark would be a second line saying the
			// same word. Any other untitled panel is a copy that lost it.
			if p.Title == "" && p.Kind != "text" {
				t.Errorf("%s carries a panel with no title", sec.Title)
			}
			if seen[p.Title] {
				t.Errorf("%s has two panels called %q", sec.Title, p.Title)
			}
			seen[p.Title] = true
		}
	}
	if total != Count() {
		t.Errorf("walked %d panels, Count says %d", total, Count())
	}
}

// TestExportedFilesAreCurrent fails when the committed JSON no longer matches
// the specification, which is the whole reason the files are generated.
func TestExportedFilesAreCurrent(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		path := filepath.Join(dashboardDir, store.File)
		old, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%v; run go run ./cmd/gen_dashboards", err)
			continue
		}
		fresh, err := json.Marshal(store.Build(nil))
		if err != nil {
			t.Fatal(err)
		}
		var a, b any
		if err = json.Unmarshal(old, &a); err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if err = json.Unmarshal(fresh, &b); err != nil {
			t.Fatal(err)
		}
		if !equal(t, a, b) {
			t.Errorf("%s is out of date; run go run ./cmd/gen_dashboards", path)
		}
	}
}

// TestConcurrentBuildsAgree renders every store's dashboard several times at
// once and holds each render to the one made alone. The Elasticsearch metric
// and bucket ids are handed out while a build walks the specification, so a
// count that two builds shared would interleave their numbers; under -race the
// sharing is reported even on a run where it happened not to change a byte.
func TestConcurrentBuildsAgree(t *testing.T) {
	t.Parallel()
	stores := AllStores()
	alone := make([][]byte, len(stores))
	for i := range stores {
		alone[i] = encode(t, &stores[i])
	}

	const rounds = 8
	together := make([][]byte, rounds*len(stores))
	var wg sync.WaitGroup
	for r := range rounds {
		for i := range stores {
			wg.Go(func() {
				together[r*len(stores)+i] = encode(t, &stores[i])
			})
		}
	}
	wg.Wait()

	for k, got := range together {
		store := &stores[k%len(stores)]
		if !bytes.Equal(got, alone[k%len(stores)]) {
			t.Errorf("round %d of %s differs from the %s dashboard built alone",
				k/len(stores), store.Name, store.Name)
		}
	}
}

// encode is one store's dashboard as JSON. It runs on the goroutines of the
// test above, so a failure is reported with Errorf, which is safe there, and
// the empty document it returns then fails the comparison as well.
func encode(t *testing.T, store *Store) []byte {
	t.Helper()
	body, err := json.Marshal(store.Build(nil))
	if err != nil {
		t.Errorf("encoding the %s dashboard: %v", store.Name, err)
		return nil
	}
	return body
}

// TestNoEmDashes guards the one typographic rule the whole repository keeps.
// The prose of a panel is written here, so this is where it can be checked.
func TestNoEmDashes(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		body, err := json.Marshal(store.Build(nil))
		if err != nil {
			t.Fatal(err)
		}
		if i := bytes.Index(body, []byte("—")); i >= 0 {
			start, end := max(0, i-60), min(len(body), i+60)
			t.Errorf("%s carries an em dash: %s", store.Name, body[start:end])
		}
	}
}

// layout is the title and place of every panel, rows included, flattened in
// the order Grafana reads them.
func layout(doc map[string]any) []string {
	var out []string
	var walk func(panels []map[string]any)
	walk = func(panels []map[string]any) {
		for _, p := range panels {
			g := p["gridPos"].(map[string]any)
			out = append(out, fmt.Sprintf("%v @ %v %v %v %v",
				p["title"], g["x"], g["y"], g["w"], g["h"]))
			if inner, ok := p["panels"].([]map[string]any); ok {
				walk(inner)
			}
		}
	}
	walk(doc["panels"].([]map[string]any))
	return out
}

// equal compares two decoded documents. Both came back from the decoder, so
// re-encoding them cannot fail; a failure here would mean the comparison never
// happened, which is worth saying out loud rather than reading as a match.
func equal(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("re-encoding the committed document: %v", err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("re-encoding the fresh document: %v", err)
	}
	return bytes.Equal(x, y)
}

// TestSQLTargetsCarryTheStatementOnce pins which key each SQL surface is read
// through, because getting it wrong is silent in both directions. A panel
// target of either SQL store is answered from `rawSql` alone, so a second copy
// under `query` is 38 KB of the InfluxDB export and a chance for the two to
// drift. The repository variable is the reverse: the InfluxDB plugin's
// metricFindQuery builds its request out of the variable object's `query` and
// never reads `rawSql`, so removing that one empties the dropdown with no
// error and no test failure anywhere else, since nothing runs a variable query.
func TestSQLTargetsCarryTheStatementOnce(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		if store.Name != "influxdb" && store.Name != "postgres" {
			continue
		}
		for _, target := range sqlTargets(t, store.Build(nil)["panels"]) {
			if _, ok := target["rawSql"].(string); !ok {
				t.Errorf("%s has a panel target with no rawSql: %v", store.Name, target)
			}
			if _, ok := target["query"]; ok {
				t.Errorf("%s repeats a panel target's statement under query: %v",
					store.Name, target)
			}
		}
	}

	influx, ok := ByName("influxdb")
	if !ok {
		t.Fatal("there is no influxdb dashboard")
	}
	q, ok := influx.Variable["query"].(map[string]any)
	if !ok {
		t.Fatal("the InfluxDB repository variable carries no query object")
	}
	if sql, _ := q["query"].(string); sql == "" {
		t.Error("the InfluxDB repository variable lost the query the plugin reads; " +
			"metricFindQuery builds its request from it, not from rawSql")
	}
}

// sqlTargets is every query target of every panel of a rendered SQL dashboard.
func sqlTargets(t *testing.T, panels any) []map[string]any {
	t.Helper()
	var out []map[string]any
	var walk func(list []map[string]any)
	walk = func(list []map[string]any) {
		for _, p := range list {
			targets, _ := p["targets"].([]any)
			for _, raw := range targets {
				if target, ok := raw.(map[string]any); ok {
					out = append(out, target)
				}
			}
			if inner, ok := p["panels"].([]map[string]any); ok {
				walk(inner)
			}
		}
	}
	list, ok := panels.([]map[string]any)
	if !ok {
		t.Fatalf("a dashboard's panels are %T, not a list", panels)
	}
	walk(list)
	if len(out) == 0 {
		t.Fatal("walked a SQL dashboard and found no targets, so this checks nothing")
	}
	return out
}
