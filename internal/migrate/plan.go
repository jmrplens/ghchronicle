package migrate

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/run"
	"github.com/jmrplens/ghchronicle/v2/internal/teardown"
)

// Status is what the planner found for one migration in one store.
type Status int

const (
	// NotNeeded: the store holds nothing of the old shape.
	NotNeeded Status = iota + 1
	// Pending: the store holds, or may hold, the old shape, and applying the
	// migration would bring it along.
	Pending
	// Applied: the state file records the migration as applied to this
	// store, and the store, where it can be asked, agrees.
	Applied
	// Frozen: the old shape is there and nothing this configuration runs
	// writes the measurement any more, so its rows are history.
	Frozen
	// Noted: the old rows are there and nothing can put them right, so the
	// plan says what they mean and changes nothing.
	Noted
	// Unreachable: the store did not answer, so nothing is known.
	Unreachable
)

// String is the word the plan prints in its status column.
func (s Status) String() string {
	switch s {
	case NotNeeded:
		return "not needed"
	case Pending:
		return "pending"
	case Applied:
		return "applied"
	case Frozen:
		return "frozen"
	case Noted:
		return "note"
	case Unreachable:
		return "unreachable"
	}
	return "unknown"
}

// Item is one migration in one store.
type Item struct {
	Migration Migration
	Status    Status
	// Evidence is what decided the status, as a sentence.
	Evidence string
	// Rows is how many rows the store holds of the measurement, -1 when it
	// was not counted. Oldest is when the earliest is dated.
	Rows   int64
	Oldest time.Time
	// Action is what applying would do to this store, and Commands what
	// somebody else has to run where ghchronicle cannot.
	Action   string
	Commands []string
	// Refill is the families that would read the measurement again, and
	// Since how far back; zero is no bound.
	Refill []string
	Since  time.Time
	// Lost is what a refill cannot bring back, one sentence each.
	Lost []string
	// Others is the accounts whose rows the store holds and this
	// configuration does not collect.
	Others []string
	// Safe says applying needs nobody's word: GitHub serves the whole
	// history, the old rows are set aside for 24 hours rather than
	// destroyed, and every row is this configuration's. Unsafe is why not.
	Safe   bool
	Unsafe []string
}

// StorePlan is one configured store and what the planner found in it.
type StorePlan struct {
	Name        string
	Destination string
	// Server is what the store said it is, for the stores that can be asked.
	Server string
	// Quiet is why nothing kept here ever needs a migration.
	Quiet string
	// Err is why the store could not be asked at all.
	Err   error
	Items []Item
}

// Plan is every configured store and what this release would change in it.
type Plan struct {
	Release string
	// Notes are what the planner could not find out, said before the stores.
	Notes  []string
	Stores []StorePlan
}

// Count is how many items of the plan are in a status.
func (p Plan) Count(s Status) int {
	n := 0
	for _, st := range p.Stores {
		for _, it := range st.Items {
			if it.Status == s {
				n++
			}
		}
	}
	return n
}

// Input is what the planner reads. Nothing in it is written.
type Input struct {
	Config  *config.Config
	State   *run.State
	Release string
	Now     time.Time
	// Inspectors are the stores that can be asked; nil asks teardown for the
	// configuration's own.
	Inspectors []teardown.Inspector
	// Repos is every repository the configuration covers, owner/name, and
	// ReposKnown whether the list could be read at all. ReposWhy is why it
	// could not, for the plan to say.
	Repos      []string
	ReposKnown bool
	ReposWhy   string
}

// Make plans every configured store. It only reads: the stores are asked
// questions, the state file is read as it is, and nothing is recorded.
func Make(ctx context.Context, in Input) Plan {
	inspectors := in.Inspectors
	if inspectors == nil {
		inspectors = teardown.Inspectors(in.Config)
	}
	byName := map[string]teardown.Inspector{}
	for _, i := range inspectors {
		byName[i.Name()] = i
	}
	p := Plan{Release: in.Release}
	if !in.ReposKnown {
		p.Notes = append(p.Notes, "the repository list was not read ("+firstOf(in.ReposWhy, "no reason given")+
			"), so rows of repositories this configuration no longer covers are not counted, and owners are "+
			"not compared with it")
	}
	for _, st := range storesOf(in.Config) {
		p.Stores = append(p.Stores, in.plan(ctx, st, byName[st.name]))
	}
	return p
}

// plan is one store.
func (in Input) plan(ctx context.Context, st store, asker teardown.Inspector) StorePlan {
	sp := StorePlan{Name: st.name, Destination: st.destination}
	switch {
	case st.reach == untouched:
		sp.Quiet = st.quiet
		return sp
	case st.reach == asked && asker == nil:
		sp.Err = fmt.Errorf("no way to ask %s was built", st.name)
		return sp
	case st.reach == asked:
		server, err := asker.Describe(ctx)
		if err != nil {
			sp.Err = err
			return sp
		}
		sp.Server = server
	}
	for _, m := range Registry {
		sp.Items = append(sp.Items, in.item(ctx, st, asker, sp.Server, m))
	}
	return sp
}

// item is one migration in one store: first whether the old shape is there,
// then what would be done about it.
func (in Input) item(ctx context.Context, st store, asker teardown.Inspector, server string, m Migration) Item {
	it := Item{Migration: m, Rows: -1}
	rec := in.State.Stores[st.name]
	// A record kept for another destination is about another store, and a
	// sink pointed at a store the state file has no history of cannot say
	// who wrote it first.
	retargeted := rec != nil && rec.Destination != st.destination
	if retargeted {
		rec = nil
	}
	var held bool
	if st.reach == asked {
		held = in.ask(ctx, &it, asker, rec)
	} else {
		held = in.recall(&it, st, rec, retargeted)
	}
	if held {
		in.decide(ctx, &it, st, asker, server)
	}
	return it
}

// ask lets the store decide. It reports whether the old shape is there, and
// otherwise settles the item.
func (in Input) ask(ctx context.Context, it *Item, asker teardown.Inspector, rec *run.StoreRecord) bool {
	m := it.Migration
	shape, err := asker.Shape(ctx, m.Measurement, m.OldTags, nil)
	if err != nil {
		it.Status, it.Evidence = Unreachable, err.Error()
		return false
	}
	it.Rows, it.Oldest = shape.Rows, shape.Oldest
	when, applied := appliedOn(rec, m.ID)
	switch {
	case m.Kind == Value:
		return in.heldValue(it, rec, shape.Exists)
	case len(shape.Old) > 0:
		it.Evidence = fmt.Sprintf("rows of %s carry %s as %s%s", m.Measurement, quoted(shape.Old),
			plural(len(shape.Old), "a tag", "tags"), span(shape))
		if applied {
			it.Evidence += fmt.Sprintf("; the state file records it as applied on %s, and the old shape is back",
				when.UTC().Format(time.DateOnly))
		}
		return true
	case applied:
		it.Status, it.Evidence = Applied, "applied on "+when.UTC().Format(time.DateOnly)
	case !shape.Exists:
		it.Status, it.Evidence = NotNeeded, "the store holds no "+m.Measurement
	default:
		it.Status, it.Evidence = NotNeeded, fmt.Sprintf("no row of %s carries %s", m.Measurement, orList(m.OldTags))
	}
	return false
}

// heldValue decides a Value change in a store that can be asked: its rows
// cannot be told apart, so the record of what first wrote the store is the
// only thing that can say none of them is older than the change.
func (in Input) heldValue(it *Item, rec *run.StoreRecord, exists bool) bool {
	m := it.Migration
	switch {
	case !exists:
		it.Status, it.Evidence = NotNeeded, "the store holds no "+m.Measurement
	case rec != nil && m.writtenBy(rec.FirstWrittenBy):
		it.Status, it.Evidence = NotNeeded, "first written by "+rec.FirstWrittenBy
	case rec == nil && in.State.Fresh():
		it.Status, it.Evidence = NotNeeded, "the state file is new, so this release is the first to write here"
	default:
		it.Evidence = fmt.Sprintf("the store holds %s%s", m.Measurement, span(teardown.Shape{Rows: it.Rows, Oldest: it.Oldest}))
		return true
	}
	return false
}

// recall lets the record decide, for a store that cannot be asked. It
// reports whether the old shape may be there, and otherwise settles the item.
func (in Input) recall(it *Item, st store, rec *run.StoreRecord, retargeted bool) bool {
	m := it.Migration
	if when, applied := appliedOn(rec, m.ID); applied {
		it.Status, it.Evidence = Applied, "applied on "+when.UTC().Format(time.DateOnly)
		return false
	}
	switch {
	case m.Release == PreRelease:
		it.Status, it.Evidence = NotNeeded, "no release wrote this shape, and only a store that can be "+
			"asked could show one a build before 1.0.0 left"
	case st.fresh:
		it.Status, it.Evidence = NotNeeded, "nothing has written this file yet"
	case rec != nil && rec.FirstWrittenBy != "" && m.writtenBy(rec.FirstWrittenBy):
		it.Status, it.Evidence = NotNeeded, "first written by "+rec.FirstWrittenBy
	case rec == nil && !retargeted && (in.State.Fresh() || len(in.State.Stores) > 0):
		// A state file that keeps records and has none for this sink was
		// written by a release that keeps them, so the sink is new since.
		it.Status, it.Evidence = NotNeeded, "this release is the first to write here"
	case rec != nil && rec.FirstWrittenBy != "":
		it.Evidence = fmt.Sprintf("first written by %s, before %s", rec.FirstWrittenBy, m.Release)
		return true
	default:
		it.Evidence = fmt.Sprintf("written before the state file kept a record of which release first "+
			"wrote it, so it may hold rows written before %s; this store cannot be asked", m.Release)
		return true
	}
	return false
}

// decide is what would be done about an old shape that is there.
func (in Input) decide(ctx context.Context, it *Item, st store, asker teardown.Inspector, server string) {
	m := it.Migration
	on, off := in.families(m)
	switch {
	case slices.Contains(st.exclude, m.Measurement):
		it.Status, it.Action = Frozen, "this sink is told not to write "+m.Measurement+
			", so its rows are history and are left as they are"
		return
	case len(on) == 0:
		it.Status, it.Action = Frozen, fmt.Sprintf("nothing this configuration runs writes %s any more "+
			"(%s off), so its rows are history and are left as they are", m.Measurement, quoted(off))
		return
	case m.Kind == Value:
		it.Status, it.Action = Noted, "nothing is changed"
		return
	case m.Reach == Current:
		it.Status, it.Action = Noted, "nothing is changed: GitHub serves only today's state, so a drop "+
			"would lose history nothing can read again"
		return
	}
	it.Status = Pending
	it.Action, it.Commands = action(st, server, m)
	if st.name != "telegraf" {
		it.Refill = on
	}
	for _, f := range off {
		it.Lost = append(it.Lost, fmt.Sprintf("the rows %s wrote: this configuration does not run it", f))
	}
	checked := false
	if st.reach == asked {
		it.Since = dayOf(it.Oldest)
		checked = in.whose(ctx, it, asker)
	} else if since, err := in.Config.Backfill.SinceTime(in.Now); err == nil {
		it.Since = dayOf(since)
	}
	it.Unsafe = unsafe(st, server, checked, it)
	it.Safe = len(it.Unsafe) == 0
}

// families splits a migration's families into the ones this configuration
// runs and the ones it has switched off.
func (in Input) families(m Migration) (on, off []string) {
	for _, f := range m.Families {
		if _, enabled := in.Config.Interval(f); enabled {
			on = append(on, f)
		} else {
			off = append(off, f)
		}
	}
	return on, off
}

// whose reads which accounts and repositories the store holds rows of, and
// reports whether the question could be answered.
func (in Input) whose(ctx context.Context, it *Item, asker teardown.Inspector) bool {
	m := it.Migration
	perRepo := !slices.ContainsFunc(m.Families, func(f string) bool { return !run.PerRepository(f) })
	values := []string{m.Account}
	if perRepo {
		values = append(values, "full_name")
	}
	shape, err := asker.Shape(ctx, m.Measurement, nil, values)
	if err != nil {
		it.Unsafe = append(it.Unsafe, "whose rows these are could not be read: "+err.Error())
		return false
	}
	if perRepo && in.ReposKnown {
		if gone := missingFrom(shape.Values["full_name"], in.Repos); len(gone) > 0 {
			it.Lost = append(it.Lost, fmt.Sprintf("the rows of %d %s this configuration no longer covers: %s",
				len(gone), plural(len(gone), "repository", "repositories"), sample(gone)))
		}
	}
	mine, known := in.accountsOf(m.Account)
	if !known {
		return false
	}
	it.Others = missingFrom(shape.Values[m.Account], mine)
	return true
}

// accountsOf is every value of an account tag that this configuration's own
// rows carry, and whether that is known.
func (in Input) accountsOf(tag string) ([]string, bool) {
	t := in.Config.Targets
	out := []string{t.User}
	if tag == "user" {
		return out, t.User != ""
	}
	out = append(out, t.Orgs...)
	for _, full := range slices.Concat(t.Repos, in.Repos) {
		owner, _, _ := strings.Cut(full, "/")
		out = append(out, owner)
	}
	return out, in.ReposKnown
}

// unsafe is every reason the migration needs somebody's word before it is
// applied.
func unsafe(st store, server string, checked bool, it *Item) []string {
	var why []string
	if reason := destroys(st, server); reason != "" {
		why = append(why, reason)
	}
	switch {
	case st.reach != asked:
	case !checked && len(it.Unsafe) == 0:
		why = append(why, "the repository list could not be read, so whose rows these are was not compared "+
			"with this configuration")
	case len(it.Others) > 0:
		why = append(why, fmt.Sprintf("it holds rows of %s, which this configuration does not collect; "+
			"set aside, they come back only when the configuration that collects them reads them again",
			quoted(it.Others)))
	}
	return append(it.Unsafe, why...)
}

// destroys is why applying to this store is more than a set-aside, or empty
// when it is one.
func destroys(st store, server string) string {
	switch st.name {
	case "influxdb":
		if isInfluxDB2(server) {
			return "InfluxDB 2 can only delete the rows, and the delete is final"
		}
		return ""
	case "postgres", "elasticsearch":
		return ""
	case "sql":
		return "the DROP reaches whatever the file is replayed into, where nothing is kept aside"
	case "graphite":
		return "only whoever runs the Graphite host can remove its files"
	case "telegraf":
		return "ghchronicle cannot reach the store behind Telegraf"
	}
	return "this store keeps nothing aside"
}

// action is what applying would do to one store, and what somebody else has
// to run where ghchronicle cannot.
func action(st store, server string, m Migration) (what string, commands []string) {
	switch st.name {
	case "influxdb":
		if isInfluxDB2(server) {
			return "delete every row of " + m.Measurement + ": InfluxDB 2 keeps no table aside, so the delete is final", nil
		}
		what = "set aside: InfluxDB renames the table " + m.Measurement + "-<time> and keeps it queryable " +
			"for 24 hours before it purges it"
		if influxBefore34(server) {
			what = "set aside: InfluxDB renames the table " + m.Measurement + "-<time>; 3.0.0 has no deleter " +
				"and 3.4.0 purges after 24 hours, so a server between may keep it until somebody drops it"
		}
		return what, nil
	case "postgres":
		return "set aside: the table is renamed " + m.Measurement + "-<time>, and dropped 24 hours later", nil
	case "elasticsearch":
		return "set aside: the index takes no more writes, is cloned to an index named after it and the time, " +
			"and is deleted; the clone is deleted 24 hours later", nil
	case "sql":
		return `write DROP TABLE IF EXISTS "` + m.Measurement + `"; into the file, ahead of the rows read again`, nil
	case "graphite":
		return "on the Graphite host, remove the old paths, one node deeper than the new ones:", graphiteCommands(st, m)
	case "telegraf":
		return "drop " + m.Measurement + " in the store behind Telegraf, then read " + quoted(m.Families) +
			" again through it with a backfill", nil
	}
	return "", nil
}

// graphiteCommands removes the whisper files of the old shape and nothing
// else. A path is a node per tag and then the field, so the old shape's files
// sit one level deeper for every tag it had and the new one lacks; measured
// on graphite-statsd 1.1.10-5, -mindepth 11 under discussion_comment took
// the 2.6.0 files and left the 2.6.1 ones.
func graphiteCommands(st store, m Migration) []string {
	tags, known := Tags(m.Measurement)
	if !known {
		return nil
	}
	dir := "<storage>/whisper/" + strings.ReplaceAll(st.prefix, ".", "/") + "/" + strings.TrimPrefix(m.Measurement, "gh_")
	depth := len(tags) + len(m.OldTags) + 1
	return []string{
		fmt.Sprintf("find %s -mindepth %d -name '*.wsp' -delete", dir, depth),
		fmt.Sprintf("find %s -type d -empty -delete", dir),
	}
}

// isInfluxDB2 reads what /ping said.
func isInfluxDB2(server string) bool { return strings.HasPrefix(server, "InfluxDB 2") }

// influxBefore34 says whether an InfluxDB 3 is older than the first release
// measured to purge what it soft deletes: 3.0.0 has no deleter at all, 3.4.0
// starts one with a 24 hour grace, and the releases between were not measured.
func influxBefore34(server string) bool {
	fields := strings.Fields(server)
	if len(fields) == 0 {
		return false
	}
	version := fields[len(fields)-1]
	return compareRelease(version, "3.0.0") >= 0 && compareRelease(version, "3.4.0") < 0
}

// appliedOn is when the record says a migration was applied, if it was.
func appliedOn(rec *run.StoreRecord, id string) (time.Time, bool) {
	if rec == nil {
		return time.Time{}, false
	}
	when, ok := rec.Applied[id]
	return when, ok
}

// span is the rows a store holds and since when, as the tail of a sentence.
func span(s teardown.Shape) string {
	switch {
	case s.Oldest.IsZero():
		return ""
	case s.Rows < 0:
		return ", the oldest dated " + s.Oldest.UTC().Format(time.DateOnly)
	}
	return fmt.Sprintf(": %d rows, the oldest dated %s", s.Rows, s.Oldest.UTC().Format(time.DateOnly))
}

// dayOf is the start of an instant's UTC day, which is what a refill is
// bounded by: the set-aside took every row back to that instant, and a bound
// later than it would lose the difference.
func dayOf(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(24 * time.Hour)
}

// missingFrom is every value of held that want does not name, compared the
// way GitHub compares logins and repository names: without case. An empty
// value is not a value.
func missingFrom(held, want []string) []string {
	var out []string
	for _, v := range held {
		if v != "" && !slices.ContainsFunc(want, func(w string) bool { return strings.EqualFold(v, w) }) {
			out = append(out, v)
		}
	}
	return out
}

// sample is the first few of a long list, and how many more there are.
func sample(names []string) string {
	const shown = 5
	if len(names) <= shown {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:shown], ", "), len(names)-shown)
}

// plural is one of two words by a count.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// orList names a list as alternatives.
func orList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// firstOf is the first of two strings that is not empty.
func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
