package fakegh

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// searchPage is the one outbound search page the fake serves, as the
// collector's query asks for it.
const searchPage = `query($query: String!, $after: String) {
  search(type: ISSUE, first: 100, after: $after, query: $query) { issueCount nodes { ... on Issue { number } } }
}`

// searchFor asks the fake one outbound search and answers the numbers of the
// items it served and the count it declared.
func searchFor(t *testing.T, s *Server, query string) (numbers []int, count int) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": searchPage, "variables": map[string]any{"query": query}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+"/graphql", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var answer struct {
		Data struct {
			Search struct {
				IssueCount int `json:"issueCount"`
				Nodes      []struct {
					Number int `json:"number"`
				} `json:"nodes"`
			} `json:"search"`
		} `json:"data"`
	}
	if err = json.NewDecoder(res.Body).Decode(&answer); err != nil {
		t.Fatalf("the answer to %q: %v", query, err)
	}
	for _, n := range answer.Data.Search.Nodes {
		numbers = append(numbers, n.Number)
	}
	slices.Sort(numbers)
	return numbers, answer.Data.Search.IssueCount
}

// TestAnOutboundSearchIsServedWhatItsQualifiersSelect holds the fake to
// GitHub's reading of the qualifiers the collector sends: a kind, a state, and
// is:unmerged taking in the pull requests still open, which is why the
// collector's search for the closed ones says is:closed as well. Each of the
// collector's five searches finds one item of the fixture, and no item is
// found by two of them.
func TestAnOutboundSearchIsServedWhatItsQualifiersSelect(t *testing.T) {
	t.Parallel()
	s := New(t, "../testdata")
	for query, want := range map[string][]int{
		"is:pr is:merged author:octocat -user:octocat sort:updated-desc":             {118},
		"is:pr is:open author:octocat -user:octocat sort:created-desc":               {131},
		"is:pr is:closed is:unmerged author:octocat -user:octocat sort:updated-desc": {124},
		"is:issue is:open author:octocat -user:octocat sort:created-desc":            {9},
		"is:issue is:closed author:octocat -user:octocat sort:updated-desc":          {7},
		"is:pr is:unmerged author:octocat":                                           {124, 131},
		"type:issue author:octocat":                                                  {7, 9},
		"author:octocat -user:octocat":                                               {7, 9, 118, 124, 131},
	} {
		got, count := searchFor(t, s, query)
		if !slices.Equal(got, want) || count != len(want) {
			t.Errorf("%q served %v counting %d, want %v counting %d", query, got, count, want, len(want))
		}
	}
}

// recorder is a test the fake reports to, so a test here can hold the fake to
// failing a test rather than answering.
type recorder struct {
	testing.TB
	failures []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// TestAnOutboundSearchTheFakeCannotReadFails: a qualifier that selects by
// kind or state and that the fake does not know would otherwise be read as
// no selection at all, which is how every search came to be answered with
// every item for as long as nobody looked.
func TestAnOutboundSearchTheFakeCannotReadFails(t *testing.T) {
	t.Parallel()
	r := &recorder{TB: t}
	s := New(r, "../testdata")
	searchFor(t, s, "is:pr is:draft author:octocat")
	if len(r.failures) != 1 {
		t.Errorf("a search for draft pull requests reported %v, want one failure naming is:draft", r.failures)
	}
}
