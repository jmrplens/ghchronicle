package collect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func repoDetailServer(f *fixtureServer, seen *[]string) {
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		if seen != nil {
			*seen = append(*seen, query)
		}
		answerRepoDetail(w, query)
	})
}

// answerRepoDetail answers every alias the query names with the same
// repository, so a test that wraps the server can refuse some queries and
// still answer the rest the way repoDetailServer does.
func answerRepoDetail(w http.ResponseWriter, query string) {
	data := map[string]any{}
	for i := 0; strings.Contains(query, fmt.Sprintf("r%d:", i)); i++ {
		data[fmt.Sprintf("r%d", i)] = map[string]any{
			"nameWithOwner": "octocat/hello-world",
			"url":           "https://github.com/octocat/hello-world",
			"languages": map[string]any{"edges": []any{
				map[string]any{"size": 84523, "node": map[string]string{"name": "Go"}},
				map[string]any{"size": 1200, "node": map[string]string{"name": "Shell"}},
			}},
			"repositoryTopics": map[string]any{"nodes": []any{
				map[string]any{
					"url":   "https://github.com/topics/go",
					"topic": map[string]string{"name": "go"},
				},
			}},
			// Two classic protections, copied from the live answer on
			// 2026-09-10. The first guards `main` behind three required
			// contexts, the second is jmrplens/phonometry's
			// `pyoctaveband-v2`, the rule that returns a null review
			// count where the audit's transcript prints a zero.
			"branchProtectionRules": map[string]any{"nodes": []any{
				map[string]any{
					"pattern":                        "main",
					"allowsDeletions":                false,
					"allowsForcePushes":              false,
					"blocksCreations":                false,
					"dismissesStaleReviews":          true,
					"isAdminEnforced":                false,
					"requiresApprovingReviews":       true,
					"requiredApprovingReviewCount":   0,
					"requiresCodeOwnerReviews":       false,
					"requiresCommitSignatures":       false,
					"requiresConversationResolution": false,
					"requiresLinearHistory":          true,
					"requiresStatusChecks":           true,
					"requiresStrictStatusChecks":     true,
					"requiresDeployments":            false,
					"restrictsPushes":                false,
					"restrictsReviewDismissals":      false,
					"requiredStatusChecks": []any{
						map[string]string{"context": "Go analysis"},
						map[string]string{"context": "Tests"},
						map[string]string{"context": "govulncheck"},
					},
				},
				map[string]any{
					"pattern":                        "pyoctaveband-v2",
					"allowsDeletions":                false,
					"allowsForcePushes":              false,
					"blocksCreations":                false,
					"dismissesStaleReviews":          false,
					"isAdminEnforced":                false,
					"requiresApprovingReviews":       false,
					"requiredApprovingReviewCount":   nil,
					"requiresCodeOwnerReviews":       false,
					"requiresCommitSignatures":       false,
					"requiresConversationResolution": false,
					"requiresLinearHistory":          false,
					"requiresStatusChecks":           false,
					"requiresStrictStatusChecks":     true,
					"requiresDeployments":            false,
					"restrictsPushes":                false,
					"restrictsReviewDismissals":      false,
					"requiredStatusChecks":           []any{},
				},
			}},
			// Both rulesets carry what jmrplens/gitlab-mcp-server's do:
			// the branch one guards refs/heads/main with two actors that
			// may always bypass it, the tag one guards ~ALL with none.
			"rulesets": map[string]any{"nodes": []any{
				map[string]any{
					"name": "Protect main", "target": "BRANCH",
					"enforcement": "ACTIVE", "updatedAt": "2026-07-15T10:00:00Z",
					"rules": map[string]any{"nodes": []any{
						map[string]string{"type": "DELETION"},
						map[string]string{"type": "NON_FAST_FORWARD"},
						map[string]string{"type": "PULL_REQUEST"},
						map[string]string{"type": "REQUIRED_STATUS_CHECKS"},
					}},
					"conditions": map[string]any{"refName": map[string]any{
						"include": []string{"refs/heads/main"}, "exclude": []string{},
					}},
					"bypassActors": map[string]any{"totalCount": 2, "nodes": []any{
						map[string]string{"bypassMode": "ALWAYS"},
						map[string]string{"bypassMode": "PULL_REQUEST"},
					}},
				},
				map[string]any{
					"name": "Tag policy", "target": "TAG",
					"enforcement": "DISABLED", "updatedAt": "2026-08-01T10:00:00Z",
					"rules": map[string]any{"nodes": []any{
						map[string]string{"type": "DELETION"},
						map[string]string{"type": "UPDATE"},
						map[string]string{"type": "NON_FAST_FORWARD"},
					}},
					"conditions": map[string]any{"refName": map[string]any{
						"include": []string{"~ALL"}, "exclude": []string{},
					}},
					"bypassActors": map[string]any{"totalCount": 0, "nodes": []any{}},
				},
				// The two shapes this account does not have and the API
				// does: an exclusion, which turns "everything" into
				// "everything but the branch that matters", and more
				// bypass actors than the page of five that is read.
				map[string]any{
					"name": "Everything but main", "target": "BRANCH",
					"enforcement": "ACTIVE", "updatedAt": "2026-08-02T10:00:00Z",
					"rules": map[string]any{"nodes": []any{
						map[string]string{"type": "NON_FAST_FORWARD"},
					}},
					"conditions": map[string]any{"refName": map[string]any{
						"include": []string{"~ALL"},
						"exclude": []string{"refs/heads/main", "refs/heads/release/*"},
					}},
					"bypassActors": map[string]any{"totalCount": 7, "nodes": []any{
						map[string]string{"bypassMode": "ALWAYS"},
						map[string]string{"bypassMode": "ALWAYS"},
						map[string]string{"bypassMode": "ALWAYS"},
						map[string]string{"bypassMode": "PULL_REQUEST"},
						map[string]string{"bypassMode": "PULL_REQUEST"},
					}},
				},
			}},
		}
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"data": data}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func TestRepoDetailAsksForEveryRepositoryAtOnce(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	repoDetailServer(f, &queries)

	repos := make([]Repo, 7)
	for i := range repos {
		repos[i] = Repo{Owner: "octocat", Name: fmt.Sprintf("r%d", i), FullName: fmt.Sprintf("octocat/r%d", i)}
	}
	points, err := RepoDetail{Repos: repos, Batch: 3}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)
	wantMeasurements(t, points, "gh_repo_language", "gh_repo_topic", "gh_ruleset",
		"gh_branch_protection", "gh_ruleset_rule")

	// Three queries for seven repositories, where the REST version this
	// replaces made three calls each: twenty one.
	if len(queries) != 3 {
		t.Errorf("%d queries for 7 repositories in batches of 3", len(queries))
	}
	if got := len(only(t, points, "gh_repo_language")); got != 14 {
		t.Errorf("got %d language points, want two per repository", got)
	}

	lang := find(t, points, "gh_repo_language", map[string]string{"repo": "r0", "language": "Go"})
	if fieldInt(t, lang, "bytes") != 84523 || !lang.Time.Equal(testNow) {
		t.Errorf("language = %v at %s", lang.Fields, lang.Time)
	}
	topic := find(t, points, "gh_repo_topic", map[string]string{"repo": "r0", "topic": "go"})
	if topic.Fields["url"] != "https://github.com/topics/go" {
		t.Errorf("a topic carries the page a reader goes to next: %v", topic.Fields)
	}
	// GraphQL shouts its enumerations; the stored tags stay as they were when
	// this came from REST, so a dashboard written against the old rows works.
	active := find(t, points, "gh_ruleset", map[string]string{"ruleset": "Protect main"})
	if active.Tags["target"] != "branch" || active.Tags["enforcement"] != "active" || active.Fields["active"] != true {
		t.Errorf("ruleset = %v %v", active.Tags, active.Fields)
	}
	if !active.Time.Equal(testNow.UTC().Truncate(24 * time.Hour)) {
		t.Errorf("a ruleset is a snapshot stamped at the start of the day, got %s", active.Time)
	}
	disabled := find(t, points, "gh_ruleset", map[string]string{"ruleset": "Tag policy"})
	if disabled.Fields["active"] != false {
		t.Errorf("disabled ruleset = %v", disabled.Fields)
	}
}

// TestRepoDetailKeepsTheBatchWhenOneRepositoryIsGone is the same refusal
// TestTotalsKeepTheBatchWhenOneRepositoryIsGone describes, against the other
// batched read of a repository: one that is gone must not cost the others
// their languages, topics and protections.
func TestRepoDetailKeepsTheBatchWhenOneRepositoryIsGone(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	f.graphQL(func(w http.ResponseWriter, _ *http.Request, query string, _ map[string]any) {
		queries = append(queries, query)
		if strings.Contains(query, `name: "gone"`) {
			_, _ = w.Write([]byte(`{"data":{"r0":null},"errors":[{"type":"FORBIDDEN",` +
				`"path":["r0"],"message":"Resource not accessible by personal access token"}]}`))
			return
		}
		answerRepoDetail(w, query)
	})

	repos := []Repo{
		{Owner: "octocat", Name: "r0", FullName: "octocat/r0"},
		{Owner: "octocat", Name: "gone", FullName: "octocat/gone"},
		{Owner: "octocat", Name: "r2", FullName: "octocat/r2"},
	}
	points, err := RepoDetail{Repos: repos}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatalf("one repository that is gone is not a failure: %v", err)
	}
	if len(queries) != 4 {
		t.Fatalf("%d queries, want the refused batch asked for one at a time", len(queries))
	}
	if got := len(only(t, points, "gh_repo_language")); got != 4 {
		t.Errorf("got %d language points, want two for each repository that answered", got)
	}
	find(t, points, "gh_repo_language", map[string]string{"repo": "r0", "language": "Go"})
	find(t, points, "gh_repo_language", map[string]string{"repo": "r2", "language": "Go"})
}

func TestBranchProtectionRecordsWhatItEnforces(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	var queries []string
	repoDetailServer(f, &queries)

	points, err := RepoDetail{Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	for _, want := range []string{"branchProtectionRules(first: 10)", "bypassActors(first: 5)"} {
		if !strings.Contains(queries[0], want) {
			t.Fatalf("the batch must ask for %s", want)
		}
	}

	main := find(t, points, "gh_branch_protection", map[string]string{
		"full_name": "octocat/hello-world", "pattern": "main",
	})
	for field, want := range map[string]bool{
		"allows_deletions": false, "allows_force_pushes": false,
		"blocks_creations": false, "dismisses_stale_reviews": true,
		"admin_enforced": false, "requires_approving_reviews": true,
		"requires_code_owner_reviews": false, "requires_commit_signatures": false,
		"requires_conversation_resolution": false, "requires_linear_history": true,
		"requires_status_checks": true, "requires_strict_status_checks": true,
		"requires_deployments": false, "restricts_pushes": false,
		"restricts_review_dismissals": false,
	} {
		if main.Fields[field] != want {
			t.Errorf("%s = %v, want %v", field, main.Fields[field], want)
		}
	}
	// The three contexts the rule waits for, which the count in
	// gh_repo_policy.branch_protection_rules cannot express.
	if got := fieldInt(t, main, "required_checks"); got != 3 {
		t.Errorf("required_checks = %d, want 3", got)
	}
	if got := fieldInt(t, main, "rules"); got != 1 {
		t.Errorf("rules = %d, want 1", got)
	}
	// A snapshot, anchored to the start of the UTC day like gh_ruleset, so two
	// sweeps in one day converge on one row instead of two.
	if !main.Time.Equal(startOfDay(testNow)) {
		t.Errorf("branch protection stamped %s, want the start of the day", main.Time)
	}
}

// A null review count is not a zero one. GitHub answers null when
// requiresApprovingReviews is false, and zero when reviews are required but
// nobody has to approve: four repositories on this account are in the second
// state, so writing a zero for the first would merge two real and different
// answers into one.
func TestBranchProtectionOmitsANullReviewCount(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	repoDetailServer(f, nil)

	points, err := RepoDetail{Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	required := find(t, points, "gh_branch_protection", map[string]string{"pattern": "main"})
	if !hasField(required, "required_reviews") {
		t.Fatal("requiresApprovingReviews is true, so the count GitHub sent must be written")
	}
	if got := fieldInt(t, required, "required_reviews"); got != 0 {
		t.Errorf("required_reviews = %d, want 0: reviews required, none of them approving", got)
	}

	off := find(t, points, "gh_branch_protection", map[string]string{"pattern": "pyoctaveband-v2"})
	if hasField(off, "required_reviews") {
		t.Errorf("required_reviews = %v, want no field at all: GitHub sent null", off.Fields["required_reviews"])
	}
	if off.Fields["requires_approving_reviews"] != false {
		t.Errorf("requires_approving_reviews = %v, want false", off.Fields["requires_approving_reviews"])
	}
}

func TestRulesetRulesRecordWhatEachRulesetDoes(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	repoDetailServer(f, nil)

	points, err := RepoDetail{Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	// Four rules on the branch ruleset, three on the tag one and one on the
	// third.
	if got := len(only(t, points, "gh_ruleset_rule")); got != 8 {
		t.Errorf("got %d ruleset rule points, want 8", got)
	}

	pr := find(t, points, "gh_ruleset_rule", map[string]string{
		"full_name": "octocat/hello-world", "ruleset": "Protect main", "rule": "pull_request",
	})
	if got := fieldInt(t, pr, "bypass_actors"); got != 2 {
		t.Errorf("bypass_actors = %d, want 2", got)
	}
	// One of the two bypasses always, the other only on a pull request. A
	// count of actors alone would call both of them the same thing.
	if got := fieldInt(t, pr, "bypass_always"); got != 1 {
		t.Errorf("bypass_always = %d, want 1", got)
	}
	if pr.Fields["ref_include"] != "refs/heads/main" {
		t.Errorf("ref_include = %v, want the pattern itself", pr.Fields["ref_include"])
	}
	if pr.Fields["ref_exclude"] != "" {
		t.Errorf("ref_exclude = %v, want nothing excluded", pr.Fields["ref_exclude"])
	}
	// Both actors were read, so bypass_always can be compared with the total.
	if got := fieldInt(t, pr, "bypass_sampled"); got != 2 {
		t.Errorf("bypass_sampled = %d, want 2", got)
	}
	if !pr.Time.Equal(startOfDay(testNow)) {
		t.Errorf("ruleset rule stamped %s, want the start of the day", pr.Time)
	}

	// A tag ruleset over every ref, with nobody allowed past it.
	update := find(t, points, "gh_ruleset_rule", map[string]string{
		"ruleset": "Tag policy", "rule": "update",
	})
	if update.Fields["ref_include"] != "~ALL" {
		t.Errorf("ref_include = %v, want ~ALL", update.Fields["ref_include"])
	}
	if fieldInt(t, update, "bypass_actors") != 0 || fieldInt(t, update, "bypass_always") != 0 {
		t.Errorf("bypass = %v, want nobody", update.Fields)
	}

	// The rules are the ruleset's own, not a replacement for it: the row that
	// says a ruleset exists and when it last changed is still written.
	if got := len(only(t, points, "gh_ruleset")); got != 3 {
		t.Errorf("got %d ruleset points, want one per ruleset", got)
	}
}

// A ruleset over every ref with the default branch carved out of it is not a
// ruleset over every ref, and a bypass list longer than the page that is read
// cannot be compared with its own total. Both are invisible in a count.
func TestRulesetRulesKeepTheExclusionsAndSayWhatTheyRead(t *testing.T) {
	t.Parallel()
	f := newFixtureServer(t)
	repoDetailServer(f, nil)

	points, err := RepoDetail{Repos: []Repo{testRepo}}.Collect(ctx(t), f.Client, testNow)
	if err != nil {
		t.Fatal(err)
	}
	checkPoints(t, points)

	row := find(t, points, "gh_ruleset_rule", map[string]string{
		"ruleset": "Everything but main", "rule": "non_fast_forward",
	})
	if row.Fields["ref_include"] != "~ALL" {
		t.Errorf("ref_include = %v, want ~ALL", row.Fields["ref_include"])
	}
	if row.Fields["ref_exclude"] != "refs/heads/main refs/heads/release/*" {
		t.Errorf("ref_exclude = %v, want both carve-outs", row.Fields["ref_exclude"])
	}
	// Seven actors, five of them read: three that always bypass are three of
	// the five seen, not three of the seven that exist, and the row says so
	// rather than letting a panel subtract one from the other.
	if got := fieldInt(t, row, "bypass_actors"); got != 7 {
		t.Errorf("bypass_actors = %d, want the exact total 7", got)
	}
	if got := fieldInt(t, row, "bypass_sampled"); got != 5 {
		t.Errorf("bypass_sampled = %d, want the page of 5 that was read", got)
	}
	if got := fieldInt(t, row, "bypass_always"); got != 3 {
		t.Errorf("bypass_always = %d, want the 3 of those 5", got)
	}
}
