package fakegh

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TakenBack is the comment TakenBackOverlay serves as no longer accepted: the
// account's answer in fosrl/pangolin#118, which the base fixtures serve as
// the accepted one.
const TakenBack = "18283966"

// TakenBackOverlay is a fixture directory for New's overlays in which the
// maintainer of fosrl/pangolin#118 has taken back the answer they accepted:
// the account's comments list TakenBack as not the answer, on a thread no
// longer answered, and the walk of accepted answers no longer lists it.
//
// It is what a store written before 2.6.1 cannot show: there the comment was
// written as two rows, one per value of the is_answer tag, and a panel that
// reads one row per comment, accepted when any of its rows says so, goes on
// reading it accepted until the measurement is cleared and read again. A
// suite about bringing such a store along asks for it; every other suite
// asserts on the base account, where the answer stands.
func TakenBackOverlay(tb testing.TB, fixtures string) string {
	tb.Helper()
	dir := tb.TempDir()

	comments := readNumbers(tb, filepath.Join(fixtures, "graphql_discussion_comments.json"))
	found := false
	for _, node := range commentNodes(comments) {
		if commentID(node) == TakenBack {
			discussion, _ := node["discussion"].(map[string]any)
			if discussion == nil {
				tb.Fatalf("comment %s has no discussion", TakenBack)
			}
			node["isAnswer"], discussion["isAnswered"] = false, false
			found = true
		}
	}
	if !found {
		tb.Fatalf("graphql_discussion_comments.json does not hold comment %s", TakenBack)
	}
	writeJSON(tb, filepath.Join(dir, "graphql_discussion_comments.json"), comments)

	answers := readNumbers(tb, filepath.Join(fixtures, "graphql_discussion_answers.json"))
	connection := viewerComments(answers)
	nodes, _ := connection["nodes"].([]any)
	kept := slices.DeleteFunc(slices.Clone(nodes), func(n any) bool {
		node, _ := n.(map[string]any)
		return commentID(node) == TakenBack
	})
	if len(kept) == len(nodes) {
		tb.Fatalf("graphql_discussion_answers.json does not hold comment %s", TakenBack)
	}
	connection["nodes"], connection["totalCount"] = kept, len(kept)
	writeJSON(tb, filepath.Join(dir, "graphql_discussion_answers.json"), answers)
	return dir
}

// readNumbers reads a fixture keeping its numbers as they are written, so a
// comment id is compared, and written back, digit for digit.
func readNumbers(tb testing.TB, path string) map[string]any {
	tb.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		tb.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err = dec.Decode(&out); err != nil {
		tb.Fatalf("%s: %v", path, err)
	}
	return out
}

// commentID is a comment node's id as it is written in the fixture.
func commentID(node map[string]any) string {
	id, _ := node["databaseId"].(json.Number)
	return id.String()
}

// viewerComments is the repositoryDiscussionComments connection of an
// answer to the account's comment walks.
func viewerComments(answer map[string]any) map[string]any {
	data, _ := answer["data"].(map[string]any)
	viewer, _ := data["viewer"].(map[string]any)
	connection, _ := viewer["repositoryDiscussionComments"].(map[string]any)
	return connection
}

// commentNodes is every comment of that connection.
func commentNodes(answer map[string]any) []map[string]any {
	nodes, _ := viewerComments(answer)["nodes"].([]any)
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		if node, ok := n.(map[string]any); ok {
			out = append(out, node)
		}
	}
	return out
}
