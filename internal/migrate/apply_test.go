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
)

// fakeApplier sets aside what it is given, or fails with err, and keeps what
// it was asked to do.
type fakeApplier struct {
	err  error
	done []string
}

func (f *fakeApplier) Apply(_ context.Context, it Item) (Outcome, error) {
	if f.err != nil {
		return Outcome{}, f.err
	}
	f.done = append(f.done, it.Migration.ID)
	return Outcome{Did: "set aside", Aside: it.Migration.Measurement + "-20261001T091004"}, nil
}

// TestApplyingRecordsEachItemAndRefillsOnlyWhatItCleared: every item is
// applied in turn and recorded as it goes, with a save after each; one that
// fails is reported, recorded as nothing, and does not stop the next; and
// the refill is handed the items that cleared a store and nothing else.
func TestApplyingRecordsEachItemAndRefillsOnlyWhatItCleared(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, oldShape(), oldShape("octocat", "hubot"), nil)
	Stamp(in.State, in.Config, in.Release)
	chosen, held := Make(t.Context(), in).Pending(false)
	if got := chosenIDs(held); !slices.Equal(got, []string{"postgres:" + comments}) {
		t.Fatalf("held back %v, want the store holding hubot's comments alone", got)
	}
	influx, postgres := &fakeApplier{}, &fakeApplier{err: errors.New("lock timeout")}
	saves := 0
	var refilled []RefillWalk
	var owedAtRefill map[string]*run.Refill
	var log bytes.Buffer
	when := time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC)
	a := Applying{
		Config: in.Config, State: in.State, Save: func() error { saves++; return nil },
		Appliers: map[string]Applier{
			"influxdb": influx, "postgres": postgres,
			"graphite": Instructions{}, "telegraf": Instructions{},
		},
		Refill: func(_ context.Context, w RefillWalk) error {
			refilled = append(refilled, w)
			owedAtRefill = map[string]*run.Refill{}
			for name, rec := range in.State.Stores {
				owedAtRefill[name] = rec.Refill
			}
			return nil
		},
		Now: func() time.Time { return when }, Log: slog.New(slog.NewTextHandler(&log, nil)),
		Resume: "run the same command again",
	}
	done, err := a.Apply(t.Context(), chosen)
	if err != nil {
		t.Fatalf("the refill failed: %v", err)
	}
	var ok, failed []string
	for _, r := range done.Results {
		id := r.Store + ":" + r.Item.Migration.ID
		if r.Err != nil {
			failed = append(failed, id)
			continue
		}
		ok = append(ok, id)
	}
	wantOK := []string{"influxdb:" + scanning, "influxdb:" + comments, "telegraf:" + comments, "graphite:" + comments}
	wantFailed := []string{"sql:" + comments, "postgres:" + scanning}
	if !slices.Equal(ok, wantOK) || !slices.Equal(failed, wantFailed) {
		t.Errorf("applied %v and failed %v, want %v and %v", ok, failed, wantOK, wantFailed)
	}
	// Once per item applied, and once more when the refill ended and was
	// no longer owed.
	if saves != len(wantOK)+1 {
		t.Errorf("the state file was saved %d times, want once per item applied (%d) and once after the refill",
			saves, len(wantOK))
	}
	for _, id := range wantOK {
		store, mig, _ := strings.Cut(id, ":")
		if got := in.State.Stores[store].Applied[mig]; !got.Equal(when) {
			t.Errorf("%s is recorded as applied at %v, want %v", id, got, when)
		}
	}
	for _, id := range wantFailed {
		store, mig, _ := strings.Cut(id, ":")
		if _, recorded := in.State.Stores[store].Applied[mig]; recorded {
			t.Errorf("%s failed and is recorded as applied", id)
		}
	}
	checkOneRefill(t, refilled, owedAtRefill, in.State)
	for _, want := range []string{
		`level=ERROR msg="migration failed" sink=postgres`, `err="lock timeout" resume="run the same command again"`,
		`sink=sql`, `this build has no way to bring sql along`,
		`level=INFO msg="migration applied" sink=influxdb measurement=gh_discussion_comment`,
		`aside=gh_discussion_comment-20261001T091004`,
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log does not say %q:\n%s", want, log.String())
		}
	}
}

// checkOneRefill holds the refill of TestApplyingRecordsEachItemAndRefillsOnlyWhatItCleared
// to what the items applied owed.
func checkOneRefill(t *testing.T, refilled []RefillWalk, owedAtRefill map[string]*run.Refill, state *run.State) {
	t.Helper()
	// Telegraf's history is read again through a backfill its reader runs,
	// so it owes no refill; a failed item has cleared nothing. One walk pays
	// every store, and writes to each what was cleared there.
	if len(refilled) != 1 {
		t.Fatalf("the refill ran %d times, want once", len(refilled))
	}
	w := refilled[0]
	wantKeep := map[string][]string{
		"influxdb": {"gh_code_scanning_alert_item", "gh_discussion_comment"},
		"graphite": {"gh_discussion_comment"},
	}
	if !maps.EqualFunc(w.Keep, wantKeep, slices.Equal) ||
		!slices.Equal(w.Families, []string{"discussions", "outbound", "security"}) || !w.Since.IsZero() {
		t.Errorf("the refill walks %+v, want %v from discussions, outbound and security with no bound, "+
			"which Graphite's backfill.since, unset, gives", w, wantKeep)
	}
	for _, store := range []string{"influxdb", "graphite"} {
		if owedAtRefill[store] == nil {
			t.Errorf("%s was not recorded as owed a refill before it began", store)
		}
		if state.Stores[store].Refill != nil {
			t.Errorf("%s is still owed a refill that ended complete", store)
		}
	}
	for _, store := range []string{"telegraf", "postgres", "sql"} {
		if owedAtRefill[store] != nil {
			t.Errorf("%s is owed a refill: %+v", store, owedAtRefill[store])
		}
	}
}

// TestApplyingWithNoRefillSaysTheHistoryIsOwed: a build that cleared a store
// and cannot read it again says so rather than reporting success; one that
// cleared nothing owes nothing.
func TestApplyingWithNoRefillSaysTheHistoryIsOwed(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, oldShape(), nil, nil)
	Stamp(in.State, in.Config, in.Release)
	chosen, _ := Make(t.Context(), in).Pending(false)
	pick := func(store string) []Chosen {
		return slices.DeleteFunc(slices.Clone(chosen), func(c Chosen) bool {
			return c.Store != store || c.Item.Migration.ID != comments
		})
	}
	a := Applying{
		Config: in.Config, State: in.State, Save: func() error { return nil }, Log: slog.New(slog.DiscardHandler),
		Appliers: map[string]Applier{"influxdb": &fakeApplier{}, "telegraf": Instructions{}},
	}
	if _, err := a.Apply(t.Context(), pick("telegraf")); err != nil {
		t.Errorf("an item that cleared nothing owes a refill: %v", err)
	}
	done, err := a.Apply(t.Context(), pick("influxdb"))
	if !errors.Is(err, ErrNoRefill) || done.Refill == nil || !errors.Is(done.Refill.Err, ErrNoRefill) {
		t.Errorf("a store cleared with no refill = %v, want ErrNoRefill", err)
	}
	if in.State.Stores["influxdb"].Refill == nil {
		t.Error("the store cleared is not recorded as owed its history")
	}
}

// TestApplyingStopsTouchingStoresOnceItIsStopped: a context that has ended
// applies nothing more, and says each item as failed so that a rerun picks
// it up.
func TestApplyingStopsTouchingStoresOnceItIsStopped(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	store := &fakeApplier{}
	a := Applying{
		Config: planConfig(t, t.TempDir(), nil),
		State:  &run.State{Stores: map[string]*run.StoreRecord{}}, Save: func() error { return nil },
		Log: slog.New(slog.DiscardHandler), Appliers: map[string]Applier{"influxdb": store},
	}
	done, _ := a.Apply(ctx, []Chosen{{Store: "influxdb", Item: Item{Migration: Registry[3]}}})
	if len(store.done) != 0 || len(done.Results) != 1 || !errors.Is(done.Results[0].Err, context.Canceled) {
		t.Errorf("a stopped run applied %v, results %+v", store.done, done.Results)
	}
}

// TestTheReportCountsWhatWasDoneAndSaysHowToGoOn: the report of -migrate -yes
// names each item applied, failed or held back, and fails whenever anything
// is left, with the sentence that says how to go on.
func TestTheReportCountsWhatWasDoneAndSaysHowToGoOn(t *testing.T) {
	t.Parallel()
	applied := Result{
		Store: "influxdb", Item: Item{Migration: Registry[3], Refill: []string{"outbound"}},
		Outcome: Outcome{Did: "set aside", Aside: "gh_discussion_comment-20261001T091004"},
	}
	graphite := Result{
		Store: "graphite", Item: Item{Migration: Registry[3]},
		Outcome: Outcome{Did: "on the Graphite host, remove the old paths:", Commands: []string{"find x -delete"}},
	}
	failed := Result{Store: "postgres", Item: Item{Migration: Registry[0]}, Err: errors.New("lock\ntimeout")}
	held := Chosen{Store: "elasticsearch", Item: Item{Migration: Registry[3], Others: []string{"hubot"}}}

	var out bytes.Buffer
	refilled := &Refilled{
		Walk: RefillWalk{
			Families: []string{"discussions", "outbound"}, Since: time.Date(2023, 11, 14, 0, 0, 0, 0, time.UTC),
			Keep: map[string][]string{"influxdb": {"gh_discussion_comment"}, "graphite": {"gh_discussion_comment"}},
		},
		Reconciled: []Reconciliation{
			{Store: "influxdb", Measurement: "gh_discussion_comment", Aside: "gh_discussion_comment-20261001T091004", Before: 3, After: 3},
			{
				Store: "postgres", Measurement: "gh_discussion_comment", Aside: "gh_discussion_comment-20261001T091004",
				Before: 3, After: 2, Gone: []string{"DC_gone"},
			},
			{Store: "elasticsearch", Measurement: "gh_discussion_comment", Aside: "x", Err: errors.New("a\nrefusal")},
		},
	}
	r := Report{Results: []Result{applied, graphite}, Refill: refilled, Others: "-migrate-others", Resume: "Run it again."}
	r.Print(&out)
	for _, want := range []string{
		"the old rows are kept as gh_discussion_comment-20261001T091004",
		"                find x -delete\n",
		"  refill      read discussions and outbound again, since 2023-11-14, writing gh_discussion_comment to " +
			"graphite and influxdb\n",
		"  reconciled  gh_discussion_comment in influxdb: 3 items in gh_discussion_comment-20261001T091004, 3 now; " +
			"GitHub served every one again\n",
		"  reconciled  gh_discussion_comment in postgres: 3 items in gh_discussion_comment-20261001T091004, 2 now; " +
			"1 GitHub no longer serves, whose rows are only in the copy until it is purged: DC_gone\n",
		"  reconciled  gh_discussion_comment in elasticsearch: not compared with x: a refusal\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("a report of everything applied does not say %q:\n%s", want, out.String())
		}
	}
	// What GitHub no longer serves is said, and is not a failure: the
	// migration and the refill did all they can.
	if r.Failed() || !strings.HasSuffix(out.String(), "\n2 applied.\n") {
		t.Errorf("a report of everything applied:\n%s", out.String())
	}

	out.Reset()
	r = Report{
		Results: []Result{applied, failed}, Held: []Chosen{held}, Refill: &Refilled{Err: errors.New("rate limited")},
		Unreached: []string{"loki did not answer"}, Others: "-migrate-others", Resume: "Run it again.",
	}
	r.Print(&out)
	for _, want := range []string{
		"  failed      " + Registry[0].ID + " in postgres: lock timeout\n",
		"  held back   " + Registry[3].ID + " in elasticsearch: it holds rows of hubot, which this configuration " +
			"does not collect; -migrate-others applies it anyway\n",
		"  unreachable loki did not answer\n",
		"reading the history again did not finish, and is still owed: rate limited",
		"\n1 applied, 1 failed, 1 held back, 1 not asked. Run it again.\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report does not say %q:\n%s", want, out.String())
		}
	}
	if !r.Failed() {
		t.Error("a report with something left is not a failure")
	}

	out.Reset()
	Report{}.Print(&out)
	if out.String() != "\nNothing to migrate.\n" {
		t.Errorf("an empty report reads %q", out.String())
	}

	// A run with nothing to apply that paid what an earlier one left owed,
	// into stores cleared of different measurements.
	out.Reset()
	r = Report{Refill: &Refilled{Walk: RefillWalk{
		Families: []string{"discussions", "outbound", "security"},
		Keep: map[string][]string{
			"influxdb": {"gh_code_scanning_alert_item", "gh_discussion_comment"},
			"postgres": {"gh_discussion_comment"}, "sql": {"gh_discussion_comment"},
		},
	}}, Resume: "Run it again."}
	r.Print(&out)
	if want := "\n  refill      read discussions, outbound and security again, with no bound, writing " +
		"gh_code_scanning_alert_item and gh_discussion_comment to influxdb; gh_discussion_comment to postgres and sql\n" +
		"\nNothing to apply, and the refill owed was read.\n"; out.String() != want || r.Failed() {
		t.Errorf("a report of a refill owed reads\n%s\nwant\n%s", out.String(), want)
	}
}
