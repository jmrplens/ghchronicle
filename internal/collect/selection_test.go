package collect

import (
	"strings"
	"testing"
)

// TestQueriesSelectNothingNobodyDecodes pins the selections that were removed
// on 2026-09-11 because no struct read them, or a struct read them and no
// point carried them. GraphQL travels uncompressed, so a field nobody decodes
// is bytes on every sweep for nothing: measured, the label colors were 4.5
// KB of the 47.6 KB planning answer and the CODEOWNERS text 3.1 KB of the
// 29.4 KB policy files answer. The list is what keeps them from coming back
// with the next copy and paste of a query from the explorer.
func TestQueriesSelectNothingNobodyDecodes(t *testing.T) {
	t.Parallel()
	dropped := []struct{ query, name, selection string }{
		{planningQuery, "planningQuery", " color "},
		{discussionCommentsQuery, "discussionCommentsQuery", "viewer {\n    login"},
		{discussionCommentsQuery, "discussionCommentsQuery", "number title url"},
		{issueCommentsQuery, "issueCommentsQuery", "issue { number url"},
		{discussionsQuery, "discussionsQuery", "emoji"},
		{historyQuery, "historyQuery", "createdAt\n"},
	}
	for _, d := range dropped {
		if strings.Contains(d.query, d.selection) {
			t.Errorf("%s still selects %q, which nothing decodes into a point", d.name, d.selection)
		}
	}
	// The blob text is parsed for dependabot.yml alone; CODEOWNERS was asked
	// for and thrown away.
	for line := range strings.SplitSeq(policyFragment(), "\n") {
		if !strings.Contains(line, "CODEOWNERS") {
			continue
		}
		if strings.Contains(line, " text") {
			t.Errorf("the CODEOWNERS blob is still asked for its text: %s", strings.TrimSpace(line))
		}
	}
	for line := range strings.SplitSeq(policyFragment(), "\n") {
		if strings.Contains(line, "object(expression: \"HEAD:.github/dependabot.yml\")") && !strings.Contains(line, " text") {
			t.Errorf("the dependabot.yml blob lost the text it is parsed from: %s", strings.TrimSpace(line))
		}
	}
}
