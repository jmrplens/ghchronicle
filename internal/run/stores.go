package run

import (
	"slices"
	"time"
)

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
	// SetAside is every copy of a measurement's rows a migration kept out of
	// the way in this store and nobody has purged yet: the copy's name is
	// what undoing the migration needs, and its instant is when it falls due
	// to be purged.
	SetAside []Aside `json:"set_aside,omitempty"`
	// Refill is the history a migration cleared out of this store and has
	// not read back yet, nil when none is owed. It is recorded with the
	// migration, before anything is read, and goes when the reading ends
	// complete. A store cleared and then left looks, to anyone who asks it,
	// exactly like one that never held the old shape: this is the only thing
	// that says its history is still to come.
	Refill *Refill `json:"refill,omitempty"`
	// Former is the records of the stores this sink pointed at before, kept
	// while they still owe something there: a refill, or a copy set aside. A
	// record replaced for a new destination took those with it, and the
	// store it was about was then never read back; a URL written with a
	// trailing slash was enough. Taken back if the sink points there again.
	Former []*StoreRecord `json:"former,omitempty"`
}

// Owes says whether the record names something still to do in its store: a
// refill owed, or a copy set aside and not yet purged.
func (r *StoreRecord) Owes() bool { return r != nil && (r.Refill != nil || len(r.SetAside) > 0) }

// Refill is what a store is owed after a migration cleared it.
type Refill struct {
	// Migrations is the IDs of the migrations whose clearing is owed.
	Migrations []string `json:"migrations"`
	// Families is every family that writes what was cleared.
	Families []string `json:"families"`
	// Measurements is what was cleared, and the only thing written back.
	Measurements []string `json:"measurements"`
	// Since is how far back the reading goes: the day of the oldest row the
	// store held. Zero is no bound.
	Since time.Time `json:"since,omitzero"`
}

// OweRefill records that a migration cleared a measurement here and that the
// families named are to read it back as far as since. Owed twice, the
// reading covers both: every family, every measurement, and the further
// back of the two bounds, since a bound later than a row the store held is
// that row lost.
func (r *StoreRecord) OweRefill(migration, measurement string, families []string, since time.Time) {
	if r.Refill == nil {
		r.Refill = &Refill{Since: since.UTC()}
	} else if since.IsZero() || since.Before(r.Refill.Since) {
		r.Refill.Since = since.UTC()
	}
	r.Refill.Migrations = sortedUnion(r.Refill.Migrations, migration)
	r.Refill.Measurements = sortedUnion(r.Refill.Measurements, measurement)
	r.Refill.Families = sortedUnion(r.Refill.Families, families...)
}

// Clone is a copy of the refill, nil for none.
func (r *Refill) Clone() *Refill {
	if r == nil {
		return nil
	}
	return &Refill{
		Migrations: slices.Clone(r.Migrations), Families: slices.Clone(r.Families),
		Measurements: slices.Clone(r.Measurements), Since: r.Since,
	}
}

// sortedUnion is list with more added, sorted, each once.
func sortedUnion(list []string, more ...string) []string {
	out := append(slices.Clone(list), more...)
	slices.Sort(out)
	return slices.Compact(out)
}

// Aside is one copy of a measurement's rows a migration set aside.
type Aside struct {
	Name        string    `json:"name"`
	Measurement string    `json:"measurement"`
	Migration   string    `json:"migration,omitempty"`
	At          time.Time `json:"at"`
	// ByServer says the store purges the copy itself, InfluxDB 3's soft
	// delete, so the record is only forgotten once it falls due.
	ByServer bool `json:"by_server,omitempty"`
	// Until is when the store said it purges the copy itself, zero when it
	// did not say.
	Until time.Time `json:"until,omitzero"`
	// ForGood says the store never purges the copy, InfluxDB 3 before 3.2.
	// The record still forgets it when a copy the server purges would fall
	// due: -migrate asks the server for the copies it keeps for good.
	ForGood bool `json:"for_good,omitempty"`
}

// KeepAside records a copy, replacing a record of the same name.
func (r *StoreRecord) KeepAside(a Aside) {
	a.At = a.At.UTC()
	r.DropAside(a.Name)
	r.SetAside = append(r.SetAside, a)
}

// DropAside forgets a copy that was purged or is the store's own to purge.
func (r *StoreRecord) DropAside(name string) {
	r.SetAside = slices.DeleteFunc(r.SetAside, func(a Aside) bool { return a.Name == name })
	if len(r.SetAside) == 0 {
		r.SetAside = nil
	}
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
