package sink

import (
	"maps"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Summarize turns the collectors' dated points into the far smaller set of
// current-state gauges that Prometheus can hold.
//
// The two stores want opposite things. InfluxDB keeps a row per fact, dated
// when the fact happened: one row per star, per pull request, per workflow run.
// Prometheus keeps a series per label combination and stamps every sample at
// scrape time, so feeding it the same rows would both lose the dates and
// create a series per star that never changes again.
//
// So each measurement gets a rule saying how to reduce it:
//
//   - keepLast: a snapshot. The most recent point per label set wins.
//   - sum: a window. Every point in the batch is added up, which is what makes
//     "views over GitHub's 14-day window" a single number.
//   - count: dated items. The points become a count plus the mean of each of
//     their numeric fields, so "how many pull requests merged and how long they
//     took" survives as two gauges instead of four hundred series.
//   - skip: history that has no honest current value at all.
//
// Anything not named here is skipped rather than guessed at, so a new collector
// cannot quietly flood an exporter with per-item series. That silence is safe
// but it is not free: a measurement nobody names here reaches InfluxDB and
// never reaches Prometheus or OTLP, with no error anywhere. Six of them did.
// TestEveryMeasurementHasAReductionRule reads the collectors' own source and
// fails when one is missing, so the choice has to be made rather than skipped
// into.
type reduce int

const (
	skip reduce = iota
	keepLast
	sum
	count
)

type rule struct {
	mode reduce
	// keep lists the tags that survive. Everything else is dropped, which is
	// what collapses the series count.
	keep []string
	// as renames the measurement when the reduction changes what it means:
	// counting pull requests is not the same thing as a pull request.
	as string
	// labels lists fields the reduction reads as labels, beside the tags in
	// keep. A value that moves after the fact, an alert's state or whether a
	// discussion has its answer, is a field in every store that keys a row by
	// its tags and its time: as a tag it opened a second series at the same
	// instant the moment it changed, and the stale row sat beside the new one
	// for ever. Here every gauge is stamped at the sweep and nothing is keyed
	// by the past, so the same value can be a label without a row ever
	// doubling, and it has to be one for "alerts by state" to exist at all.
	labels []string
}

var promRules = map[string]rule{
	// Account-wide snapshots.
	"gh_account":             {mode: keepLast, keep: []string{"user"}},
	"gh_contributions_total": {mode: keepLast, keep: []string{"user"}},
	"gh_contribution_repo":   {mode: keepLast, keep: []string{"user", "repo", "kind"}},
	"gh_social_account":      {mode: keepLast, keep: []string{"user", "provider"}},
	"gh_achievement":         {mode: keepLast, keep: []string{"user", "achievement"}},
	// The progress beside each badge: a daily snapshot like the badge, and
	// the newest row per badge is the whole answer.
	"gh_achievement_progress": {mode: keepLast, keep: []string{"user", "achievement"}},
	"gh_package":              {mode: keepLast, keep: []string{"user", "package", "type", "visibility"}},
	"gh_gist":                 {mode: keepLast, keep: []string{"user", "gist", "public"}},

	// The profile's standing furniture. The collector stamps all of it now,
	// so the newest reading is the only one that means anything: which
	// repositories are pinned and where, which badges the account wears, and
	// what the sponsors page earns and is owed.
	"gh_pinned_item":      {mode: keepLast, keep: []string{"user", "repo"}},
	"gh_profile_flag":     {mode: keepLast, keep: []string{"user", "flag"}},
	"gh_sponsors_listing": {mode: keepLast, keep: []string{"user"}},
	// Tiers are inventory rather than a stream. The collector anchors them to
	// the start of the UTC day so two sweeps converge on one row per tier;
	// counted, the same eight tiers would be eight more every day forever.
	"gh_sponsors_tier": {mode: keepLast, keep: []string{"user", "tier"}},
	// The lists the stars given are filed into: a daily snapshot anchored
	// like the tiers, so the newest reading per list is the whole answer.
	"gh_star_list": {mode: keepLast, keep: []string{"user", "list"}},

	// Repository snapshots.
	"gh_repo": {mode: keepLast, keep: []string{
		"owner", "repo", "full_name", "language", "visibility", "license",
		"archived", "fork", "default_branch",
	}},
	"gh_repo_language":  {mode: keepLast, keep: []string{"repo", "language"}},
	"gh_repo_topic":     {mode: keepLast, keep: []string{"repo", "topic"}},
	"gh_repo_community": {mode: keepLast, keep: []string{"repo"}},
	"gh_workflow":       {mode: keepLast, keep: []string{"repo", "workflow", "state"}},
	"gh_actions_cache":  {mode: keepLast, keep: []string{"repo"}},
	"gh_artifact_total": {mode: keepLast, keep: []string{"repo"}},
	// How many runs a repository has ever had. The walk only ever sees the
	// newest few hundred, so this is a current total in its own right, the
	// twin of gh_artifact_total above it.
	"gh_workflow_run_total": {mode: keepLast, keep: []string{"repo"}},
	"gh_release":            {mode: keepLast, keep: []string{"repo", "tag", "draft", "prerelease"}},

	// Security, which is a current state by definition.
	"gh_dependabot_alert":    {mode: keepLast, keep: []string{"repo", "severity", "ecosystem"}},
	"gh_code_scanning_alert": {mode: keepLast, keep: []string{"repo", "severity", "tool"}},
	"gh_security_feature":    {mode: keepLast, keep: []string{"repo", "feature"}},

	// Windows that only mean anything added up.
	"gh_traffic":          {mode: sum, keep: []string{"repo", "kind"}},
	"gh_traffic_referrer": {mode: sum, keep: []string{"repo", "referrer"}},
	"gh_billing_usage":    {mode: sum, keep: []string{"product", "sku", "unit", "repo"}},

	// Dated items, reduced to a count and the mean of their numbers.
	"gh_pull_request": {mode: count, as: "gh_pull_requests", keep: []string{"repo", "state"}},
	"gh_issue":        {mode: count, as: "gh_issues", keep: []string{"repo", "state"}, labels: []string{"resolution"}},
	// resolution is kept because closed says nothing about how: an issue
	// finished and an issue abandoned are the same tag without it, and a
	// mean time to close that mixes them answers a question nobody asked.
	"gh_workflow_run": {
		mode: count, as: "gh_workflow_runs",
		keep: []string{"repo", "workflow", "conclusion"},
	},
	"gh_workflow_job": {
		mode: count, as: "gh_workflow_jobs",
		keep: []string{"repo", "job_name", "conclusion"},
	},
	"gh_star":         {mode: count, as: "gh_stars_gained", keep: []string{"repo"}},
	"gh_event":        {mode: count, as: "gh_events", keep: []string{"type"}},
	"gh_notification": {mode: count, as: "gh_notifications", keep: []string{"reason", "subject_type"}},
	"gh_discussion":   {mode: count, as: "gh_discussions", keep: []string{"repo", "category"}, labels: []string{"has_answer"}},
	// A sponsorship is a payment, dated when the money moved, and it is the
	// only dated record of it: gh_account.sponsoring counts as of now and
	// says neither when nor to whom. It reduces the way a star does, so
	// increase() can answer "sponsorships this month". Direction survives
	// because money out and money in are not the same series; the other
	// party does not, or every sponsor would be a series of its own.
	"gh_sponsorship": {mode: count, as: "gh_sponsorships", keep: []string{"user", "direction"}},

	// History with no current value: the calendar, the weekly commit series,
	// per-day traffic paths, per-artifact rows and the per-step timings.
	"gh_contribution_day": {mode: skip},
	"gh_commits_week":     {mode: skip},
	"gh_traffic_path":     {mode: skip},
	"gh_artifact":         {mode: skip},
	"gh_workflow_step":    {mode: skip},

	// Two more that are skipped for size rather than meaning, measured on an
	// account with 18 repositories and 132 releases: the punch card is one
	// series per repository, weekday and hour (1,217 of them) and the release
	// assets one per file ever published (1,216). Both are distributions the
	// InfluxDB dashboard draws properly; as gauges they would be four fifths
	// of the exporter's whole output. gh_release keeps the per-release
	// download counts, which is what the assets were being read for.
	"gh_commit_punchcard": {mode: skip},
	"gh_release_asset":    {mode: skip},

	// The publication date of every container tag is history; the count of
	// them is already a field on gh_package.
	"gh_package_version": {mode: skip},

	// Text, not a number. It goes to a log store.
	"gh_job_log": {mode: skip},

	"gh_webhook":          {mode: keepLast, keep: []string{"repo", "hook", "host", "active"}},
	"gh_webhook_delivery": {mode: count, as: "gh_webhook_deliveries", keep: []string{"repo", "hook", "ok", "code"}},
	"gh_ruleset":          {mode: keepLast, keep: []string{"repo", "ruleset", "enforcement"}},
	// Every saved version of a ruleset, dated when it was saved. Counted,
	// because the changelog is the history gh_ruleset.days_since_change only
	// summarizes, and a series per version id would never move again.
	"gh_ruleset_version": {mode: count, as: "gh_ruleset_versions", keep: []string{"repo", "ruleset", "actor_type"}},
	"gh_environment":     {mode: keepLast, keep: []string{"repo", "environment"}},
	"gh_deploy_key":      {mode: keepLast, keep: []string{"repo", "key", "read_only"}},

	// Added with the coverage audit. The per-item ones become counts, the
	// standing ones keep their newest value, and the pure history is skipped.
	"gh_pull_request_review": {mode: count, as: "gh_reviews", keep: []string{"repo", "reviewer", "bot", "self"}, labels: []string{"review_state"}},
	// The gate's verdict is a field, since it lands after the commit's own
	// date; the dashboard's "commits by gate state" reads it as a label.
	"gh_commit":                 {mode: count, as: "gh_commits", keep: []string{"repo", "author", "signature"}, labels: []string{"gate"}},
	"gh_repo_activity":          {mode: count, as: "gh_repo_activities", keep: []string{"repo", "activity"}},
	"gh_code_scanning_analysis": {mode: count, as: "gh_code_scanning_analyses", keep: []string{"repo", "tool"}},
	"gh_fork":                   {mode: count, as: "gh_forks_seen", keep: []string{"repo"}},
	"gh_star_given":             {mode: count, as: "gh_stars_given", keep: []string{"user"}},
	"gh_external_contribution":  {mode: count, as: "gh_external_contributions", keep: []string{"user", "repo"}},
	"gh_dependabot_alert_item":  {mode: count, as: "gh_dependabot_alerts", keep: []string{"repo", "severity"}, labels: []string{"alert_state"}},
	"gh_label":                  {mode: keepLast, keep: []string{"repo", "label"}},
	"gh_milestone":              {mode: keepLast, keep: []string{"repo", "milestone", "state"}},
	"gh_contribution_year":      {mode: keepLast, keep: []string{"user", "year"}},

	// Lifetime counts, which are already one row each: the exporter serves
	// them as they are. This is the one family that needs no reduction,
	// because it was built to be read without one.
	"gh_account_total": {mode: keepLast, keep: []string{"user"}},
	"gh_repo_total": {mode: keepLast, keep: []string{
		"owner", "repo", "full_name", "visibility", "archived", "fork",
	}},
	"gh_rate_limit":  {mode: keepLast, keep: []string{"resource"}},
	"gh_repo_policy": {mode: keepLast, keep: []string{"owner", "repo", "full_name"}},

	// Code scanning per item, the twin of the Dependabot rule above.
	"gh_code_scanning_alert_item": {mode: count, as: "gh_code_scanning_alerts", keep: []string{"repo", "severity"}, labels: []string{"alert_state"}},
	// Repositories created, including the forks a sweep never discovers.
	"gh_repo_created": {mode: count, as: "gh_repos_created", keep: []string{"user", "fork"}},

	// Comments, wherever they were left. `own` is what separates the work in
	// one's own repositories from the work in everybody else's.
	"gh_discussion_comment": {mode: count, as: "gh_discussion_comments", keep: []string{"user", "own", "is_answer"}},
	"gh_issue_comment":      {mode: count, as: "gh_issue_comments", keep: []string{"user", "own"}},
	// Checks that are not Actions: the gate nothing else in here can see.
	"gh_commit_check": {mode: count, as: "gh_commit_checks", keep: []string{"repo", "app", "conclusion"}},

	// Transitions, which are events rather than state.
	"gh_issue_event": {mode: count, as: "gh_issue_events", keep: []string{"repo", "event", "kind", "bot"}},
	// One row per cache entry, and per key rather than per build.
	"gh_actions_cache_entry": {mode: keepLast, keep: []string{"repo", "cache", "ref"}},
	// The account's keys, which are a standing fact with an expiry date.
	"gh_key": {mode: keepLast, keep: []string{"user", "kind", "key"}},

	// The dependency graph: a photograph and a difference.
	"gh_dependency":         {mode: keepLast, keep: []string{"repo", "ecosystem"}},
	"gh_dependency_license": {mode: keepLast, keep: []string{"repo", "license"}},
	"gh_dependency_change":  {mode: sum, keep: []string{"repo", "change", "ecosystem"}},

	// Added with the fourth metrics audit.
	//
	// The standing configuration of a repository, all of it stamped daily or
	// now by its collector, so the newest reading is the only one that means
	// anything. Each keeps the tags that carry the answer and drops `owner`
	// and `full_name`, which is what the other per-repository snapshots above
	// already do: `full_name` is `owner/repo` and repeats the label for
	// nothing.
	"gh_security_setting": {mode: keepLast, keep: []string{"repo", "setting", "status"}},
	"gh_actions_policy":   {mode: keepLast, keep: []string{"repo", "permissions"}},
	"gh_secret":           {mode: keepLast, keep: []string{"repo", "kind", "secret"}},
	"gh_code_scanning_setup": {mode: keepLast, keep: []string{
		"repo", "state", "query_suite", "schedule",
	}},
	"gh_policy_file":          {mode: keepLast, keep: []string{"repo", "file"}},
	"gh_dependabot_ecosystem": {mode: keepLast, keep: []string{"repo", "ecosystem", "interval"}},
	"gh_branch":               {mode: keepLast, keep: []string{"repo", "branch", "is_default"}},
	"gh_branch_protection":    {mode: keepLast, keep: []string{"repo", "pattern"}},
	"gh_ruleset_rule":         {mode: keepLast, keep: []string{"repo", "ruleset", "rule"}},

	// Dated items, the same reduction their siblings get.
	//
	// A deployment reduces like a workflow run, and for the same reason: the
	// question is how often this environment is deployed to and how long it
	// takes, not what each individual deployment did. `state` is the stable
	// outcome the collector derives, not the raw enum, which is a field
	// precisely because it moves.
	"gh_deployment": {mode: count, as: "gh_deployments", keep: []string{"repo", "environment"}, labels: []string{"outcome"}},
	// Review threads reduce to the size of the debt and to two shares: how
	// much of it is resolved and how much no longer applies to the current
	// code. Both are integer fields rather than tags, so the mean of each is
	// exactly that share. `bot` stays because a backlog of bot objections
	// reads differently from a backlog of human ones.
	"gh_review_thread": {mode: count, as: "gh_review_threads", keep: []string{"repo", "bot"}},
	// Repositories archived, the dated twin of gh_repo_created. Counted and
	// not kept: every sweep re-emits the whole list, so the count is
	// "repositories ever archived" and stays one gauge per owner, where
	// keepLast would publish one series per archived repository that can
	// never move again.
	"gh_repo_archived": {mode: count, as: "gh_repos_archived", keep: []string{"owner"}},

	// The green calendar split per repository per day. Skipped for the same
	// reason gh_contribution_day is: it is history with no current value, and
	// counting it would mint a Prometheus series per repository per day.
	"gh_contribution_day_repo": {mode: skip},
}

// notAveraged is every field the count reduction publishes no mean of, because
// the number carries no quantity to average.
//
// Two kinds land here. A marker exists only so a point has a field at all, so
// its mean is 1.0 for ever and says nothing `count` has not already said. An
// identifier is worse than saying nothing: the arithmetic mean of a set of
// workflow run ids is a number shaped exactly like a run id that belongs to no
// run, and a reader has no way to tell it from one. gh_deployment published
// `run_id_mean` that way, and gh_workflow_run, gh_workflow_job,
// gh_repo_activity, gh_issue_event, gh_issue, gh_commit and gh_pull_request
// each published one or two of their own.
//
// Keyed by field name and not by measurement, deliberately: an identifier is
// an identifier wherever it is written, and the same name means the same thing
// in every collector here. A future field that genuinely counts something must
// not be given one of these names.
var notAveraged = map[string]bool{
	"events": true, "notifications": true, "starred": true, "present": true,
	// The counters the audit's new per-item measurements carry so that a
	// point always has a field. Their mean is 1.0 for ever.
	"deployments": true, "archived": true,
	// gh_sponsorship carries this so a sponsorship with a hidden tier still
	// has a field. Averaged it is 1.0 for ever, which is what `count` already
	// says honestly.
	"sponsorship": true,

	// Identifiers. Each is a field rather than a tag because it is unbounded,
	// which is right, and each is there so one row can be joined to another:
	// a job to its run, a commit to the pull request that carried it, a
	// deployment to the workflow run that performed it.
	"run_id": true, "workflow_id": true, "id": true, "repo_id": true,
	"commit_id": true, "number": true, "pull_request": true,
	// The issue an issue sits under, or zero: a number, not a quantity. It
	// was the tag `parent` until it became a field for moving after the
	// row's date, and a field on a counted measurement is averaged unless
	// named here.
	"parent_issue": true,
	// The "#1483" a run is quoted by: a serial, not a quantity, and its
	// mean is a run number that belongs to no run.
	"run_number": true,
	// The stack a pull request belongs to, which is the stack's own number and
	// not a size: stack_size and stack_position are quantities and keep their
	// means.
	"stack": true,
	// A ruleset version names three things and measures none: the version,
	// the ruleset it belongs to and the actor that saved it. `versions` is
	// its marker.
	"version_id": true, "ruleset_id": true, "actor_id": true, "versions": true,
}

type acc struct {
	measurement string
	mode        reduce
	tags        map[string]string
	fields      map[string]float64
	n           int
	total       int
	newest      time.Time
	last        map[string]any
}

// Reducer turns batches of dated points into current-state gauges, and
// remembers enough between batches to publish counters.
//
// A stateless reduction can say how many pull requests were in the last sweep
// window. It cannot say how many there have been, and "how many per day" is
// the question a dashboard actually asks. Prometheus answers that from a
// monotonically increasing total and increase(), so the reducer keeps the
// identity of every item it has seen, per reduced label set, and publishes
// the running distinct count as `total`. It resets when the process does,
// which is the counter reset Prometheus is built to handle.
type Reducer struct {
	mu sync.Mutex
	// seen holds, per reduced series key, the identities already counted
	// and how many events each stands for; totals is the sum of those, kept
	// as it goes rather than added up on every point.
	seen   map[string]map[string]int
	totals map[string]int
}

// NewReducer returns an empty reducer.
func NewReducer() *Reducer {
	return &Reducer{seen: map[string]map[string]int{}, totals: map[string]int{}}
}

// Summarize reduces a batch statelessly. Counters are not produced; use a
// Reducer for those.
func Summarize(points []Point) []Point { return NewReducer().Reduce(points) }

// Reduce reduces a batch of points. The result is safe to expose as gauges.
func (rd *Reducer) Reduce(points []Point) []Point {
	rd.mu.Lock()
	defer rd.mu.Unlock()

	var s series
	for _, p := range points {
		rd.fold(&s, p)
	}
	return s.gauges(time.Now())
}

// series is the set of accumulators a reduction is building, in the order each
// one first appeared. The order is kept because the output would otherwise
// depend on map iteration, and a caller comparing two runs would see the same
// gauges shuffled.
type series struct {
	byKey map[string]*acc
	order []string
}

// at returns the accumulator for one reduced series, creating it on the first
// point that lands in it.
func (s *series) at(key, name string, mode reduce, tags map[string]string) *acc {
	if a, seen := s.byKey[key]; seen {
		return a
	}
	if s.byKey == nil {
		s.byKey = map[string]*acc{}
	}
	a := &acc{measurement: name, mode: mode, tags: tags, fields: map[string]float64{}}
	s.byKey[key] = a
	s.order = append(s.order, key)
	return a
}

// fold adds one point to the series its rule reduces it to. A measurement with
// no rule, or one whose rule is skip, has no honest current value and is left
// out entirely.
func (rd *Reducer) fold(s *series, p Point) {
	r, known := promRules[p.Measurement]
	if !known || r.mode == skip {
		return
	}
	name := p.Measurement
	if r.as != "" {
		name = r.as
	}
	tags := keptTags(p.Tags, r.keep)
	promote(tags, p.Fields, r.labels)
	key := name + "|" + tagKey(tags)

	a := s.at(key, name, r.mode, tags)
	a.n++
	switch r.mode {
	case keepLast:
		if p.Time.After(a.newest) || a.last == nil {
			a.newest, a.last = p.Time, p.Fields
		}
	case count:
		a.total = rd.countDistinct(key, p)
		a.addNumbers(p.Fields)
	case sum:
		a.addNumbers(p.Fields)
	}
}

// keptTags is the reduced label set: the tags the rule names and nothing else,
// which is what collapses a series per item into a series per thing.
func keptTags(tags map[string]string, keep []string) map[string]string {
	out := map[string]string{}
	for _, k := range keep {
		if v, ok := tags[k]; ok && v != "" {
			out[k] = v
		}
	}
	return out
}

// promote copies the fields a rule names into the reduced label set, as
// text: a string as it is, a boolean as true or false, a number in decimal.
// A field the point does not carry, or carries empty, adds no label, the
// same way keptTags treats a tag.
func promote(tags map[string]string, fields map[string]any, labels []string) {
	for _, k := range labels {
		v, ok := fields[k]
		if !ok {
			continue
		}
		var text string
		switch t := v.(type) {
		case string:
			text = t
		case bool:
			text = strconv.FormatBool(t)
		default:
			if n, isNumber := numeric(v); isNumber {
				text = strconv.FormatFloat(n, 'g', -1, 64)
			}
		}
		if text != "" {
			tags[k] = text
		}
	}
}

// countDistinct remembers this item under its reduced series and reports how
// many distinct items that series has seen since the process started. The
// identity of an item is its full tag set and its own time, which is exactly
// what makes it one row in InfluxDB.
//
// An item that stands for several events counts as that many: gh_repo_activity
// folds the branches one push moved in one second into one row whose `events`
// says how many, because the stores key by tags and time and would have kept
// one of them. Every other counted measurement writes `events` as 1 or not at
// all, so nothing else changes. The identity is remembered with the newest
// weight it was seen with, so a row rewritten with a larger count corrects
// the total rather than adding to it.
func (rd *Reducer) countDistinct(key string, p Point) int {
	ident := tagKey(p.Tags) + "@" + strconv.FormatInt(p.Time.UnixNano(), 10)
	if rd.seen[key] == nil {
		rd.seen[key] = map[string]int{}
	}
	weight := 1
	if n, ok := numeric(p.Fields["events"]); ok && n > 0 {
		weight = int(n)
	}
	rd.totals[key] += weight - rd.seen[key][ident]
	rd.seen[key][ident] = weight
	return rd.totals[key]
}

// addNumbers adds every numeric field into the running total. A string field
// has no total, so it is passed over rather than coerced.
func (a *acc) addNumbers(fields map[string]any) {
	for f, v := range fields {
		if n, ok := numeric(v); ok {
			a.fields[f] += n
		}
	}
}

// gauges renders each accumulator as the one point that now stands for it, all
// stamped with the same now: these are current values, and Prometheus and OTLP
// both read them as of the moment they were produced.
func (s *series) gauges(now time.Time) []Point {
	out := make([]Point, 0, len(s.order))
	for _, key := range s.order {
		a := s.byKey[key]
		fields := a.reduced()
		if len(fields) == 0 {
			continue
		}
		out = append(out, Point{Measurement: a.measurement, Tags: a.tags, Fields: fields, Time: now})
	}
	return out
}

// reduced is the accumulator's fields in the shape its mode publishes.
func (a *acc) reduced() map[string]any {
	fields := map[string]any{}
	switch a.mode {
	case keepLast:
		maps.Copy(fields, a.last)
	case count:
		// The count itself, then the mean of each number the items carried.
		// A sum of "seconds to merge" would be meaningless; the average is
		// the thing a maintainer reads.
		fields["count"] = a.n
		fields["total"] = a.total
		for f, total := range a.fields {
			if notAveraged[f] {
				continue // a marker or an identifier: see notAveraged
			}
			fields[f+"_mean"] = total / float64(a.n)
		}
	default:
		for f, total := range a.fields {
			fields[f] = total
		}
	}
	return fields
}

func tagKey(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(tags[k])
		b.WriteByte(',')
	}
	return b.String()
}
