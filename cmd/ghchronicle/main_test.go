package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/jmrplens/ghchronicle"
	"github.com/jmrplens/ghchronicle/internal/config"
)

// TestResolveBuild verifies the precedence the release depends on: a value
// stamped with -ldflags always wins, an unstamped version falls back to the
// VERSION file the root package embeds rather than to "dev", and an unstamped
// commit and date come from the VCS stamps the toolchain records in any build
// made inside a checkout.
//
// The stamped-wins case is the one that matters most and the one nothing else
// can catch: a release names its version from the git tag, and if build info
// were ever allowed to overwrite it the published binary would report the
// VERSION file it was compiled with instead of the tag it was cut from, with
// no error anywhere.
func TestResolveBuild(t *testing.T) {
	buildInfo := func(settings ...debug.BuildSetting) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Settings: settings}, true
		}
	}
	vcs := buildInfo(
		debug.BuildSetting{Key: "vcs.revision", Value: "cafe1234"},
		debug.BuildSetting{Key: "vcs.time", Value: "2026-01-02T03:04:05Z"},
	)

	tests := []struct {
		name                              string
		ldVersion, ldCommit, ldDate       string
		readInfo                          func() (*debug.BuildInfo, bool)
		wantVersion, wantCommit, wantDate string
	}{
		{
			name:        "nothing stamped inside a checkout",
			readInfo:    vcs,
			wantVersion: ghchronicle.Version,
			wantCommit:  "cafe1234",
			wantDate:    "2026-01-02T03:04:05Z",
		},
		{
			name:        "stamped values are never overridden",
			ldVersion:   "9.9.9",
			ldCommit:    "deadbeef",
			ldDate:      "2020-12-31T23:59:59Z",
			readInfo:    vcs,
			wantVersion: "9.9.9",
			wantCommit:  "deadbeef",
			wantDate:    "2020-12-31T23:59:59Z",
		},
		{
			name:        "a stamped version still takes the commit and date from VCS",
			ldVersion:   "9.9.9",
			readInfo:    vcs,
			wantVersion: "9.9.9",
			wantCommit:  "cafe1234",
			wantDate:    "2026-01-02T03:04:05Z",
		},
		{
			name:        "build info carrying no VCS settings",
			readInfo:    buildInfo(debug.BuildSetting{Key: "-trimpath", Value: "true"}),
			wantVersion: ghchronicle.Version,
		},
		{
			name:        "unavailable build info, which is what go run gets",
			readInfo:    func() (*debug.BuildInfo, bool) { return nil, false },
			wantVersion: ghchronicle.Version,
		},
		{
			name:        "nil build info returned with ok true",
			readInfo:    func() (*debug.BuildInfo, bool) { return nil, true },
			wantVersion: ghchronicle.Version,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, c, d := resolveBuild(tt.ldVersion, tt.ldCommit, tt.ldDate, tt.readInfo)
			if v != tt.wantVersion {
				t.Errorf("version = %q, want %q", v, tt.wantVersion)
			}
			if c != tt.wantCommit {
				t.Errorf("commit = %q, want %q", c, tt.wantCommit)
			}
			if d != tt.wantDate {
				t.Errorf("date = %q, want %q", d, tt.wantDate)
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

// TestBuildLine verifies the single line -version prints, in both the shape a
// release produces and the shape an unstamped build does. The release
// workflow's post-publish smoke test matches on the "ghchronicle <version> "
// prefix, so the leading two fields are a contract, not a formatting choice.
func TestBuildLine(t *testing.T) {
	oldVersion, oldCommit, oldDate := version, commit, date
	t.Cleanup(func() { version, commit, date = oldVersion, oldCommit, oldDate })

	version, commit, date = "1.2.3", "abc1234", "2026-01-02T03:04:05Z"
	if got, want := buildLine(), "ghchronicle 1.2.3 (commit abc1234, built 2026-01-02T03:04:05Z)"; got != want {
		t.Errorf("buildLine() = %q, want %q", got, want)
	}

	version, commit, date = "1.2.3", "", ""
	if got, want := buildLine(), "ghchronicle 1.2.3 (commit unknown, built unknown)"; got != want {
		t.Errorf("buildLine() with nothing to report = %q, want %q", got, want)
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

// TestTheFirstSweepIsPrimedOnlyForAServingExporter keeps the priming sweep to
// the one run that needs it. An exporter holds nothing until a sweep fills it,
// so a serving run with one primes unless told not to; a store that is pushed
// to already has what it collected, and -once or -card is a full sweep anyway.
// Each clause is its own case, because any one of them read the other way
// primes a run that should not be, or leaves an exporter empty for a cadence.
func TestTheFirstSweepIsPrimedOnlyForAServingExporter(t *testing.T) {
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
		name  string
		cfg   *config.Config
		o     options
		prime bool
	}{
		{"a serving exporter", exporter(false), options{}, true},
		{"a serving exporter told not to prime", exporter(true), options{}, false},
		{"a serving run with no exporter", pushOnly, options{}, false},
		{"an exporter under -once", exporter(false), options{once: true}, false},
		{"an exporter under -card", exporter(false), options{card: "card.svg"}, false},
		{"no exporter under -once", pushOnly, options{once: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRunner(tc.cfg, nil, nil, logger, &tc.o)
			if r.Prime != tc.prime {
				t.Errorf("Prime = %v, want %v", r.Prime, tc.prime)
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
