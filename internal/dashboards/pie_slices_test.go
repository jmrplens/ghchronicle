package dashboards

import (
	"slices"
	"testing"
)

// TestEventsByTypeDrawsEachTypeAsASliceOfItsOwn is "Events by type" in the
// 2.6.1 review: every slice the same green in all five stores, and every
// slice named "Events" in Graphite and Elasticsearch. The query answers a row
// per type, and a pie of rows is one field, which the palette colors once,
// and names each row by the field's display name wherever a rename gave it
// one. So in every store the last transformation makes each row a field of
// its own, named by the type and valued by the count, out of the two columns
// the store's query or renames call Type and Events.
func TestEventsByTypeDrawsEachTypeAsASliceOfItsOwn(t *testing.T) {
	t.Parallel()
	for _, store := range AllStores() {
		p := mustPanel(t, rendered(t, store.Name), "Events by type")
		tfs, _ := p["transformations"].([]any)
		if len(tfs) == 0 {
			t.Errorf("%s: Events by type has no transformation, so its rows are one field", store.Name)
			continue
		}
		last, _ := tfs[len(tfs)-1].(map[string]any)
		options, _ := last["options"].(map[string]any)
		mappings, _ := options["mappings"].([]any)
		roles := map[string]string{}
		for _, raw := range mappings {
			m, _ := raw.(map[string]any)
			name, _ := m["fieldName"].(string)
			handler, _ := m["handlerKey"].(string)
			roles[handler] = name
		}
		if last["id"] != "rowsToFields" || roles["field.name"] != "Type" || roles["field.value"] != "Events" {
			t.Errorf("%s: Events by type ends with %v, want each row made a field named by "+
				"Type and valued by Events", store.Name, last)
			continue
		}
		names := drawnNames(p)
		for _, column := range []string{"Type", "Events"} {
			if !names[column] {
				t.Errorf("%s: the slices are made of a %s column the query does not draw: it draws %v",
					store.Name, column, slices.Sorted(keysOf(names)))
			}
		}
	}
}
