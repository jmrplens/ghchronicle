package migrate

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
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
	var refilled []Chosen
	var log bytes.Buffer
	when := time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC)
	a := Applying{
		State: in.State, Save: func() error { saves++; return nil },
		Appliers: map[string]Applier{
			"influxdb": influx, "postgres": postgres,
			"graphite": Instructions{}, "telegraf": Instructions{},
		},
		Refill: func(_ context.Context, cleared []Chosen) error { refilled = cleared; return nil },
		Now:    func() time.Time { return when }, Log: slog.New(slog.NewTextHandler(&log, nil)),
		Resume: "run the same command again",
	}
	results, err := a.Apply(t.Context(), chosen)
	if err != nil {
		t.Fatalf("the refill failed: %v", err)
	}
	var ok, failed []string
	for _, r := range results {
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
	if saves != len(wantOK) {
		t.Errorf("the state file was saved %d times, want once per item applied (%d)", saves, len(wantOK))
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
	// Telegraf's history is read again through a backfill its reader runs,
	// so it is not handed to the refill; a failed item has cleared nothing.
	if got := chosenIDs(refilled); !slices.Equal(got, []string{
		"influxdb:" + scanning, "influxdb:" + comments,
		"graphite:" + comments,
	}) {
		t.Errorf("the refill was handed %v", got)
	}
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

// TestApplyingWithNoRefillSaysTheHistoryIsOwed: a build that cleared a store
// and cannot read it again says so rather than reporting success; one that
// cleared nothing owes nothing.
func TestApplyingWithNoRefillSaysTheHistoryIsOwed(t *testing.T) {
	t.Parallel()
	state := &run.State{Stores: map[string]*run.StoreRecord{}}
	a := Applying{
		State: state, Save: func() error { return nil }, Log: slog.New(slog.DiscardHandler),
		Appliers: map[string]Applier{"influxdb": &fakeApplier{}, "telegraf": Instructions{}},
	}
	cleared := Chosen{Store: "influxdb", Item: Item{Migration: Registry[3], Refill: []string{"outbound"}}}
	if _, err := a.Apply(t.Context(), []Chosen{cleared}); !errors.Is(err, ErrNoRefill) {
		t.Errorf("a store cleared with no refill = %v, want ErrNoRefill", err)
	}
	said := Chosen{Store: "telegraf", Item: Item{Migration: Registry[3]}}
	if _, err := a.Apply(t.Context(), []Chosen{said}); err != nil {
		t.Errorf("an item that cleared nothing owes a refill: %v", err)
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
		State: &run.State{Stores: map[string]*run.StoreRecord{}}, Save: func() error { return nil },
		Log: slog.New(slog.DiscardHandler), Appliers: map[string]Applier{"influxdb": store},
	}
	results, _ := a.Apply(ctx, []Chosen{{Store: "influxdb", Item: Item{Migration: Registry[3]}}})
	if len(store.done) != 0 || len(results) != 1 || !errors.Is(results[0].Err, context.Canceled) {
		t.Errorf("a stopped run applied %v, results %+v", store.done, results)
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
	r := Report{Results: []Result{applied, graphite}, Others: "-migrate-others", Resume: "Run it again."}
	r.Print(&out)
	if r.Failed() || !strings.HasSuffix(out.String(), "\n2 applied.\n") ||
		!strings.Contains(out.String(), "the old rows are kept as gh_discussion_comment-20261001T091004") ||
		!strings.Contains(out.String(), "                find x -delete\n") ||
		!strings.Contains(out.String(), "the history of every store cleared was read again") {
		t.Errorf("a report of everything applied:\n%s", out.String())
	}

	out.Reset()
	r = Report{
		Results: []Result{applied, failed}, Held: []Chosen{held}, Refill: errors.New("rate limited"),
		Unreached: []string{"loki did not answer"}, Others: "-migrate-others", Resume: "Run it again.",
	}
	r.Print(&out)
	for _, want := range []string{
		"  failed      " + Registry[0].ID + " in postgres: lock timeout\n",
		"  held back   " + Registry[3].ID + " in elasticsearch: it holds rows of hubot, which this configuration " +
			"does not collect; -migrate-others applies it anyway\n",
		"  unreachable loki did not answer\n",
		"reading the history again did not finish: rate limited",
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
}
