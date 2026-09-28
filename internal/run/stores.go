package run

import "time"

// StoreRecord is what the state file remembers about one store, for the one
// question a store that cannot be asked leaves open: whether rows an earlier
// release wrote are in it.
//
// A store that can be asked (InfluxDB, PostgreSQL, Elasticsearch) says itself
// whether it holds a measurement in an old shape, and this record only adds
// what it cannot say: that a migration was applied to it. A SQL file, a
// Graphite and whatever is behind Telegraf cannot be asked, so there the
// release that first wrote the store is the whole answer.
type StoreRecord struct {
	// Destination is where the sink pointed when the record was made, from
	// the settings that name the store and never a credential. A sink pointed
	// somewhere else starts a new record, because what the old one says is
	// about another store.
	Destination string `json:"destination"`
	// FirstWrittenBy is the release that first wrote the store, or empty when
	// that is not known: the store was in use before the state file kept this
	// record, or the sink was pointed at it while the state file already had
	// a history.
	FirstWrittenBy string `json:"first_written_by,omitempty"`
	// WrittenBy is the release that last started against the store. One
	// newer than the running binary is a downgrade.
	WrittenBy string `json:"written_by,omitempty"`
	// Applied is when each migration was applied to this store, by the ID
	// the registry gives it.
	Applied map[string]time.Time `json:"applied,omitempty"`
	// NotNeeded is when a start found the store holding nothing of a
	// migration's old shape. A later start takes the record's word for it
	// and does not ask the store again, which is what makes a start cost the
	// stores nothing once every migration is settled. -migrate still asks.
	NotNeeded map[string]time.Time `json:"not_needed,omitempty"`
	// Noted is when a start said, at Info, a migration nothing can apply
	// here: rows it can only explain, or rows nothing this configuration
	// writes any more. Later starts say it at Debug, since a line at every
	// start about rows that will never change is a line nobody reads.
	Noted map[string]time.Time `json:"noted,omitempty"`
}

// MarkApplied records a migration as applied here, which also ends any
// earlier finding that it was not needed.
func (r *StoreRecord) MarkApplied(id string, when time.Time) {
	r.Applied = marked(r.Applied, id, when)
	delete(r.NotNeeded, id)
}

// MarkNotNeeded records that a start found nothing of a migration's old
// shape here.
func (r *StoreRecord) MarkNotNeeded(id string, when time.Time) {
	r.NotNeeded = marked(r.NotNeeded, id, when)
}

// MarkNoted records that a start has said a migration nothing can apply.
func (r *StoreRecord) MarkNoted(id string, when time.Time) {
	r.Noted = marked(r.Noted, id, when)
}

// marked is m with id set to when, made the first time it is wanted so that
// a record with nothing in it keeps its keys out of the file.
func marked(m map[string]time.Time, id string, when time.Time) map[string]time.Time {
	if m == nil {
		m = map[string]time.Time{}
	}
	m[id] = when.UTC()
	return m
}

// Fresh says whether this file has never recorded anything, which is what a
// first start looks like and what every run of the Action looks like.
func (s *State) Fresh() bool { return len(s.LastRun) == 0 && len(s.Stores) == 0 }
