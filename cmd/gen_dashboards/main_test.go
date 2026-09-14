package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/cmd/internal/dashboards"
)

// TestLayoutOf flattens a collapsed row the way Grafana reads it: the row,
// then the panels it carries, then whatever follows the row.
func TestLayoutOf(t *testing.T) {
	t.Parallel()
	place := func(x, y, w, h int) map[string]any {
		return map[string]any{"x": x, "y": y, "w": w, "h": h}
	}
	doc := map[string]any{"panels": []map[string]any{
		{"title": "Overview", "gridPos": place(0, 0, 24, 1), "panels": []map[string]any{}},
		{"title": "Stars", "gridPos": place(0, 1, 12, 8)},
		{"title": "Security", "gridPos": place(0, 9, 24, 1), "panels": []map[string]any{
			{"title": "Alerts", "gridPos": place(0, 10, 24, 6)},
		}},
		{"title": "Releases", "gridPos": place(0, 10, 24, 1)},
	}}
	got, err := layoutOf(doc)
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]any{
		{"Overview", "0 0 24 1"},
		{"Stars", "0 1 12 8"},
		{"Security", "0 9 24 1"},
		{"Alerts", "0 10 24 6"},
		{"Releases", "0 10 24 1"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("layoutOf =\n%v\nwant\n%v", got, want)
	}
}

// TestLayoutOfRefusesAMalformedDocument reports a document whose panels have
// no place on the grid, at the top level or inside a row, rather than
// stopping the generator on a failed type assertion.
func TestLayoutOfRefusesAMalformedDocument(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]map[string]any{
		"no panel list":          {},
		"panels of another type": {"panels": []any{map[string]any{"title": "Stars"}}},
		"a panel with no place":  {"panels": []map[string]any{{"title": "Stars"}}},
		"a row holding a panel with no place": {"panels": []map[string]any{{
			"title":   "Security",
			"gridPos": map[string]any{"x": 0, "y": 0, "w": 24, "h": 1},
			"panels":  []map[string]any{{"title": "Alerts"}},
		}}},
	} {
		if _, err := layoutOf(doc); err == nil {
			t.Errorf("%s: layoutOf accepted it", name)
		}
	}
}

// generate runs the command and returns its status and both streams.
func generate(t *testing.T, args ...string) (status int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	status = run(append([]string{"gen_dashboards"}, args...), &out, &errOut)
	return status, out.String(), errOut.String()
}

// TestWriteThenCheckRoundTrips writes the five files, finds them current, and
// then finds an edited one and a missing one out of date.
func TestWriteThenCheckRoundTrips(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	status, stdout, stderr := generate(t, "-dir", dir)
	if status != 0 || stderr != "" {
		t.Fatalf("write = %d, %q, want a clean run", status, stderr)
	}
	stores := dashboards.AllStores()
	for _, s := range stores {
		line := fmt.Sprintf("wrote %s, %d panels\n", filepath.Join(dir, s.File), dashboards.Count())
		if !strings.Contains(stdout, line) {
			t.Errorf("stdout = %q, want %q", stdout, line)
		}
	}
	if !strings.HasSuffix(stdout, fmt.Sprintf("%d dashboards, identical layouts\n", len(stores))) {
		t.Errorf("stdout = %q, want the layouts confirmed identical", stdout)
	}

	if status, stdout, stderr = generate(t, "-check", "-dir", dir); status != 0 || stdout != "" || stderr != "" {
		t.Errorf("check right after write = %d, %q, %q, want a silent pass", status, stdout, stderr)
	}

	edited := filepath.Join(dir, stores[0].File)
	if err := os.WriteFile(edited, []byte(`{"panels":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, stores[1].File)
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	status, _, stderr = generate(t, "-check", "-dir", dir)
	if status != 1 || !strings.Contains(stderr, edited+" is out of date\n") ||
		!strings.Contains(stderr, missing+": open ") {
		t.Errorf("check after an edit = %d, %q, want both files named", status, stderr)
	}
}

// TestTheCommittedDashboardsAreCurrent is the check CI runs, from the test
// suite: the files under dashboards/ are what the specification produces.
func TestTheCommittedDashboardsAreCurrent(t *testing.T) {
	t.Parallel()
	if status, _, stderr := generate(t, "-check", "-dir", "../../dashboards"); status != 0 {
		t.Errorf("the committed dashboards are stale, regenerate them with `make gen-dashboards`:\n%s", stderr)
	}
}

// notFoundText is how this platform words the failure to reach path, which the
// command passes on as it is: "no such file or directory" on Unix, and on
// Windows the system's own sentence, which for a file whose directory is
// missing is not the one for a missing file. So it is read off the same
// failure here rather than written out in Linux's words.
func notFoundText(t *testing.T, path string) string {
	t.Helper()
	_, err := os.Stat(path)
	var pathErr *fs.PathError
	if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pathErr) {
		t.Fatalf("stat %s = %v, want it missing", path, err)
	}
	return pathErr.Err.Error()
}

// TestRunStopsOnWhatItCannotDo covers a directory that is not there, the
// usage, and a flag it does not know, with the statuses the flag package
// exits with.
func TestRunStopsOnWhatItCannotDo(t *testing.T) {
	t.Parallel()
	absent := filepath.Join(t.TempDir(), "absent")
	status, stdout, stderr := generate(t, "-dir", absent)
	if want := notFoundText(t, filepath.Join(absent, dashboards.AllStores()[0].File)); status != 1 || stdout != "" ||
		!strings.Contains(stderr, want) {
		t.Errorf("write into a missing directory = %d, %q, %q, want the failure, %q", status, stdout, stderr, want)
	}
	if status, _, stderr = generate(t, "-h"); status != 0 || !strings.Contains(stderr, "-check") {
		t.Errorf("-h = %d, %q, want the flags listed and a clean exit", status, stderr)
	}
	if status, _, stderr = generate(t, "-force"); status != 2 || !strings.Contains(stderr, "flag provided but not defined: -force") {
		t.Errorf("-force = %d, %q, want 2 and the flag named", status, stderr)
	}
}

// TestCompareNamesTheFirstDifference reports a different count of panels, and
// otherwise the first panel whose title or place differs.
func TestCompareNamesTheFirstDifference(t *testing.T) {
	t.Parallel()
	first := [][2]any{{"Stars", "0 0 12 8"}, {"Forks", "12 0 12 8"}}
	if got := compare(first, first[:1], nil); got != "1 panels against the first dashboard's 2" {
		t.Errorf("compare = %q, want the counts", got)
	}
	moved := [][2]any{{"Stars", "0 0 12 8"}, {"Forks", "0 8 12 8"}}
	if got := compare(first, moved, nil); got != "panel 1 is [Forks 0 8 12 8], the first dashboard has [Forks 12 0 12 8]" {
		t.Errorf("compare = %q, want the moved panel named", got)
	}
	if got := compare(first, first, nil); got != "" {
		t.Errorf("compare = %q for identical layouts, want nothing", got)
	}
	// A declared Prometheus title is allowed at its position and nowhere
	// else, and never excuses a moved panel.
	renamed := [][2]any{{"Stars", "0 0 12 8"}, {"Forks, 14-day window", "12 0 12 8"}}
	if got := compare(first, renamed, map[int]bool{1: true}); got != "" {
		t.Errorf("compare = %q for a declared title, want nothing", got)
	}
	if got := compare(first, renamed, map[int]bool{0: true}); got == "" {
		t.Error("compare accepted a title that differs at a position not declared")
	}
	renamedAndMoved := [][2]any{{"Stars", "0 0 12 8"}, {"Forks, 14-day window", "0 8 12 8"}}
	if got := compare(first, renamedAndMoved, map[int]bool{1: true}); got == "" {
		t.Error("compare accepted a moved panel because its title was declared")
	}
}

// TestSameJSONComparesMeaning ignores the formatting of two documents and
// never calls a document that does not parse the same as anything.
func TestSameJSONComparesMeaning(t *testing.T) {
	t.Parallel()
	if !sameJSON([]byte(`{"a": [1, 2]}`), []byte("{\n  \"a\": [1,2]\n}\n")) {
		t.Error("sameJSON told two formattings of one document apart")
	}
	if sameJSON([]byte(`{"a": 1}`), []byte(`{"a": 2}`)) {
		t.Error("sameJSON called two documents the same")
	}
	if sameJSON([]byte(`{`), []byte(`{`)) {
		t.Error("sameJSON called a document that does not parse the same as itself")
	}
}
