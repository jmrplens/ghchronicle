package migrate

import (
	"context"
	"fmt"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// Clearing is the Applier of a store that clears a measurement itself:
// InfluxDB 3 soft deletes the table, PostgreSQL renames it, Elasticsearch
// clones the index and deletes it, each keeping the old rows for a day under
// a name of their own, and InfluxDB 2 deletes them.
type Clearing struct {
	Store teardown.Clearer
	// Forget, when set, is told the measurement once the store no longer
	// holds it, for the sink of this process to forget the table it
	// declared: PostgreSQL's would otherwise write to the copy's old name
	// and be refused once before it declared the table again.
	Forget func(measurement string)
	Now    func() time.Time
}

// Apply clears the item's measurement.
func (c Clearing) Apply(ctx context.Context, it Item) (Outcome, error) {
	m := it.Migration.Measurement
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	aside, held, err := c.Store.Clear(ctx, m, now())
	if err != nil {
		// A copy the clear made before it failed is still a copy, and only
		// the record purges it.
		return Outcome{Aside: aside.Name, Kept: kept(it, aside)}, err
	}
	if c.Forget != nil {
		c.Forget(m)
	}
	out := Outcome{Aside: aside.Name}
	switch {
	case !held:
		out.Did = "the store held no " + m + " any more, so nothing was set aside"
	case aside.ByServer && aside.Name == "":
		out.Did = "InfluxDB deleted the table " + m + ", and listed no copy of it afterwards"
	case aside.ByServer:
		out.Did = "InfluxDB set the table " + m + " aside, and purges it itself from " +
			PurgedFrom(run.Aside{At: aside.At, ByServer: true, Until: aside.Until}).UTC().Format(timeLayout)
	case aside.Name == "":
		out.Did = "every row of " + m + " was deleted: InfluxDB 2 keeps no copy"
	default:
		out.Did = "set " + m + " aside, and ghchronicle purges the copy once 24 hours have passed"
	}
	out.Kept = kept(it, aside)
	return out, nil
}

// kept is the record of the copy a clear made, nil when it made none.
func kept(it Item, aside teardown.Aside) *run.Aside {
	if aside.Name == "" {
		return nil
	}
	return &run.Aside{
		Name: aside.Name, Measurement: it.Migration.Measurement, Migration: it.Migration.ID, At: aside.At,
		ByServer: aside.ByServer, Until: aside.Until,
	}
}

// PurgedFrom is when a copy falls due: when the store said it purges it,
// or, where it did not say, a day after it was made for a copy ghchronicle
// purges and InfluxDB 3's own default for one the server does.
func PurgedFrom(a run.Aside) time.Time {
	switch {
	case !a.Until.IsZero():
		return a.Until
	case a.ByServer:
		return a.At.Add(teardown.ServerKeeps)
	}
	return a.At.Add(teardown.Grace)
}

// Dropper is a sink that can write the drop of a table into what it writes.
type Dropper interface {
	// Drop writes DROP TABLE IF EXISTS for the measurement, and forgets
	// the table, so that its next rows declare it again after the drop.
	Drop(measurement string) error
}

// Dropping is the Applier of the SQL file: the drop goes into the stream,
// ahead of the rows read again, so that replayed in order the target loses
// the table in the old shape and gains it in the new one. The file is only
// written by the process that holds the state file, which is why nothing but
// -migrate -yes, and a start that holds it, applies this.
type Dropping struct {
	Sink Dropper
	// Path is the file, for the sentence that says where the drop went.
	Path string
}

// Apply writes the drop.
func (d Dropping) Apply(_ context.Context, it Item) (Outcome, error) {
	m := it.Migration.Measurement
	if err := d.Sink.Drop(m); err != nil {
		return Outcome{}, err
	}
	return Outcome{Did: fmt.Sprintf(`wrote DROP TABLE IF EXISTS %q; into %s, ahead of the rows read again`, m, d.Path)}, nil
}
