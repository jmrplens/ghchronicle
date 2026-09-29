package migrate

import (
	"slices"
	"strings"

	"github.com/jmrplens/ghchronicle/v2/internal/run"
)

// Salt is what the write ledger mixes into the identity of a measurement's
// points in the store a record is kept for: the migrations of it applied
// there, or whose refill is owed there. Each one changes it once, which makes
// the ledger forget that measurement in that store and nothing else, and it
// then stays the same at every start. Empty for a measurement no migration
// was applied to, whose identity is then the one it always had.
//
// A refill owed is in it because it is recorded before the store is touched,
// and a clear whose answer did not arrive may have emptied the store without
// being recorded as applied: the refill that follows would otherwise be held
// back from writing every row the ledger remembers writing before.
func Salt(rec *run.StoreRecord, measurement string) string {
	if rec == nil {
		return ""
	}
	var ids []string
	for _, m := range Registry {
		_, applied := rec.Applied[m.ID]
		owed := rec.Refill != nil && slices.Contains(rec.Refill.Migrations, m.ID)
		if (applied || owed) && m.Measurement == measurement {
			ids = append(ids, m.ID)
		}
	}
	slices.Sort(ids)
	return strings.Join(ids, " ")
}

// Salts is Salt for every store and measurement a migration was applied to,
// by the sink's name and then the measurement's.
func Salts(state *run.State) map[string]map[string]string {
	out := map[string]map[string]string{}
	for name, rec := range state.Stores {
		for _, m := range Registry {
			if salt := Salt(rec, m.Measurement); salt != "" {
				if out[name] == nil {
					out[name] = map[string]string{}
				}
				out[name][m.Measurement] = salt
			}
		}
	}
	return out
}
