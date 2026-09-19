package dashboards

import (
	"fmt"
	"strings"
	"testing"
)

// TestTheUnboundedPrometheusTablesAreCapped: a SQL table ends in LIMIT and
// shows what it kept; its Prometheus twin has no such clause and returns
// every series. The review of 2026-09-14 counted what that became once the
// backfill brought the forks in, and the number beside each title here is
// what that panel returned at every one of the four ranges it was run at.
// The cap is the number the SQL twin keeps, except Stale branches, whose SQL
// twin is filtered rather than limited.
func TestTheUnboundedPrometheusTablesAreCapped(t *testing.T) {
	t.Parallel()
	capped := map[string]struct {
		n      int
		series int
	}{
		"Commits by author":           {20, 12826},
		"Workflows":                   {30, 878},
		"Workflows that keep failing": {20, 688},
		"Stale branches":              {50, 543},
		"Slowest jobs":                {30, 496},
		"Labels":                      {25, 291},
	}
	seen := map[string]bool{}
	b := &builder{}
	for _, sec := range Sections {
		for _, p := range sec.Build(b) {
			want, ok := capped[p.Title]
			if !ok {
				continue
			}
			seen[p.Title] = true
			st := p.Stores["prometheus"]
			top := fmt.Sprintf("topk(%d,", want.n)
			for i := range st.Q {
				if !strings.Contains(st.Q[i].Expr, top) {
					t.Errorf("%q query %s returned %d series and still has no topk(%d): %s",
						p.Title, st.Q[i].Ref, want.series, want.n, st.Q[i].Expr)
				}
			}
			// A merged table joins its queries on their labels, so every
			// query but the ranking one is intersected onto the rows the
			// ranking one kept. Without that each column picks its own top
			// set and the merge shows rows whose other columns are empty.
			for i := 1; i < len(st.Q); i++ {
				if !strings.Contains(st.Q[i].Expr, " and on (") {
					t.Errorf("%q query %s is capped on its own ranking, not on the table's: %s",
						p.Title, st.Q[i].Ref, st.Q[i].Expr)
				}
			}
		}
	}
	for title := range capped {
		if !seen[title] {
			t.Errorf("no panel is called %q, so this checked nothing for it", title)
		}
	}
}
