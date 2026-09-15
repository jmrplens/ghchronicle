package dashboards

import "testing"

// TestLowerFirstWordLowersOnlyTheLettersOfTheFirstWord: the alias is the
// column the traffic window panels select, so it has to be the first word
// in lowercase with every other byte kept, the way the Python that wrote
// these files did, including a title of one word.
func TestLowerFirstWordLowersOnlyTheLettersOfTheFirstWord(t *testing.T) {
	t.Parallel()
	for title, want := range map[string]string{
		"Views over time": "views",
		"Clones":          "clones",
		"AZ@[`z09 Rest":   "az@[`z09",
		"":                "",
	} {
		if got := lowerFirstWord(title); got != want {
			t.Errorf("lowerFirstWord(%q) = %q, want %q", title, got, want)
		}
	}
}
