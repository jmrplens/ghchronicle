package dashboards

import "strings"

// dsPrefix is what an exported dashboard carries in place of a datasource.
// Every string starting with it is one of those placeholders, wherever it sits.
const dsPrefix = "${DS_"

// Publishable is the store's dashboard bound to the datasource at uid, in the
// shape a live server takes rather than the shape an importer does. The
// committed JSON carries the ${DS_*} placeholder and an __inputs block that
// asks the importer to pick a datasource, which is exactly what a server
// cannot do for itself.
//
// A Loki uid, when given, is the second datasource the failed job output panel
// is drawn from; without one that panel stays the text that says where the
// lines went, because a reader may have no log store.
func Publishable(store *Store, uid, lokiUID string) map[string]any {
	ds := map[string]any{"type": PluginID(store), "uid": uid}
	var logs any
	if lokiUID != "" {
		logs = map[string]any{"type": "loki", "uid": lokiUID}
	}
	doc := store.BuildWith(ds, logs)
	// The builder puts the datasource where it knows about one, but a target
	// can carry the placeholder in a field of its own, so sweep the document.
	substituted, _ := substitute(doc, ds).(map[string]any)
	// Both blocks only mean something in an exported file: they ask the
	// importer for a datasource that is already chosen here.
	delete(substituted, "__inputs")
	delete(substituted, "__requires")
	return substituted
}

// substitute replaces the placeholder wherever it appears, including inside
// targets, and leaves everything else as it found it.
func substitute(v, ds any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, item := range value {
			out[k] = substitute(item, ds)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = substitute(item, ds)
		}
		return out
	case []map[string]any:
		// The builder keeps panel lists in their concrete type.
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = substitute(item, ds)
		}
		return out
	case string:
		if strings.HasPrefix(value, dsPrefix) {
			return ds
		}
		return value
	default:
		return v
	}
}

// PluginID is the datasource type Grafana expects for a store. The export's
// own __requires block already names it, which is more reliable than mapping
// the store name by hand: PostgreSQL is `grafana-postgresql-datasource`.
func PluginID(s *Store) string {
	for _, raw := range s.Requires {
		entry, ok := raw.(map[string]any)
		if !ok || entry["type"] != "datasource" {
			continue
		}
		if id, _ := entry["id"].(string); id != "" {
			return id
		}
	}
	return s.Name
}
