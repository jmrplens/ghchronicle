package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// committed is the file the site reads, and actionFile the Action its inputs
// come from, both from this package's directory.
var (
	committed  = filepath.Join("..", "..", defaultOut)
	actionFile = filepath.Join("..", "..", defaultAction)
)

// generate runs the command and returns its status and both streams.
func generate(t *testing.T, args ...string) (status int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	status = run(append([]string{"gen_config"}, args...), &out, &errOut)
	return status, out.String(), errOut.String()
}

// exported is the file this command writes today, read back as the site reads
// it. Nothing here writes a count out: every number asserted below comes from
// the code the file is generated from.
func exported(t *testing.T) surface {
	t.Helper()
	body, count, err := surfaceJSON(actionFile)
	if err != nil {
		t.Fatal(err)
	}
	var got surface
	if err = json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if count != len(got.Options) {
		t.Fatalf("the command reports %d settings and the file carries %d", count, len(got.Options))
	}
	return got
}

// TestWritingTheFileThenCheckingItPasses writes the file, finds it current,
// and then finds an edited one and a missing one stale, naming the command
// that repairs both.
func TestWritingTheFileThenCheckingItPasses(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config-options.json")
	status, stdout, stderr := generate(t, "-out", path, "-action", actionFile)
	if status != 0 || stderr != "" || !strings.Contains(stdout, "settings") {
		t.Fatalf("write = %d, %q, %q, want a clean run naming the count", status, stdout, stderr)
	}

	if status, stdout, stderr = generate(t, "-check", "-out", path, "-action", actionFile); status != 0 ||
		!strings.Contains(stdout, "up to date") || stderr != "" {
		t.Errorf("check right after write = %d, %q, %q, want a pass", status, stdout, stderr)
	}

	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, _, stderr = generate(t, "-check", "-out", path, "-action", actionFile); status != 1 ||
		!strings.Contains(stderr, "go run ./cmd/gen_config") {
		t.Errorf("check after an edit = %d, %q, want the failure and the remedy", status, stderr)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if status, _, stderr = generate(t, "-check", "-out", path, "-action", actionFile); status != 1 ||
		!strings.Contains(stderr, path) {
		t.Errorf("check on a missing file = %d, %q, want the path named", status, stderr)
	}
}

// TestTheFileCarriesEverySettingTheTypesDeclare is the whole point of the
// export: the builder offers a control per entry here, so an entry missing is
// a setting nobody can configure on the page and an entry too many is a
// control that writes a key the loader refuses.
func TestTheFileCarriesEverySettingTheTypesDeclare(t *testing.T) {
	t.Parallel()
	got := exported(t)
	want, err := config.Options()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Options) != len(want) {
		t.Fatalf("the file holds %d settings, internal/config declares %d", len(got.Options), len(want))
	}
	for i, option := range want {
		have := got.Options[i]
		if have.Key != option.Key {
			t.Errorf("entry %d is %q, the types have %q there", i, have.Key, option.Key)
			continue
		}
		if have.Kind != option.Kind || have.Group != option.Group || have.Sink != option.Sink {
			t.Errorf("%s is %s in group %s (sink %q), the types say %s in %s (sink %q)",
				have.Key, have.Kind, have.Group, have.Sink, option.Kind, option.Group, option.Sink)
		}
		if have.Secret != option.Secret || have.Required != option.Required {
			t.Errorf("%s is secret %v required %v, the tags say %v and %v",
				have.Key, have.Secret, have.Required, option.Secret, option.Required)
		}
		if have.Default != option.Default || have.Example != option.Example {
			t.Errorf("%s defaults to %q with example %q, the code says %q and %q",
				have.Key, have.Default, have.Example, option.Default, option.Example)
		}
		if !slices.Equal(have.Choices, option.Choices) || !slices.Equal(have.Keys, option.Keys) {
			t.Errorf("%s offers %v out of %v, the code says %v out of %v",
				have.Key, have.Choices, have.Keys, option.Choices, option.Keys)
		}
	}
}

// TestEveryFamilyAndGroupReachesThePage holds the cadence section's vocabulary
// to the family table, which is the part of a configuration a reader is most
// likely to get wrong by hand: a family named in every.families that does not
// exist is fatal at start-up.
func TestEveryFamilyAndGroupReachesThePage(t *testing.T) {
	t.Parallel()
	got := exported(t)
	var names []string
	for _, f := range got.Families {
		names = append(names, f.Name)
		group, known := config.GroupOf(f.Name)
		if !known {
			t.Errorf("the file offers %s, which is not a family", f.Name)
			continue
		}
		if f.Group != group {
			t.Errorf("the file puts %s in group %s, the table says %s", f.Name, f.Group, group)
		}
		every, _ := config.BuiltinEvery(f.Name)
		want := "0"
		if every > 0 {
			want = config.Compact(every)
		}
		if f.Every != want {
			t.Errorf("the file gives %s a built-in cadence of %s, the table says %s", f.Name, f.Every, want)
		}
	}
	if !slices.Equal(names, config.Families()) {
		t.Errorf("the file offers families %v, the table has %v", names, config.Families())
	}
	var groupNames []string
	for _, g := range got.Groups {
		groupNames = append(groupNames, g.Name)
		if g.Description != config.GroupDescription(g.Name) {
			t.Errorf("%s is described as %q, -groups prints %q", g.Name, g.Description, config.GroupDescription(g.Name))
		}
		if !slices.Equal(g.Families, config.FamiliesIn(g.Name)) {
			t.Errorf("%s holds %v in the file, the table says %v", g.Name, g.Families, config.FamiliesIn(g.Name))
		}
	}
	if !slices.Equal(groupNames, config.Groups()) {
		t.Errorf("the file offers groups %v, the table has %v", groupNames, config.Groups())
	}
}

// TestEveryMappedInputIsAnInputTheActionDeclares is the other half of the
// export. The step the builder writes is only useful if every `with:` key in
// it is one the composite Action reads, and an input renamed in action.yml
// would otherwise leave the page writing a step the Action ignores.
func TestEveryMappedInputIsAnInputTheActionDeclares(t *testing.T) {
	t.Parallel()
	got := exported(t)
	declared := map[string]bool{}
	for _, in := range got.Action.Inputs {
		declared[in.Name] = true
	}
	if len(declared) == 0 {
		t.Fatal("the file carries no Action inputs, so nothing below is checked")
	}
	for _, key := range slices.Sorted(maps.Keys(got.Action.Map)) {
		if !declared[got.Action.Map[key]] {
			t.Errorf("%s is sent to the input %q, which the Action does not declare", key, got.Action.Map[key])
		}
	}
	// The token is the one input the builder must never give a value, so it
	// has to be mapped: a page that did not know it was a setting would offer
	// it as one more text field to paste a credential into.
	if got.Action.Map["github.token"] != "token" {
		t.Errorf("github.token maps to %q, want the Action's token input", got.Action.Map["github.token"])
	}
}

// TestAMappingNamingSomethingGoneIsRefused covers what -check cannot: both
// sides of the table can go stale without the file changing shape, and the
// failure has to name which side.
func TestAMappingNamingSomethingGoneIsRefused(t *testing.T) {
	t.Parallel()
	options, err := config.Options()
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := readInputs(actionFile)
	if err != nil {
		t.Fatal(err)
	}
	gone := map[string]string{"targets.nothing_like_this": "user"}
	if err = checkMapping(gone, options, inputs); err == nil ||
		!strings.Contains(err.Error(), "targets.nothing_like_this") {
		t.Errorf("a mapping from a setting that is gone = %v, want it named", err)
	}
	renamed := map[string]string{"targets.user": "no-such-input"}
	if err = checkMapping(renamed, options, inputs); err == nil ||
		!strings.Contains(err.Error(), "no-such-input") {
		t.Errorf("a mapping to an input that is gone = %v, want it named", err)
	}
}

// TestAnActionWithNoInputsIsRefused covers the empty case -check agrees with
// itself about: a committed file with no inputs and a generated file with no
// inputs are equal, and the gate would pass while the builder could write no
// step at all.
func TestAnActionWithNoInputsIsRefused(t *testing.T) {
	t.Parallel()
	empty := filepath.Join(t.TempDir(), "action.yml")
	if err := os.WriteFile(empty, []byte("name: nothing\ninputs: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := surfaceJSON(empty)
	if err == nil || !strings.Contains(err.Error(), "no inputs") {
		t.Fatalf("an Action with no inputs = %v, want a refusal", err)
	}
}

// TestTheFileIsWrittenAsTheSitesFormatterLeavesIt pins the two things prettier
// asks of a JSON file in the site, because the site's format:check reads this
// file like any other: two-space indentation and a trailing newline.
func TestTheFileIsWrittenAsTheSitesFormatterLeavesIt(t *testing.T) {
	t.Parallel()
	body, _, err := surfaceJSON(actionFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(body), "}\n") {
		t.Error("the file does not end with a newline after its last brace")
	}
	if !strings.Contains(string(body), "\n  \"options\": [\n    {\n      \"key\": ") {
		t.Error("the file is not indented with two spaces a level")
	}
}

// TestTheCommittedFileIsCurrent is the gate CI runs, from the suite as well:
// site/src/data/config-options.json is what the code produces today.
func TestTheCommittedFileIsCurrent(t *testing.T) {
	t.Parallel()
	if status, _, stderr := generate(t, "-check", "-out", committed, "-action", actionFile); status != 0 {
		t.Errorf("the committed configuration surface is stale, regenerate it with `make config-options`:\n%s", stderr)
	}
}

// TestRunStopsOnWhatItCannotDo covers a directory that is not there, an Action
// that is not there, the usage, and a flag it does not know, with the statuses
// the flag package exits with.
func TestRunStopsOnWhatItCannotDo(t *testing.T) {
	t.Parallel()
	absent := filepath.Join(t.TempDir(), "absent", "config-options.json")
	status, stdout, stderr := generate(t, "-out", absent, "-action", actionFile)
	if want := notFoundText(t, absent); status != 1 || stdout != "" || !strings.Contains(stderr, want) {
		t.Errorf("write into a missing directory = %d, %q, %q, want the failure, %q", status, stdout, stderr, want)
	}
	noAction := filepath.Join(t.TempDir(), "action.yml")
	if status, _, stderr = generate(t, "-out", absent, "-action", noAction); status != 1 ||
		!strings.Contains(stderr, noAction) {
		t.Errorf("an Action that is not there = %d, %q, want the path named", status, stderr)
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
