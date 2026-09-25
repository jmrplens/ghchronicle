package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle/v2"
	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// TestResolveBuild verifies the precedence the release depends on: a value
// stamped with -ldflags always wins, an unstamped version falls back to the
// VERSION file the root package embeds rather than to "dev", an unstamped
// commit and date come from the VCS stamps the toolchain records in any build
// made inside a checkout, and only when there is no commit at all does the
// main module's version stand in for one.
//
// The stamped-wins case is the one that matters most and the one nothing else
// can catch: a release names its version from the git tag, and if build info
// were ever allowed to overwrite it the published binary would report the
// VERSION file it was compiled with instead of the tag it was cut from, with
// no error anywhere.
func TestResolveBuild(t *testing.T) {
	buildInfo := func(mainVersion string, settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{
				GoVersion: "go1.27.1",
				Main:      debug.Module{Path: "github.com/jmrplens/ghchronicle/v2", Version: mainVersion},
				Settings:  settings,
			}, true
		}
	}
	vcs := buildInfo("v2.5.0+dirty",
		debug.BuildSetting{Key: "vcs.revision", Value: "cafe1234"},
		debug.BuildSetting{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
	)
	download := buildInfo("v2.5.1")

	tests := []struct {
		name                        string
		ldVersion, ldCommit, ldDate string
		readInfo                    func() (*debug.BuildInfo, bool)
		want                        buildFacts
	}{
		{
			name:     "nothing stamped inside a checkout",
			readInfo: vcs,
			want:     buildFacts{version: ghchronicle.Version, commit: "cafe1234", date: "2026-01-02T03:04:05Z"},
		},
		{
			name:      "stamped values are never overridden",
			ldVersion: "9.9.9",
			ldCommit:  "deadbeef",
			ldDate:    "2020-12-31T23:59:59Z",
			readInfo:  vcs,
			want:      buildFacts{version: "9.9.9", commit: "deadbeef", date: "2020-12-31T23:59:59Z"},
		},
		{
			name:      "a stamped version still takes the commit and date from VCS",
			ldVersion: "9.9.9",
			readInfo:  vcs,
			want:      buildFacts{version: "9.9.9", commit: "cafe1234", date: "2026-01-02T03:04:05Z"},
		},
		{
			name:     "a module download names its version and the Go that built it",
			readInfo: download,
			want:     buildFacts{version: ghchronicle.Version, module: "v2.5.1", toolchain: "go1.27.1"},
		},
		{
			name:     "a pseudo-version is reported as the go command wrote it",
			readInfo: buildInfo("v2.5.2-0.20260926101500-0123456789ab"),
			want: buildFacts{
				version: ghchronicle.Version, module: "v2.5.2-0.20260926101500-0123456789ab", toolchain: "go1.27.1",
			},
		},
		{
			name:      "a stamped version leaves the module to say where the source came from",
			ldVersion: "9.9.9",
			readInfo:  download,
			want:      buildFacts{version: "9.9.9", module: "v2.5.1", toolchain: "go1.27.1"},
		},
		{
			name:     "a stamped commit makes the module version redundant",
			ldCommit: "deadbeef",
			readInfo: download,
			want:     buildFacts{version: ghchronicle.Version, commit: "deadbeef"},
		},
		{
			name: "a module download whose build info has no Go version",
			readInfo: func() (*debug.BuildInfo, bool) {
				return &debug.BuildInfo{Main: debug.Module{Version: "v2.5.1"}}, true
			},
			want: buildFacts{version: ghchronicle.Version, module: "v2.5.1", toolchain: runtime.Version()},
		},
		{
			name:     "a devel main module and no VCS, which is go run and -buildvcs=false",
			readInfo: buildInfo("(devel)", debug.BuildSetting{Key: "-trimpath", Value: "true"}),
			want:     buildFacts{version: ghchronicle.Version},
		},
		{
			name:     "no main module version at all",
			readInfo: buildInfo(""),
			want:     buildFacts{version: ghchronicle.Version},
		},
		{
			name:     "unavailable build info",
			readInfo: func() (*debug.BuildInfo, bool) { return nil, false },
			want:     buildFacts{version: ghchronicle.Version},
		},
		{
			name:     "nil build info returned with ok true",
			readInfo: func() (*debug.BuildInfo, bool) { return nil, true },
			want:     buildFacts{version: ghchronicle.Version},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveBuild(tt.ldVersion, tt.ldCommit, tt.ldDate, tt.readInfo); got != tt.want {
				t.Errorf("resolveBuild() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestVersionIsTheTrimmedVersionFile verifies the embedded floor is the file's
// contents without its trailing newline. VERSION ends in one, every reference
// repository's does, and an untrimmed value would put a line break in the
// middle of every -version line, every OCI label and every image tag built
// from a Makefile that reads the file.
func TestVersionIsTheTrimmedVersionFile(t *testing.T) {
	if ghchronicle.Version == "" {
		t.Fatal("embedded Version is empty; VERSION did not reach the build")
	}
	if strings.TrimSpace(ghchronicle.Version) != ghchronicle.Version {
		t.Errorf("embedded Version %q carries surrounding whitespace", ghchronicle.Version)
	}
}

// TestBuildLine pins the line -version prints for every way this binary is
// built, from the build information each one really records (read with
// `go version -m` on 2026-09-25) to the exact text. The stamped shapes are
// what the release, the Makefile and the Dockerfile produce and must never
// move; the module shape is what `go install ...@version` produces, which
// said "commit unknown, built unknown" until 2.5.1 although it knows which
// version it is. The release workflow's post-publish smoke test and CI's
// image smoke test match on the "ghchronicle <version> " prefix, so every
// shape keeps it.
func TestBuildLine(t *testing.T) {
	const (
		rev     = "30148b1b9d3389e49624e549a2e5d26691b2bed0"
		revTime = "2026-09-25T09:24:02Z"
	)
	checkout := func(mainVersion, modified string) *debug.BuildInfo {
		return &debug.BuildInfo{
			GoVersion: "go1.27.1",
			Main:      debug.Module{Path: "github.com/jmrplens/ghchronicle/v2", Version: mainVersion},
			Settings: []debug.BuildSetting{
				{Key: "-trimpath", Value: "true"},
				{Key: "vcs", Value: "git"},
				{Key: "vcs.revision", Value: rev},
				{Key: "vcs.time", Value: revTime},
				{Key: "vcs.modified", Value: modified},
			},
		}
	}
	// What the go command records for a main module built without VCS stamps:
	// `go run`, `go build -buildvcs=false`, and a Docker context without .git.
	devel := &debug.BuildInfo{
		GoVersion: "go1.27.1",
		Main:      debug.Module{Path: "github.com/jmrplens/ghchronicle/v2", Version: "(devel)"},
		Settings:  []debug.BuildSetting{{Key: "-trimpath", Value: "true"}},
	}
	download := func(v string) *debug.BuildInfo {
		return &debug.BuildInfo{
			GoVersion: "go1.27.1",
			Main: debug.Module{
				Path:    "github.com/jmrplens/ghchronicle/v2",
				Version: v,
				Sum:     "h1:V5rCtRv1GdtWMKlbWfo9WU/840pV8BpadvhXVOkxsJ0=",
			},
			Settings: []debug.BuildSetting{{Key: "CGO_ENABLED", Value: "1"}},
		}
	}

	tests := []struct {
		name                        string
		ldVersion, ldCommit, ldDate string
		info                        *debug.BuildInfo
		want                        string
	}{
		{
			name:      "a release, stamped by GoReleaser from a clean checkout",
			ldVersion: "2.5.1", ldCommit: "30148b1", ldDate: revTime,
			info: checkout("v2.5.1", "false"),
			want: "ghchronicle 2.5.1 (commit 30148b1, built 2026-09-25T09:24:02Z)",
		},
		{
			name:      "make build, stamped from a tree with local changes",
			ldVersion: "2.5.1", ldCommit: "30148b1", ldDate: revTime,
			info: checkout("v2.5.1+dirty", "true"),
			want: "ghchronicle 2.5.1 (commit 30148b1, built 2026-09-25T09:24:02Z)",
		},
		{
			name:      "the Dockerfile with its build arguments, whose context has no .git",
			ldVersion: "ci", ldCommit: rev, ldDate: revTime,
			info: devel,
			want: "ghchronicle ci (commit " + rev + ", built 2026-09-25T09:24:02Z)",
		},
		{
			name: "go install of a tag, through the module proxy",
			info: download("v2.5.1"),
			want: "ghchronicle " + ghchronicle.Version + " (module v2.5.1, built with go1.27.1)",
		},
		{
			name: "go install of a branch, which the go command names by pseudo-version",
			info: download("v2.5.2-0.20260926101500-0123456789ab"),
			want: "ghchronicle " + ghchronicle.Version +
				" (module v2.5.2-0.20260926101500-0123456789ab, built with go1.27.1)",
		},
		{
			name: "go build inside a checkout, unstamped",
			info: checkout("v2.5.0+dirty", "true"),
			want: "ghchronicle " + ghchronicle.Version + " (commit " + rev + ", built 2026-09-25T09:24:02Z)",
		},
		{
			name: "a build that recorded nothing: go run, -buildvcs=false, the bare Dockerfile",
			info: devel,
			want: "ghchronicle " + ghchronicle.Version + " (commit unknown, built unknown)",
		},
		{
			name: "a binary with no build information at all",
			want: "ghchronicle " + ghchronicle.Version + " (commit unknown, built unknown)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := resolveBuild(tt.ldVersion, tt.ldCommit, tt.ldDate, func() (*debug.BuildInfo, bool) {
				return tt.info, tt.info != nil
			})
			got := b.line()
			if got != tt.want {
				t.Errorf("line() = %q, want %q", got, tt.want)
			}
			if prefix := "ghchronicle " + b.version + " "; !strings.HasPrefix(got, prefix) {
				t.Errorf("line() = %q, which the smoke tests cannot match: it does not start with %q", got, prefix)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("line() = %q spans more than one line", got)
			}
		})
	}
}

// TestGroupsFlagListsEveryGroupAndItsFamilies keeps `-groups` the
// authoritative list, so the documentation never has to repeat thirty-two
// names and cannot come to disagree with the binary about them.
func TestGroupsFlagListsEveryGroupAndItsFamilies(t *testing.T) {
	var out bytes.Buffer
	printGroups(&out)
	printed := out.String()
	for _, group := range config.Groups() {
		if !strings.Contains(printed, group) {
			t.Errorf("group %q is missing from -groups", group)
		}
		if desc := config.GroupDescription(group); !strings.Contains(printed, desc) {
			t.Errorf("group %q is printed without its description", group)
		}
	}
	// Checked as the whole line rather than name by name: a family name that
	// happens to be a substring of some group's prose would otherwise pass
	// this without ever being listed.
	for _, group := range config.Groups() {
		if members := strings.Join(config.FamiliesIn(group), ", "); !strings.Contains(printed, members) {
			t.Errorf("group %q does not list its families %q", group, members)
		}
	}
	for _, family := range config.Families() {
		if _, known := config.GroupOf(family); !known {
			t.Errorf("family %q is not in the table -groups prints from", family)
		}
	}
}

// TestTheRunTheFlagsAskForIsTheRunnerTheyBuild holds every field newRunner
// decides from the command line.
//
// Prime is for the one run that needs it: an exporter holds nothing until a
// sweep fills it, so a serving run with one primes unless told not to; a store
// that is pushed to already has what it collected, and -once starts no
// exporter to fill. Card is what makes a card's sweep collect every family,
// whatever the cadences say, since every number on the card comes from that
// sweep alone; -once has no such claim on the families and does not prime.
// CardOnly is the run whose points reach the card and no store, which is the
// one that may write nothing to the state file.
//
// Each clause is its own case, because any one of them read the other way
// primes a run that should not be, leaves an exporter empty for a cadence,
// draws a card of zeros, or lets a run that delivered nothing tell the next
// collection that a family is done.
func TestTheRunTheFlagsAskForIsTheRunnerTheyBuild(t *testing.T) {
	exporter := func(noPrime bool) *config.Config {
		return &config.Config{
			StateFile: filepath.Join(t.TempDir(), "state.json"),
			Sinks:     config.Sinks{Prometheus: &config.PrometheusSink{NoPrime: noPrime}},
		}
	}
	pushOnly := &config.Config{
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Sinks:     config.Sinks{Stdout: true},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tc := range []struct {
		name                  string
		cfg                   *config.Config
		o                     options
		prime, card, cardOnly bool
	}{
		{name: "a serving exporter", cfg: exporter(false), prime: true},
		{name: "a serving exporter told not to prime", cfg: exporter(true)},
		{name: "a serving run with no exporter", cfg: pushOnly},
		{name: "an exporter under -once", cfg: exporter(false), o: options{once: true}},
		{
			name: "an exporter under -card", cfg: exporter(false),
			o: options{card: "card.svg"}, card: true,
		},
		{name: "no exporter under -once", cfg: pushOnly, o: options{once: true}},
		{
			name: "-card with the sinks", cfg: pushOnly,
			o: options{card: "card.svg"}, card: true,
		},
		{
			name: "-card -card-only", cfg: pushOnly,
			o: options{card: "card.svg", cardOnly: true}, card: true, cardOnly: true,
		},
		{
			// -card-only without -card writes no card and is not a card run:
			// the flag alone must not stop the state file being saved.
			name: "-once, and -card-only without a card", cfg: pushOnly,
			o: options{once: true, cardOnly: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRunner(tc.cfg, nil, nil, logger, &tc.o)
			if r.Prime != tc.prime {
				t.Errorf("Prime = %v, want %v", r.Prime, tc.prime)
			}
			if r.Card != tc.card {
				t.Errorf("Card = %v, want %v", r.Card, tc.card)
			}
			if r.CardOnly != tc.cardOnly {
				t.Errorf("CardOnly = %v, want %v", r.CardOnly, tc.cardOnly)
			}
		})
	}
}

// TestTheStartUpLineCountsTheFamiliesThatRun holds the "families" attribute
// of the start-up line to the families the selected groups hold. It is the
// one number on that line an operator can check against -groups, so a count
// that ran the wrong way would read as a configuration error that is not
// there.
func TestTheStartUpLineCountsTheFamiliesThatRun(t *testing.T) {
	line := startUpNote(t)
	want := fmt.Sprintf(`families="%d of %d"`, len(config.FamiliesIn("audience")), len(config.Families()))
	if len(config.FamiliesIn("audience")) == 0 {
		t.Fatal("the audience group holds no families, so this test proves nothing")
	}
	if !strings.Contains(line, want) {
		t.Errorf("the start-up line lacks %s:\n%s", want, line)
	}
}

// TestBothThemesNameTheDarkCardTheWayAPictureElementReadsIt pins the file
// names -card-theme both writes: the path as given for the light card and the
// same name with _dark before the extension for the dark one, which is the
// pair the <picture> in the documentation points at.
func TestBothThemesNameTheDarkCardTheWayAPictureElementReadsIt(t *testing.T) {
	for _, tc := range []struct {
		path, theme string
		want        []cardFile
	}{
		{"card.svg", "dark", []cardFile{{path: "card.svg", theme: "dark"}}},
		{"out/card.svg", "both", []cardFile{{path: "out/card.svg", theme: "light"}, {path: "out/card_dark.svg", theme: "dark"}}},
		{"profile", "both", []cardFile{{path: "profile", theme: "light"}, {path: "profile_dark", theme: "dark"}}},
		{"a.b/card.v2.svg", "both", []cardFile{{path: "a.b/card.v2.svg", theme: "light"}, {path: "a.b/card.v2_dark.svg", theme: "dark"}}},
	} {
		if got := cardFiles(tc.path, tc.theme); !slices.Equal(got, tc.want) {
			t.Errorf("cardFiles(%q, %q) = %v, want %v", tc.path, tc.theme, got, tc.want)
		}
	}
}
