package dashboards

import "testing"

// TestSubstituteReachesEveryPlaceholder replaces the placeholder in every
// container the builder and the decoder produce, and nothing else.
func TestSubstituteReachesEveryPlaceholder(t *testing.T) {
	t.Parallel()
	ds := map[string]any{"uid": "x"}
	doc := map[string]any{
		"panels": []map[string]any{{"datasource": "${DS_INFLUXDB}", "gridPos": 3}},
		"list":   []any{"${DS_X}", "kept", true},
	}
	got, _ := substitute(doc, ds).(map[string]any)
	panels, _ := got["panels"].([]any)
	first, _ := panels[0].(map[string]any)
	if kept, _ := first["datasource"].(map[string]any); kept["uid"] != "x" || first["gridPos"] != 3 {
		t.Errorf("panel = %v, want the datasource bound and the rest kept", first)
	}
	list, _ := got["list"].([]any)
	if bound, _ := list[0].(map[string]any); bound["uid"] != "x" || list[1] != "kept" || list[2] != true {
		t.Errorf("list = %v, want only the placeholder replaced", list)
	}
}

// TestPluginIDReadsTheRequiresBlock names each store's plugin from the export,
// and falls back to the store's own name when the block names none.
func TestPluginIDReadsTheRequiresBlock(t *testing.T) {
	t.Parallel()
	pg, _ := ByName("postgres")
	if got := PluginID(pg); got != "grafana-postgresql-datasource" {
		t.Errorf("PluginID(postgres) = %q, want the plugin its export requires", got)
	}
	bare := &Store{Name: "graphite", Requires: []any{
		"not an entry",
		map[string]any{"type": "grafana", "id": "grafana"},
		map[string]any{"type": "datasource"},
	}}
	if got := PluginID(bare); got != "graphite" {
		t.Errorf("PluginID = %q, want the store's name when no datasource is named", got)
	}
}

// TestPublishableLeavesNothingForAnImporterToAnswer: what goes to a live
// server carries no placeholder and neither of the two blocks that ask an
// importer to choose, because there is nobody there to ask.
func TestPublishableLeavesNothingForAnImporterToAnswer(t *testing.T) {
	t.Parallel()
	store, ok := ByName("influxdb")
	if !ok {
		t.Fatal("no influxdb store")
	}
	doc := Publishable(store, "ds-uid", "")
	if _, found := doc["__inputs"]; found {
		t.Error("__inputs survived, so the server is being asked to pick a datasource")
	}
	if _, found := doc["__requires"]; found {
		t.Error("__requires survived")
	}
	if left := unresolved(doc); left > 0 {
		t.Errorf("%d placeholder(s) left unbound", left)
	}
}

// unresolved counts the datasource placeholders still in the document.
func unresolved(v any) int {
	switch value := v.(type) {
	case map[string]any:
		n := 0
		for _, item := range value {
			n += unresolved(item)
		}
		return n
	case []any:
		n := 0
		for _, item := range value {
			n += unresolved(item)
		}
		return n
	case string:
		if len(value) > len(dsPrefix) && value[:len(dsPrefix)] == dsPrefix {
			return 1
		}
		return 0
	default:
		return 0
	}
}
