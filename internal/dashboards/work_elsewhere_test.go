package dashboards

import (
	"strings"
	"testing"
)

// TestWorkElsewhereBreaksATieTheSameWayInBothSQLStores holds the newest row
// of an item to one choice when two of its rows share an instant: InfluxDB and
// PostgreSQL drew the suite's pull request as open in one and closed in the
// other, a different one each run.
func TestWorkElsewhereBreaksATieTheSameWayInBothSQLStores(t *testing.T) {
	t.Parallel()
	for _, store := range []string{"influxdb", "postgres"} {
		sql := queriesOf(mustPanel(t, rendered(t, store), "Work elsewhere"))
		if !strings.Contains(sql, "PARTITION BY full_name, kind, number ORDER BY time DESC, state)") {
			t.Errorf("%s picks the newest row of an item with nothing to break a tie on the instant:\n%s",
				store, sql)
		}
	}
}
