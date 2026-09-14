package run

import (
	"time"

	"github.com/jmrplens/ghchronicle/internal/collect"
)

// refusalsFor is the memory of "not available" answers for one family, made
// on first use.
//
// A 403 or 404 carries no ETag, so a feature that is switched off is charged
// on every sweep: measured on 2026-09-11, 589 core points a day across the
// security, analyses, inventory and deps families, a fifth of the daily
// spend. A day is the window because the author knows when he switches
// Dependabot or code scanning on for a repository, and a restart empties the
// memory anyway. A backfill is run deliberately and asks everything, so it
// does not consult it.
func (r *Runner) refusalsFor(family string) *collect.Refusals {
	if r.Backfill {
		return nil
	}
	if r.refusals == nil {
		r.refusals = map[string]*collect.Refusals{}
	}
	m, ok := r.refusals[family]
	if !ok {
		m = &collect.Refusals{For: 24 * time.Hour}
		r.refusals[family] = m
	}
	return m
}
