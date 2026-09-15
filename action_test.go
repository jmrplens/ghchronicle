package ghchronicle

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
