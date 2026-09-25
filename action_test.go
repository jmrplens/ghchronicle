package ghchronicle

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/render"
)

// TestTheActionsDefaultConfigLeavesPrivateRepositoriesOut runs the script the
// Action uses when it is given no configuration. That configuration only ever
// feeds a card, which is published, and a card that counts private
// repositories puts their names in a profile README. So private repositories
// stay out unless the caller says true, and anything that is not exactly true
// or false is refused rather than read as one of them. The login is written
// into the same YAML, so anything that is not a GitHub login is refused too:
// a line break in it would add keys of its own to the configuration.
func TestTheActionsDefaultConfigLeavesPrivateRepositoriesOut(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("the Action's steps run in bash, and there is none here")
	}
	script := filepath.Join("scripts", "action-config.sh")
	for _, tc := range []struct {
		name, login, include string
		status               int
		want                 string
	}{
		{"not given", "octocat", "false", 0, "include_private: false"},
		{"asked for", "octocat", "true", 0, "include_private: true"},
		{"a word that is neither", "octocat", "yes", 2, "include-private must be true or false"},
		{"a line break", "octocat", "false\nsinks: {x: 1}", 2, "include-private must be true or false"},
		{"a login with a key after a line break", "octocat\nsinks: {x: 1}", "false", 2, "user must be a GitHub login"},
		{"a login that starts with a dash", "-octocat", "false", 2, "user must be a GitHub login"},
		{"no login", "", "false", 2, "user must be a GitHub login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "ghchronicle.yaml")
			cmd := exec.CommandContext(t.Context(), "bash", script)
			cmd.Env = append(os.Environ(),
				"USER_LOGIN="+tc.login, "INCLUDE_PRIVATE="+tc.include,
				"OUT="+out, "STATE="+filepath.Join(dir, "state.json"))
			output, _ := cmd.CombinedOutput()
			if cmd.ProcessState.ExitCode() != tc.status {
				t.Fatalf("exit %d, want %d:\n%s", cmd.ProcessState.ExitCode(), tc.status, output)
			}
			got := string(output)
			if tc.status == 0 {
				body, readErr := os.ReadFile(out)
				if readErr != nil {
					t.Fatal(readErr)
				}
				got = string(body)
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("want %q in:\n%s", tc.want, got)
			}
		})
	}
}

// stubMode is the ghchronicle stand-in's permission. It has to be a named
// constant, since gosec only reads literals: the script under test runs
// ghchronicle by name off PATH, not through bash, so the stand-in needs its
// executable bit to be found at all, the same reasoning internal/sink/file.go
// documents for its own dumpMode constant.
const stubMode = 0o755

// writeGHChronicleStub puts a stand-in ghchronicle on a PATH directory of its
// own. It records the arguments it was called with, one per line, to the
// file named by $RECORD, rather than doing anything a real sweep or card
// render would do: what the Run step's script is tested for is the argument
// list it builds, not the binary's behavior.
func writeGHChronicleStub(t *testing.T) (pathDir, record string) {
	t.Helper()
	pathDir = t.TempDir()
	record = filepath.Join(pathDir, "record")
	stub := filepath.Join(pathDir, "ghchronicle")
	body := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > \"$RECORD\"\n"
	if err := os.WriteFile(stub, []byte(body), stubMode); err != nil {
		t.Fatal(err)
	}
	return pathDir, record
}

// runInputs is one set of the environment variables the Run step's script
// reads.
type runInputs struct {
	mode, since, card, layout, theme, fields, motion string
	// width is the card-width input, as the workflow author typed it. Empty
	// and "0" both mean the layout's own width, and neither reaches the
	// binary: an older pinned release would not know the flag.
	width string
	// speed is the card-speed input, likewise. Empty and "0.5" both mean the
	// pace every card is drawn at anyway, so neither reaches the binary; "0"
	// does, because it is the slowest animation and not the absence of one.
	speed string
}

// runActionRunScript runs scripts/action-run.sh with a stand-in ghchronicle
// on PATH and the given inputs, and returns its exit status, its combined
// output, and the path the stand-in would have recorded its arguments to
// (present only if it ran). dir, when non-empty, becomes the subprocess's
// working directory, so a relative CARD resolves against it rather than
// against the module root; the script path is made absolute first so bash
// can still find it once Dir points elsewhere.
func runActionRunScript(t *testing.T, in runInputs, configPath, dir string) (status int, output, record string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("the Action's steps run in bash, and there is none here")
	}
	script, err := filepath.Abs(filepath.Join("scripts", "action-run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	stubDir, record := writeGHChronicleStub(t)

	cmd := exec.CommandContext(t.Context(), "bash", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"PATH="+stubDir+":"+os.Getenv("PATH"),
		"RECORD="+record,
		"GHC_CONFIG="+configPath,
		"MODE="+in.mode,
		"SINCE="+in.since,
		"CARD="+in.card,
		"LAYOUT="+in.layout,
		"THEME="+in.theme,
		"FIELDS="+in.fields,
		"MOTION="+in.motion,
		"WIDTH="+in.width,
		"SPEED="+in.speed,
	)
	out, _ := cmd.CombinedOutput()
	return cmd.ProcessState.ExitCode(), string(out), record
}

// runCommandLineCase is one case of
// TestTheActionsRunStepBuildsTheRightCommandLineForEachMode.
type runCommandLineCase struct {
	name string
	in   runInputs
	// cardSubdir is the directory component under the test's own tmp dir
	// that the card path's placeholder resolves to. Empty means
	// "generated", the default every other case uses. Ignored when
	// cardRelative is set.
	cardSubdir string
	// cardRelative, when non-empty, is used as the literal CARD value
	// (in place of the usual absolute, tmp-dir-rooted path) and the
	// subprocess is run with the tmp dir as its working directory, so
	// this resolves exactly as given: a value starting with a dash, once
	// dirname strips the file name off it, stays a dash-led string.
	cardRelative string
	wantArgs     []string
	// checkDirCreated asserts that the card's directory exists after the
	// run, in addition to the argument list.
	checkDirCreated bool
}

// resolveCard turns tc.in.card's "CARD" placeholder (or tc.cardRelative) into
// the real path the subprocess sees, and returns the subprocess's working
// directory and the directory the card's mkdir -p is expected to have
// created, for one runCommandLineCase.
func resolveCard(tc runCommandLineCase, workDir string) (in runInputs, dir, wantCardDir string) {
	in = tc.in
	if tc.cardRelative != "" {
		in.card = tc.cardRelative
		dir = workDir
		wantCardDir = filepath.Join(workDir, filepath.Dir(tc.cardRelative))
		return in, dir, wantCardDir
	}
	if in.card != "" {
		subdir := tc.cardSubdir
		if subdir == "" {
			subdir = "generated"
		}
		in.card = filepath.Join(workDir, subdir, "card.svg")
		wantCardDir = filepath.Join(workDir, subdir)
	}
	return in, dir, wantCardDir
}

// expandWantArgs substitutes the CONFIG and CARD placeholders in a case's
// wantArgs with the paths the test actually used, and joins the result the
// same way the stand-in's record file is compared against.
func expandWantArgs(wantArgs []string, configPath, card string) string {
	want := make([]string, len(wantArgs))
	for i, a := range wantArgs {
		switch a {
		case "CONFIG":
			a = configPath
		case "CARD":
			a = card
		}
		want[i] = a
	}
	return strings.Join(want, "\n")
}

// TestTheActionsRunStepBuildsTheRightCommandLineForEachMode runs the script
// the Run step calls, with a stand-in ghchronicle on PATH, and checks the
// exact argument list it assembles for once, backfill and card mode, and for
// each of the card options.
func TestTheActionsRunStepBuildsTheRightCommandLineForEachMode(t *testing.T) {
	for _, tc := range []runCommandLineCase{
		{
			name:     "once sweeps and exits",
			in:       runInputs{mode: "once", layout: "summary", theme: "auto", motion: "once"},
			wantArgs: []string{"-config", "CONFIG", "-once"},
		},
		{
			name:     "backfill without a bound reaches as far back as GitHub allows",
			in:       runInputs{mode: "backfill", layout: "summary", theme: "auto", motion: "once"},
			wantArgs: []string{"-config", "CONFIG", "-backfill"},
		},
		{
			name:     "backfill with a bound stops there",
			in:       runInputs{mode: "backfill", since: "90d", layout: "summary", theme: "auto", motion: "once"},
			wantArgs: []string{"-config", "CONFIG", "-backfill", "-backfill-since", "90d"},
		},
		{
			name:     "card mode with a card path renders only the card",
			in:       runInputs{mode: "card", card: "CARD", layout: "summary", theme: "auto", motion: "once"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "summary", "-card-theme", "auto", "-card-only"},
		},
		{
			name:     "once mode with a card path also writes it, without card-only",
			in:       runInputs{mode: "once", card: "CARD", layout: "summary", theme: "auto", motion: "once"},
			wantArgs: []string{"-config", "CONFIG", "-once", "-card", "CARD", "-card-layout", "summary", "-card-theme", "auto"},
		},
		{
			name:     "card-fields is passed through when given",
			in:       runInputs{mode: "card", card: "CARD", layout: "summary", theme: "auto", fields: "stars,followers", motion: "once"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "summary", "-card-theme", "auto", "-card-fields", "stars,followers", "-card-only"},
		},
		{
			name:     "card-fields is left out when empty",
			in:       runInputs{mode: "card", card: "CARD", layout: "summary", theme: "auto", motion: "once"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "summary", "-card-theme", "auto", "-card-only"},
		},
		{
			name:     "card-motion once is the default and is not passed",
			in:       runInputs{mode: "card", card: "CARD", layout: "summary", theme: "auto", motion: "once"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "summary", "-card-theme", "auto", "-card-only"},
		},
		{
			name:     "card-motion loop is passed through",
			in:       runInputs{mode: "card", card: "CARD", layout: "summary", theme: "auto", motion: "loop"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "summary", "-card-theme", "auto", "-card-motion", "loop", "-card-only"},
		},
		{
			name:     "card-theme both is passed through",
			in:       runInputs{mode: "card", card: "CARD", layout: "summary", theme: "both", motion: "once"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "summary", "-card-theme", "both", "-card-only"},
		},
		{
			name:     "card-width is passed through when given",
			in:       runInputs{mode: "card", card: "CARD", layout: "activity-heatmap", theme: "auto", motion: "once", width: "900"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "activity-heatmap", "-card-theme", "auto", "-card-width", "900", "-card-only"},
		},
		{
			// Empty and zero are the same answer, the layout's own width, and
			// neither may reach the binary: a workflow that pins `version` to
			// a release from before the flag existed would stop running.
			name:     "card-width is left out when empty",
			in:       runInputs{mode: "card", card: "CARD", layout: "summary", theme: "auto", motion: "once"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "summary", "-card-theme", "auto", "-card-only"},
		},
		{
			name:     "card-width is left out when it is zero",
			in:       runInputs{mode: "card", card: "CARD", layout: "summary", theme: "auto", motion: "once", width: "0"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "summary", "-card-theme", "auto", "-card-only"},
		},
		{
			name:     "card-speed is passed through when given",
			in:       runInputs{mode: "card", card: "CARD", layout: "terminal", theme: "auto", motion: "once", speed: "0.8"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "terminal", "-card-theme", "auto", "-card-speed", "0.8", "-card-only"},
		},
		{
			// The slow end of the range is a speed a reader means, so unlike
			// the width's zero it reaches the binary. A card at 0 animates,
			// slowly; a card that does not animate is card-motion off.
			name:     "card-speed zero is passed through, being the slowest animation and not the absence of one",
			in:       runInputs{mode: "card", card: "CARD", layout: "ticker", theme: "auto", motion: "once", speed: "0"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "ticker", "-card-theme", "auto", "-card-speed", "0", "-card-only"},
		},
		{
			name:     "card-speed is left out when empty",
			in:       runInputs{mode: "card", card: "CARD", layout: "summary", theme: "auto", motion: "once"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "summary", "-card-theme", "auto", "-card-only"},
		},
		{
			// Its own default, which draws the same card the flag's absence
			// draws, so it must not reach a release from before the flag.
			name:     "card-speed is left out when it is the default",
			in:       runInputs{mode: "card", card: "CARD", layout: "summary", theme: "auto", motion: "once", speed: "0.5"},
			wantArgs: []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "summary", "-card-theme", "auto", "-card-only"},
		},
		{
			// mkdir -p -- "$(dirname -- "$CARD")" carries both "--"s
			// precisely so a directory name starting with a dash is never
			// read as an option: without them, dirname itself would refuse
			// "-dash/card.svg" as an unrecognized flag before mkdir even
			// runs. Nothing pinned that before this case. The path has to be
			// relative for the dash to land as the first character dirname
			// and mkdir see; a tmp-dir-rooted absolute path never does,
			// since it starts with the tmp prefix instead.
			name:            "a card path whose directory starts with a dash still gets created and passed through as a value",
			in:              runInputs{mode: "card", layout: "summary", theme: "auto", motion: "once"},
			cardRelative:    "-dash/card.svg",
			wantArgs:        []string{"-config", "CONFIG", "-card", "CARD", "-card-layout", "summary", "-card-theme", "auto", "-card-only"},
			checkDirCreated: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			configPath := filepath.Join(workDir, "config.yaml")
			in, dir, wantCardDir := resolveCard(tc, workDir)

			status, output, record := runActionRunScript(t, in, configPath, dir)
			if status != 0 {
				t.Fatalf("exit %d, want 0:\n%s", status, output)
			}

			body, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.TrimRight(string(body), "\n")
			want := expandWantArgs(tc.wantArgs, configPath, in.card)

			// The card path reaches the binary as the value right after
			// -card, exactly as given, whether or not its directory starts
			// with a dash: the recorded argument list above already proves
			// that, since a value split or swallowed as an option would show
			// up as a mismatch there.
			if got != want {
				t.Errorf("args =\n%s\nwant\n%s", got, want)
			}

			if tc.checkDirCreated {
				if _, statErr := os.Stat(wantCardDir); statErr != nil {
					t.Errorf("card directory not created: %v", statErr)
				}
			}
		})
	}
}

// TestTheActionsRunStepRejectsAModeItDoesNotUnderstand checks that an unknown
// mode, and card mode with no card path, both exit 2 with their message and
// never reach ghchronicle.
func TestTheActionsRunStepRejectsAModeItDoesNotUnderstand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      runInputs
		wantMsg string
	}{
		{
			name:    "an unknown mode",
			in:      runInputs{mode: "bogus", layout: "summary", theme: "auto", motion: "once"},
			wantMsg: "mode must be once, backfill or card, got 'bogus'",
		},
		{
			name:    "card mode without a card path",
			in:      runInputs{mode: "card", layout: "summary", theme: "auto", motion: "once"},
			wantMsg: "mode card needs a card path",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			configPath := filepath.Join(workDir, "config.yaml")

			status, output, record := runActionRunScript(t, tc.in, configPath, "")
			if status != 2 {
				t.Fatalf("exit %d, want 2:\n%s", status, output)
			}
			if !strings.Contains(output, tc.wantMsg) {
				t.Errorf("want %q in:\n%s", tc.wantMsg, output)
			}
			if _, err := os.Stat(record); err == nil {
				t.Error("ghchronicle ran, want it skipped")
			}
		})
	}
}

// TestTheActionsRunStepCreatesTheCardsDirectoryOnlyWhenGivenAPath checks the
// one filesystem side effect the script has outside the environment: a card
// path whose directory does not exist yet gets one, and no card path means
// nothing is created at all.
func TestTheActionsRunStepCreatesTheCardsDirectoryOnlyWhenGivenAPath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		withCard bool
	}{
		{"a card path creates its directory", true},
		{"no card path creates nothing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			configPath := filepath.Join(workDir, "config.yaml")
			cardDir := filepath.Join(workDir, "generated", "nested")

			card := ""
			if tc.withCard {
				card = filepath.Join(cardDir, "card.svg")
			}

			status, output, _ := runActionRunScript(t, runInputs{
				mode: "once", card: card, layout: "summary", theme: "auto", motion: "once",
			}, configPath, "")
			if status != 0 {
				t.Fatalf("exit %d, want 0:\n%s", status, output)
			}

			_, statErr := os.Stat(cardDir)
			exists := statErr == nil
			if exists != tc.withCard {
				t.Errorf("card directory exists = %v, want %v", exists, tc.withCard)
			}
		})
	}
}

// builderStepCase is one case of the configuration builder's corpus, as this
// file needs it: the workflow step, and whether the answers behind it name a
// destination.
//
// The whole case file is generated by site/scripts/gen-config-cases.mjs out of
// the module the documentation page runs, so the steps below are the ones a
// reader copies off the page rather than steps written here to pass.
type builderStepCase struct {
	Name         string `json:"name"`
	Step         string `json:"step"`
	AllowNoSinks bool   `json:"allowNoSinks"`
}

// readBuilderSteps is every step the builder writes, from the committed cases.
func readBuilderSteps(t *testing.T) []builderStepCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("internal", "config", "testdata", "config-cases.json"))
	if err != nil {
		t.Fatalf("%v. Generate it with `make config-cases`", err)
	}
	var file struct {
		Cases []builderStepCase `json:"cases"`
	}
	if err = json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Cases) == 0 {
		t.Fatal("the builder's cases file holds no cases, so nothing below is checked")
	}
	return file.Cases
}

// stepInputs is the `with:` block of a workflow step, as a map.
func stepInputs(step string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(step, "\n") {
		if !strings.HasPrefix(line, "    ") {
			continue
		}
		key, value, isPair := strings.Cut(strings.TrimSpace(line), ": ")
		if !isPair {
			continue
		}
		out[key] = strings.Trim(strings.TrimSpace(value), `"`)
	}
	return out
}

// TestTheStepTheBuilderWritesWithNoConfigFileStarts is the question the shape
// of a step cannot answer: would the run it describes get past start-up.
//
// A step with no `config:` sends the Action down its own path: it writes a
// configuration of its own, with `sinks: {}`, and a configuration that names
// no destination is refused unless the run waives the rule. The Action waives
// it in exactly one place, `mode: card` with a `card:` path, so a step the
// builder writes without a file has to be that shape or it fails on its first
// use, in public, with an error about sinks nobody asked for.
//
// This runs the Action's two scripts exactly as action.yml runs them, with a
// stand-in ghchronicle recording the argument list, and then loads the
// configuration the first script wrote with the waiver the second script
// decided on.
func TestTheStepTheBuilderWritesWithNoConfigFileStarts(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("the Action's steps run in bash, and there is none here")
	}
	t.Setenv("GITHUB_TOKEN", "a-token-from-the-runner")
	script := filepath.Join("scripts", "action-config.sh")
	var checked int
	for _, c := range readBuilderSteps(t) {
		with := stepInputs(c.Step)
		if _, hasFile := with["config"]; hasFile {
			continue // the reader's own file, which internal/config loads
		}
		checked++
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			written := filepath.Join(dir, "ghchronicle.yaml")
			// A step that names no account leaves the input at its own
			// default, which is the expression ${{ github.repository_owner }}
			// and is the runner's to resolve, not this test's. A login stands
			// in for it, since what is under test is whether the run starts.
			login := with["user"]
			if login == "" {
				login = "octocat"
			}
			cmd := exec.CommandContext(t.Context(), "bash", script)
			cmd.Env = append(os.Environ(),
				"USER_LOGIN="+login,
				"INCLUDE_PRIVATE="+with["include-private"],
				"OUT="+written,
				"STATE="+filepath.Join(dir, "state.json"))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("the Action refused the step's own inputs: %v\n%s\n%s", err, output, c.Step)
			}
			// The card path is the step's own, resolved against the
			// checkout the way a runner resolves it; a step with no card
			// leaves the input empty rather than pointing at a directory.
			var card string
			if with["card"] != "" {
				card = filepath.Join(dir, with["card"])
			}
			status, output, record := runActionRunScript(t, runInputs{
				mode:   with["mode"],
				card:   card,
				layout: "summary", theme: "auto", motion: "once",
			}, written, dir)
			if status != 0 {
				t.Fatalf("the Action's run step exited %d for this step:\n%s\n%s", status, c.Step, output)
			}
			args := strings.Split(strings.TrimSpace(readRecord(t, record)), "\n")
			cardOnly := slices.Contains(args, "-card-only")
			if cardOnly != c.AllowNoSinks {
				t.Errorf("the run waives the sink rule %v and the answers name no destination %v: %v",
					cardOnly, c.AllowNoSinks, args)
			}
			if _, err := config.LoadWith(written, config.Relax{NoSinks: cardOnly}); err != nil {
				t.Errorf("the step the page writes cannot start:\n%s\nand the Action ran %v, which the loader refused: %v",
					c.Step, args, err)
			}
		})
	}
	if checked == 0 {
		t.Error("no case writes a step without a config file, so the Action's own path is not covered")
	}
}

// readRecord is the argument list the stand-in ghchronicle recorded.
func readRecord(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the stand-in ghchronicle never ran: %v", err)
	}
	return string(body)
}

// TestTheCardWidthInputNamesNoWidth holds the Action's most public piece of
// prose to the one rule that keeps it true: it may say where the widths are,
// and it may not say what they are.
//
// This description is what GitHub renders on the Marketplace listing and in the
// Action's own input documentation, so it is what a workflow author reads
// immediately before typing a number. It said "between its own minimum and
// 1200" and "the whole year at about 900" for two commits after the widths had
// moved: activity-heatmap's far end became 891, which made 900 a width the
// binary refuses and the recommendation a failed workflow. Nothing noticed,
// because cmd/gen_config exports only an input's name, required and default,
// so no generated file carries this sentence and no check compares it.
//
// A width belongs to a layout and lives in internal/render's registry, which
// -card-layouts prints and the layouts page states per layout from the same
// export. Written here it is a copy, and a copy of a number that nothing
// regenerates is a number waiting to go stale. So: no run of two or more
// digits. A single digit still passes, which is what lets the sentence say
// that 0 means the layout's own width.
//
// The rule is this input's alone and is meant to stay that way. It is not
// "descriptions may not hold numbers": it is "a description may not copy a
// number that belongs to a layout". card-speed's three, the two ends of its
// range and its default, belong to no layout and are the option's own
// definition, so the test below holds that description to them instead of
// forbidding them.
//
// Digits are not the only way to write a number, and the spelled form is not
// hypothetical here: the comment above heatGridWeeks went stale as "about nine
// hundred units", in this same repository, in this same change. A rule that
// caught the shape that just went wrong and missed the one beside it would be
// the weaker half of a gate, so the magnitudes are refused too.
func TestTheCardWidthInputNamesNoWidth(t *testing.T) {
	description := actionInputDescription(t, "card-width")
	written := regexp.MustCompile(`(?i)\d\d+|hundred|thousand`)
	if found := written.FindAllString(description, -1); found != nil {
		t.Errorf("the card-width input names the width(s) %v. A width belongs to a layout, "+
			"and every layout's two ends are printed by -card-layouts and stated in its own "+
			"section of the layouts page, both from internal/render. Point at those instead "+
			"of copying a number nothing regenerates:\n%s", found, description)
	}
	// Prohibiting the number is only half a rule: the sentence has to leave a
	// reader somewhere to find it.
	if !strings.Contains(description, "-card-layouts") {
		t.Errorf("the card-width input names no width, which is right, and points the reader "+
			"at nothing that does. Name -card-layouts:\n%s", description)
	}
}

// TestTheCardSpeedInputNamesTheRangeItsReaderHasToType is the other half of
// the rule above, on the input that has to state its numbers rather than point
// at them.
//
// A speed is not a per-layout fact. There is no registry entry to look it up
// in, -card-layouts prints nothing about it, and the layouts page states per
// layout only what the registry holds. The two ends and the default are the
// option's own definition and live in internal/render as SpeedSlowest,
// SpeedDefault and SpeedFastest, so a description that refused to name them
// could not tell a workflow author which end is which, and could not say the
// one thing this option most needs said: that the default is the pace every
// card already has.
//
// Naming them makes them a copy, which is what the width rule is about, so
// they are held to the constants here. The Marketplace listing is where a
// workflow author reads this immediately before typing a number, and a range
// that no longer matches the binary is a workflow that fails on its first run.
func TestTheCardSpeedInputNamesTheRangeItsReaderHasToType(t *testing.T) {
	description := actionInputDescription(t, "card-speed")
	decimal := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	claims := []string{
		"from " + decimal(render.SpeedSlowest) + " to " + decimal(render.SpeedFastest),
		decimal(render.SpeedDefault),
	}
	for _, want := range claims {
		if !strings.Contains(description, want) {
			t.Errorf("the card-speed input does not say %q, and the renderer draws from %v to %v "+
				"with %v as the default. Correct the description:\n%s",
				want, render.SpeedSlowest, render.SpeedFastest, render.SpeedDefault, description)
		}
	}
	// And the thing a range starting at zero does not say for itself. A reader
	// who wants a still card and reaches for 0 gets the slowest animation
	// there is, which is the opposite of what was asked for.
	if !strings.Contains(description, render.MotionOff) {
		t.Errorf("the card-speed input never says that %s is the slowest animation rather than "+
			"none, and never names card-motion %s, which is what draws a still card:\n%s",
			decimal(render.SpeedSlowest), render.MotionOff, description)
	}
}

// actionInputDescription is one input's description, read out of action.yml the
// way cmd/gen_config reads the rest of the input, and failing the test when the
// Action does not declare it.
func actionInputDescription(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("action.yml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Inputs map[string]struct {
			Description string `yaml:"description"`
		} `yaml:"inputs"`
	}
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("action.yml: %v", err)
	}
	in, ok := doc.Inputs[name]
	if !ok {
		t.Fatalf("action.yml declares no %q input; it has %v", name, slices.Sorted(maps.Keys(doc.Inputs)))
	}
	if strings.TrimSpace(in.Description) == "" {
		t.Fatalf("action.yml declares %q with no description", name)
	}
	return in.Description
}

// TestTheActionInstallsTheReleaseItIsGivenWithOrWithoutItsV runs the Action's
// install step exactly as action.yml carries it, with a stand-in curl and
// uname on PATH, and checks the one download URL it builds for each way of
// naming a release.
//
// The version input used to go into that URL as typed, so "2.5.0" asked for a
// tag called 2.5.0, which does not exist, and the step failed with a 404 where
// install.sh and install.ps1, given the same string, installed. A release is
// the same release with or without its v in all three. The major tag is
// refused by name before anything is fetched: it is what `uses:` takes, and
// its release, where there is one, holds no binaries.
func TestTheActionInstallsTheReleaseItIsGivenWithOrWithoutItsV(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("the Action's steps run in bash, and there is none here")
	}
	step := newInstallStep(t)
	for _, tc := range []struct {
		name, version string
		installs      bool
	}{
		{"a release number", fakeVersion, true},
		{"a release tag", "v" + fakeVersion, true},
		{"the newest", "latest", true},
		{"the major tag", "v9", false},
		{"nothing at all", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := step.run(t, tc.version)
			if !tc.installs {
				run.checkRefused(t)
				return
			}
			run.checkInstalled(t, step.want)
		})
	}
}

// installStep is the Action's install step written out to run, with the
// stand-ins and the fake release it runs against.
type installStep struct {
	script, stubs, releases, archive string
	// want is the one download URL every accepted spelling must end at.
	want string
}

func newInstallStep(t *testing.T) installStep {
	t.Helper()
	work := t.TempDir()
	rel := buildFakeReleaseFor(t, "linux", "amd64")
	step := installStep{
		script:   filepath.Join(work, "install-step.sh"),
		stubs:    writeInstallStepStubs(t),
		releases: filepath.Join(work, "releases.json"),
		archive:  filepath.Join(work, rel.archiveName),
		want:     "https://github.com/jmrplens/ghchronicle/releases/download/v" + fakeVersion + "/" + rel.archiveName,
	}
	// The trap the latest branch exists for: a release named after the moving
	// major tag, newest by date, with nothing attached.
	listing := `[{"tag_name": "v9", "assets": []}, {"tag_name": "v` + fakeVersion + `"}, {"tag_name": "v9.9.8"}]`
	for path, body := range map[string][]byte{
		step.script:   []byte(actionStep(t, "Install ghchronicle")),
		step.releases: []byte(listing),
		step.archive:  rel.archive,
	} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return step
}

// installRun is what one run of the install step left behind.
type installRun struct {
	version, out, temp string
	code               int
	// fetched is every URL the stand-in curl was asked for, in order.
	fetched []string
	// onPath is what the step added to the job's PATH.
	onPath string
}

func (s installStep) run(t *testing.T, version string) installRun {
	t.Helper()
	temp := t.TempDir()
	fetched := filepath.Join(temp, "fetched")
	githubPath := filepath.Join(temp, "github_path")
	// Built up rather than passed to the call, as runInstaller does: the path
	// is this test's own temporary file either way.
	cmd := exec.CommandContext(t.Context(), "bash")
	cmd.Args = append(cmd.Args, s.script)
	cmd.Env = append(os.Environ(),
		"PATH="+s.stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"VERSION="+version,
		"RUNNER_TEMP="+temp,
		"GITHUB_PATH="+githubPath,
		"FETCHED="+fetched,
		"RELEASES="+s.releases,
		"ARCHIVE="+s.archive,
		"ARCHIVE_URL="+s.want)
	out, _ := cmd.CombinedOutput()
	asked, _ := os.ReadFile(fetched)
	onPath, _ := os.ReadFile(githubPath)
	return installRun{
		version: version, out: string(out), temp: temp,
		code:    cmd.ProcessState.ExitCode(),
		fetched: strings.Fields(string(asked)),
		onPath:  strings.TrimSpace(string(onPath)),
	}
}

// checkRefused holds a refusal to naming both spellings that would have
// worked, and to deciding before it fetched anything.
func (r installRun) checkRefused(t *testing.T) {
	t.Helper()
	if r.code == 0 {
		t.Fatalf("version %q installed something:\n%s", r.version, r.out)
	}
	for _, form := range []string{"2.5.1", "v2.5.1"} {
		if !strings.Contains(r.out, form) {
			t.Errorf("the refusal does not show the form %s that would have worked:\n%s", form, r.out)
		}
	}
	if len(r.fetched) != 0 {
		t.Errorf("it fetched after deciding the version names no release: %v", r.fetched)
	}
}

// checkInstalled holds a run to the one URL, the binary in RUNNER_TEMP and
// RUNNER_TEMP on the job's PATH.
func (r installRun) checkInstalled(t *testing.T, want string) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("version %q: exit %d, want 0\n%s\nfetched: %v", r.version, r.code, r.out, r.fetched)
	}
	if got := r.fetched[len(r.fetched)-1]; got != want {
		t.Errorf("version %q downloaded %s, want %s", r.version, got, want)
	}
	if _, err := os.Stat(filepath.Join(r.temp, "ghchronicle")); err != nil {
		t.Errorf("no binary in RUNNER_TEMP: %v\n%s", err, r.out)
	}
	if r.onPath != r.temp {
		t.Errorf("the job's PATH gained %q, want %s", r.onPath, r.temp)
	}
}

// writeInstallStepStubs puts a stand-in curl and uname on a PATH directory of
// their own. uname answers as a Linux x86_64 runner whatever this machine is.
// curl records every URL it is asked for to $FETCHED and answers the releases
// listing with $RELEASES, $ARCHIVE_URL with $ARCHIVE, and anything else with
// the 404 a tag that does not exist gets, to a file after -o or to stdout.
func writeInstallStepStubs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stubs := map[string]string{
		"uname": `#!/usr/bin/env bash
case "$1" in -s) echo Linux ;; -m) echo x86_64 ;; esac
`,
		"curl": `#!/usr/bin/env bash
out= url=
while [ $# -gt 0 ]; do
  case $1 in
    -o) out=$2; shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
printf '%s\n' "$url" >> "$FETCHED"
case $url in
  https://api.github.com/repos/jmrplens/ghchronicle/releases*) body=$RELEASES ;;
  "$ARCHIVE_URL") body=$ARCHIVE ;;
  *) echo "curl: (22) The requested URL returned error: 404" >&2; exit 22 ;;
esac
if [ -n "$out" ]; then cat "$body" > "$out"; else cat "$body"; fi
`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), stubMode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// actionStep is the script of one of the Action's steps, found by its name and
// returned exactly as action.yml carries it.
func actionStep(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("action.yml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Runs struct {
			Steps []struct {
				Name string `yaml:"name"`
				Run  string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"runs"`
	}
	if err = yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("action.yml: %v", err)
	}
	var names []string
	for _, step := range doc.Runs.Steps {
		if step.Name == name {
			return step.Run
		}
		names = append(names, step.Name)
	}
	t.Fatalf("action.yml has no step named %q; it has %q", name, names)
	return ""
}
