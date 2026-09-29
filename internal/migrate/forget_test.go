package migrate

import (
	"bytes"
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/run"
)

// TestTheSaltIsTheMigrationsAppliedToTheMeasurementThere, and nothing for
// any other measurement or any other store, so their identities stay what
// they were.
func TestTheSaltIsTheMigrationsAppliedToTheMeasurementThere(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	state := run.LoadState("")
	state.Stores["influxdb"] = &run.StoreRecord{Destination: "d"}
	state.Stores["influxdb"].MarkApplied(comments, when)
	state.Stores["influxdb"].MarkApplied("9.9.9/gh_repo/unknown", when)
	state.Stores["influxdb"].MarkNotNeeded(scanning, when)
	state.Stores["postgres"] = &run.StoreRecord{Destination: "e"}
	got := Salts(state)
	want := map[string]map[string]string{"influxdb": {"gh_discussion_comment": comments}}
	if len(got) != 1 || len(got["influxdb"]) != 1 || got["influxdb"]["gh_discussion_comment"] != want["influxdb"]["gh_discussion_comment"] {
		t.Errorf("salts = %v, want %v", got, want)
	}
	if Salt(nil, "gh_discussion_comment") != "" || Salt(state.Stores["influxdb"], "gh_repo") != "" {
		t.Error("a measurement nothing was applied to has a salt")
	}
}

// TestWhatWasAppliedIsForgottenBeforeTheRefill: the hook that makes this
// process forget the measurement in the store is told of each item applied
// once it is recorded, so the salt it reads is the new one, and before the
// refill, which the ledger would otherwise hold back.
func TestWhatWasAppliedIsForgottenBeforeTheRefill(t *testing.T) {
	t.Parallel()
	in := upgradeInput(t, oldShape(), nil, nil)
	Stamp(in.State, in.Config, in.Release)
	chosen, _ := Make(t.Context(), in).Pending(false)
	chosen = slices.DeleteFunc(chosen, func(c Chosen) bool { return c.Store != "influxdb" })
	var order []string
	a := Applying{
		Config: in.Config, State: in.State, Save: func() error { return nil },
		Appliers: map[string]Applier{"influxdb": &fakeApplier{}},
		Cleared: func(c Chosen) {
			salt := Salt(in.State.Stores[c.Store], c.Item.Migration.Measurement)
			order = append(order, "forget "+c.Item.Migration.Measurement+" "+salt)
		},
		Refill: func(context.Context, RefillWalk) error { order = append(order, "refill"); return nil },
		Log:    slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
	if _, err := a.Apply(t.Context(), chosen); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"forget gh_code_scanning_alert_item " + scanning,
		"forget gh_discussion_comment " + comments,
		"refill",
	}
	if !slices.Equal(order, want) {
		t.Errorf("order %v, want %v", order, want)
	}
}
