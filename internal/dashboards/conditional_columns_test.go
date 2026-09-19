package dashboards

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The class the alert panel belonged to, checked rather than listed by hand.
//
// A column of InfluxDB, PostgreSQL or Elasticsearch exists once a point has
// carried it. A collector that writes a field only when something has happened
// therefore leaves the column uncreated on an account it has not happened to,
// and a panel naming it is refused whole: "Time to resolve an alert" drew "No
// data" over eight resolved alerts for a release because it asked for
// `dismissed_reason` on an account that had only ever fixed alerts. That one
// had an unconditional twin, `alert_state`, and was fixed by naming it. Most
// have none: a pull request nobody else reviewed has no wait to write, and
// writing a zero would be inventing one.
//
// So they are listed, and the list is kept complete by this file rather than by
// hand: conditionalWrites parses every collector for a field written under a
// condition, namedByAPanel keeps the ones the two SQL dashboards read, and
// the test below fails when the two agree on something the list does not.
//
// What it cannot see, said plainly rather than implied: a field skipped inside
// a helper it does not recognize, a value that is written but never non-zero,
// and the three stores where a missing field answers empty instead of erroring
// (Elasticsearch, Prometheus and Graphite), where the defect is invisible at
// query time whatever any list says. The complete answer is a second
// end-to-end fixture, for an account that has done none of the optional
// things; the fixture we have carries every field of every measurement, which
// is exactly why it cannot show this.

// conditionalColumn is one such column, the measurement it belongs to, the
// collector that writes it and the condition under which it does.
type conditionalColumn struct{ column, measurement, writtenIn, when string }

// conditionalColumns is every one of them a panel names today. An entry is not
// a defect: it is a panel that will be refused on an account that has never
// done the thing in `when`, recorded so the next reader knows which panels
// those are. `.github/RELEASING.md` names the handful an ordinary account
// meets, and the live check finds them for whichever account it is pointed at.
var conditionalColumns = []conditionalColumn{
	{"advanced", "gh_fork", "community.go", "the fork has ever been pushed to"},
	{"age_days", "gh_sponsors_tier", "account.go", "GitHub dated the tier"},
	{"age_days", "gh_star_list", "account.go", "GitHub dated the list"},
	{"age_days_at_archive", "gh_repo_archived", "totals.go", "the archived repository has a creation date to subtract from"},
	{"amount_cents", "gh_sponsorship", "account.go", "the sponsorship names a tier"},
	{"author_association", "gh_pull_request", "pulls.go", "GitHub sent one"},
	{"commits", "gh_contribution_repo", "account.go", "the contribution kind is commits"},
	{"cvss", "gh_dependabot_alert_item", "security.go", "the advisory carries a v3 score, a zero not being a score the scale gives out"},
	{"cvss_v4", "gh_dependabot_alert_item", "security.go", "the advisory carries a v4 score"},
	{"days_since_add", "gh_star_list", "account.go", "something has been added to the list"},
	{"days_since_commit", "gh_branch", "branches.go", "the ref points at a commit rather than at a tag or a tree"},
	{"days_since_push", "gh_pinned_item", "account.go", "the pinned item is a repository that has been pushed to"},
	{"days_since_use", "gh_deploy_key", "settings.go", "the deploy key has ever been used"},
	{"days_since_use", "gh_key", "profile.go", "the SSH key has ever been used"},
	{"days_to_expiry", "gh_key", "profile.go", "the GPG key has an expiry"},
	{"environment_url", "gh_deployment", "deployments.go", "the deployment status carried one"},
	{"headline", "gh_workflow_run", "actions.go", "the run carries its head commit's message"},
	{"image", "gh_achievement_progress", "achievement_progress.go", "the badge card has an image and its tiers agree with the count"},
	{"label_names", "gh_issue", "pulls.go", "the issue carries at least one label"},
	{"label_names", "gh_pull_request", "pulls.go", "the pull request carries at least one label"},
	{"never_used", "gh_key", "profile.go", "the SSH key has never been used, the other side of days_since_use"},
	{"next_threshold", "gh_achievement_progress", "achievement_progress.go", "the badge's tiers agree with the count"},
	{"owner_commits", "gh_commits_week", "repoactivity.go", "GitHub returned the owner's own series for that week"},
	{"percent", "gh_achievement_progress", "achievement_progress.go", "the badge's tiers agree with the count"},
	{"queued_seconds", "gh_workflow_job", "actions.go", "the job carries both the moment it was created and the moment it started"},
	{"referrer_url", "gh_traffic_referrer", "traffic.go", "the referrer looks like a host rather than a place"},
	{"retired", "gh_sponsors_tier", "account.go", "GitHub sent the tier's admin block"},
	{"run_number", "gh_workflow_run", "actions.go", "the run has a number"},
	{"seconds_open", "gh_issue", "pulls.go", "the issue is still open"},
	{"seconds_to_close", "gh_issue", "pulls.go", "the issue has closed"},
	{"seconds_to_first_human_review", "gh_pull_request", "pulls.go", "somebody other than the author reviewed the pull request, which is fourteen rows in the whole of the store this was read against"},
	{"seconds_to_merge", "gh_pull_request", "pulls.go", "the pull request was merged, so a fresh install and anyone who pushes to main has none"},
	{"seconds_to_push", "gh_fork", "community.go", "the fork has ever been pushed to"},
	{"stars", "gh_pinned_item", "account.go", "the pinned item is a repository"},
	{"summary", "gh_dependabot_alert_item", "security.go", "the advisory has summary text"},
	{"tier", "gh_sponsorship", "account.go", "the sponsorship names a tier"},
	{"title", "gh_pull_request", "pulls.go", "GitHub sent one"},
	// The url entries are the ones a collector writes itself. Around fifty
	// measurements carry one through `withURL` (urls.go), a helper whose
	// callers are in other files, so the scan cannot say which measurement
	// those belong to and does not try: that is the helper case this file's
	// header names as its blind spot, and the rule it follows is written in
	// CLAUDE.md, a url is absolute or it is absent. What watches those is the
	// link machinery rather than this list: cmd/check_dashboards holds every
	// link column of a rendered dashboard to the rule that the column comes
	// back and holds nothing but absolute urls and empty cells.
	{"url", "gh_achievement_progress", "achievement_progress.go", "the badge has a page"},
	{"url", "gh_deployment", "deployments.go", "the repository has a deployments page to point at"},
	{"url", "gh_notification", "events.go", "the notification's subject has a page"},
	{"url", "gh_policy_file", "policyfiles.go", "the policy file is present"},
	{"url", "gh_repo_community", "repo.go", "GitHub answered the community profile"},
	{"url", "gh_sponsors_tier", "account.go", "the listing has a page"},
	{"url", "gh_sponsorship", "account.go", "the sponsorship is not private"},
	{"user_url", "gh_star", "stars.go", "the stargazer has a page"},
}

// TestEveryColumnAnAccountMightNotHaveIsOnTheList: the list above is the whole
// of the class as far as a parser can see it, and neither side may drift. A
// column that becomes conditional, or a new panel that names one, fails here
// with the entry to add; an entry whose collector stopped writing it under a
// condition, or whose panels stopped naming it, fails as a line that describes
// nothing.
func TestEveryColumnAnAccountMightNotHaveIsOnTheList(t *testing.T) {
	t.Parallel()
	named := namedByAPanel(t, conditionalWrites(t))
	listed := map[conditionalColumn]bool{}
	for _, c := range conditionalColumns {
		key := conditionalColumn{column: c.column, measurement: c.measurement, writtenIn: c.writtenIn}
		if listed[key] {
			t.Errorf("%s of %s is on the list twice", c.column, c.measurement)
		}
		listed[key] = true
		if c.when == "" {
			t.Errorf("%s of %s is listed without the condition it is written under",
				c.column, c.measurement)
		}
	}
	for key, panels := range named {
		if !listed[key] {
			t.Errorf("%s of %s is written only when a condition holds (internal/collect/%s) and "+
				"is named by %s, which that account would see refused whole. Add it to "+
				"conditionalColumns with the condition, and if it has an unconditional twin "+
				"the way alert_state was dismissed_reason's, name that in the panel instead",
				key.column, key.measurement, key.writtenIn, strings.Join(panels, ", "))
		}
	}
	for key := range listed {
		if _, ok := named[key]; !ok {
			t.Errorf("%s of %s (internal/collect/%s) is on the list and is no longer both "+
				"written under a condition and named by a panel, so the entry describes nothing",
				key.column, key.measurement, key.writtenIn)
		}
	}
}

// namedByAPanel keeps the writes some panel of the two SQL dashboards reads,
// from the measurement that write belongs to, and answers which panels those
// are. A statement naming the column and reading the measurement is the test:
// the column is what the planner refuses, and the measurement is what makes
// `commits` of gh_contribution_repo a different question from `commits` of
// gh_repo_total.
func namedByAPanel(t *testing.T, writes map[conditionalColumn]bool) map[conditionalColumn][]string {
	t.Helper()
	type statement struct {
		panel  string
		sql    string
		tables map[string]bool
	}
	from := regexp.MustCompile(`FROM\s+(gh_\w+)`)
	var statements []statement
	for _, store := range []string{"influxdb", "postgres"} {
		for title, p := range rendered(t, store) {
			targets, _ := p["targets"].([]any)
			for _, raw := range targets {
				target, _ := raw.(map[string]any)
				sql, _ := target["rawSql"].(string)
				if sql == "" {
					continue
				}
				tables := map[string]bool{}
				for _, m := range from.FindAllStringSubmatch(sql, -1) {
					tables[m[1]] = true
				}
				statements = append(statements, statement{title, sql, tables})
			}
		}
	}
	out := map[conditionalColumn][]string{}
	for w := range writes {
		word := regexp.MustCompile(`\b` + regexp.QuoteMeta(w.column) + `\b`)
		seen := map[string]bool{}
		for _, s := range statements {
			if s.tables[w.measurement] && word.MatchString(s.sql) {
				seen[s.panel] = true
			}
		}
		if len(seen) == 0 {
			continue
		}
		panels := make([]string, 0, len(seen))
		for p := range seen {
			panels = append(panels, p)
		}
		sort.Strings(panels)
		out[w] = panels
	}
	return out
}

// fieldMaps are the names a collector builds a point's fields and tags under.
// A collector that uses a fifth name is how a write escapes this scan, which is
// why they are listed here rather than inferred: `vars`, the GraphQL variables,
// is deliberately not one of them.
var fieldMaps = map[string]bool{"fields": true, "f": true, "listing": true, "tags": true}

// conditionalWrites answers every field a collector writes on some rows of a
// measurement and not on others: a write inside an `if` or a `case` that is not
// the branch building the point, and every `setNonEmpty`, whose whole purpose
// is to skip a value that is not there. A name written on every branch of one
// `if`, or written unguarded anywhere in the same function, is not one of them.
func conditionalWrites(t *testing.T) map[conditionalColumn]bool {
	t.Helper()
	dir := filepath.Join("..", "collect")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the collectors: %v", err)
	}
	out := map[conditionalColumn]bool{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parsing internal/collect/%s: %v", name, parseErr)
		}
		for _, w := range writesOf(file, name) {
			out[w] = true
		}
	}
	return out
}

// writesOf is one file's conditional writes, each already resolved to the
// measurement it belongs to.
func writesOf(file *ast.File, name string) []conditionalColumn {
	funcs := map[string]*ast.FuncDecl{}
	points := map[*ast.FuncDecl][]measurementAt{}
	callers := map[string][]*ast.FuncDecl{}
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		funcs[fn.Name.Name] = fn
		points[fn] = measurementsIn(fn)
		ast.Inspect(fn, func(x ast.Node) bool {
			call, isCall := x.(*ast.CallExpr)
			if !isCall {
				return true
			}
			if id, isID := call.Fun.(*ast.Ident); isID {
				callers[id.Name] = append(callers[id.Name], fn)
			}
			return true
		})
	}
	var out []conditionalColumn
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok {
			continue
		}
		guarded, always := fieldWrites(fn)
		for column, pos := range guarded {
			if always[column] {
				continue
			}
			out = append(out, conditionalColumn{
				column:      column,
				measurement: measurementOf(fn, pos, points, callers),
				writtenIn:   name,
			})
		}
	}
	return out
}

type measurementAt struct {
	pos  token.Pos
	name string
}

// measurementsIn is every sink.Point a function builds, in source order.
func measurementsIn(n ast.Node) []measurementAt {
	var out []measurementAt
	ast.Inspect(n, func(x ast.Node) bool {
		lit, isLit := x.(*ast.CompositeLit)
		if !isLit {
			return true
		}
		if sel, isSel := lit.Type.(*ast.SelectorExpr); !isSel || sel.Sel.Name != "Point" {
			return true
		}
		for _, el := range lit.Elts {
			kv, isPair := el.(*ast.KeyValueExpr)
			if !isPair {
				continue
			}
			if id, isID := kv.Key.(*ast.Ident); !isID || id.Name != "Measurement" {
				continue
			}
			if name, isStr := kv.Value.(*ast.BasicLit); isStr && name.Kind == token.STRING {
				s, _ := strconv.Unquote(name.Value)
				out = append(out, measurementAt{lit.Pos(), s})
			}
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i].pos < out[j].pos })
	return out
}

// measurementOf is the measurement a write belongs to: the next point the same
// function builds, or its last one, or, for a helper that builds none, the
// first point of a function that calls it.
func measurementOf(fn *ast.FuncDecl, pos token.Pos,
	points map[*ast.FuncDecl][]measurementAt, callers map[string][]*ast.FuncDecl,
) string {
	own := points[fn]
	for _, m := range own {
		if m.pos > pos {
			return m.name
		}
	}
	if len(own) > 0 {
		return own[len(own)-1].name
	}
	for _, caller := range callers[fn.Name.Name] {
		if m := points[caller]; len(m) > 0 {
			return m[0].name
		}
	}
	return ""
}

// fieldWrites walks one function and answers the fields it writes under a
// condition, with a position each, and the fields it writes whatever happens.
func fieldWrites(fn *ast.FuncDecl) (guarded map[string]token.Pos, always map[string]bool) {
	scan := &writeScan{always: map[string]bool{}}
	guarded = scan.walk(fn, false)
	for name := range scan.always {
		delete(guarded, name)
	}
	return guarded, scan.always
}

// writeScan carries the one thing the walk cannot answer branch by branch:
// the fields written whatever happens, which cancel a conditional write of the
// same name elsewhere in the function.
type writeScan struct{ always map[string]bool }

// walk answers the fields the subtree writes under a condition. `under` says
// whether the subtree is already inside a branch that is not the one building
// the point.
func (w *writeScan) walk(n ast.Node, under bool) map[string]token.Pos {
	switch v := n.(type) {
	case *ast.IfStmt:
		return w.branch(v, under)
	case *ast.CaseClause:
		inner := under || !buildsAPoint(v)
		here := map[string]token.Pos{}
		for _, s := range v.Body {
			merge(here, w.walk(s, inner))
		}
		return here
	case *ast.CallExpr:
		return w.skipped(v)
	case *ast.AssignStmt:
		return w.assigned(v, under)
	}
	here := map[string]token.Pos{}
	for _, c := range directChildren(n) {
		merge(here, w.walk(c, under))
	}
	return here
}

// branch is one `if`. The branch that builds the point is the request's own
// guard and not a condition on the row, so it does not make its fields
// conditional; a field both branches write is not conditional either, which is
// how an if/else pair writes one of two values.
func (w *writeScan) branch(v *ast.IfStmt, under bool) map[string]token.Pos {
	body := w.walk(v.Body, under || !buildsAPoint(v.Body))
	var other map[string]token.Pos
	if v.Else != nil {
		other = w.walk(v.Else, under || !buildsAPoint(v.Else))
	}
	here := map[string]token.Pos{}
	for name, pos := range body {
		if _, inBoth := other[name]; inBoth {
			w.always[name] = true
			continue
		}
		here[name] = pos
	}
	for name, pos := range other {
		if _, inBoth := body[name]; !inBoth {
			here[name] = pos
		}
	}
	return here
}

// skipped is a setNonEmpty call, whose whole purpose is to write nothing when
// the value is not there.
func (w *writeScan) skipped(v *ast.CallExpr) map[string]token.Pos {
	here := map[string]token.Pos{}
	fn, isID := v.Fun.(*ast.Ident)
	if !isID || fn.Name != "setNonEmpty" || len(v.Args) < 2 {
		return here
	}
	target, isTarget := v.Args[0].(*ast.Ident)
	if !isTarget || !fieldMaps[target.Name] {
		return here
	}
	if name, isStr := v.Args[1].(*ast.BasicLit); isStr && name.Kind == token.STRING {
		s, _ := strconv.Unquote(name.Value)
		here[s] = name.Pos()
	}
	return here
}

// assigned is a write to a field map by name, conditional or not.
func (w *writeScan) assigned(v *ast.AssignStmt, under bool) map[string]token.Pos {
	here := map[string]token.Pos{}
	for _, lhs := range v.Lhs {
		name, ok := fieldName(lhs)
		if !ok {
			continue
		}
		if under {
			here[name.value] = name.pos
		} else {
			w.always[name.value] = true
		}
	}
	return here
}

// namedField is a field map's key as it was written: the name and where.
type namedField struct {
	value string
	pos   token.Pos
}

// fieldName reads `fields["x"]`, and says no to anything else.
func fieldName(e ast.Expr) (namedField, bool) {
	index, isIndex := e.(*ast.IndexExpr)
	if !isIndex {
		return namedField{}, false
	}
	target, isTarget := index.X.(*ast.Ident)
	if !isTarget || !fieldMaps[target.Name] {
		return namedField{}, false
	}
	key, isStr := index.Index.(*ast.BasicLit)
	if !isStr || key.Kind != token.STRING {
		return namedField{}, false
	}
	s, _ := strconv.Unquote(key.Value)
	return namedField{s, key.Pos()}, true
}

// merge folds one subtree's answer into another's.
func merge(into, from map[string]token.Pos) {
	maps.Copy(into, from)
}

// buildsAPoint says whether a branch constructs the point itself, which is
// what tells the request's own error guard from a condition on one row.
func buildsAPoint(n ast.Node) bool { return len(measurementsIn(n)) > 0 }

// directChildren is the nodes one level down, so the walk above can carry its
// own state instead of ast.Inspect's.
func directChildren(n ast.Node) []ast.Node {
	var out []ast.Node
	ast.Inspect(n, func(x ast.Node) bool {
		if x == nil || x == n {
			return true
		}
		out = append(out, x)
		return false
	})
	return out
}
