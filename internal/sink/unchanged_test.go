package sink

import (
	"context"
	"errors"
	"math"
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
func (r *recorder) Write(_ context.Context, points []Point) (int, error) {
	if r.fail != nil {
		return 0, r.fail
	}
	batch := make([]Point, len(points))
	copy(batch, points)
	r.writes = append(r.writes, batch)
	return len(points), nil
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
	if _, err := s.Write(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if len(inner.writes) != 1 || len(inner.writes[0]) != 2 {
		t.Fatalf("the same history was written twice: %v", inner.writes)
	}

	// A field that moved is a new observation, even at the same timestamp.
	moved := []Point{point("gh_star", "a", 1, at), point("gh_star", "b", 3, at)}
	if _, err := s.Write(ctx, moved); err != nil {
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
		if _, err := s.Write(context.Background(), []Point{point("gh_traffic", "a", 7, day.AddDate(0, 0, i))}); err != nil {
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
		if _, err := s.Write(context.Background(), batch); err != nil {
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

	if _, err := s.Write(context.Background(), batch); err == nil {
		t.Fatal("a failing sink reported success")
	}
	inner.fail = nil
	if _, err := s.Write(context.Background(), batch); err != nil {
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
	if _, err := OnlyChanged(one, first).Write(context.Background(), batch); err != nil {
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
	if _, err := OnlyChanged(two, second).Write(context.Background(), batch); err != nil {
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
	if _, err := OnlyChanged(inner, l).Write(context.Background(), []Point{point("gh_star", "a", 1, at)}); err != nil {
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
		if _, err := s.Write(context.Background(), batch); err != nil {
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
	if _, err := s.Write(context.Background(), []Point{a}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), []Point{b}); err != nil {
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
	if _, err := OnlyChanged(&recorder{name: "influxdb"}, l).Write(
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

// TestDayNumberCountsWholeDaysAndStaysInItsField divides by the length of a
// day, and clamps a clock before 1970 to day zero and one past the four-byte
// field to its largest value, rather than wrapping either.
func TestDayNumberCountsWholeDaysAndStaysInItsField(t *testing.T) {
	for _, tc := range []struct {
		at   time.Time
		want uint32
	}{
		{time.Unix(0, 0), 0},
		{time.Unix(3*86400+86399, 0), 3},
		{time.Date(2026, 9, 15, 23, 59, 59, 0, time.UTC), 20711},
		{time.Unix(-86400*10, 0), 0},
		{time.Unix(86400*(math.MaxUint32+10), 0), math.MaxUint32},
	} {
		if got := dayNumber(tc.at); got != tc.want {
			t.Errorf("dayNumber(%v) = %d, want %d", tc.at, got, tc.want)
		}
	}
}

// TestLoadLedgerFillsInOnlyWhatWasLeftOut gives a zero horizon and size their
// documented values and keeps the ones that were set.
func TestLoadLedgerFillsInOnlyWhatWasLeftOut(t *testing.T) {
	l := LoadLedger("", 0, 0)
	if l.horizon != 30*24*time.Hour || l.maxEntries != defaultMaxLen {
		t.Errorf("defaults = horizon %v, max %d, want thirty days and %d", l.horizon, l.maxEntries, defaultMaxLen)
	}
	l = LoadLedger("", time.Hour, 7)
	if l.horizon != time.Hour || l.maxEntries != 7 {
		t.Errorf("given = horizon %v, max %d, want 1h and 7 kept", l.horizon, l.maxEntries)
	}
}

// TestALedgerWithTheWrongHeaderIsAFirstRun reads nothing from a file that is
// long enough to hold records but does not start with the ledger's magic, nor
// from one too short to hold the magic at all.
func TestALedgerWithTheWrongHeaderIsAFirstRun(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string][]byte{
		"foreign": append([]byte("NOTLDG1\n"), make([]byte, 3*ledgerRecord)...),
		"short":   []byte("GHC"),
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if n := LoadLedger(path, 0, 0).Len(); n != 0 {
			t.Errorf("%s: read %d points, want none", name, n)
		}
	}
}

// TestANilLedgerRemembersNothingAndSavesNothing lets a caller with no ledger
// call Reserve, commit and Save without branching.
func TestANilLedgerRemembersNothingAndSavesNothing(t *testing.T) {
	var l *Ledger
	batch := []Point{point("gh_star", "a", 1, time.Unix(1, 0))}
	keep, commit := l.Reserve("influxdb", batch)
	commit()
	if len(keep) != 1 {
		t.Errorf("Reserve on no ledger kept %d points, want all of them", len(keep))
	}
	if err := l.Save(); err != nil {
		t.Errorf("Save on no ledger = %v", err)
	}
	if err := LoadLedger("", 0, 0).Save(); err != nil {
		t.Errorf("Save on a ledger with no path = %v", err)
	}
}

// TestAnUnchangedPointStillOfferedIsNotPruned moves the day of an entry that is
// offered again with the same value, so an item the collectors still produce
// survives the horizon even though it is never written again.
func TestAnUnchangedPointStillOfferedIsNotPruned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "written.bin")
	l := LoadLedger(path, time.Hour, 0)
	inner := &recorder{name: "influxdb"}
	s := OnlyChanged(inner, l)
	batch := []Point{point("gh_star", "a", 1, time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC))}
	if _, err := s.Write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	for id, e := range l.seen {
		l.seen[id] = entry{value: e.value, day: e.day - 2}
	}
	l.mu.Unlock()
	if _, err := s.Write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	if l.Len() != 1 || len(inner.writes) != 1 {
		t.Errorf("ledger holds %d entries after %d writes, want the offered entry kept and written once", l.Len(), len(inner.writes))
	}
}

// TestSaveKeepsAnEntryOnTheCutoffDay prunes what is older than the horizon and
// nothing on its last day. The cutoff is read before and after the save, and
// the check is repeated if midnight fell in between.
func TestSaveKeepsAnEntryOnTheCutoffDay(t *testing.T) {
	const horizon = 48 * time.Hour
	for range 3 {
		l := LoadLedger(filepath.Join(t.TempDir(), "written.bin"), horizon, 0)
		cutoff := dayNumber(time.Now().Add(-horizon))
		l.seen[1] = entry{value: 1, day: cutoff}
		l.seen[2] = entry{value: 2, day: cutoff - 1}
		if err := l.Save(); err != nil {
			t.Fatal(err)
		}
		if dayNumber(time.Now().Add(-horizon)) != cutoff {
			continue
		}
		if _, kept := l.seen[1]; !kept || l.Len() != 1 {
			t.Errorf("ledger = %v, want only the entry on the cutoff day", l.seen)
		}
		return
	}
	t.Fatal("the day changed during every attempt")
}

// TestSaveTrimsTheLedgerToItsMostRecentEntries keeps the entries offered most
// recently once there are more than the limit, and none of the older ones.
func TestSaveTrimsTheLedgerToItsMostRecentEntries(t *testing.T) {
	l := LoadLedger(filepath.Join(t.TempDir(), "written.bin"), 0, 2)
	today := dayNumber(time.Now())
	for i := range uint32(4) {
		l.seen[uint64(i)] = entry{value: 1, day: today - 3 + i}
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	_, second := l.seen[2]
	_, newest := l.seen[3]
	if l.Len() != 2 || !second || !newest {
		t.Errorf("ledger = %v, want the two most recent days kept", l.seen)
	}
}

// TestSaveCreatesTheDirectoryTheLedgerLivesIn makes a missing parent directory,
// and writes a bare file name into the working directory.
func TestSaveCreatesTheDirectoryTheLedgerLivesIn(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "state", "written.bin")
	if err := LoadLedger(nested, 0, 0).Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("no ledger in a directory that had to be made: %v", err)
	}
	t.Chdir(dir)
	if err := LoadLedger("written.bin", 0, 0).Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "written.bin")); err != nil {
		t.Errorf("no ledger in the working directory: %v", err)
	}
}

// TestSaveReportsWhereItCannotWrite fails when the directory is a file, and
// when the temporary name is taken by a directory.
func TestSaveReportsWhereItCannotWrite(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "a-file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := LoadLedger(filepath.Join(blocker, "written.bin"), 0, 0).Save(); err == nil {
		t.Error("Save succeeded under a path that is a file")
	}
	path := filepath.Join(dir, "written.bin")
	if err := os.Mkdir(path+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := LoadLedger(path, 0, 0).Save(); err == nil {
		t.Error("Save succeeded with its temporary name taken by a directory")
	}
}

// TestTheLedgerSavesOnlyWhenSomethingChangedAndAWhileHasPassed writes the file
// from a commit only once five minutes have gone by since the last save and
// the commit had something to record, so a sweep does not rewrite eighty
// megabytes per family.
func TestTheLedgerSavesOnlyWhenSomethingChangedAndAWhileHasPassed(t *testing.T) {
	at := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	saved := func(l *Ledger) bool {
		_, err := os.Stat(l.path)
		return err == nil
	}

	recent := LoadLedger(filepath.Join(t.TempDir(), "written.bin"), 0, 0)
	_, commit := recent.Reserve("influxdb", []Point{point("gh_star", "a", 1, at)})
	commit()
	if saved(recent) {
		t.Error("a commit saved the ledger a moment after it was loaded")
	}

	due := LoadLedger(filepath.Join(t.TempDir(), "written.bin"), 0, 0)
	due.saved = due.saved.Add(-6 * time.Minute)
	_, commit = due.Reserve("influxdb", []Point{point("gh_star", "a", 1, at)})
	commit()
	if !saved(due) {
		t.Error("a commit with a change did not save a ledger last saved six minutes ago")
	}

	idle := LoadLedger(filepath.Join(t.TempDir(), "written.bin"), 0, 0)
	_, commit = idle.Reserve("influxdb", []Point{point("gh_star", "a", 1, at)})
	commit()
	if err := idle.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(idle.path); err != nil {
		t.Fatal(err)
	}
	idle.saved = idle.saved.Add(-6 * time.Minute)
	keep, commit := idle.Reserve("influxdb", []Point{point("gh_star", "a", 1, at)})
	commit()
	if len(keep) != 0 || saved(idle) {
		t.Errorf("a commit with nothing new kept %d points and saved %v, want neither", len(keep), saved(idle))
	}
}

// TestAFieldTheLineProtocolDropsIsNotAChange hashes the value the way the line
// protocol writes it, so a nil field added to a point does not make it new.
func TestAFieldTheLineProtocolDropsIsNotAChange(t *testing.T) {
	at := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	a := point("gh_repo", "a", 1, at)
	b := point("gh_repo", "a", 1, at)
	b.Fields["note"] = nil
	inner := &recorder{name: "influxdb"}
	s := OnlyChanged(inner, LoadLedger("", 0, 0))
	for _, p := range []Point{a, b} {
		if _, err := s.Write(context.Background(), []Point{p}); err != nil {
			t.Fatal(err)
		}
	}
	if len(inner.writes) != 1 {
		t.Errorf("a nil field made the point new: %v", inner.writes)
	}
}

// TestUnchangedCountsThePointsItSpared reports nothing spared for a batch that
// was all new, and every point of a batch offered again.
func TestUnchangedCountsThePointsItSpared(t *testing.T) {
	at := time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)
	u := &Unchanged{inner: &recorder{name: "influxdb"}, ledger: LoadLedger("", 0, 0)}
	if u.Name() != "influxdb" {
		t.Errorf("Name = %q, want the wrapped sink's", u.Name())
	}
	batch := []Point{point("gh_star", "a", 1, at), point("gh_star", "b", 1, at)}
	if _, err := u.Write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if n := u.Dropped(); n != 0 {
		t.Errorf("Dropped = %d after a new batch, want 0", n)
	}
	if _, err := u.Write(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	if n := u.Dropped(); n != 2 {
		t.Errorf("Dropped = %d after the same batch again, want 2", n)
	}
	if err := u.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}

// TestAPartlyRefusedWriteIsRemembered commits a batch the store answered with
// a RejectedError or a DroppedError, which both mean every writable point was
// written, so the refused line is not offered again on every sweep.
func TestAPartlyRefusedWriteIsRemembered(t *testing.T) {
	at := time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)
	for _, partial := range []error{&RejectedError{N: 1}, &DroppedError{N: 1, Older: time.Hour}} {
		inner := &recorder{name: "influxdb", fail: partial}
		s := OnlyChanged(inner, LoadLedger("", 0, 0))
		batch := []Point{point("gh_repo", "a", 1, at)}
		if _, err := s.Write(context.Background(), batch); !errors.Is(err, partial) {
			t.Fatalf("Write = %v, want %v passed on", err, partial)
		}
		inner.fail = nil
		if _, err := s.Write(context.Background(), batch); err != nil {
			t.Fatal(err)
		}
		if len(inner.writes) != 0 {
			t.Errorf("after %T the same batch was written again: %v", partial, inner.writes)
		}
	}
}
