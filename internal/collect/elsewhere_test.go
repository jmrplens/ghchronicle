package collect

import (
	"slices"
	"strings"
	"testing"
)

// TestCollectedLeavesOutEveryRepositoryTheTargetsCollect: the account and each
// organization once, whatever the case, and a named repository only when no
// owner already left out holds it, since a fork of the account's is under
// -user: already and GitHub counts a negation like any other.
func TestCollectedLeavesOutEveryRepositoryTheTargetsCollect(t *testing.T) {
	t.Parallel()
	got := Collected{
		Orgs: []string{"acme", "ACME", "Octocat", ""},
		Repos: []string{
			"octocat/a-fork", "acme/tooling", "conda-forge/hello-feedstock",
			"Conda-Forge/hello-feedstock", "bare",
		},
	}.notIn("octocat")
	want := "-user:octocat -user:acme -repo:conda-forge/hello-feedstock"
	if got != want {
		t.Errorf("notIn = %q, want %q", got, want)
	}
	if alone := (Collected{}).notIn("octocat"); alone != "-user:octocat" {
		t.Errorf("with nothing beside the account, notIn = %q, want the account alone", alone)
	}
}

// TestEverySearchForWorkElsewhereLeavesOutWhatIsCollected: the items and the
// lifetime counts both, so a pull request in a collected organization is the
// repository families' row and nobody's work elsewhere.
func TestEverySearchForWorkElsewhereLeavesOutWhatIsCollected(t *testing.T) {
	t.Parallel()
	collected := Collected{Orgs: []string{"acme"}, Repos: []string{"conda-forge/hello-feedstock"}}
	wantIn := func(what, q string) {
		t.Helper()
		words := strings.Fields(q)
		for _, w := range []string{"-user:octocat", "-user:acme", "-repo:conda-forge/hello-feedstock"} {
			if !slices.Contains(words, w) {
				t.Errorf("%s searches %q, without %s", what, q, w)
			}
		}
	}
	for _, sc := range (Totals{Login: "octocat", Collected: collected}).searchCounts() {
		if strings.HasSuffix(sc.field, "_elsewhere") {
			wantIn(sc.field, sc.query)
		}
	}
	var searches []string
	var starred []map[string]any
	f := outboundFixture(t, &searches, &starred)
	if _, err := (Outbound{Login: "octocat", Collected: collected}).Collect(ctx(t), f.Client, testNow); err != nil {
		t.Fatal(err)
	}
	if len(searches) == 0 {
		t.Fatal("Outbound searched nothing")
	}
	for _, q := range searches {
		wantIn("Outbound", q)
	}
}
