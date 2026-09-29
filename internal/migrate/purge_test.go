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

// fakePurger lists and purges copies, and counts every call.
type fakePurger struct {
	name   string
	listed []teardown.Aside
	refuse bool
	calls  []string
}

func (f *fakePurger) Name() string { return f.name }

func (f *fakePurger) Asides(_ context.Context, ms []string) ([]teardown.Aside, error) {
	f.calls = append(f.calls, "list "+strings.Join(ms, ","))
	return f.listed, nil
}

func (f *fakePurger) Purge(_ context.Context, name string) error {
	f.calls = append(f.calls, "purge "+name)
	if f.refuse {
		return errors.New("lock timeout")
	}
	return nil
}

// TestACopyIsPurgedOnceItHasBeenKeptItsDay and not before: the record names
// it, the store purges it, the record forgets it and is saved. A copy the
// server purges is only forgotten, once the day the server named has come or,
// where it named none, once the 72 hours InfluxDB 3 keeps one by default have
// passed; and one not yet due is when the next purge is.
func TestACopyIsPurgedOnceItHasBeenKeptItsDay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := planConfig(t, dir, nil)
	state := run.LoadState("")
	Stamp(state, cfg, "2.6.2")
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	pg := state.Stores["postgres"]
	pg.KeepAside(run.Aside{
		Name: "gh_discussion_comment-20261001T091004", Measurement: "gh_discussion_comment",
		At: time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC),
	})
	pg.KeepAside(run.Aside{
		Name: "gh_discussion_comment-20261002T091004", Measurement: "gh_discussion_comment",
		At: time.Date(2026, 10, 2, 9, 10, 4, 0, time.UTC),
	})
	state.Stores["influxdb"].KeepAside(run.Aside{
		Name:        "gh_discussion_comment-20261001T091004",
		Measurement: "gh_discussion_comment", At: time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC), ByServer: true,
		Until: time.Date(2026, 10, 2, 9, 10, 4, 0, time.UTC),
	})
	state.Stores["influxdb"].KeepAside(run.Aside{
		Name:        "gh_code_scanning_alert_item-20261001T091004",
		Measurement: "gh_code_scanning_alert_item", At: time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC), ByServer: true,
	})
	purger := &fakePurger{name: "postgres"}
	es := &fakePurger{name: "elasticsearch"}
	saves := 0
	var log bytes.Buffer
	p := Purging{
		Config: cfg, State: state, Save: func() error { saves++; return nil },
		Purgers: []teardown.Purger{purger, es}, Now: now, Log: slog.New(slog.NewTextHandler(&log, nil)),
	}
	next := p.Run(t.Context())
	if !slices.Equal(purger.calls, []string{"purge gh_discussion_comment-20261001T091004"}) || len(es.calls) != 0 {
		t.Errorf("postgres was sent %v and elasticsearch %v", purger.calls, es.calls)
	}
	if want := time.Date(2026, 10, 3, 9, 10, 4, 0, time.UTC); !next.Equal(want) {
		t.Errorf("next = %s, want %s", next, want)
	}
	if left := state.Stores["influxdb"].SetAside; len(left) != 1 || left[0].Measurement != "gh_code_scanning_alert_item" {
		t.Errorf("InfluxDB's records after: %v, want the one it named no day for, not yet 72 hours old", left)
	}
	if len(pg.SetAside) != 1 || saves != 1 {
		t.Errorf("records after: postgres %v, influxdb %v, %d saves", pg.SetAside, state.Stores["influxdb"].SetAside, saves)
	}
	if !strings.Contains(log.String(), `msg="set-aside copy purged"`) {
		t.Errorf("the purge is not in the log:\n%s", log.String())
	}

	// Nothing due: nothing asked of any store, nothing saved.
	purger.calls = nil
	if p.Run(t.Context()); len(purger.calls) != 0 || saves != 1 {
		t.Errorf("with nothing due it sent %v and saved %d times", purger.calls, saves)
	}

	// A store that refuses keeps the record, for the next try.
	p.Now = next
	purger.refuse = true
	if p.Run(t.Context()); len(pg.SetAside) != 1 || saves != 1 {
		t.Errorf("a refused purge dropped the record: %v", pg.SetAside)
	}
}

// TestANewStateFileAsksTheStoresForTheirCopies: every run of the Action
// starts on one, and a copy its migration set aside would otherwise stay for
// good. What the store lists is purged the same way, by the instant in its
// name, and a copy of a record for another destination is left alone.
func TestANewStateFileAsksTheStoresForTheirCopies(t *testing.T) {
	t.Parallel()
	cfg := planConfig(t, t.TempDir(), nil)
	state := run.LoadState("")
	Stamp(state, cfg, "2.6.2")
	state.Stores["elasticsearch"].Destination = "url=http://elsewhere:9200 prefix=ghchronicle"
	state.Stores["elasticsearch"].KeepAside(run.Aside{Name: "ghchronicle-gh_repo-20260101t000000", At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	es := &fakePurger{name: "elasticsearch", listed: []teardown.Aside{
		{
			Name: "ghchronicle-gh_discussion_comment-20261001t091004", Measurement: "gh_discussion_comment",
			At: time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC),
		},
		{
			Name: "ghchronicle-gh_discussion_comment-20261002t110000", Measurement: "gh_discussion_comment",
			At: time.Date(2026, 10, 2, 11, 0, 0, 0, time.UTC),
		},
	}}
	p := Purging{
		Config: cfg, State: state, Purgers: []teardown.Purger{es}, Ask: true,
		Now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
	p.Run(t.Context())
	want := []string{
		"list gh_code_scanning_alert_item,gh_dependabot_alert_item,gh_discussion_comment",
		"purge ghchronicle-gh_discussion_comment-20261001t091004",
	}
	if !slices.Equal(es.calls, want) {
		t.Errorf("sent %v, want %v", es.calls, want)
	}
}
