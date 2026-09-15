package sink

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSummarizeCountsAndAverages(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	points := []Point{
		{
			Measurement: "gh_pull_request", Tags: map[string]string{"repo": "a", "state": "MERGED", "author": "x"},
			Fields: map[string]any{"seconds_to_merge": 100}, Time: base,
		},
		{
			Measurement: "gh_pull_request", Tags: map[string]string{"repo": "a", "state": "MERGED", "author": "y"},
			Fields: map[string]any{"seconds_to_merge": 300}, Time: base.Add(time.Hour),
		},
	}
	out := Summarize(points)
	if len(out) != 1 {
		t.Fatalf("two pull requests of one repository and state must reduce to one series, got %d", len(out))
	}
	p := out[0]
	if p.Measurement != "gh_pull_requests" {
		t.Errorf("counting pull requests is a different metric: got %s", p.Measurement)
	}
	if _, ok := p.Tags["author"]; ok {
		t.Error("author is not in the keep list and would multiply the series")
	}
	if p.Fields["count"] != 2 {
		t.Errorf("count = %v, want 2", p.Fields["count"])
	}
	if p.Fields["seconds_to_merge_mean"] != 200.0 {
		t.Errorf("mean = %v, want 200", p.Fields["seconds_to_merge_mean"])
	}
}

func TestSummarizeKeepsTheNewestSnapshot(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tags := map[string]string{"repo": "a", "owner": "o"}
	out := Summarize([]Point{
		{Measurement: "gh_repo", Tags: tags, Fields: map[string]any{"stars": 5}, Time: base},
		{Measurement: "gh_repo", Tags: tags, Fields: map[string]any{"stars": 9}, Time: base.Add(time.Hour)},
	})
	if len(out) != 1 || out[0].Fields["stars"] != 9 {
		t.Fatalf("a snapshot must keep the newest value, got %+v", out)
	}
}

func TestSummarizeSumsAWindow(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var pts []Point
	for i := range 14 {
		pts = append(pts, Point{
			Measurement: "gh_traffic",
			Tags:        map[string]string{"repo": "a", "kind": "views"},
			Fields:      map[string]any{"count": 10}, Time: base.AddDate(0, 0, i),
		})
	}
	out := Summarize(pts)
	if len(out) != 1 || out[0].Fields["count"] != 140.0 {
		t.Fatalf("the traffic window is only meaningful added up, got %+v", out)
	}
}

func TestSummarizeSkipsHistory(t *testing.T) {
	out := Summarize([]Point{{
		Measurement: "gh_contribution_day",
		Tags:        map[string]string{"user": "u"}, Fields: map[string]any{"contributions": 3},
		Time: time.Now(),
	}})
	if len(out) != 0 {
		t.Fatalf("the contribution calendar has no current value; got %+v", out)
	}
}

func TestSummarizeIgnoresUnknownMeasurements(t *testing.T) {
	out := Summarize([]Point{{
		Measurement: "gh_something_new",
		Fields:      map[string]any{"x": 1}, Time: time.Now(),
	}})
	if len(out) != 0 {
		t.Fatal("an unruled measurement must be skipped, not guessed at")
	}
}

func TestSponsorshipsCountAndTiersDoNot(t *testing.T) {
	// The two look alike and reduce in opposite ways. A sponsorship is a
	// payment, dated when it was made, so re-reading it twice must not make it
	// two. A tier is inventory the collector re-stamps at the start of every
	// UTC day, so counting it would add the same tier again every day.
	day := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	paid := Point{
		Measurement: "gh_sponsorship",
		Tags:        map[string]string{"user": "u", "direction": "maintainer", "sponsorable": "someone"},
		Fields:      map[string]any{"sponsorship": 1, "amount_cents": 1000, "active": false},
		Time:        time.Date(2021, 8, 31, 6, 42, 50, 0, time.UTC),
	}
	tier := func(at time.Time) Point {
		return Point{
			Measurement: "gh_sponsors_tier",
			Tags:        map[string]string{"user": "u", "tier": "$30 a month"},
			Fields:      map[string]any{"tiers": 1, "price_cents": 3000, "age_days": 1980},
			Time:        at,
		}
	}

	rd := NewReducer()
	first := rd.Reduce([]Point{paid, tier(day)})
	second := rd.Reduce([]Point{paid, tier(day.AddDate(0, 0, 1))})

	var money, tiers *Point
	for i := range second {
		switch second[i].Measurement {
		case "gh_sponsorships":
			money = &second[i]
		case "gh_sponsors_tier":
			tiers = &second[i]
		}
	}
	if money == nil || tiers == nil {
		t.Fatalf("both must survive the reduction, got %+v (first batch %+v)", second, first)
	}
	if money.Fields["total"] != 1 {
		t.Errorf("the same sponsorship read twice is still one payment: total = %v", money.Fields["total"])
	}
	if _, ok := money.Tags["sponsorable"]; ok {
		t.Error("the other party is not in the keep list; a series per sponsor is what the count avoids")
	}
	if _, ok := money.Fields["sponsorship_mean"]; ok {
		t.Error("the marker field averages to a constant 1 and says nothing count does not")
	}
	if tiers.Measurement != "gh_sponsors_tier" || tiers.Fields["price_cents"] != 3000 {
		t.Errorf("a tier keeps its newest reading rather than being counted: %+v", tiers)
	}
	if _, ok := tiers.Fields["total"]; ok {
		t.Error("a running total of standing inventory grows by the whole catalog every day")
	}
}

func TestReducerPublishesARunningTotal(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	rd := NewReducer()
	pr := func(n int, at time.Time) Point {
		return Point{
			Measurement: "gh_pull_request",
			Tags:        map[string]string{"repo": "a", "state": "MERGED", "number": strconv.Itoa(n)},
			Fields:      map[string]any{"churn": 1}, Time: at,
		}
	}
	first := rd.Reduce([]Point{pr(1, base), pr(2, base.Add(time.Hour))})
	if first[0].Fields["total"] != 2 {
		t.Fatalf("total after the first batch = %v, want 2", first[0].Fields["total"])
	}
	// The next sweep sees one of the same items again and one new one. The
	// window count is two; the total is three, because an item seen twice is
	// still one item.
	second := rd.Reduce([]Point{pr(2, base.Add(time.Hour)), pr(3, base.Add(2*time.Hour))})
	if second[0].Fields["count"] != 2 || second[0].Fields["total"] != 3 {
		t.Errorf("count = %v total = %v, want 2 and 3", second[0].Fields["count"], second[0].Fields["total"])
	}
}

// TestReducerCountsTheEventsAFoldedRowStandsFor pins the weight: a
// gh_repo_activity row that folds the three branches one push moved in one
// second carries events 3, and the total counts three activities, not one
// row. Seen again with a different count, the same row corrects the total
// rather than adding to it; a row with no events field is still one item.
func TestReducerCountsTheEventsAFoldedRowStandsFor(t *testing.T) {
	at := time.Date(2026, 9, 12, 7, 23, 54, 0, time.UTC)
	rd := NewReducer()
	push := func(events int, at time.Time) Point {
		return Point{
			Measurement: "gh_repo_activity",
			Tags:        map[string]string{"repo": "a", "activity": "force_push", "actor": "octocat"},
			Fields:      map[string]any{"events": events, "ref_name": "x"}, Time: at,
		}
	}
	first := rd.Reduce([]Point{push(3, at), push(1, at.Add(time.Hour))})
	if first[0].Fields["count"] != 2 || first[0].Fields["total"] != 4 {
		t.Errorf("count = %v total = %v, want 2 rows standing for 4 pushes", first[0].Fields["count"], first[0].Fields["total"])
	}
	second := rd.Reduce([]Point{push(5, at)})
	if second[0].Fields["total"] != 6 {
		t.Errorf("total = %v after the same row came back counting 5, want 6", second[0].Fields["total"])
	}
	star := Point{
		Measurement: "gh_star", Tags: map[string]string{"repo": "a", "user": "u"},
		Fields: map[string]any{"stars": 1}, Time: at,
	}
	if got := rd.Reduce([]Point{star}); got[0].Fields["total"] != 1 {
		t.Errorf("a row without events is one item, total = %v", got[0].Fields["total"])
	}
}

// measurementsInCollectors reads every measurement name the collectors write
// out of their own source.
//
// The alternative list, the tags table in cmd/internal/dashboards, is a copy
// and not a source: it is written by hand after the collector is, and it does
// not even claim to name every measurement (gh_event is excluded on purpose,
// its own comment says so, and gh_job_log is simply not in it), so a rule
// missing for either would sail past a test built on it. Go's internal rule
// settles it anyway: cmd/internal/... is importable only from cmd/..., so this
// package cannot read that table at all. internal/collect is the thing that
// decides what exists, so it is the thing to ask.
func measurementsInCollectors(t *testing.T) map[string]string {
	t.Helper()
	const dir = "../collect"
	found := map[string]string{}
	fset := token.NewFileSet()
	// Every .go file under the tree, not just the top level: a collector moved
	// into a sub-package would otherwise go unread, and an unread collector is
	// the hole this test exists to close rather than a file it may skip.
	walkErr := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		recordMeasurements(t, fset, file, name, found)
		return nil
	})
	if walkErr != nil {
		// Skipping here would restore exactly the hole this test closes.
		t.Fatalf("cannot read the collectors: %v", walkErr)
	}
	if len(found) == 0 {
		t.Fatal("no measurement found in internal/collect, so this test proves nothing")
	}
	return found
}

// recordMeasurements adds every measurement name one collector file writes to
// found, against the file's name.
func recordMeasurements(t *testing.T, fset *token.FileSet, file *ast.File, name string, found map[string]string) {
	t.Helper()
	ast.Inspect(file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok || !isMeasurementKey(kv) {
			return true
		}
		lit, ok := kv.Value.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			// A name assembled at run time would make this test blind
			// without failing, which is the failure mode it exists to
			// prevent. Every one is a literal today.
			t.Errorf("%s:%d writes a measurement whose name is not a literal, "+
				"so no rule can be checked against it", name, fset.Position(kv.Pos()).Line)
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Errorf("%s: cannot read measurement name %s: %v", name, lit.Value, err)
			return true
		}
		found[value] = name
		return true
	})
}

// isMeasurementKey reports whether a key-value pair sets a Measurement field.
func isMeasurementKey(kv *ast.KeyValueExpr) bool {
	key, ok := kv.Key.(*ast.Ident)
	return ok && key.Name == "Measurement"
}

func TestEveryMeasurementHasAReductionRule(t *testing.T) {
	written := measurementsInCollectors(t)
	for _, m := range sortedKeys2(written) {
		if _, ok := promRules[m]; !ok {
			t.Errorf("%s is written by internal/collect/%s and has no rule in promRules, "+
				"so Reduce drops it: it reaches InfluxDB and never reaches Prometheus or OTLP. "+
				"Give it a rule, mode skip included. Skip is a decision, absence is an accident.",
				m, written[m])
		}
	}
}

func TestNoReductionRuleOutlivesItsCollector(t *testing.T) {
	written := measurementsInCollectors(t)
	ruled := make([]string, 0, len(promRules))
	for m := range promRules {
		ruled = append(ruled, m)
	}
	sort.Strings(ruled)
	for _, m := range ruled {
		if _, ok := written[m]; !ok {
			t.Errorf("promRules reduces %s, which no collector writes any more", m)
		}
	}
}

// TestIdentifiersAreNeverAveraged pins the reduction that produced the two
// worst gauges this exporter has published.
//
// Each of these fields joins one row to another: a deployment to the workflow
// run that performed it, a job to its run, a commit to the pull request that
// carried it. Averaged over a count they become a number shaped exactly like
// the identifier they are made of, which is the failure that hides: 3.61K
// looks like a run id, so nothing about the tile says it belongs to no run.
func TestIdentifiersAreNeverAveraged(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		measurement string
		reduced     string
		tags        map[string]string
		identifier  string
		values      [2]any
		quantity    string
	}{
		{
			"gh_deployment", "gh_deployments",
			map[string]string{"repo": "a", "environment": "prod", "state": "success"},
			"run_id",
			[2]any{int64(17_600_000_001), int64(17_600_000_003)},
			"seconds_to_status",
		},
		{
			"gh_workflow_run", "gh_workflow_runs",
			map[string]string{"repo": "a", "workflow": ".github/workflows/ci.yml", "conclusion": "success"},
			"run_id",
			[2]any{int64(9_001), int64(9_003)},
			"duration_seconds",
		},
		{
			"gh_workflow_run", "gh_workflow_runs",
			map[string]string{"repo": "a", "workflow": ".github/workflows/ci.yml", "conclusion": "success"},
			"workflow_id",
			[2]any{int64(41), int64(43)},
			"duration_seconds",
		},
		{
			"gh_workflow_run", "gh_workflow_runs",
			map[string]string{"repo": "a", "workflow": ".github/workflows/ci.yml", "conclusion": "success"},
			"run_number",
			[2]any{1481, 1483},
			"duration_seconds",
		},
		{
			"gh_workflow_job", "gh_workflow_jobs",
			map[string]string{"repo": "a", "job_name": "build", "conclusion": "success"},
			"run_id",
			[2]any{int64(9_001), int64(9_003)},
			"duration_seconds",
		},
		{
			"gh_repo_activity", "gh_repo_activities",
			map[string]string{"repo": "a", "activity": "push"},
			"id",
			[2]any{int64(700_001), int64(700_003)},
			"",
		},
		{
			"gh_issue_event", "gh_issue_events",
			map[string]string{"repo": "a", "event": "closed", "kind": "issue", "bot": "false"},
			"number",
			[2]any{11, 13},
			"",
		},
		{
			"gh_issue", "gh_issues",
			map[string]string{"repo": "a", "state": "CLOSED"},
			"pull_request",
			[2]any{494, 496},
			"seconds_to_close",
		},
		{
			"gh_issue", "gh_issues",
			map[string]string{"repo": "a", "state": "OPEN"},
			"parent_issue",
			[2]any{365, 367},
			"seconds_open",
		},
		{
			"gh_commit", "gh_commits",
			map[string]string{"repo": "a", "author": "octocat", "signature": "VALID"},
			"pull_request",
			[2]any{494, 496},
			"additions",
		},
		{
			"gh_pull_request", "gh_pull_requests",
			map[string]string{"repo": "a", "state": "MERGED"},
			"stack",
			[2]any{693, 695},
			"churn",
		},
	}

	for _, c := range cases {
		t.Run(c.measurement+"."+c.identifier, func(t *testing.T) {
			point := func(id, quantity any, at time.Time) Point {
				fields := map[string]any{c.identifier: id}
				if c.quantity != "" {
					fields[c.quantity] = quantity
				}
				return Point{Measurement: c.measurement, Tags: c.tags, Fields: fields, Time: at}
			}
			out := Summarize([]Point{
				point(c.values[0], 10, at),
				point(c.values[1], 30, at.Add(time.Hour)),
			})
			if len(out) != 1 {
				t.Fatalf("two items of one series reduce to one point, got %d", len(out))
			}
			if got, published := out[0].Fields[c.identifier+"_mean"]; published {
				t.Errorf("%s reduces to %s_mean = %v, which is shaped like an identifier and names nothing",
					c.measurement, c.identifier, got)
			}
			if out[0].Fields["count"] != 2 {
				t.Errorf("the items are still counted: count = %v, want 2", out[0].Fields["count"])
			}
			// A measurement whose only other number is its marker has nothing
			// left to average, and the count above is the whole answer.
			if c.quantity == "" {
				return
			}
			if out[0].Fields[c.quantity+"_mean"] != 20.0 {
				t.Errorf("the quantity beside it still averages: %s_mean = %v, want 20",
					c.quantity, out[0].Fields[c.quantity+"_mean"])
			}
		})
	}
}

// TestEveryIdentifierFieldIsNamed reads the collectors and fails when one
// writes a field whose name says it is an identifier and notAveraged does not
// carry it.
//
// The shape is the mechanical half of the rule: `id` and anything ending in
// `_id` is an identity by name, wherever it is written and whatever its
// measurement reduces to today. A measurement that is keepLast now can become
// count later, and the mean would arrive with it silently.
func TestEveryIdentifierFieldIsNamed(t *testing.T) {
	for _, f := range fieldNamesInCollectors(t) {
		if f.name != "id" && !strings.HasSuffix(f.name, "_id") {
			continue
		}
		if !notAveraged[f.name] {
			t.Errorf("internal/collect/%s writes the field %q, which names an identity, "+
				"and notAveraged does not carry it: a measurement counted rather than kept "+
				"publishes the arithmetic mean of it", f.where, f.name)
		}
	}
}

// collectorField is one field name a collector writes, and the file it is in.
type collectorField struct{ name, where string }

// fieldNamesInCollectors reads every field name the collectors write, in the
// two shapes they write them: the keys of a map[string]any literal, which is
// what a Point's Fields is built from, and an index assignment into one for
// the values that are only written sometimes.
//
// The tag maps are map[string]string and are left out by their own type, which
// is what keeps a tag called `number` from being read as a field of the same
// name.
func fieldNamesInCollectors(t *testing.T) []collectorField {
	t.Helper()
	const dir = "../collect"
	var found []collectorField
	seen := map[string]bool{}
	fset := token.NewFileSet()

	walkErr := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, f := range fieldNamesInFile(file, name) {
			if key := f.name + "|" + f.where; !seen[key] {
				seen[key] = true
				found = append(found, f)
			}
		}
		return nil
	})
	if walkErr != nil {
		// Skipping here would restore exactly the hole this test closes.
		t.Fatalf("cannot read the collectors: %v", walkErr)
	}
	if len(found) == 0 {
		t.Fatal("no field found in internal/collect, so this test proves nothing")
	}
	return found
}

// fieldNamesInFile reads one collector's field names, in the order it writes
// them.
func fieldNamesInFile(file *ast.File, where string) []collectorField {
	var out []collectorField
	add := func(lit *ast.BasicLit) {
		if lit == nil || lit.Kind != token.STRING {
			return
		}
		if name, err := strconv.Unquote(lit.Value); err == nil {
			out = append(out, collectorField{name, where})
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CompositeLit:
			if isAnyValuedMap(node.Type) {
				for _, elt := range node.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						key, _ := kv.Key.(*ast.BasicLit)
						add(key)
					}
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range node.Lhs {
				add(fieldMapKey(lhs))
			}
		}
		return true
	})
	return out
}

// fieldMapKey reads the key out of an assignment into a field map, which is
// the shape the collectors use for a value they write only sometimes.
//
// The map is named rather than typed at this point, so the name is what tells
// a field map from a tag map. Both spellings the collectors use are here; a
// third would go unread, which is why the map literals are the main reading
// and this is the supplement.
func fieldMapKey(lhs ast.Expr) *ast.BasicLit {
	index, ok := lhs.(*ast.IndexExpr)
	if !ok {
		return nil
	}
	target, ok := index.X.(*ast.Ident)
	if !ok || (target.Name != "fields" && target.Name != "f") {
		return nil
	}
	key, _ := index.Index.(*ast.BasicLit)
	return key
}

// isAnyValuedMap reports whether a composite literal is a map[string]any,
// which is the type every Fields map in the collectors has.
func isAnyValuedMap(expr ast.Expr) bool {
	m, ok := expr.(*ast.MapType)
	if !ok {
		return false
	}
	key, ok := m.Key.(*ast.Ident)
	if !ok || key.Name != "string" {
		return false
	}
	value, ok := m.Value.(*ast.Ident)
	return ok && value.Name == "any"
}

// TestPromotedFieldsBecomeLabels pins the one place a moving state is still a
// label.
//
// An alert's state, an issue's resolution and whether a discussion has its
// answer are fields in every store that keys a row by its tags and its time,
// because as tags they doubled the row the moment they changed. The exporter
// stamps every gauge at the sweep, so it reads them back as labels: without
// that, "alerts by state" and "time to resolve" could not be asked of
// Prometheus at all, and the mean time to resolve would be diluted by every
// alert still open.
func TestPromotedFieldsBecomeLabels(t *testing.T) {
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	alert := func(number, state string, resolve any) Point {
		fields := map[string]any{"alerts": 1, "alert_state": state}
		if resolve != nil {
			fields["seconds_to_resolve"] = resolve
		}
		return Point{
			Measurement: "gh_dependabot_alert_item",
			Tags:        map[string]string{"repo": "a", "severity": "high", "number": number},
			Fields:      fields, Time: at,
		}
	}
	out := Summarize([]Point{
		alert("1", "open", nil),
		alert("2", "fixed", 100),
		alert("3", "fixed", 300),
		{
			Measurement: "gh_discussion",
			Tags:        map[string]string{"repo": "a", "category": "Q&A", "number": "7"},
			Fields:      map[string]any{"comments": 2, "has_answer": true}, Time: at,
		},
		{
			Measurement: "gh_deployment",
			Tags:        map[string]string{"repo": "a", "environment": "production", "deployment": "9"},
			Fields:      map[string]any{"deployments": 1, "outcome": "pending", "success": false}, Time: at,
		},
	})
	byState := map[string]Point{}
	var discussion, deployment Point
	for _, p := range out {
		switch p.Measurement {
		case "gh_dependabot_alerts":
			byState[p.Tags["alert_state"]] = p
		case "gh_discussions":
			discussion = p
		case "gh_deployments":
			deployment = p
		}
	}
	// The dashboard's pending column selects on it, and a deployment is
	// pending before it is anything else.
	if deployment.Tags["outcome"] != "pending" {
		t.Errorf("a deployment's outcome is read as a label, got %v", deployment.Tags)
	}
	if len(byState) != 2 {
		t.Fatalf("one open and two fixed alerts reduce to two series by state, got %v", byState)
	}
	if byState["fixed"].Fields["count"] != 2 || byState["open"].Fields["count"] != 1 {
		t.Errorf("counts by state: fixed = %v, open = %v", byState["fixed"].Fields["count"], byState["open"].Fields["count"])
	}
	if byState["fixed"].Fields["seconds_to_resolve_mean"] != 200.0 {
		t.Errorf("the mean time to resolve is over the resolved alerts only: got %v, want 200",
			byState["fixed"].Fields["seconds_to_resolve_mean"])
	}
	if _, published := byState["open"].Fields["alert_state_mean"]; published {
		t.Error("a string field has no mean to publish")
	}
	if discussion.Tags["has_answer"] != "true" {
		t.Errorf("a boolean field is read as the label true or false, got %q", discussion.Tags["has_answer"])
	}
	if discussion.Fields["has_answer_mean"] != 1.0 {
		t.Errorf("the boolean still averages into the share answered: got %v", discussion.Fields["has_answer_mean"])
	}
}

// TestSummarizeKeepsTheNewestSnapshotWhateverTheOrder keeps the newest reading
// when it comes first in the batch, and keeps a reading that carries no time
// rather than losing the series.
func TestSummarizeKeepsTheNewestSnapshotWhateverTheOrder(t *testing.T) {
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	tags := map[string]string{"repo": "a"}
	out := Summarize([]Point{
		{Measurement: "gh_repo", Tags: tags, Fields: map[string]any{"stars": 9}, Time: base.Add(time.Hour)},
		{Measurement: "gh_repo", Tags: tags, Fields: map[string]any{"stars": 5}, Time: base},
	})
	if len(out) != 1 || out[0].Fields["stars"] != 9 {
		t.Errorf("an older reading after the newest replaced it: %+v", out)
	}
	out = Summarize([]Point{{Measurement: "gh_repo", Tags: tags, Fields: map[string]any{"stars": 4}}})
	if len(out) != 1 || out[0].Fields["stars"] != 4 {
		t.Errorf("a reading with no time was lost: %+v", out)
	}
}

// TestKeptTagsLeavesOutAnEmptyValue treats an empty tag the way the line
// protocol does, as no tag, so it does not open a series of its own.
func TestKeptTagsLeavesOutAnEmptyValue(t *testing.T) {
	got := keptTags(map[string]string{"repo": "a", "language": "", "owner": "o"}, []string{"repo", "language", "license"})
	if len(got) != 1 || got["repo"] != "a" {
		t.Errorf("keptTags = %v, want repo alone", got)
	}
}

// TestPromoteWritesEachKindOfFieldAsItsText writes a number in full decimal,
// never rounded to one digit, and adds no label for an empty string, a missing
// field or a value that is not a number.
func TestPromoteWritesEachKindOfFieldAsItsText(t *testing.T) {
	tags := map[string]string{}
	promote(tags, map[string]any{
		"parent": 12345, "share": 0.25, "answered": false, "state": "open",
		"empty": "", "when": time.Unix(1, 0),
	}, []string{"parent", "share", "answered", "state", "empty", "when", "absent"})
	want := map[string]string{"parent": "12345", "share": "0.25", "answered": "false", "state": "open"}
	if !maps.Equal(tags, want) {
		t.Errorf("labels = %v, want %v", tags, want)
	}
}

// TestReducerCountsARowWithoutAPositiveEventCountAsOne treats events of zero
// or less as the collector saying nothing, which is one item, since a weight
// of zero would make a counted item vanish from the total.
func TestReducerCountsARowWithoutAPositiveEventCountAsOne(t *testing.T) {
	at := time.Date(2026, 9, 12, 7, 23, 54, 0, time.UTC)
	for _, events := range []any{0, -2} {
		out := NewReducer().Reduce([]Point{{
			Measurement: "gh_repo_activity",
			Tags:        map[string]string{"repo": "a", "activity": "push"},
			Fields:      map[string]any{"events": events}, Time: at,
		}})
		if len(out) != 1 || out[0].Fields["total"] != 1 {
			t.Errorf("events %v: %+v, want a total of one", events, out)
		}
	}
}

// TestSummarizeLeavesOutAWindowWithNothingToAdd publishes no gauge for a summed
// measurement whose points carry no number.
func TestSummarizeLeavesOutAWindowWithNothingToAdd(t *testing.T) {
	out := Summarize([]Point{{
		Measurement: "gh_traffic", Tags: map[string]string{"repo": "a", "kind": "views"},
		Fields: map[string]any{"note": "text"}, Time: time.Unix(1, 0),
	}})
	if len(out) != 0 {
		t.Errorf("a window with no number became %+v", out)
	}
}
