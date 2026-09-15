package dashboards

import "testing"

// TestBuildWithBindsTheDatasourceItIsGiven: cmd/publish_dashboard builds a
// dashboard against a datasource that already exists, so the repository
// variable and every panel must read that one and not the import input the
// exported file names, and the store's own variable must stay as it was for
// the next build.
func TestBuildWithBindsTheDatasourceItIsGiven(t *testing.T) {
	t.Parallel()
	s, ok := ByName("influxdb")
	if !ok {
		t.Fatal("no influxdb store")
	}
	ds := map[string]any{"type": "influxdb", "uid": "live"}
	doc := s.BuildWith(ds, nil)
	templating, _ := doc["templating"].(map[string]any)
	list, _ := templating["list"].([]any)
	variable, _ := list[0].(map[string]any)
	if got, _ := variable["datasource"].(map[string]any); got["uid"] != "live" {
		t.Errorf("the repository variable reads %v, want the datasource given", variable["datasource"])
	}
	if s.Variable["datasource"] != "${DS_INFLUXDB}" {
		t.Errorf("building changed the store's own variable to %v", s.Variable["datasource"])
	}
	bound := 0
	for _, p := range doc["panels"].([]map[string]any) {
		if got, isMap := p["datasource"].(map[string]any); isMap && got["uid"] == "live" {
			bound++
		}
		if p["datasource"] == "${DS_INFLUXDB}" {
			t.Errorf("panel %v still reads the import input", p["title"])
		}
	}
	if bound == 0 {
		t.Error("no panel reads the datasource given")
	}
	if _, found := ByName("no such store"); found {
		t.Error("ByName found a store that does not exist")
	}
}
