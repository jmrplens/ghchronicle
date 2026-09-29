package e2e

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
	"github.com/jmrplens/ghchronicle/v2/test/e2e/fakegh"
)

// An upgrade from 2.6.0, from the stores up. The other tests of -migrate
// answer the binary's questions with what a store would say; these write
// gh_discussion_comment the way 2.6.0 did, through the sinks themselves, into
// an InfluxDB 3 held in memory, a SQL file and the file sink, and then run
// this release against them. Between the two the maintainer of one thread
// took back the answer they had accepted, which is what a store written
// before 2.6.1 cannot show: it holds that comment as two rows, one per value
// of is_answer, and a panel reading one row per comment reads it accepted
// until the measurement is cleared and read again.

// commentFamilies are the two families that write gh_discussion_comment.
var commentFamilies = []string{"discussions", "outbound"}

// onlyTheCommentFamilies switches off every family but those two.
func onlyTheCommentFamilies() map[string]string {
	out := map[string]string{}
	for _, f := range families {
		if !slices.Contains(commentFamilies, f) {
			out[f] = "0"
		}
	}
	return out
}

// writtenBy260 is what 2.6.0 wrote for the two families against the fake
// GitHub as it stood before the answer was taken back: this release's sweep,
// through the file sink, with is_answer put back as the tag it was, and each
// accepted comment written a second time at the same instant as it was read
// before its acceptance.
func writtenBy260(t *testing.T, at time.Time) []sink.Point {
	t.Helper()
	gh := newFakeGitHub(t)
	gh.FreezeAt(at)
	dir := t.TempDir()
	path := filepath.Join(dir, "points.lp")
	sweepOnce(t, writeSinkConfigAt(t, dir, gh.URL(), "  file:\n    path: "+path, onlyTheCommentFamilies()))
	var out []sink.Point
	for _, p := range parseLineProtocol(t, readFile(t, path)) {
		point := sink.Point{Measurement: p.Measurement, Tags: p.Tags, Fields: p.Fields, Time: time.Unix(0, p.Time).UTC()}
		if p.Measurement != "gh_discussion_comment" {
			out = append(out, point)
			continue
		}
		accepted := p.Fields["answers"] == int64(1)
		point.Tags = maps.Clone(p.Tags)
		point.Tags["is_answer"] = strconv.FormatBool(accepted)
		out = append(out, point)
		if accepted {
			before := point
			before.Tags, before.Fields = maps.Clone(point.Tags), maps.Clone(point.Fields)
			before.Tags["is_answer"], before.Fields["answers"] = "false", int64(0)
			out = append(out, before)
		}
	}
	if n := countComments(out); n < 6 {
		t.Fatalf("the sweep 2.6.0's shape is made from wrote %d comments", n)
	}
	return out
}

// countComments is how many distinct comments the points hold.
func countComments(points []sink.Point) int {
	seen := map[string]bool{}
	for _, p := range points {
		if p.Measurement == "gh_discussion_comment" {
			seen[p.Tags["comment"]] = true
		}
	}
	return len(seen)
}

// upgraded is the stores 2.6.0 wrote, and the configuration this release
// runs against them, which collects every family.
type upgraded struct {
	dir, cfg string
	gh       *fakegh.Server
	influx   *influx3
	old      []sink.Point
	// sql and lp are what the SQL file and the file sink held before this
	// release ran.
	sql, lp string
	// seeded is every table the InfluxDB held, with its rows.
	seeded string
}

// upgradeFrom260 writes 2.6.0's shape into the three stores through the
// sinks, with the state file 2.6.0 left, and points a configuration at
// them and at a fake GitHub on which the answer has been taken back.
func upgradeFrom260(t *testing.T, at time.Time, old []sink.Point) *upgraded {
	t.Helper()
	u := &upgraded{dir: t.TempDir(), old: old}
	u.gh = fakegh.New(t, "testdata", fakegh.TakenBackOverlay(t, "testdata"))
	u.gh.FreezeAt(at)
	var influxURL string
	u.influx, influxURL = newInflux3(t, "github")
	sqlPath, lpPath := filepath.Join(u.dir, "points.sql"), filepath.Join(u.dir, "points.lp")
	for _, s := range []sink.Sink{
		sink.NewInflux(influxURL, "", "", "github", 1000, 10*time.Second),
		sink.NewSQL("postgres", sqlPath, 0, 0),
		sink.NewFile(lpPath, "influx", 0, 0),
	} {
		if n, err := s.Write(t.Context(), old); err != nil || n != len(old) {
			t.Fatalf("2.6.0's shape through the %s sink: %d of %d, %v", s.Name(), n, len(old), err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(u.dir, "state.json"),
		[]byte(`{"last_run":{"repo":"2026-09-27T10:00:00Z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	u.cfg = writeSinkConfig(t, u.dir, u.gh.URL(), `  influxdb:
    url: `+influxURL+`
    bucket: github
  sql:
    path: `+sqlPath+`
  file:
    path: `+lpPath)
	u.sql, u.lp, u.seeded = readFile(t, sqlPath), readFile(t, lpPath), u.influx.dump()
	u.influx.forget()
	return u
}

// oldestComment is the day of the oldest comment 2.6.0 wrote.
func oldestComment(points []sink.Point) string {
	var oldest time.Time
	for _, p := range points {
		if p.Measurement == "gh_discussion_comment" && (oldest.IsZero() || p.Time.Before(oldest)) {
			oldest = p.Time
		}
	}
	return oldest.Format(time.DateOnly)
}

func TestAStoreWrittenBy260IsBroughtAlong(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC()
	old := writtenBy260(t, at)

	t.Run("-migrate plans it and changes nothing", func(t *testing.T) {
		t.Parallel()
		planned(t, upgradeFrom260(t, at, old))
	})
	t.Run("-migrate -yes brings it along", func(t *testing.T) {
		t.Parallel()
		appliedByHand(t, upgradeFrom260(t, at, old))
	})
	t.Run("a start under migrate: auto brings along what loses nothing", func(t *testing.T) {
		t.Parallel()
		appliedByAStart(t, upgradeFrom260(t, at, old))
	})
	t.Run("a start under migrate: warn keeps the answer taken back", func(t *testing.T) {
		t.Parallel()
		leftByAStart(t, upgradeFrom260(t, at, old))
	})
}

// planned: -migrate finds the old shape in InfluxDB and says the SQL file may
// hold it, and changes nothing anywhere: not the store, not a file beside the
// configuration, and GitHub is asked for the repository list alone.
func planned(t *testing.T, u *upgraded) {
	t.Helper()
	oldRows := 0
	for _, p := range u.old {
		if p.Measurement == "gh_discussion_comment" {
			oldRows++
		}
	}
	before := snapshot(t, u.dir)
	plan, stderr, err := runSplit(t, time.Minute, "-config", u.cfg, "-migrate")
	if err != nil {
		t.Fatalf("-migrate: %v\n%s\n%s", err, plan, stderr)
	}
	t.Logf("the plan:\n%s", plan)
	for _, want := range []string{
		"(InfluxDB 3 Core 3.11.2)\n  not needed  1.0.0/gh_code_scanning_alert_item/state",
		fmt.Sprintf("rows of gh_discussion_comment carry is_answer as a tag: %d rows, the oldest dated %s\n",
			oldRows, oldestComment(u.old)),
		"read again: discussions and outbound, since " + oldestComment(u.old) + ", writing gh_discussion_comment only\n",
		"safe to apply unattended",
		`write DROP TABLE IF EXISTS "gh_discussion_comment"; into the file`,
		"file\n  nothing to migrate",
		"Nothing was changed.",
	} {
		if !strings.Contains(plan, want) {
			t.Errorf("the plan does not say %q:\n%s", want, plan)
		}
	}
	if got := u.influx.changes(); len(got) != 0 {
		t.Errorf("a dry run sent InfluxDB %q", got)
	}
	if after := u.influx.dump(); after != u.seeded {
		t.Errorf("a dry run changed InfluxDB:\nbefore\n%s\nafter\n%s", u.seeded, after)
	}
	if after := snapshot(t, u.dir); !maps.Equal(before, after) {
		t.Errorf("a dry run changed the files beside it:\nbefore %v\nafter  %v", fileNames(before), fileNames(after))
	}
	for _, r := range u.gh.Requests() {
		if r.Method != http.MethodGet || r.Path != "/user/repos" {
			t.Errorf("a dry run asked GitHub %s %s, want the repository list alone", r.Method, r.Path)
		}
	}
}

// appliedByHand: -migrate -yes sets the InfluxDB table aside, writes the drop
// into the SQL file, reads the comments back into both, touches nothing
// else, and leaves nothing owed for the plan after it to find.
func appliedByHand(t *testing.T, u *upgraded) {
	t.Helper()
	stdout, stderr, err := runSplit(t, 2*time.Minute, "-config", u.cfg, "-migrate", "-yes")
	if err != nil {
		t.Fatalf("-migrate -yes: %v\n%s\n%s", err, stdout, stderr)
	}
	aside := u.setAside(t)
	t.Logf("-migrate -yes:\n%s", stdout)
	for _, want := range []string{
		"  applied     2.6.1/gh_discussion_comment/is_answer in influxdb: InfluxDB set the table gh_discussion_comment aside",
		"  applied     2.6.1/gh_discussion_comment/is_answer in sql: wrote DROP TABLE IF EXISTS \"gh_discussion_comment\";",
		"  refill      read discussions and outbound again, with no bound, writing gh_discussion_comment to influxdb and sql\n",
		fmt.Sprintf("  reconciled  gh_discussion_comment in influxdb: %d items in %s, %[1]d now; GitHub served every one again\n",
			countComments(u.old), aside),
		"\n2 applied.\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("-migrate -yes does not say %q:\n%s", want, stdout)
		}
	}
	u.broughtAlong(t, aside)
	u.onlyTheComments(t, aside)
	u.sqlDroppedAndRefilled(t)
	if got := readFile(t, filepath.Join(u.dir, "points.lp")); got != u.lp {
		t.Error("the file sink, which keeps nothing a release could reshape, was written to")
	}
	if _, err = os.Stat(filepath.Join(u.dir, "state-refill.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refill ended and left its checkpoint: %v", err)
	}

	dump := u.influx.dump()
	plan, _, err := runSplit(t, time.Minute, "-config", u.cfg, "-migrate")
	if err != nil || strings.Contains(plan, "pending") || !strings.Contains(plan,
		"  applied     2.6.1/gh_discussion_comment/is_answer: applied on ") {
		t.Errorf("the plan after it: %v\n%s", err, plan)
	}
	if u.influx.dump() != dump {
		t.Error("the plan after it changed InfluxDB")
	}
}

// appliedByAStart: a start under migrate: auto sets InfluxDB's table aside and
// reads it back before it sweeps, and leaves the SQL file, whose drop reaches
// whatever it is replayed into, to somebody's word.
func appliedByAStart(t *testing.T, u *upgraded) {
	t.Helper()
	_, log, err := runSplit(t, 2*time.Minute, "-config", u.cfg, "-once")
	if err != nil {
		t.Fatalf("-once: %v\n%s", err, log)
	}
	for _, want := range []string{
		`msg="applying a migration before the first sweep" sink=influxdb measurement=gh_discussion_comment`,
		`msg="migration pending" sink=sql measurement=gh_discussion_comment`,
		`not_applied="the DROP reaches whatever the file is replayed into, where nothing is kept aside"`,
		`msg="refill complete" families=discussions,outbound measurements=gh_discussion_comment sinks=influxdb`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("the start does not say %q:\n%s", want, log)
		}
	}
	u.broughtAlong(t, u.setAside(t))
	if strings.Contains(readFile(t, filepath.Join(u.dir, "points.sql")), "DROP TABLE") {
		t.Error("a start dropped the SQL file's table, which it may not do on its own")
	}
}

// leftByAStart: under migrate: warn a start writes this release's shape
// beside 2.6.0's, whose row still says the answer taken back is accepted.
func leftByAStart(t *testing.T, u *upgraded) {
	t.Helper()
	appendToConfig(t, u.cfg, "migrate: warn\n")
	if _, log, err := runSplit(t, 2*time.Minute, "-config", u.cfg, "-once"); err != nil {
		t.Fatalf("-once: %v\n%s", err, log)
	}
	if got := u.influx.changes(); slices.ContainsFunc(got, func(r string) bool { return !strings.HasPrefix(r, "POST ") }) {
		t.Errorf("a start under warn sent InfluxDB %q", got)
	}
	// The sweep writes this release's row beside the two 2.6.0 wrote, and
	// one of those still says accepted: what a panel reads until the
	// measurement is brought along.
	var answers []any
	for _, r := range u.influx.rowsOf("gh_discussion_comment") {
		if r.tags["comment"] == fakegh.TakenBack {
			answers = append(answers, r.fields["answers"])
		}
	}
	if len(answers) != 3 || !slices.Contains(answers, any(int64(1))) {
		t.Errorf("comment %s is held as rows answering %v, want 2.6.0's two beside this release's one",
			fakegh.TakenBack, answers)
	}
}

// setAside is the one table InfluxDB set aside, which holds exactly what
// 2.6.0 wrote of the comments.
func (u *upgraded) setAside(t *testing.T) string {
	t.Helper()
	var asides []string
	for _, name := range u.influx.tableNames() {
		if strings.HasPrefix(name, "gh_discussion_comment-") {
			asides = append(asides, name)
		}
	}
	if len(asides) != 1 {
		t.Fatalf("InfluxDB holds %v, want one table set aside", u.influx.tableNames())
	}
	n := 0
	for _, r := range u.influx.rowsOf(asides[0]) {
		if _, tagged := r.tags["is_answer"]; !tagged {
			t.Errorf("the copy holds a row without is_answer: %v", r.tags)
		}
		n++
	}
	if want := len(slices.DeleteFunc(slices.Clone(u.old), func(p sink.Point) bool {
		return p.Measurement != "gh_discussion_comment"
	})); n != want {
		t.Errorf("the copy holds %d rows, want the %d 2.6.0 wrote", n, want)
	}
	return asides[0]
}

// broughtAlong holds the table under the old name to this release's shape:
// no is_answer column, one row per comment, and the answer taken back read
// as not accepted, while every accepted one still is.
func (u *upgraded) broughtAlong(t *testing.T, aside string) {
	t.Helper()
	if kind, tagged := u.influx.kindsOf("gh_discussion_comment")["is_answer"]; tagged {
		t.Errorf("gh_discussion_comment still has is_answer, as %s", kind)
	}
	rows := u.influx.rowsOf("gh_discussion_comment")
	answers := map[string]any{}
	for _, r := range rows {
		if _, twice := answers[r.tags["comment"]]; twice {
			t.Errorf("comment %s is more than one row", r.tags["comment"])
		}
		answers[r.tags["comment"]] = r.fields["answers"]
	}
	if len(answers) != countComments(u.old) {
		t.Errorf("gh_discussion_comment holds %d comments, want the %d 2.6.0 wrote", len(answers), countComments(u.old))
	}
	for _, p := range u.old {
		comment := p.Tags["comment"]
		if p.Measurement != "gh_discussion_comment" || p.Tags["is_answer"] != "true" {
			continue
		}
		want := int64(1)
		if comment == fakegh.TakenBack {
			want = 0
		}
		if answers[comment] != want {
			t.Errorf("comment %s reads answers=%v, want %d", comment, answers[comment], want)
		}
	}
	if _, found := answers[fakegh.TakenBack]; !found {
		t.Errorf("comment %s, whose answer was taken back, was not read again", fakegh.TakenBack)
	}
	if !slices.Contains(u.influx.tableNames(), aside) {
		t.Errorf("the copy %s went", aside)
	}
}

// onlyTheComments holds InfluxDB to the one delete, the refill to writing
// the comments and nothing else, and every other table to what 2.6.0 left.
func (u *upgraded) onlyTheComments(t *testing.T, aside string) {
	t.Helper()
	var deletes []string
	for _, r := range u.influx.changes() {
		if !strings.HasPrefix(r, "POST /api/v2/write") {
			deletes = append(deletes, r)
		}
	}
	if !slices.Equal(deletes, []string{"DELETE /api/v3/configure/table "}) {
		t.Errorf("InfluxDB was sent %q besides writes, want the one delete", deletes)
	}
	if wrote := slices.Compact(slices.Sorted(slices.Values(u.influx.measurementsWritten()))); !slices.Equal(wrote,
		[]string{"gh_discussion_comment"}) {
		t.Errorf("the refill wrote %v", wrote)
	}
	others := func(dump string) string {
		var b strings.Builder
		keep := false
		for line := range strings.SplitSeq(dump, "\n") {
			if !strings.HasPrefix(line, " ") {
				name, _, _ := strings.Cut(line, " ")
				keep = name != "gh_discussion_comment" && name != aside
			}
			if keep {
				b.WriteString(line + "\n")
			}
		}
		return b.String()
	}
	if before, after := others(u.seeded), others(u.influx.dump()); before != after {
		t.Errorf("the other tables changed:\nbefore\n%s\nafter\n%s", before, after)
	}
}

// sqlDroppedAndRefilled holds the SQL file to what 2.6.0 wrote, then the
// drop, then the comments read back, one statement each, in this release's
// shape, and nothing else.
func (u *upgraded) sqlDroppedAndRefilled(t *testing.T) {
	t.Helper()
	after, dropped := strings.CutPrefix(readFile(t, filepath.Join(u.dir, "points.sql")),
		u.sql+"DROP TABLE IF EXISTS \"gh_discussion_comment\";\n")
	if !dropped {
		t.Fatal("the SQL file does not hold what 2.6.0 wrote followed by the drop")
	}
	for line := range strings.SplitSeq(strings.TrimSpace(after), "\n") {
		if !strings.Contains(line, `"gh_discussion_comment"`) || strings.Contains(line, "is_answer") {
			t.Errorf("after the drop the SQL file holds %.160s", line)
		}
	}
	if got := strings.Count(after, "INSERT INTO"); got != countComments(u.old) {
		t.Errorf("the SQL file got %d comments back, want %d", got, countComments(u.old))
	}
}
