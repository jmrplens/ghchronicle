package migrate

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// fakeItems answers the items of each table it was given, and fails for
// one it was not.
type fakeItems struct {
	name   string
	tables map[string][]string
	asked  []string
}

func (f *fakeItems) Name() string { return f.name }

func (f *fakeItems) Items(_ context.Context, table string, tags []string) ([]string, error) {
	f.asked = append(f.asked, table+":"+strings.Join(tags, ","))
	items, ok := f.tables[table]
	if !ok {
		return nil, errors.New(table + " is not there")
	}
	return items, nil
}

// owedStores is a state whose influxdb and postgres records owe the comments
// back, each with the copy the migration kept, as -migrate -yes leaves them,
// and a record of a sink since pointed elsewhere that owes one too.
func owedStores(t *testing.T, at time.Time) Input {
	t.Helper()
	in := upgradeInput(t, nil, nil, nil)
	Stamp(in.State, in.Config, in.Release)
	for i, store := range []string{"influxdb", "postgres"} {
		rec := in.State.Stores[store]
		rec.MarkApplied(comments, at)
		since := time.Date(2023, 11, 14+i, 0, 0, 0, 0, time.UTC)
		rec.OweRefill(comments, "gh_discussion_comment", []string{"discussions", "outbound"}, since)
		rec.KeepAside(run.Aside{
			Name: "gh_discussion_comment-20261001T091004", Measurement: "gh_discussion_comment",
			Migration: comments, At: at, ByServer: store == "influxdb",
		})
	}
	in.State.Stores["gone"] = &run.StoreRecord{Destination: "url=http://elsewhere", Refill: &run.Refill{
		Migrations: []string{comments}, Families: []string{"outbound"}, Measurements: []string{"gh_discussion_comment"},
	}}
	in.State.Stores["elasticsearch"].Destination = "url=http://another-cluster prefix=ghchronicle"
	in.State.Stores["elasticsearch"].OweRefill(comments, "gh_discussion_comment", []string{"outbound"}, time.Time{})
	return in
}

// TestTheRefillOwedIsOneWalkIntoTheStoresOwedIt: the stores owed a refill are
// paid by one walk of every family either needs, back to the furthest either
// is owed, writing to each what was cleared there; a record of a store no
// sink points at now is left for when one does.
func TestTheRefillOwedIsOneWalkIntoTheStoresOwedIt(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC)
	in := owedStores(t, at)
	owed := OwedIn(in.State, in.Config)
	var stores []string
	for _, o := range owed {
		stores = append(stores, o.Store)
	}
	if !slices.Equal(stores, []string{"influxdb", "postgres"}) {
		t.Fatalf("owed to %v, want influxdb and postgres alone", stores)
	}
	w := walkOf(owed)
	want := map[string][]string{"influxdb": {"gh_discussion_comment"}, "postgres": {"gh_discussion_comment"}}
	if !slices.Equal(w.Families, []string{"discussions", "outbound"}) || !maps.EqualFunc(w.Keep, want, slices.Equal) ||
		!w.Since.Equal(time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the walk is %+v", w)
	}
}

// refilling is an Applying over owedStores that reads the items of the
// copies from influx and postgres, and saves by counting.
func refilling(t *testing.T, at time.Time, log *bytes.Buffer, saves *int) (Input, Applying, *fakeItems) {
	t.Helper()
	in := owedStores(t, at)
	influx := &fakeItems{name: "influxdb", tables: map[string][]string{
		"gh_discussion_comment-20261001T091004": {"1", "2", "3"},
		"gh_discussion_comment":                 {"1", "3", "4"},
	}}
	postgres := &fakeItems{name: "postgres", tables: map[string][]string{"gh_discussion_comment": {"1"}}}
	return in, Applying{
		Config: in.Config, State: in.State, Save: func() error { *saves++; return nil },
		Readers: map[string]teardown.ItemReader{"influxdb": influx, "postgres": postgres},
		Now:     func() time.Time { return at.Add(time.Hour) }, Log: slog.New(slog.NewTextHandler(log, nil)),
		Resume: "run it again",
	}, influx
}

// TestARefillThatDidNotEndIsStillOwed, and nothing is compared with a copy
// before the history is all back.
func TestARefillThatDidNotEndIsStillOwed(t *testing.T) {
	t.Parallel()
	var log bytes.Buffer
	saves := 0
	in, a, influx := refilling(t, time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC), &log, &saves)
	a.Refill = func(context.Context, RefillWalk) error { return errors.New("stopped") }
	done, err := a.Apply(t.Context(), nil)
	if err == nil || done.Refill == nil || in.State.Stores["influxdb"].Refill == nil || saves != 0 {
		t.Fatalf("a refill that did not end: %v, %+v, saved %d times", err, done.Refill, saves)
	}
	if len(influx.asked) != 0 {
		t.Errorf("a refill that did not end was compared: %v", influx.asked)
	}
	if !strings.Contains(log.String(), `level=ERROR msg="refill did not finish, and is still owed"`) {
		t.Errorf("the log does not say it is still owed:\n%s", log.String())
	}
}

// TestARefillThatEndedIsForgottenAndCompared: every store it wrote to owes
// nothing any more, a store no sink points at still does, and the
// comparison with each copy names what GitHub no longer served.
func TestARefillThatEndedIsForgottenAndCompared(t *testing.T) {
	t.Parallel()
	var log bytes.Buffer
	saves := 0
	in, a, _ := refilling(t, time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC), &log, &saves)
	a.Refill = func(context.Context, RefillWalk) error { return nil }
	done, err := a.Apply(t.Context(), nil)
	if err != nil || saves != 1 {
		t.Fatalf("a refill that ended: %v, saved %d times", err, saves)
	}
	for _, store := range []string{"influxdb", "postgres"} {
		if in.State.Stores[store].Refill != nil {
			t.Errorf("%s is still owed a refill that ended", store)
		}
	}
	if in.State.Stores["gone"].Refill == nil || in.State.Stores["elasticsearch"].Refill == nil {
		t.Error("a refill owed to a store no sink points at was forgotten by a walk that wrote none of it")
	}
	got := done.Refill.Reconciled
	if len(got) != 2 {
		t.Fatalf("reconciled %+v, want influxdb and postgres", got)
	}
	if r := got[0]; r.Store != "influxdb" || r.Before != 3 || r.After != 3 || !slices.Equal(r.Gone, []string{"2"}) || r.Err != nil {
		t.Errorf("influxdb reconciled as %+v, want comment 2 no longer served", r)
	}
	if r := got[1]; r.Store != "postgres" || r.Err == nil || !strings.Contains(r.Err.Error(), "reading the copy") {
		t.Errorf("postgres, whose copy did not answer, reconciled as %+v", r)
	}
	for _, want := range []string{
		`level=INFO msg="refill complete" families=discussions,outbound measurements=gh_discussion_comment sinks=influxdb,postgres since=2023-11-14`,
		`level=WARN msg=reconciled sink=influxdb measurement=gh_discussion_comment aside=gh_discussion_comment-20261001T091004 items_before=3 items_after=3 not_served=1 first=2`,
		`level=WARN msg="not reconciled: the copy and the table could not be compared" sink=postgres`,
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log does not say %q:\n%s", want, log.String())
		}
	}
}

// TestACopyPastItsDayIsNotCompared: a refill resumed after the copy was due
// to be purged says so rather than asking a store for a table it may have
// dropped, and one whose store keeps no copy is not compared at all.
func TestACopyPastItsDayIsNotCompared(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC)
	in := owedStores(t, at)
	in.State.Stores["postgres"].SetAside = nil
	influx := &fakeItems{name: "influxdb"}
	a := Applying{
		Config: in.Config, State: in.State, Save: func() error { return nil },
		Readers: map[string]teardown.ItemReader{"influxdb": influx, "postgres": &fakeItems{name: "postgres"}},
		Refill:  func(context.Context, RefillWalk) error { return nil },
		Now:     func() time.Time { return at.Add(73 * time.Hour) }, Log: slog.New(slog.DiscardHandler),
	}
	done, err := a.Apply(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := done.Refill.Reconciled; len(got) != 1 || got[0].Store != "influxdb" || got[0].Err == nil ||
		!strings.Contains(got[0].Err.Error(), "due to be purged at 2026-10-04 09:10 UTC") || len(influx.asked) != 0 {
		t.Errorf("reconciled %+v, asked %v", got, influx.asked)
	}
}

// TestThePlanSaysARefillIsOwed: a store cleared and not read back looks, to
// anyone asking it, like a store that never held the old shape, so the plan
// says the refill from the record, and counts it in its last line.
func TestThePlanSaysARefillIsOwed(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC)
	in := owedStores(t, at)
	var out bytes.Buffer
	Make(t.Context(), in).Print(&out)
	for _, want := range []string{
		"  refill owed discussions and outbound, since 2023-11-14, writing gh_discussion_comment: cleared by " +
			comments + " and not read back yet; -migrate -yes, or a start under migrate: auto, reads it\n",
		"2 stores owed a refill. Nothing was changed.\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan does not say %q:\n%s", want, out.String())
		}
	}
}
