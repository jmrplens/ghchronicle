package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "cfg-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	// Checked, because a close that fails is a config file that never landed
	// and a test that would then read a truncated one.
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestLoadExpandsEnvAndFillsDefaults(t *testing.T) {
	t.Setenv("TEST_GH_TOKEN", "ghp_secret")
	c, err := Load(write(t, `
github:
  token: ${TEST_GH_TOKEN}
targets:
  user: jmrplens
sinks:
  stdout: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.GitHub.Token != "ghp_secret" {
		t.Errorf("token not expanded from the environment: %q", c.GitHub.Token)
	}
	if got, ok := c.Interval("traffic"); !ok || got != 6*time.Hour {
		t.Errorf("traffic default = %v, %v", got, ok)
	}
	if c.GitHub.ReserveRate == 0 {
		t.Error("reserve_rate should default to something non-zero")
	}
}

func TestZeroIntervalDisablesACollector(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	c, err := Load(write(t, `
targets: {user: jmrplens}
sinks: {stdout: true}
every:
  families:
    stats: 0s
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Interval("stats"); ok {
		t.Error("an interval of 0 must disable the collector, not run it constantly")
	}
	if _, ok := c.Interval("traffic"); !ok {
		t.Error("disabling one collector must not disable the others")
	}
}

func TestUnknownKeysAreRejected(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	// A typo that silently does nothing is worse than a startup failure.
	if _, err := Load(write(t, `
targets: {user: jmrplens}
sinks: {stdout: true}
every: {families: {nosuchfamily: 1h}}
`)); err == nil {
		t.Fatal("expected a typo in a collector name to fail")
	}
	// The layer names are fields of a struct, so the strict decoder refuses a
	// family written where a layer goes without this package having to look.
	if _, err := Load(write(t, `
targets: {user: jmrplens}
sinks: {stdout: true}
every: {default: 15m, families: {traffic: 6h}, nosuchlayer: 1h}
`)); err == nil {
		t.Fatal("expected an unknown layer in every to fail")
	}
}

// TestNoSinkIsAnError also holds the message to naming the one run that is
// allowed no destination. A reader who wanted exactly that, a card and nothing
// else, was told to add a sink, which is the opposite of what they asked for.
func TestNoSinkIsAnError(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	_, err := Load(write(t, "targets: {user: jmrplens}\n"))
	if err == nil {
		t.Fatal("a run with nowhere to write should not start")
	}
	// Both flags, because -card-only on its own draws nothing: it is the
	// waiver, and -card is the path it writes to.
	for _, flag := range []string{"-card ", "-card-only"} {
		if !strings.Contains(err.Error(), flag) {
			t.Errorf("err = %q, want it to name %s, without which the run that needs no destination cannot be spelled", err, flag)
		}
	}
}

// TestAnEmptyConfigurationFileSaysItIsEmpty: a file with nothing in it ends
// where it starts, and the decoder calls that the end of the file. "EOF" names
// neither the problem nor anything to do about it, and the configuration
// builder can write an empty file from an empty form.
func TestAnEmptyConfigurationFileSaysItIsEmpty(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	path := write(t, "")
	_, err := Load(path)
	if err == nil {
		t.Fatal("a configuration with nothing in it should not start")
	}
	if msg := err.Error(); !strings.HasPrefix(msg, path+": ") || !strings.Contains(msg, "the file is empty") {
		t.Errorf("err = %q, want the path and that the file is empty", msg)
	}
}

func TestParseSinceAcceptsWhatPeopleType(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"":           {},
		"unlimited":  {},
		"all":        {},
		"0":          {},
		"2024-01-01": time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		"90d":        now.AddDate(0, 0, -90),
		"2y":         now.AddDate(-2, 0, 0),
		"720h":       now.Add(-720 * time.Hour),
	}
	for in, want := range cases {
		got, err := parseSince(in, now)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("%q = %v, want %v", in, got, want)
		}
	}
	if _, err := parseSince("last tuesday", now); err == nil {
		t.Error("nonsense must be rejected, not silently unbounded")
	}
}

// TestABareNumberIsNotACountOfDaysOrYears keeps the two suffixes Go's parser
// does not know from swallowing input they were never written for. "90" with
// no unit is ambiguous, and reading it as ninety days or ninety years would
// turn a typo into a backfill of a lifetime; a suffix with more after it is a
// mixed duration Go refuses, and so must this.
func TestABareNumberIsNotACountOfDaysOrYears(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	for _, in := range []string{"90", "halfd", "1y2d", "twoy"} {
		if got, err := parseSince(in, now); err == nil {
			t.Errorf("%q resolved to %v, want it refused as not a date, a duration or a count", in, got)
		}
	}
}

// TestAMissingTokenIsRefusedAtStartUp: every request this tool makes needs
// one, so a config that has none must fail before the first sweep rather
// than on it, and the message names both places a token can come from.
func TestAMissingTokenIsRefusedAtStartUp(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	_, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {stdout: true}\n"))
	if err == nil || !strings.Contains(err.Error(), "github.token is empty and GITHUB_TOKEN is unset") {
		t.Fatalf("err = %v, want it to name github.token and GITHUB_TOKEN", err)
	}
}

// TestAnInlineTokenWinsOverTheEnvironment: the environment is the fallback,
// so a token written in the file is the one used.
func TestAnInlineTokenWinsOverTheEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "from-env")
	c, err := Load(write(t, "github: {token: inline}\ntargets: {user: jmrplens}\nsinks: {stdout: true}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.GitHub.Token != "inline" {
		t.Errorf("token = %q, want the inline one", c.GitHub.Token)
	}
}

// TestAnyOneKindOfTargetIsEnough: an organization or a list of repositories
// is a complete answer to "what to collect" without a user, and naming none
// of the three is the one config with nothing to sweep.
func TestAnyOneKindOfTargetIsEnough(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	for _, targets := range []string{"{user: jmrplens}", "{orgs: [acme]}", "{repos: [acme/tool]}"} {
		if _, err := Load(write(t, "targets: "+targets+"\nsinks: {stdout: true}\n")); err != nil {
			t.Errorf("targets %s: %v", targets, err)
		}
	}
	_, err := Load(write(t, "targets: {}\nsinks: {stdout: true}\n"))
	if err == nil || !strings.Contains(err.Error(), "set at least one of user, orgs or repos") {
		t.Errorf("no target at all: err = %v, want it to say what to set", err)
	}
}

// TestTheLedgerLivesBesideTheStateFile pins where a sweep remembers itself.
// The ledger follows a state file that was moved, and neither default may
// overwrite a path the config chose.
func TestTheLedgerLivesBesideTheStateFile(t *testing.T) {
	cases := []struct{ body, state, dedupe string }{
		{"", "ghchronicle-state.json", "ghchronicle-state-written.bin"},
		{"state_file: /var/lib/g/state.json\n", "/var/lib/g/state.json", "/var/lib/g/state-written.bin"},
		{"state_file: s.json\nsinks: {stdout: true, dedupe_file: \"off\"}\n", "s.json", "off"},
	}
	for _, tc := range cases {
		t.Setenv("GITHUB_TOKEN", "x")
		body := "targets: {user: jmrplens}\n" + tc.body
		if !strings.Contains(tc.body, "sinks:") {
			body += "sinks: {stdout: true}\n"
		}
		c, err := Load(write(t, body))
		if err != nil {
			t.Fatalf("%q: %v", tc.body, err)
		}
		if c.StateFile != tc.state || c.Sinks.DedupeFile != tc.dedupe {
			t.Errorf("%q: state_file = %q, dedupe_file = %q, want %q and %q",
				tc.body, c.StateFile, c.Sinks.DedupeFile, tc.state, tc.dedupe)
		}
	}
}

// TestAConfigFileThatCannotBeReadSaysSo: a wrong path is the commonest
// start-up failure there is, and it must not read as an empty config.
func TestAConfigFileThatCannotBeReadSaysSo(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want it to report the file does not exist", err)
	}
}

// TestMalformedYAMLIsReportedAsItselfWithItsPath: the migration hint answers
// only a file that is valid YAML in the old shape, so a file that is not YAML
// at all gets the parser's own words, prefixed with the file they are about.
func TestMalformedYAMLIsReportedAsItselfWithItsPath(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	path := write(t, "every: [traffic: 6h\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected malformed YAML to be refused")
	}
	if msg := err.Error(); !strings.HasPrefix(msg, path+": ") || strings.Contains(msg, "three layers") {
		t.Errorf("err = %q, want the path and the parser's error, not the migration hint", msg)
	}
}

// TestGrafanaSettingsExpandFromTheEnvironment. Expansion is per field, so a
// field nobody remembered to name carries the reference through as text and
// the server is handed the characters ${GRAFANA_TOKEN} as a credential. It
// happened: the first run of -publish-dashboard against a real server was
// refused with "Invalid API key" for exactly that reason.
func TestGrafanaSettingsExpandFromTheEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("TEST_GRAFANA_TOKEN", "glsa_secret")
	t.Setenv("TEST_DS_URL", "http://influxdb:8181")
	t.Setenv("TEST_DS_UID", "adopted")
	t.Setenv("TEST_LOKI_UID", "logs")
	t.Setenv("TEST_GRAFANA_URL", "http://grafana:3000")
	c, err := Load(write(t, `
targets: {user: jmrplens}
sinks: {stdout: true}
grafana:
  url: ${TEST_GRAFANA_URL}
  token: ${TEST_GRAFANA_TOKEN}
  datasource:
    url: ${TEST_DS_URL}
    uid: ${TEST_DS_UID}
    loki_uid: ${TEST_LOKI_UID}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ field, got, want string }{
		{"url", c.Grafana.URL, "http://grafana:3000"},
		{"token", c.Grafana.Token, "glsa_secret"},
		{"datasource.url", c.Grafana.Datasource.URL, "http://influxdb:8181"},
		{"datasource.uid", c.Grafana.Datasource.UID, "adopted"},
		{"datasource.loki_uid", c.Grafana.Datasource.LokiUID, "logs"},
	} {
		if tc.got != tc.want {
			t.Errorf("grafana.%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
}

// TestGrafanaTokenFallsBackToTheEnvironment, the way the GitHub one does, so a
// service unit can keep the credential out of the file entirely.
func TestGrafanaTokenFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("GRAFANA_TOKEN", "from-the-unit")
	c, err := Load(write(t, `
targets: {user: jmrplens}
sinks: {stdout: true}
grafana:
  url: http://grafana:3000
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Grafana.Token != "from-the-unit" {
		t.Errorf("token = %q, want the one the environment carries", c.Grafana.Token)
	}
}

// TestNoGrafanaSectionStaysNil: the whole feature is opt-in, and a config
// without the key must not grow a Grafana it never asked for.
func TestNoGrafanaSectionStaysNil(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("GRAFANA_TOKEN", "from-the-unit")
	c, err := Load(write(t, `
targets: {user: jmrplens}
sinks: {stdout: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Grafana != nil {
		t.Errorf("grafana = %+v, want nothing at all", c.Grafana)
	}
}

// TestEveryPathSettingExpandsTheHomeAndTheEnvironment: a path setting means
// what a shell would make of it. Until 2.6.1 only credentials and addresses
// were expanded, so `state_file: ~/.ghchronicle/state.json` was a directory
// called ~ under the working directory, the cache recipe the Actions page gave
// cached nothing, and the Windows page's ${LOCALAPPDATA} paths were folders
// named after the reference. The files derived from the state file follow it,
// and the two sentinels, "off" and "-", are left alone.
func TestEveryPathSettingExpandsTheHomeAndTheEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	home := t.TempDir()
	// Both, so the test means the same on Windows, where the home directory
	// is USERPROFILE and HOME is nobody's.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("TEST_GHC_DIR", "/srv/ghc")

	c, err := Load(write(t, `
targets: {user: jmrplens}
state_file: ~/.ghchronicle/state.json
log: {file: "${TEST_GHC_DIR}/ghchronicle.log"}
sinks:
  file: {path: ~/points.lp}
  sql: {path: "${TEST_GHC_DIR}/points.sql"}
`))
	if err != nil {
		t.Fatal(err)
	}
	for key, pair := range map[string][2]string{
		"state_file":              {c.StateFile, home + "/.ghchronicle/state.json"},
		"the derived dedupe_file": {c.Sinks.DedupeFile, home + "/.ghchronicle/state-written.bin"},
		"the derived cache file":  {c.CacheFile(), home + "/.ghchronicle/state-cache.bin"},
		"the derived checkpoint":  {c.BackfillProgressFile(), home + "/.ghchronicle/state-progress.json"},
		"log.file":                {c.Log.File, "/srv/ghc/ghchronicle.log"},
		"sinks.file.path":         {c.Sinks.File.Path, home + "/points.lp"},
		"sinks.sql.path":          {c.Sinks.SQL.Path, "/srv/ghc/points.sql"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", key, pair[0], pair[1])
		}
	}

	c, err = Load(write(t, `
targets: {user: jmrplens}
state_file: "${TEST_GHC_DIR}/state.json"
log: {file: ~/ghchronicle.log}
sinks:
  dedupe_file: ~/written.bin
  file: {path: "${TEST_GHC_DIR}/points.lp"}
  sql: {path: ~/points.sql}
`))
	if err != nil {
		t.Fatal(err)
	}
	for key, pair := range map[string][2]string{
		"state_file":        {c.StateFile, "/srv/ghc/state.json"},
		"sinks.dedupe_file": {c.Sinks.DedupeFile, home + "/written.bin"},
		"log.file":          {c.Log.File, home + "/ghchronicle.log"},
		"sinks.file.path":   {c.Sinks.File.Path, "/srv/ghc/points.lp"},
		"sinks.sql.path":    {c.Sinks.SQL.Path, home + "/points.sql"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", key, pair[0], pair[1])
		}
	}

	// ~name is another account's home in a shell, which this does not guess
	// at, and the sentinels are words rather than paths.
	c, err = Load(write(t, `
targets: {user: jmrplens}
state_file: ~other/state.json
sinks:
  dedupe_file: "off"
  sql: {path: "-"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.StateFile != "~other/state.json" || c.Sinks.DedupeFile != "off" || c.Sinks.SQL.Path != "-" {
		t.Errorf("state_file = %q, dedupe_file = %q, sql.path = %q, want each as written",
			c.StateFile, c.Sinks.DedupeFile, c.Sinks.SQL.Path)
	}
}

// TestAnUnsetVariableInAPathIsRefusedNamingTheKey: expanded to nothing,
// `state_file: ${GHC_REVIEW_UNSET_DIR}/state.json` was /state.json, and the
// ledger and the cache file went beside it: run as root, written at the root
// of the filesystem without a word, and `-backfill-status` looked for
// /state-progress.json. The ~ is refused for that reason already, and a
// variable that is set but empty leaves the same path.
func TestAnUnsetVariableInAPathIsRefusedNamingTheKey(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("GHC_TEST_EMPTY_DIR", "")
	// Registered with t.Setenv so it is put back, then unset for the test.
	t.Setenv("GHC_TEST_UNSET_DIR", "")
	if err := os.Unsetenv("GHC_TEST_UNSET_DIR"); err != nil {
		t.Fatal(err)
	}
	for key, body := range map[string]string{
		"state_file":        "state_file: ${GHC_TEST_UNSET_DIR}/state.json\nsinks: {stdout: true}\n",
		"sinks.dedupe_file": "sinks: {stdout: true, dedupe_file: \"${GHC_TEST_UNSET_DIR}/w.bin\"}\n",
		"log.file":          "log: {file: \"${GHC_TEST_EMPTY_DIR}/g.log\"}\nsinks: {stdout: true}\n",
		"sinks.file.path":   "sinks: {file: {path: \"${GHC_TEST_UNSET_DIR}/p.lp\"}}\n",
		"sinks.sql.path":    "sinks: {sql: {path: \"${GHC_TEST_EMPTY_DIR}/p.sql\"}}\n",
	} {
		_, err := Load(write(t, "targets: {user: jmrplens}\n"+body))
		if err == nil || !strings.Contains(err.Error(), key+": ") ||
			!strings.Contains(err.Error(), "is unset") && !strings.Contains(err.Error(), "is empty") {
			t.Errorf("%s naming a variable with no value: err = %v, want it refused naming the key", key, err)
		}
	}
}

// TestATildeWithNoHomeIsRefusedNamingTheKey: a service account with no home
// directory would otherwise get a directory called ~ wherever it started,
// which is the last place anybody reading the configuration would look.
func TestATildeWithNoHomeIsRefusedNamingTheKey(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	for key, body := range map[string]string{
		"state_file":        "state_file: ~/state.json\nsinks: {stdout: true}\n",
		"sinks.dedupe_file": "sinks: {stdout: true, dedupe_file: ~/w.bin}\n",
		"log.file":          "log: {file: ~/g.log}\nsinks: {stdout: true}\n",
		"sinks.file.path":   "sinks: {file: {path: ~/p.lp}}\n",
		"sinks.sql.path":    "sinks: {sql: {path: ~/p.sql}}\n",
	} {
		_, err := Load(write(t, "targets: {user: jmrplens}\n"+body))
		if err == nil || !strings.Contains(err.Error(), key+": ") || !strings.Contains(err.Error(), "home directory") {
			t.Errorf("%s with no home: err = %v, want it refused naming the key", key, err)
		}
	}
}

// TestMigrateIsAutoUnlessToldToWarn: an empty migrate is auto, the two
// words are taken as written, and anything else is refused naming both,
// because a misspelled word read as the default would let a start change a
// store its reader meant to leave alone.
func TestMigrateIsAutoUnlessToldToWarn(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	for _, tc := range []struct {
		body, want string
		own        bool
	}{
		{"", MigrateAuto, true},
		{"migrate: auto\n", MigrateAuto, true},
		{"migrate: warn\n", MigrateWarn, false},
	} {
		c, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {stdout: true}\n"+tc.body))
		if err != nil {
			t.Fatalf("%q: %v", tc.body, err)
		}
		if c.Migrate != tc.want || c.MigratesOnItsOwn() != tc.own {
			t.Errorf("%q: migrate = %q, on its own = %v, want %q and %v",
				tc.body, c.Migrate, c.MigratesOnItsOwn(), tc.want, tc.own)
		}
	}
	for _, word := range []string{"off", "always", "yes"} {
		_, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {stdout: true}\nmigrate: "+word+"\n"))
		if err == nil || !strings.Contains(err.Error(), `migrate: "`+word+`" is not auto or warn`) {
			t.Errorf("migrate: %s = %v, want it refused naming auto and warn", word, err)
		}
	}
}

// TestTheLockLivesBesideTheStateFile: the lock is named after the state
// file, like every other file a run keeps, so two configurations that keep
// separate state files never wait for each other.
func TestTheLockLivesBesideTheStateFile(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "x")
	c, err := Load(write(t, "targets: {user: jmrplens}\nsinks: {stdout: true}\nstate_file: /var/lib/g/state.json\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.LockFile(); got != "/var/lib/g/state-lock" {
		t.Errorf("LockFile = %q, want /var/lib/g/state-lock", got)
	}
	if got := (&Config{}).LockFile(); got != "" {
		t.Errorf("with no state file LockFile = %q, want none", got)
	}
}
