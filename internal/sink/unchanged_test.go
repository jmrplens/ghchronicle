package sink

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// recorder is a sink that remembers every batch it was given, and can be told
// to fail.
type recorder struct {
	name   string
	writes [][]Point
	fail   error
}

func (r *recorder) Name() string { return r.name }
func (r *recorder) Close() error { return nil }
func (r *recorder) Write(_ context.Context, points []Point) error {
	if r.fail != nil {
		return r.fail
	}
	batch := make([]Point, len(points))
	copy(batch, points)
	r.writes = append(r.writes, batch)
	return nil
}

func point(measurement, repo string, value int, at time.Time) Point {
	return Point{
		Measurement: measurement,
		Tags:        map[string]string{"repo": repo},
		Fields:      map[string]any{"count": value},
		Time:        at,
	}
}

func TestUnchangedPointsAreWrittenOnceAndChangedOnesAgain(t *testing.T) {
	at := time.Date(2019, 4, 2, 0, 0, 0, 0, time.UTC)
	inner := &recorder{name: "influxdb"}
	s := OnlyChanged(inner, LoadLedger("", 0, 0))
	ctx := context.Background()

	batch := []Point{point("gh_star", "a", 1, at), point("gh_star", "b", 2, at)}
	if err := s.Write(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if len(inner.writes) != 1 || len(inner.writes[0]) != 2 {
		t.Fatalf("the same history was written twice: %v", inner.writes)
	}

	// A field that moved is a new observation, even at the same timestamp.
	moved := []Point{point("gh_star", "a", 1, at), point("gh_star", "b", 3, at)}
	if err := s.Write(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if len(inner.writes) != 2 || len(inner.writes[1]) != 1 {
		t.Fatalf("a changed point did not reach the sink: %v", inner.writes)
	}
	if inner.writes[1][0].Tags["repo"] != "b" {
		t.Fatalf("the wrong point was written: %v", inner.writes[1][0])
	}
}

func TestADifferentTimestampIsADifferentPoint(t *testing.T) {
	inner := &recorder{name: "influxdb"}
	s := OnlyChanged(inner, LoadLedger("", 0, 0))
	day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := range 3 {
		if err := s.Write(context.Background(), []Point{point("gh_traffic", "a", 7, day.AddDate(0, 0, i))}); err != nil {
			t.Fatal(err)
		}
	}
	if len(inner.writes) != 3 {
		t.Fatalf("three days should be three writes, got %d", len(inner.writes))
	}
}

func TestEachSinkGetsItsOwnAnswer(t *testing.T) {
	ledger := LoadLedger("", 0, 0)
	at := time.Date(2026, 2, 2, 0, 0, 0, 0, time.UTC)
	one := &recorder{name: "influxdb"}
	two := &recorder{name: "elasticsearch"}
	a, b := OnlyChanged(one, ledger), OnlyChanged(two, ledger)

	batch := []Point{point("gh_repo", "a", 1, at)}
	for _, s := range []Sink{a, b, a, b} {
		if err := s.Write(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
	}
	if len(one.writes) != 1 || len(two.writes) != 1 {
		t.Fatalf("a shared ledger starved a sink: influx %d, elasticsearch %d", len(one.writes), len(two.writes))
	}
}

func TestAFailedWriteIsNotRemembered(t *testing.T) {
	at := time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)
	inner := &recorder{name: "influxdb", fail: errors.New("connection refused")}
	s := OnlyChanged(inner, LoadLedger("", 0, 0))
	batch := []Point{point("gh_repo", "a", 1, at)}

	if err := s.Write(context.Background(), batch); err == nil {
		t.Fatal("a failing sink reported success")
	}
	inner.fail = nil
	if err := s.Write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if len(inner.writes) != 1 {
		t.Fatal("the point lost to a failed write never arrived")
	}
}

func TestTheLedgerSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "written.bin")
	at := time.Date(2026, 4, 4, 0, 0, 0, 0, time.UTC)
	batch := []Point{point("gh_star", "a", 1, at), point("gh_star", "b", 2, at)}

	first := LoadLedger(path, 0, 0)
	one := &recorder{name: "influxdb"}
	if err := OnlyChanged(one, first).Write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the ledger was not written: %v", err)
	}

	second := LoadLedger(path, 0, 0)
	if second.Len() != 2 {
		t.Fatalf("read back %d points, wrote 2", second.Len())
	}
	two := &recorder{name: "influxdb"}
	if err := OnlyChanged(two, second).Write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if len(two.writes) != 0 {
		t.Fatalf("a restart rewrote history: %v", two.writes)
	}
}

func TestAnUnreadableLedgerIsAFirstRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "written.bin")
	if err := os.WriteFile(path, []byte("not a ledger"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n := LoadLedger(path, 0, 0).Len(); n != 0 {
		t.Fatalf("garbage was read as %d points", n)
	}
}

func TestSaveForgetsWhatNothingOffersAnyMore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "written.bin")
	l := LoadLedger(path, time.Hour, 0) // a one hour horizon, so a day is stale
	at := time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC)
	inner := &recorder{name: "influxdb"}
	if err := OnlyChanged(inner, l).Write(context.Background(), []Point{point("gh_star", "a", 1, at)}); err != nil {
		t.Fatal(err)
	}
	// Backdate the entry, which is what a collector that stopped producing it
	// looks like once a day has passed.
	l.mu.Lock()
	for id, e := range l.seen {
		l.seen[id] = entry{value: e.value, day: e.day - 2}
	}
	l.mu.Unlock()
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	if l.Len() != 0 {
		t.Fatalf("a stale entry survived the horizon: %d left", l.Len())
	}
}

func TestANilLedgerWritesEverything(t *testing.T) {
	inner := &recorder{name: "file"}
	s := OnlyChanged(inner, nil)
	at := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	batch := []Point{point("gh_star", "a", 1, at)}
	for range 3 {
		if err := s.Write(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
	}
	if len(inner.writes) != 3 {
		t.Fatalf("a sink with no ledger dropped a write: %d", len(inner.writes))
	}
}

func TestAnEmptyTagIsNotPartOfIdentity(t *testing.T) {
	// The line protocol drops an empty tag, so two points that differ only
	// there are the same row in the store and must be the same to the ledger.
	at := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	a := point("gh_repo", "a", 1, at)
	b := point("gh_repo", "a", 1, at)
	b.Tags["branch"] = ""
	inner := &recorder{name: "influxdb"}
	s := OnlyChanged(inner, LoadLedger("", 0, 0))
	if err := s.Write(context.Background(), []Point{a}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(context.Background(), []Point{b}); err != nil {
		t.Fatal(err)
	}
	if len(inner.writes) != 1 {
		t.Fatalf("an empty tag made a second series: %v", inner.writes)
	}
}

func TestTheLedgerIsForThisProcessAlone(t *testing.T) {
	// The dump the File sink writes is meant to be read by something else, so
	// a mode an operator sets on it is kept. The ledger is the opposite case:
	// nothing else is meant to read it, so it is written closed and it stays
	// closed. Pinning both is what keeps the two from drifting into the same
	// answer by accident.
	//
	// The number is written out rather than compared against ledgerMode.
	// gosec used to check this value where it was passed to os.WriteFile and
	// no longer can, now that it arrives there as a constant, so a test that
	// agreed with whatever the constant said would leave nothing at all
	// between 0600 and 0644.
	//
	// storedPerm reads each number the way the platform stores it. On Windows
	// that is the read-only attribute alone, which is exactly what the chmod
	// below sets there, so the second half is the Windows case too: a
	// read-only ledger has to be replaced by the next save, not refuse it.
	path := filepath.Join(t.TempDir(), "written.bin")
	l := LoadLedger(path, 0, 0)
	at := time.Date(2026, 6, 6, 0, 0, 0, 0, time.UTC)
	if err := OnlyChanged(&recorder{name: "influxdb"}, l).Write(
		context.Background(), []Point{point("gh_star", "a", 1, at)},
	); err != nil {
		t.Fatal(err)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := st.Mode().Perm(), storedPerm(0o600); got != want {
		t.Errorf("a saved ledger is mode %04o, want %04o", got, want)
	}

	// A mode set on the ledger by hand is not a grant to anybody, and the
	// next save puts it back.
	if err = os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if err = l.Save(); err != nil {
		t.Fatalf("saving over a read-only ledger: %v", err)
	}
	if st, err = os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if got, want := st.Mode().Perm(), storedPerm(0o600); got != want {
		t.Errorf("a resaved ledger is mode %04o, want %04o", got, want)
	}
}
