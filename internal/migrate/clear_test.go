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
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// fakeClearer clears the way a store that keeps a copy does, or fails.
type fakeClearer struct {
	name    string
	aside   teardown.Aside
	missing bool
	err     error
	cleared []string
}

func (f *fakeClearer) Name() string { return f.name }

func (f *fakeClearer) Clear(_ context.Context, m string, at time.Time) (teardown.Aside, bool, error) {
	if f.err != nil {
		return teardown.Aside{}, false, f.err
	}
	f.cleared = append(f.cleared, m)
	a := f.aside
	if a.Name == "x" {
		a.Name, a.At = m+"-"+at.UTC().Format("20060102T150405"), at.UTC()
	}
	return a, !f.missing, nil
}

// TestClearingKeepsTheRecordOfTheCopy: the copy is named in the outcome with
// the migration that made it, the sink of this process is told to forget
// the table, and a store that deletes keeps no record of a copy.
func TestClearingKeepsTheRecordOfTheCopy(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC)
	it := Item{Migration: Registry[slices.IndexFunc(Registry, func(m Migration) bool { return m.ID == comments })]}
	forgot := []string{}
	pg := &fakeClearer{name: "postgres", aside: teardown.Aside{Name: "x"}}
	out, err := Clearing{Store: pg, Forget: func(m string) { forgot = append(forgot, m) }, Now: func() time.Time { return at }}.
		Apply(t.Context(), it)
	if err != nil {
		t.Fatal(err)
	}
	want := run.Aside{
		Name: "gh_discussion_comment-20261001T091004", Measurement: "gh_discussion_comment",
		Migration: comments, At: at,
	}
	if out.Kept == nil || *out.Kept != want || out.Aside != want.Name {
		t.Errorf("kept %+v, want %+v", out.Kept, want)
	}
	if !slices.Equal(forgot, []string{"gh_discussion_comment"}) {
		t.Errorf("the sink was told to forget %v", forgot)
	}
	influx2 := &fakeClearer{name: "influxdb", aside: teardown.Aside{Measurement: "gh_discussion_comment"}}
	out, err = Clearing{Store: influx2}.Apply(t.Context(), it)
	if err != nil || out.Kept != nil || !strings.Contains(out.Did, "deleted") {
		t.Errorf("a delete: %+v, %v", out, err)
	}
	failing := &fakeClearer{name: "elasticsearch", err: errors.New("403 Forbidden")}
	forgot = nil
	if _, err = (Clearing{Store: failing, Forget: func(m string) { forgot = append(forgot, m) }}).Apply(t.Context(), it); err == nil || len(forgot) != 0 {
		t.Errorf("a store that refused: %v, forgot %v", err, forgot)
	}
}

// TestApplyingRecordsTheCopy with the migration that made it, in the record
// of the store it is in, which is what purges it once its day is over.
func TestApplyingRecordsTheCopy(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, oldShape(), nil, nil)
	Stamp(in.State, in.Config, in.Release)
	chosen, _ := Make(t.Context(), in).Pending(false)
	chosen = slices.DeleteFunc(chosen, func(c Chosen) bool { return c.Store != "influxdb" })
	a := Applying{
		State: in.State, Save: func() error { return nil },
		Appliers: map[string]Applier{"influxdb": Clearing{Store: &fakeClearer{
			name: "influxdb", aside: teardown.Aside{Name: "x", ByServer: true},
		}}},
		Refill: func(context.Context, []Chosen) error { return nil },
		Log:    slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
	if _, err := a.Apply(t.Context(), chosen); err != nil {
		t.Fatal(err)
	}
	kept := in.State.Stores["influxdb"].SetAside
	if len(kept) != 2 || !kept[1].ByServer || kept[1].Migration != comments || !strings.HasPrefix(kept[1].Name, "gh_discussion_comment-") {
		t.Errorf("the record keeps %+v", kept)
	}
}

// dropRecorder is a sink that writes drops.
type dropRecorder struct{ dropped []string }

func (d *dropRecorder) Drop(m string) error { d.dropped = append(d.dropped, m); return nil }

// TestTheSQLFileIsToldToDropTheTable, the one measurement, and says where.
func TestTheSQLFileIsToldToDropTheTable(t *testing.T) {
	t.Parallel()
	d := &dropRecorder{}
	it := Item{Migration: Registry[slices.IndexFunc(Registry, func(m Migration) bool { return m.ID == comments })]}
	out, err := Dropping{Sink: d, Path: "/var/lib/ghchronicle/points.sql"}.Apply(t.Context(), it)
	if err != nil || !slices.Equal(d.dropped, []string{"gh_discussion_comment"}) || out.Kept != nil ||
		!strings.Contains(out.Did, `DROP TABLE IF EXISTS "gh_discussion_comment"; into /var/lib/ghchronicle/points.sql`) {
		t.Errorf("dropped %v: %+v, %v", d.dropped, out, err)
	}
}

// TestThePlanSaysWhatIsKeptAsideAndUntilWhen: the name of each copy and the
// hour it goes are what undoing a migration needs, and -migrate is where a
// reader looks.
func TestThePlanSaysWhatIsKeptAsideAndUntilWhen(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, nil, nil, nil)
	Stamp(in.State, in.Config, in.Release)
	at := time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC)
	in.State.Stores["postgres"].KeepAside(run.Aside{
		Name:        "gh_discussion_comment-20261001T091004",
		Measurement: "gh_discussion_comment", Migration: comments, At: at,
	})
	in.State.Stores["influxdb"].KeepAside(run.Aside{
		Name:        "gh_discussion_comment-20261001T091005",
		Measurement: "gh_discussion_comment", Migration: comments, At: at, ByServer: true,
	})
	var out bytes.Buffer
	Make(t.Context(), in).Print(&out)
	for _, want := range []string{
		"  kept aside  " + comments + ": gh_discussion_comment-20261001T091004, set aside 2026-10-01 09:10 UTC, " +
			"purged by ghchronicle after 2026-10-02 09:10 UTC\n",
		"  kept aside  " + comments + ": gh_discussion_comment-20261001T091005, set aside 2026-10-01 09:10 UTC, " +
			"purged by the store itself after 2026-10-02 09:10 UTC\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan does not say %q:\n%s", want, out.String())
		}
	}
}
