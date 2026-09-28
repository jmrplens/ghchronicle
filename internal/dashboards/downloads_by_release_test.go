package dashboards

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAReleaseNobodyDownloadedIsNoBar holds "Downloads by release" to the
// SQL stores' `downloads > 0` in every store, and Elasticsearch to naming its
// bars by tag, as its description says. Comparing what the five stores draw,
// Graphite drew a release nobody had downloaded as a bar of nothing and
// Elasticsearch as a row of 0, one release more than the other three, and
// Elasticsearch named both of its bars after the repository.
func TestAReleaseNobodyDownloadedIsNoBar(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		p := mustPanel(t, rendered(t, store.Name), "Downloads by release")
		text := queriesOf(p)
		var want string
		switch store.Name {
		case "influxdb", "postgres":
			want = "downloads > 0"
		case "prometheus":
			want = ") > 0)"
		case "graphite":
			want = "removeBelowValue(" + rp("gh_release", "downloads") + ", 1)"
		case "elasticsearch":
			raw, err := json.Marshal(p["transformations"])
			if err != nil {
				t.Fatal(err)
			}
			text, want = string(raw), `"fieldName":"Downloads"`
			if !strings.Contains(text, `"id":"greater","options":{"value":0}`) {
				t.Errorf("elasticsearch keeps no filter of the releases above 0:\n%s", text)
			}
			// A bar chart names its bars by its first string field, and the
			// repository's bucket comes ahead of the tag's: both of the
			// suite's releases were bars called hello-world.
			if !strings.Contains(text, `"indexByName":{"Release":0}`) {
				t.Errorf("elasticsearch leaves the repository ahead of the tag, so the bars are "+
					"named by repository:\n%s", text)
			}
		}
		if !strings.Contains(text, want) {
			t.Errorf("%s: Downloads by release lacks %s, so it draws a release nobody downloaded:\n%s",
				store.Name, want, text)
		}
	}
}
