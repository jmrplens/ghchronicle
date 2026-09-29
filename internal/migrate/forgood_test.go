package migrate

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// unpurgingStore is an InfluxDB that says which copies nothing will purge.
type unpurgingStore struct {
	*fakeStore
	found []teardown.Aside
}

func (u *unpurgingStore) Unpurged(context.Context, []string) ([]teardown.Aside, error) {
	return u.found, nil
}

// TestThePlanSaysWhichInfluxDBKeepsTheCopyForGood: 3.0 and 3.1 have no hard
// deletion, so the plan says the copy stays and gives the request that
// removes it on a later release; 3.2 and 3.3 purge it as 3.4 and later do,
// 72 hours after the delete (measured on 3.2.0, 3.3.0 and 3.4.0).
func TestThePlanSaysWhichInfluxDBKeepsTheCopyForGood(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		release string
		says    string
		command bool
	}{
		{"3.0.0", "set aside for good: InfluxDB renames the table gh_discussion_comment-<time> and keeps it queryable; " +
			"InfluxDB 3 Core 3.0.0 has no hard deletion, so it never purges a table it deleted, and a delete of one " +
			"only renames it again. A release from 3.2 to 3.9 removes it when told to, once the server runs one:", true},
		{"3.1.0", "InfluxDB 3 Core 3.1.0 has no hard deletion", true},
		{"3.2.0", "until it purges it itself, 72 hours later by default", false},
		{"3.3.0", "until it purges it itself, 72 hours later by default", false},
		{"3.11.2", "until it purges it itself, 72 hours later by default", false},
	} {
		t.Run(c.release, func(t *testing.T) {
			t.Parallel()
			in := upgradeInput(t, oldShape(), nil, nil)
			in.Inspectors[0].(*fakeStore).server = "InfluxDB 3 Core " + c.release
			it := itemOf(t, Make(t.Context(), in), "influxdb", comments)
			if it.Status != Pending || !strings.Contains(it.Action, c.says) {
				t.Errorf("%s would %q, want it to say %q", it.Status, it.Action, c.says)
			}
			want := "curl -X DELETE '" + in.Config.Sinks.Influx.URL + "/api/v3/configure/table?db=" +
				in.Config.Sinks.Influx.Bucket + "&table=gh_discussion_comment-<time>&hard_delete_at=now' " +
				"-H 'Authorization: Bearer <token>'"
			if got := slices.Contains(it.Commands, want); got != c.command {
				t.Errorf("commands %q, want %q among them: %v", it.Commands, want, c.command)
			}
		})
	}
}

// TestApplyingSaysTheCopyIsKeptForGood: the line that says what was done
// names the copy, says it stays, and gives what removes it, and the record
// knows it is the server's for good.
func TestApplyingSaysTheCopyIsKeptForGood(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC)
	it := Item{Migration: Registry[slices.IndexFunc(Registry, func(m Migration) bool { return m.ID == comments })]}
	stays := "InfluxDB 3 Core 3.1.0 has no hard deletion. A release from 3.2 to 3.9 removes it when told to: curl"
	influx := &fakeClearer{name: "influxdb", aside: teardown.Aside{Name: "x", ByServer: true, ForGood: true, Stays: stays}}
	out, err := Clearing{Store: influx, Now: func() time.Time { return at }}.Apply(t.Context(), it)
	if err != nil {
		t.Fatal(err)
	}
	if want := "InfluxDB set the table gh_discussion_comment aside as gh_discussion_comment-20261001T091004, for " +
		"good: " + stays; out.Did != want {
		t.Errorf("did %q, want %q", out.Did, want)
	}
	if out.Kept == nil || !out.Kept.ForGood || !out.Kept.ByServer {
		t.Errorf("kept %+v, want the record to say the server keeps it for good", out.Kept)
	}
}

// TestThePlanListsTheCopiesAStoreKeepsForGood: -migrate asks the store which
// copies nothing will purge, so one the state file forgot, or never knew,
// is listed with why it stays, and one the state file calls kept for good
// that the store no longer does reads like any copy the server purges.
func TestThePlanListsTheCopiesAStoreKeepsForGood(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, nil, nil, nil)
	Stamp(in.State, in.Config, in.Release)
	at := time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC)
	rec := in.State.Stores["influxdb"]
	for _, name := range []string{"gh_discussion_comment-20261001T091004", "gh_discussion_comment-20261001T091005"} {
		rec.KeepAside(run.Aside{
			Name: name, Measurement: "gh_discussion_comment", Migration: comments, At: at, ByServer: true, ForGood: true,
		})
	}
	store := &unpurgingStore{fakeStore: in.Inspectors[0].(*fakeStore), found: []teardown.Aside{
		{Name: "gh_discussion_comment-20261001T091004", Measurement: "gh_discussion_comment", At: at, Stays: "it stays"},
		{
			Name: "gh_code_scanning_alert_item-20260901T000000", Measurement: "gh_code_scanning_alert_item",
			At: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Stays: "it stays too",
		},
	}}
	in.Inspectors[0] = store
	var out bytes.Buffer
	Make(t.Context(), in).Print(&out)
	for _, want := range []string{
		"  kept aside  " + comments + ": gh_discussion_comment-20261001T091004, set aside 2026-10-01 09:10 UTC, " +
			"for good: it stays\n",
		"  kept aside  " + comments + ": gh_discussion_comment-20261001T091005, set aside 2026-10-01 09:10 UTC, " +
			"purged by the store itself after 2026-10-04 09:10 UTC\n",
		"  kept aside  gh_code_scanning_alert_item-20260901T000000, set aside 2026-09-01 00:00 UTC, for good: it stays too\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the plan does not say %q:\n%s", want, out.String())
		}
	}
	if !rec.SetAside[1].ForGood {
		t.Error("the plan changed the state file's record while reading it")
	}
}

// TestACopyKeptForGoodIsComparedWhenever: a refill resumed days later still
// has the copy to compare with when the server never purges it.
func TestACopyKeptForGoodIsComparedWhenever(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 1, 9, 10, 4, 0, time.UTC)
	in := owedStores(t, at)
	in.State.Stores["postgres"].SetAside = nil
	in.State.Stores["influxdb"].SetAside[0].ForGood = true
	influx := &fakeItems{name: "influxdb", tables: map[string][]string{
		"gh_discussion_comment-20261001T091004": {"1"}, "gh_discussion_comment": {"1"},
	}}
	a := Applying{
		Config: in.Config, State: in.State, Save: func() error { return nil },
		Readers: map[string]teardown.ItemReader{"influxdb": influx, "postgres": &fakeItems{name: "postgres"}},
		Refill:  func(context.Context, RefillWalk) error { return nil },
		Now:     func() time.Time { return at.Add(30 * 24 * time.Hour) }, Log: slog.New(slog.DiscardHandler),
	}
	done, err := a.Apply(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := done.Refill.Reconciled; len(got) != 1 || got[0].Err != nil || got[0].Before != 1 || got[0].After != 1 {
		t.Errorf("reconciled %+v, asked %v", got, influx.asked)
	}
}
