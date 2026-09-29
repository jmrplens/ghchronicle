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
	for store, tie := range map[string]string{
		"influxdb": "PARTITION BY full_name, kind, number ORDER BY time DESC, state)",
		// The state is text, and PostgreSQL compares it by the database's
		// collation unless told to compare its bytes as InfluxDB does.
		"postgres": `PARTITION BY full_name, kind, number ORDER BY time DESC, state COLLATE "C")`,
	} {
		sql := queriesOf(mustPanel(t, rendered(t, store), "Work elsewhere"))
		if !strings.Contains(sql, tie) {
			t.Errorf("%s picks the newest row of an item with nothing to break a tie on the instant:\n%s",
				store, sql)
		}
	}
}
