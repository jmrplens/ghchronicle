package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/config"
)

// Progress is where a backfill has got to, written down often enough that a
// deliberate stop costs one unit of work rather than the walk.
//
// The unit is a repository inside a family, and it is that rather than the
// family because of what a family costs. Measured on the author's account on
// 2026-09-17, fifty nine repositories, unbounded: the eleven account families
// together took four minutes, and then actions took two hours and forty five
// minutes, commits was still walking after three hours and a quarter when the
// run was stopped, and issueevents took seventeen. A checkpoint at the end of
// each family would have kept the four minutes and thrown away the six and a
// half hours, which is the walk this was asked for.
//
// A record here is a claim that somebody's store already holds those rows, so
// it is written after the rows have been handed to every sink and never
// before. That is the whole of why a resume may skip what it names: the
// repository was walked to the end of its own pagination and everything that
// walk produced was delivered. A repository whose walk failed part way, or
// whose rows one sink refused, is not recorded, so a resume walks it again.
// Walking it again is free of consequence: a point carries the date the thing
// happened, so re-collection rewrites the same rows rather than doubling them,
// which is the same property that lets a backfill be run twice.
//
// It is not the sweep's state file and deliberately not in it. Every field of
// State is a claim about cadences that the long running service reads on its
// next tick; this is a claim about one walk that stops existing when the walk
// ends. They are written at different rates, they are deleted for different
// reasons, and the one thing they share is the directory.
type Progress struct {
	path  string
	build string
	// resumed says this checkpoint was read back from disk rather than opened
	// on a walk that had not started, which is what the log line at start-up
	// and the version note below are decided by.
	resumed bool

	// Scope is the walk this checkpoint belongs to. A checkpoint read back
	// under a different one is refused, never resumed: see Scope.
	Scope Scope `json:"scope"`
	// WrittenBy is the build that wrote it. Not part of Scope and not a reason
	// to refuse: replacing the binary is the very thing that stopped the walk
	// this was written for. It is reported at resume so a reader who upgraded
	// mid walk knows that the families already recorded were collected by the
	// older build.
	WrittenBy string `json:"written_by"`
	// Started is when the walk this resumes first began, kept across resumes,
	// so a reader can see how long it has been going rather than how long this
	// process has.
	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
	// Complete is every family whose whole pass reached every sink, in the
	// order they finished, with the instant each one did. The instant is what
	// Restore puts back into the sweep's state file, so a resumed walk leaves
	// the same marks behind as one that was never stopped.
	Complete []FamilyDone `json:"families_complete"`
	// Written is, per family that is not complete, the repositories whose rows
	// have reached every sink, in the order they were walked. A family moves
	// out of here and into Complete when its last repository is done, so what
	// is left is the family that was in flight and how far into it the walk
	// had got.
	Written map[string][]string `json:"repositories_written"`
}

// FamilyDone is one family's completion: which, and when.
type FamilyDone struct {
	Family string    `json:"family"`
	At     time.Time `json:"at"`
}

// Scope is what a backfill was asked to walk: against which API, over which
// targets, for which families and how far back.
//
// It is compared before a checkpoint is resumed because the checkpoint is a
// list of work not to do again, and that list only means anything against the
// walk it was written for. The date bound is the one that would corrupt a
// history rather than merely waste a request: a walk bounded at a year records
// its repositories as written, and a resume with no bound at all would skip
// every one of them and leave a store that claims to hold everything while the
// first half of it stops a year back. Nothing in the rows themselves would ever
// say so.
//
// The bound is kept as it was asked for and not as it resolved. "2y" resolves
// to a different instant every time it is read, so comparing instants would
// refuse every resume of a relative bound, which is the spelling the
// documentation offers first.
type Scope struct {
	// BaseURL is the API this walk reads. Empty is api.github.com. A
	// checkpoint from one host says nothing about what another holds.
	BaseURL string `json:"base_url"`
	// Targets is every setting that decides which repositories the walk
	// covers, by the name the configuration file gives it. A map rather than
	// one rendered line so a refusal can name the setting that changed.
	Targets map[string]string `json:"targets"`
	// Families is the enabled families, sorted, since the walk covers those
	// and no others.
	Families []string `json:"families"`
	// Since is the date bound exactly as the command line or the file spelled
	// it. Empty is no bound.
	Since string `json:"since"`
}

// ScopeOf is the scope of the backfill cfg describes, bounded by since, which
// is the bound the command line gave or the configured one, as a caller
// resolves it before the walk starts.
func ScopeOf(cfg *config.Config, since string) Scope {
	families := []string{}
	for _, name := range config.Families() {
		if _, enabled := cfg.Interval(name); enabled {
			families = append(families, name)
		}
	}
	slices.Sort(families)
	return Scope{
		BaseURL:  cfg.GitHub.BaseURL,
		Targets:  scopeTargets(cfg.Targets),
		Families: families,
		Since:    strings.TrimSpace(since),
	}
}

// scopeTargets renders the target settings under the names the configuration
// file uses, so a refusal can quote the key the reader would edit.
//
// Every key here is a yaml name of config.Targets, and
// TestEveryTargetSettingIsPartOfTheWalkItShapes holds this to the type: a
// setting added to Targets and left out here would quietly stop being a reason
// to refuse, and a checkpoint would then be resumed into a repository set it
// was never written for.
func scopeTargets(t config.Targets) map[string]string {
	sorted := func(values []string) string {
		out := slices.Clone(values)
		slices.Sort(out)
		return strings.Join(out, ",")
	}
	return map[string]string{
		"user":             t.User,
		"orgs":             sorted(t.Orgs),
		"repos":            sorted(t.Repos),
		"exclude":          sorted(t.Exclude),
		"include_forks":    strconv.FormatBool(t.IncludeForks),
		"include_archived": strconv.FormatBool(t.IncludeArchived),
		"include_private":  strconv.FormatBool(t.PrivateIncluded()),
	}
}

// Differs reports, in a sentence, the first way other is not the same walk as
// s, and the empty string when it is the same walk.
//
// One difference and not all of them: the sentence goes into a refusal a
// person reads, and the question it answers is whether to put the setting back
// or to delete the checkpoint, which the first difference settles.
func (s Scope) Differs(other Scope) string {
	if s.BaseURL != other.BaseURL {
		return fmt.Sprintf("the API base url was %s and is now %s", shown(s.BaseURL), shown(other.BaseURL))
	}
	for _, key := range targetKeys(s.Targets, other.Targets) {
		if s.Targets[key] != other.Targets[key] {
			return fmt.Sprintf("targets.%s was %s and is now %s", key, shown(s.Targets[key]), shown(other.Targets[key]))
		}
	}
	for _, family := range other.Families {
		if !slices.Contains(s.Families, family) {
			return fmt.Sprintf("the family %s is collected now and was not when the walk began", family)
		}
	}
	for _, family := range s.Families {
		if !slices.Contains(other.Families, family) {
			return fmt.Sprintf("the family %s was collected when the walk began and is not now", family)
		}
	}
	if s.Since != other.Since {
		return fmt.Sprintf("the date bound was %s and is now %s", bound(s.Since), bound(other.Since))
	}
	return ""
}

// targetKeys is every key of both maps, sorted: a key one side is missing is a
// difference and not something to skip.
func targetKeys(a, b map[string]string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	keys := make([]string, 0, len(a)+len(b))
	for _, m := range []map[string]string{a, b} {
		for key := range m {
			if seen[key] {
				continue
			}
			seen[key] = true
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

// shown quotes a value for a refusal, and says so when there is none.
func shown(v string) string {
	if v == "" {
		return "unset"
	}
	return strconv.Quote(v)
}

// bound is shown for the date bound, whose empty value has a meaning of its
// own rather than being absent.
func bound(v string) string {
	if v == "" {
		return "none at all"
	}
	return strconv.Quote(v)
}

// OpenProgress reads the checkpoint at path, or starts one, and refuses
// anything it cannot resume honestly.
//
// Three answers. A path that holds nothing is a walk that has not started. A
// path that holds a checkpoint of this same scope is a walk to carry on. A
// path that holds anything else is an error, and the caller stops: a
// checkpoint that cannot be parsed, or that was written for another walk, is a
// list of work somebody's store may or may not already hold, and neither
// trusting it nor quietly ignoring it is defensible. Ignoring it is the worse
// of the two, because it looks like success.
//
// An empty path is no checkpoint at all, the way a state file with no path is
// never written, and it comes back as a Progress that answers no to Active.
// The tests and the one shot probe run that way.
func OpenProgress(path, build string, scope Scope, now time.Time) (*Progress, error) {
	if path == "" {
		return &Progress{build: build}, nil
	}
	fresh := &Progress{
		path: path, build: build, Scope: scope, WrittenBy: build,
		Started: now, Updated: now, Written: map[string][]string{},
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return fresh, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the backfill checkpoint %s cannot be read: %w", path, err)
	}
	saved := &Progress{}
	if err = json.Unmarshal(body, saved); err != nil {
		return nil, fmt.Errorf(
			"the backfill checkpoint %s cannot be read as one (%w). It is where an interrupted walk "+
				"recorded what it had already written, and a walk cannot be resumed from a file that "+
				"cannot be read. Delete it to start the walk again from the first repository", path, err,
		)
	}
	if why := saved.Scope.Differs(scope); why != "" {
		return nil, fmt.Errorf(
			"the backfill checkpoint %s belongs to a different walk: %s. It lists repositories that were "+
				"already written, and that list means nothing under settings it was not written for. "+
				"Put the settings back to resume it, or delete the file to start the walk again", path, why,
		)
	}
	saved.path, saved.build, saved.resumed = path, build, true
	if saved.Written == nil {
		saved.Written = map[string][]string{}
	}
	saved.Updated = now
	return saved, nil
}

// Active reports whether there is a checkpoint to write. A nil Progress, and
// one with nowhere to put itself, are both runs that keep none: every sweep is
// one, and so is a run whose state lives in memory.
//
// The methods that write guard on this; the ones that only read guard on nil,
// so a checkpoint read back off disk by hand answers about itself whether or
// not it is the one this process is keeping.
func (p *Progress) Active() bool { return p != nil && p.path != "" }

// Resumed reports whether this checkpoint was read back from disk, which is
// the difference between carrying a walk on and starting one.
func (p *Progress) Resumed() bool { return p != nil && p.resumed }

// Path is the file, for the lines that tell a reader where to look.
func (p *Progress) Path() string {
	if p == nil {
		return ""
	}
	return p.path
}

// WrittenByAnother reports the build that wrote the checkpoint when it is not
// the one reading it.
func (p *Progress) WrittenByAnother() (string, bool) {
	if !p.Resumed() || p.WrittenBy == p.build {
		return "", false
	}
	return p.WrittenBy, true
}

// FamilyDone reports whether a family's whole pass has already been written.
func (p *Progress) FamilyDone(family string) bool {
	if p == nil {
		return false
	}
	return slices.ContainsFunc(p.Complete, func(d FamilyDone) bool { return d.Family == family })
}

// RepoDone reports whether one repository's rows for a family have already
// been written.
func (p *Progress) RepoDone(family, repo string) bool {
	if p == nil {
		return false
	}
	return slices.Contains(p.Written[family], repo)
}

// WroteRepo records that a repository's rows for a family reached every sink,
// and saves.
//
// Saved on every repository rather than on a timer. The walk is hours long and
// the cost is one small file rewritten at most a few thousand times over those
// hours, against the alternative of a stop landing between two saves and
// costing whatever fell in the gap.
func (p *Progress) WroteRepo(family, repo string, now time.Time) error {
	if !p.Active() || p.RepoDone(family, repo) {
		return nil
	}
	p.Written[family] = append(p.Written[family], repo)
	return p.save(now)
}

// FinishFamily records that a family's whole pass is written, and saves.
//
// The repositories it listed go with it: they are the detail of a family that
// was in flight, and once the family is done the detail is noise in a file
// somebody reads to find out where a walk stopped.
func (p *Progress) FinishFamily(family string, now time.Time) error {
	if !p.Active() || p.FamilyDone(family) {
		return nil
	}
	p.Complete = append(p.Complete, FamilyDone{Family: family, At: now})
	delete(p.Written, family)
	return p.save(now)
}

// Restore puts the marks of the families this checkpoint already completed
// back into the sweep's state file, so a walk that was stopped and resumed
// leaves the same state behind as one that was never stopped.
//
// The instants are the ones the interrupted run recorded, not this one's: the
// family ran when it ran, and a mark dated now would tell the next sweep that
// a family collected six hours ago is fresh.
func (p *Progress) Restore(s *State) {
	if p == nil || s == nil {
		return
	}
	for _, done := range p.Complete {
		s.Mark(done.Family, done.At)
	}
}

// Where is what the checkpoint holds, for the line a reader meets at the stop:
// how many families are complete, which family was in flight, and how many of
// its repositories were written.
func (p *Progress) Where() (families int, family string, repos int) {
	if p == nil {
		return 0, "", 0
	}
	// The family in flight is the one with repositories written and no
	// completion. There is at most one: a family is walked to its end or the
	// walk stops inside it.
	for name, written := range p.Written {
		if !p.FamilyDone(name) && len(written) > 0 {
			family, repos = name, len(written)
			break
		}
	}
	return len(p.Complete), family, repos
}

// Elapsed is how long the walk this checkpoint records has been going,
// counting the runs before this one.
func (p *Progress) Elapsed(now time.Time) time.Duration {
	if p == nil || p.Started.IsZero() {
		return 0
	}
	return now.Sub(p.Started)
}

// Clear removes the checkpoint, which is what the end of a walk does with it.
// The temporary file of an interrupted save goes too, so nothing is left
// behind for the next walk to find and wonder about.
func (p *Progress) Clear() error {
	if !p.Active() {
		return nil
	}
	_ = os.Remove(p.path + ".tmp")
	if err := os.Remove(p.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// save writes the checkpoint through a temporary file and a rename, so a kill
// between two saves leaves either the checkpoint before it or the one after it
// and never a half of either. See replaceFile for the durability half of that.
func (p *Progress) save(now time.Time) error {
	p.Updated = now
	p.WrittenBy = p.build
	if dir := filepath.Dir(p.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	// Indented, because this file is also the answer to "what had it done",
	// which somebody reads with cat after stopping a six hour walk.
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return replaceFile(p.path, body)
}

// openProgress says what a resumable backfill is carrying on from, once, at
// the start of the walk.
//
// The checkpoint itself is opened by the caller that can refuse the run, so
// that a walk which must not be resumed never starts collecting; this is only
// the line the reader of the journal meets.
func (r *Runner) openProgress(now time.Time) {
	if !r.Progress.Resumed() {
		return
	}
	r.Progress.Restore(r.State)
	families, family, repos := r.Progress.Where()
	args := []any{
		"file", r.Progress.Path(),
		"started", r.Progress.Started.Format(time.RFC3339),
		"running_for", r.Progress.Elapsed(now).Round(time.Second).String(),
		"families_complete", families,
	}
	if family != "" {
		args = append(args, "family", family, "repositories_written", repos)
	}
	r.Log.Info("resuming the backfill this checkpoint was left by", args...)
	if was, upgraded := r.Progress.WrittenByAnother(); upgraded {
		// Not a refusal: a binary being replaced is what stopped the walk
		// this resumes. It is worth one line, because what the checkpoint
		// names was collected by the build that wrote it, and a family whose
		// walk this build reaches further back will not be walked again.
		r.Log.Warn("this checkpoint was written by another build, and what it names was collected by that one",
			"written_by", was, "running", r.Progress.build)
	}
}

// closeProgress ends a resumable backfill: the checkpoint goes when the walk
// reached the end of every family, and stays, with a line saying where it
// stopped, when it did not.
//
// The context is asked as well as the error, because a stopped walk can reach
// here with neither. A cancellation during a rate limit wait makes awaitBudget
// answer no, and every family left is then skipped rather than failed, which
// arrives as a sweep that finished. Removing the checkpoint there would throw
// away the hours it records for exactly the stop this was written for.
func (r *Runner) closeProgress(ctx context.Context, err error) {
	if !r.Progress.Active() {
		return
	}
	if err != nil || ctx.Err() != nil {
		families, family, repos := r.Progress.Where()
		args := []any{
			"file", r.Progress.Path(),
			"running_for", r.Progress.Elapsed(r.clock()).Round(time.Second).String(),
			"families_complete", families,
		}
		if family != "" {
			args = append(args, "family", family, "repositories_written", repos)
		}
		args = append(args, "resume", "run the same command again")
		r.Log.Info("backfill stopped, and what it had written is kept", args...)
		return
	}
	if clearErr := r.Progress.Clear(); clearErr != nil {
		r.Log.Warn("the backfill checkpoint could not be removed",
			"file", r.Progress.Path(), "err", clearErr)
		return
	}
	r.Log.Info("backfill complete, its checkpoint is removed", "file", r.Progress.Path())
}

// checkpointRepo records a repository whose rows have reached every sink.
func (r *Runner) checkpointRepo(family, repo string) {
	r.noteCheckpoint(r.Progress.WroteRepo(family, repo, r.clock()))
}

// checkpointFamily records a family whose whole pass has reached every sink.
func (r *Runner) checkpointFamily(family string) {
	r.noteCheckpoint(r.Progress.FinishFamily(family, r.clock()))
}

// noteCheckpoint reports a checkpoint that could not be saved, once.
//
// It is a warning and not a failure. The walk is hours of somebody's quota and
// it is collecting correctly; what it has lost is the ability to be resumed,
// and stopping it to say so would cost the very thing the checkpoint exists to
// protect.
func (r *Runner) noteCheckpoint(err error) {
	if err == nil || r.progressWarned {
		return
	}
	r.progressWarned = true
	r.Log.Warn("the backfill checkpoint cannot be written, so a stop will cost the whole walk",
		"file", r.Progress.Path(), "err", err)
}
