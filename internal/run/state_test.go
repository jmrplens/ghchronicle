package run

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// TestTheStateCommentAccountsForEveryFieldItKeeps reads this package's own
// source and fails when State persists something its doc comment does not
// mention.
//
// The comment said "Two things only" while the type had grown to six, and the
// four it left out are the four with consequences: deleting the file loses the
// commit a dependency diff starts from, when each family last read a whole
// page, where the notification window was cut, and the newest event seen. Two
// documents and the site repeated the sentence, because the comment is what
// they were written from.
func TestTheStateCommentAccountsForEveryFieldItKeeps(t *testing.T) {
	doc, fields := stateDocAndFields(t)
	for _, name := range fields {
		if !strings.Contains(doc, name) {
			t.Errorf("State persists %q and its doc comment never names it:\n%s", name, doc)
		}
	}
	if strings.Contains(doc, "Two things only") {
		t.Errorf("State keeps %d fields and its doc comment still says two:\n%s", len(fields), doc)
	}
}

// stateDocAndFields returns State's doc comment and the json name of every
// field it writes to disk, read out of the source rather than by reflection
// because the comment is only in the source.
func stateDocAndFields(t *testing.T) (string, []string) {
	t.Helper()
	doc, st := stateStruct(t)
	var names []string
	for _, field := range st.Fields.List {
		if field.Tag == nil {
			continue // path, which is not persisted
		}
		_, tag, _ := strings.Cut(field.Tag.Value, `json:"`)
		key, _, _ := strings.Cut(tag, `"`)
		if name, _, _ := strings.Cut(key, ","); name != "" {
			names = append(names, name)
		}
	}
	if doc == "" || len(names) == 0 {
		t.Fatal("state.go has no documented State struct, so this test proves nothing")
	}
	return doc, names
}

// stateStruct finds the State declaration in this package's own source.
func stateStruct(t *testing.T) (string, *ast.StructType) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "state.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("cannot read state.go: %v", err)
	}
	for _, decl := range file.Decls {
		gen, isType := decl.(*ast.GenDecl)
		if !isType || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, isSpec := spec.(*ast.TypeSpec)
			if !isSpec || ts.Name.Name != "State" {
				continue
			}
			if st, isStruct := ts.Type.(*ast.StructType); isStruct {
				return gen.Doc.Text(), st
			}
		}
	}
	t.Fatal("state.go declares no State struct, so this test proves nothing")
	return "", nil
}

// TestAStateFileThatNullsItsMapsStillLoadsUsable: a hand-edited or truncated
// state can say null where a map was, and the sweep that loads it writes to
// every one of those maps, which on a nil map is a panic at start-up.
func TestAStateFileThatNullsItsMapsStillLoadsUsable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	nulled := `{"last_run":null,"first_saw":null,"history_read":null,"last_head":null,"last_full":null,"last_event":"42"}`
	if err := os.WriteFile(path, []byte(nulled), 0o600); err != nil {
		t.Fatal(err)
	}
	s := LoadState(path)
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	s.Mark("traffic", now)
	s.MarkFull("issues", now)
	s.LastHead["o/n"] = "aaa"
	if !s.FirstSight("o/n") {
		t.Error("a repository nobody walked reads as walked")
	}
	s.MarkSeen("o/n", now)
	if s.FirstSight("o/n") {
		t.Error("a repository recorded as walked still reads as never walked")
	}
	s.MarkHistory("o/n", now)
	if s.LastEvent != "42" {
		t.Errorf("LastEvent = %q, want what the file said beside the nulls", s.LastEvent)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if saved := LoadState(path); !saved.LastRun["traffic"].Equal(now) || saved.LastHead["o/n"] != "aaa" {
		t.Errorf("the state written after the nulls = %+v", saved)
	}
}

// TestAStateWithoutAPathIsNeverWritten: the tests and the one-shot probe
// keep their state in memory, and Save must not drop a file named after
// nothing into whatever directory the process runs in.
func TestAStateWithoutAPathIsNeverWritten(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	s := LoadState("")
	s.Mark("traffic", time.Now())
	if err := s.Save(); err != nil {
		t.Fatalf("Save of a state with no path = %v", err)
	}
	if left, err := os.ReadDir(dir); err != nil || len(left) != 0 {
		t.Errorf("a state with no path wrote %v (%v)", left, err)
	}
}

// TestAStateNamedWithoutADirectoryIsSavedInTheWorkingOne: a bare file name
// has no directory to create, and asking for "." must not stand in the way.
func TestAStateNamedWithoutADirectoryIsSavedInTheWorkingOne(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	s := LoadState("state.json")
	s.Mark("traffic", now)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if saved := LoadState(filepath.Join(dir, "state.json")); !saved.LastRun["traffic"].Equal(now) {
		t.Errorf("the state saved beside the process = %+v", saved)
	}
}

// TestAStateThatCannotBeWrittenSaysWhy: each of the three steps of a save
// can fail, and each failure is returned, so the sweep can say the state was
// not kept instead of believing it was.
func TestAStateThatCannotBeWrittenSaysWhy(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory where the temporary file goes, and a non-empty directory
	// where the state itself goes.
	for _, blocked := range []string{filepath.Join(dir, "tmp", "state.json.tmp"), filepath.Join(dir, "over", "state.json", "x")} {
		if err := os.MkdirAll(blocked, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for name, path := range map[string]string{
		"a parent directory that is a file":      filepath.Join(file, "state.json"),
		"a directory in the way of the new file": filepath.Join(dir, "tmp", "state.json"),
		"a directory in the way of the state":    filepath.Join(dir, "over", "state.json"),
	} {
		if err := LoadState(path).Save(); err == nil {
			t.Errorf("%s: Save reported success", name)
		}
	}
}

// TestAWholeReadIsDueTheMomentItsIntervalHasPassed: the inbox is read whole
// once a day, and a sweep landing exactly a day after the last whole read is
// that day's read. Putting it off to the sweep after would let the daily read
// slip a cadence further every day it lands on the mark.
func TestAWholeReadIsDueTheMomentItsIntervalHasPassed(t *testing.T) {
	t.Parallel()
	last := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	s := LoadState("")
	if !s.FullDue("notifs", fullInboxEvery, last) {
		t.Error("a whole read never done is not due")
	}
	s.MarkFull("notifs", last)
	for _, tc := range []struct {
		name string
		at   time.Time
		due  bool
	}{
		{"a moment short of a day", last.Add(fullInboxEvery - time.Nanosecond), false},
		{"exactly a day", last.Add(fullInboxEvery), true},
		{"past a day", last.Add(fullInboxEvery + time.Minute), true},
	} {
		if got := s.FullDue("notifs", fullInboxEvery, tc.at); got != tc.due {
			t.Errorf("%s after the last whole read: due = %t, want %t", tc.name, got, tc.due)
		}
	}
}

// TestAStateFromBeforeTheStarHistoryReadsEveryHistoryOnce: a state file
// written before history_read existed has repositories in first_saw and no
// history_read at all, and that has to read as "never read whole" for every
// one of them, so the first sweep after the upgrade reads each history back
// to its first week. Once recorded, the record survives the file.
func TestAStateFromBeforeTheStarHistoryReadsEveryHistoryOnce(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	older := `{"last_run":{"stars":"2026-09-12T10:00:00Z"},"first_saw":{"o/a":"2026-01-01T00:00:00Z"}}`
	if err := os.WriteFile(path, []byte(older), 0o600); err != nil {
		t.Fatal(err)
	}
	s := LoadState(path)
	if !s.HistoryDue("o/a") || !s.HistoryDue("o/never-seen") {
		t.Fatal("a state without history_read did not read as every history unread")
	}
	now := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	s.MarkHistory("o/a", now)
	if s.HistoryDue("o/a") {
		t.Error("a history recorded as read whole is still due")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	saved := LoadState(path)
	if saved.HistoryDue("o/a") || !saved.HistoryRead["o/a"].Equal(now) {
		t.Errorf("history_read after a save = %v, want o/a at %s", saved.HistoryRead, now)
	}
	if !saved.HistoryDue("o/b") {
		t.Error("a repository never read reads as read")
	}
}

// TestAFamilyRunsOnEveryTickItsCadenceReaches sweeps the way the loop does:
// a ticker at the shortest cadence, and the sweep's clock read after the tick
// arrives, a little late by an amount that varies from tick to tick. A family
// asked for its interval to the millisecond waited a second tick whenever
// that lateness shrank: in production on 2026-09-26 the quarter-hour
// families ran every 24 minutes on average and the hourly ones every 69.
func TestAFamilyRunsOnEveryTickItsCadenceReaches(t *testing.T) {
	t.Parallel()
	r, tick := builtinRunner(t)
	families := []string{"actions", "events", "repo", "traffic"}
	runs := runsInADay(r, tick, families)
	for _, family := range families {
		every, _ := r.Cfg.Interval(family)
		if want := int(24 * time.Hour / every); runs[family] != want {
			t.Errorf("%s, every %s, ran %d times in a day of %s ticks, want %d", family, every, runs[family], tick, want)
		}
	}
}

// TestADayRunsTheCheapFamiliesAsOftenAsDecided is the cadence change of #93
// as the loop carries it out: a day of ticks at the built-in cadences runs
// each of the ten families whose extra passes the production audit costed at
// next to nothing the number of times that was decided, and the tick stays at
// the quarter hour, so no other family moves. The numbers are written out
// rather than derived from the table, because deriving them would pass
// whatever the table said.
func TestADayRunsTheCheapFamiliesAsOftenAsDecided(t *testing.T) {
	t.Parallel()
	want := map[string]int{
		"events": 96, "notifs": 96, "activity": 96, "deployments": 48,
		"stars": 24, "billing": 24, "analyses": 24, "account": 24, "totals": 24, "discussions": 24,
	}
	r, tick := builtinRunner(t)
	runs := runsInADay(r, tick, slices.Sorted(maps.Keys(want)))
	for family, n := range want {
		if runs[family] != n {
			every, _ := r.Cfg.Interval(family)
			t.Errorf("%s ran %d times in a day at its built-in %s, want %d", family, runs[family], every, n)
		}
	}
}

// builtinRunner is a runner on the built-in cadences and no store, and the
// tick its loop would take from them.
func builtinRunner(t *testing.T) (*Runner, time.Duration) {
	t.Helper()
	cfg := &config.Config{GitHub: config.GitHub{Token: "t"}, Targets: config.Targets{User: "u"}, AllowNoSinks: true}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	r := &Runner{Cfg: cfg, State: LoadState(filepath.Join(t.TempDir(), "state.json"))}
	tick, _ := r.tick()
	if tick != 15*time.Minute {
		t.Fatalf("tick = %s, want the quarter hour the built-in cadences give", tick)
	}
	return r, tick
}

// runsInADay sweeps a day of ticks and counts how often each family was due.
func runsInADay(r *Runner, tick time.Duration, families []string) map[string]int {
	start := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	runs := map[string]int{}
	for i := range int(24 * time.Hour / tick) {
		// Late by 3 ms on even ticks and 1 ms on odd ones, so every other
		// gap between two sweeps is 2 ms short of a tick.
		late := time.Millisecond * time.Duration(1+2*((i+1)%2))
		now := start.Add(time.Duration(i)*tick + late)
		for _, family := range families {
			every, _ := r.Cfg.Interval(family)
			if r.due(family, every, now) {
				runs[family]++
				r.State.Mark(family, now)
			}
		}
	}
	return runs
}

// TestASaveKeepsWhatAnotherProcessRecordedOfTheStores: a one-shot run reads
// the state file when it starts and saves it when it ends, with no lock in
// between, and a -migrate -yes that ran meanwhile recorded the migration it
// applied and the refill it still owes. Before, the one-shot's save put back
// the copy it had read, the owed refill was forgotten, and the store it
// cleared looked like one that never held the old shape.
func TestASaveKeepsWhatAnotherProcessRecordedOfTheStores(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	first := LoadState(path)
	first.Stores["influxdb"] = &StoreRecord{Destination: "url=http://i bucket=g", FirstWrittenBy: "2.6.1"}
	first.Stores["postgres"] = &StoreRecord{Destination: "host=db", FirstWrittenBy: "2.6.1"}
	if err := first.Save(); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)

	oneShot := LoadState(path)
	migrate := LoadState(path)
	rec := migrate.Stores["influxdb"]
	rec.MarkApplied("2.6.1/gh_discussion_comment/is_answer", when)
	rec.OweRefill("2.6.1/gh_discussion_comment/is_answer", "gh_discussion_comment", []string{"discussions", "outbound"}, when)
	rec.KeepAside(Aside{Name: "gh_discussion_comment-20260929T090000", Measurement: "gh_discussion_comment", At: when})
	if err := migrate.Save(); err != nil {
		t.Fatal(err)
	}

	// The one-shot changes what it has, a store record of its own among
	// it, and saves over the file the other wrote.
	oneShot.Mark("repo", when)
	oneShot.Stores["postgres"].MarkNotNeeded("1.0.0/gh_dependabot_alert_item/state", when)
	if err := oneShot.Save(); err != nil {
		t.Fatal(err)
	}
	after := LoadState(path)
	got := after.Stores["influxdb"]
	if got.Refill == nil || len(got.Applied) != 1 || len(got.SetAside) != 1 {
		t.Errorf("after the one-shot's save the store records %+v; the migration's records are gone", got)
	}
	if _, kept := after.Stores["postgres"].NotNeeded["1.0.0/gh_dependabot_alert_item/state"]; !kept {
		t.Errorf("the one-shot's own record was lost: %+v", after.Stores["postgres"])
	}
	if _, ran := after.LastRun["repo"]; !ran {
		t.Error("the one-shot's sweep marks were lost")
	}

	// Both changed one record: each keeps what it recorded, and a refill
	// owed by either stays owed, since one read twice costs its time and one
	// forgotten costs the history.
	a, b := LoadState(path), LoadState(path)
	a.Stores["influxdb"].Refill = nil
	a.Stores["influxdb"].MarkNotNeeded("1.0.0/gh_code_scanning_alert_item/state", when)
	b.Stores["influxdb"].OweRefill("1.0.0/gh_code_scanning_alert_item/state", "gh_code_scanning_alert_item", []string{"security"}, time.Time{})
	b.Stores["influxdb"].DropAside("gh_discussion_comment-20260929T090000")
	if err := b.Save(); err != nil {
		t.Fatal(err)
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	got = LoadState(path).Stores["influxdb"]
	if got.Refill == nil || !slices.Equal(got.Refill.Families, []string{"discussions", "outbound", "security"}) ||
		!got.Refill.Since.IsZero() {
		t.Errorf("two refills owed at once merged as %+v", got.Refill)
	}
	if len(got.SetAside) != 0 || len(got.NotNeeded) != 1 {
		t.Errorf("a copy one purged, or a finding the other made, was not kept as such: %+v", got)
	}
}

// TestAStateFileThatCannotBeReadIsNotAFirstRun: a run that writes to the
// stores refuses a state file it cannot read or parse rather than starting
// afresh, since a first run's save renames its own file over the one it could
// not read and forgets for good the refills a migration still owes there. A
// file that is not there is a first run, as it always was.
func TestAStateFileThatCannotBeReadIsNotAFirstRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if s, err := OpenState(filepath.Join(dir, "none.json")); err != nil || !s.Fresh() {
		t.Errorf("a state file that is not there opens as %v, %v", s, err)
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte(`{"last_run":{"repo":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenState(broken); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Errorf("a truncated state file opens with %v", err)
	}
	unreadable := filepath.Join(dir, "unreadable.json")
	if err := os.WriteFile(unreadable, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A directory where the file should be is what a read refuses on every
	// platform and for every user, root included.
	if err := os.Remove(unreadable); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(unreadable, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenState(unreadable); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Errorf("a state file that cannot be read opens with %v", err)
	}
}
