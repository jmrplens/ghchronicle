package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/internal/render"
)

// committed is the file the site reads, from this package's directory.
var committed = filepath.Join("..", "..", defaultOut)

// generate runs the command and returns its status and both streams.
func generate(t *testing.T, args ...string) (status int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	status = run(append([]string{"gen_layouts"}, args...), &out, &errOut)
	return status, out.String(), errOut.String()
}

// TestWritingTheFileThenCheckingItPasses writes the file, finds it current,
// and then finds an edited one and a missing one stale, naming the command
// that repairs both.
func TestWritingTheFileThenCheckingItPasses(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "layouts.json")
	status, stdout, stderr := generate(t, "-out", path)
	count := fmt.Sprintf("%d layouts", len(render.Layouts()))
	if status != 0 || stderr != "" || !strings.Contains(stdout, count) {
		t.Fatalf("write = %d, %q, %q, want a clean run naming the count", status, stdout, stderr)
	}

	if status, stdout, stderr = generate(t, "-check", "-out", path); status != 0 ||
		!strings.Contains(stdout, "up to date") || stderr != "" {
		t.Errorf("check right after write = %d, %q, %q, want a pass", status, stdout, stderr)
	}

	if err := os.WriteFile(path, []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, _, stderr = generate(t, "-check", "-out", path); status != 1 ||
		!strings.Contains(stderr, "go run ./cmd/gen_layouts") {
		t.Errorf("check after an edit = %d, %q, want the failure and the remedy", status, stderr)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if status, _, stderr = generate(t, "-check", "-out", path); status != 1 ||
		!strings.Contains(stderr, path) {
		t.Errorf("check on a missing file = %d, %q, want the path named", status, stderr)
	}
}

// TestTheFileCarriesEveryRegisteredLayoutInOrder reads the file back and holds
// it to the registry, field by field. This is what lets the layouts page state
// a width or a default field without anybody typing it: the page reads this
// file, and this file is the registry.
func TestTheFileCarriesEveryRegisteredLayoutInOrder(t *testing.T) {
	t.Parallel()
	body, err := layoutsJSON(render.Layouts())
	if err != nil {
		t.Fatal(err)
	}
	var got []layout
	if err = json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	registered := render.Layouts()
	if len(got) != len(registered) {
		t.Fatalf("the file holds %d layouts, the registry %d", len(got), len(registered))
	}
	for i, want := range registered {
		have := got[i]
		if have.Name != want.Name {
			t.Errorf("entry %d is %q, the registry has %q there", i, have.Name, want.Name)
			continue
		}
		if have.Family != want.Family || have.Animated != want.Animated || have.Loops != want.Loops {
			t.Errorf("%s is %+v, the registry says family %q, animated %v, loops %v",
				have.Name, have, want.Family, want.Animated, want.Loops)
		}
		if have.Width != want.Width || have.MinWidth != want.MinWidth {
			t.Errorf("%s is drawn at %d (minimum %d), the registry says %d (minimum %d)",
				have.Name, have.Width, have.MinWidth, want.Width, want.MinWidth)
		}
		if !slices.Equal(have.Fields, want.Fields) {
			t.Errorf("%s draws %v by default, the registry says %v", have.Name, have.Fields, want.Fields)
		}
	}
}

// TestTheFileIsWrittenAsTheSitesFormatterLeavesIt pins the two things prettier
// asks of a JSON file in the site, because the site's format:check reads this
// file like any other: two-space indentation and a trailing newline.
func TestTheFileIsWrittenAsTheSitesFormatterLeavesIt(t *testing.T) {
	t.Parallel()
	body, err := layoutsJSON(render.Layouts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(body), "}\n]\n") {
		t.Errorf("the file ends %q, want a closed list and a newline", string(body[len(body)-8:]))
	}
	if !strings.Contains(string(body), "\n  {\n    \"name\": ") {
		t.Error("the file is not indented with two spaces a level")
	}
}

// TestAnEmptyRegistryIsRefused covers the one thing -check cannot catch on its
// own: a committed `[]` and a generated `[]` are equal, so the gate would pass
// while stating the facts of no layout at all.
func TestAnEmptyRegistryIsRefused(t *testing.T) {
	t.Parallel()
	body, err := layoutsJSON(nil)
	if body != nil || err == nil || !strings.Contains(err.Error(), "layouts.go") {
		t.Fatalf("layoutsJSON(nil) = %q, %v, want no bytes and the registry named", body, err)
	}
}

// TestTheCommittedFileIsCurrent is the gate CI runs, from the suite as well:
// site/src/data/layouts.json is what the registry produces today.
func TestTheCommittedFileIsCurrent(t *testing.T) {
	t.Parallel()
	if status, _, stderr := generate(t, "-check", "-out", committed); status != 0 {
		t.Errorf("the committed layouts are stale, regenerate them with `make layouts`:\n%s", stderr)
	}
}

// TestRunStopsOnWhatItCannotDo covers a directory that is not there, the
// usage, and a flag it does not know, with the statuses the flag package
// exits with.
func TestRunStopsOnWhatItCannotDo(t *testing.T) {
	t.Parallel()
	absent := filepath.Join(t.TempDir(), "absent", "layouts.json")
	status, stdout, stderr := generate(t, "-out", absent)
	if want := notFoundText(t, absent); status != 1 || stdout != "" || !strings.Contains(stderr, want) {
		t.Errorf("write into a missing directory = %d, %q, %q, want the failure, %q", status, stdout, stderr, want)
	}
	if status, _, stderr = generate(t, "-h"); status != 0 || !strings.Contains(stderr, "-check") {
		t.Errorf("-h = %d, %q, want the flags listed and a clean exit", status, stderr)
	}
	if status, _, stderr = generate(t, "-force"); status != 2 ||
		!strings.Contains(stderr, "flag provided but not defined: -force") {
		t.Errorf("-force = %d, %q, want 2 and the flag named", status, stderr)
	}
}

// notFoundText is how this platform words the failure to reach path, which the
// command passes on as it is: "no such file or directory" on Unix, and on
// Windows the system's own sentence. So it is read off the same failure here
// rather than written out in Linux's words.
func notFoundText(t *testing.T, path string) string {
	t.Helper()
	_, err := os.Stat(path)
	var pathErr *fs.PathError
	if !errors.Is(err, fs.ErrNotExist) || !errors.As(err, &pathErr) {
		t.Fatalf("stat %s = %v, want it missing", path, err)
	}
	return pathErr.Err.Error()
}
