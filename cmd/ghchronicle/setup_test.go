package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The guided setup, driven from both ends: the answers go in as a string and
// what the person would have seen comes out as one.

// githubStub answers the one question the setup asks GitHub, which is who the
// token belongs to.
func githubStub(t *testing.T, login string, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		_, _ = w.Write([]byte(`{"login":"` + login + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// converse runs the setup with a scripted set of answers.
func converse(t *testing.T, answers, path, base string) (string, error) {
	t.Helper()
	var said strings.Builder
	ask := newAsker(strings.NewReader(answers), &said)
	err := runSetup(t.Context(), ask, path, base, time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC))
	return said.String(), err
}

// TestTheGuidedSetupWritesAConfigurationThatLoads. The whole promise: answer
// the questions and what comes out is a file the parser takes, not a draft.
func TestTheGuidedSetupWritesAConfigurationThatLoads(t *testing.T) {
	base := githubStub(t, "octocat", http.StatusOK)
	path := filepath.Join(t.TempDir(), "config.yaml")
	// token, account (default), sink, address, database, token, dashboard? no,
	// service? no.
	said, err := converse(t, "ghp_atoken\n\n1\nhttp://influx:8181\ngithub\nitstoken\nn\nn\n", path, base)
	if err != nil {
		t.Fatalf("%v\n%s", err, said)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"token: ${GITHUB_TOKEN}",
		"user: octocat",
		"url: http://influx:8181",
		"bucket: github",
	} {
		if !strings.Contains(string(written), want) {
			t.Errorf("the file does not carry %q:\n%s", want, written)
		}
	}
	// A credential never reaches the file, whatever was typed.
	if strings.Contains(string(written), "ghp_atoken") ||
		strings.Contains(string(written), "itstoken") {
		t.Errorf("a credential was written into the configuration:\n%s", written)
	}
}

// TestATokenThatDoesNotWorkStopsThere, rather than writing a configuration
// whose first sweep is the thing that finds out.
func TestATokenThatDoesNotWorkStopsThere(t *testing.T) {
	base := githubStub(t, "", http.StatusUnauthorized)
	path := filepath.Join(t.TempDir(), "config.yaml")
	said, err := converse(t, "ghp_wrong\n", path, base)
	if err == nil {
		t.Fatalf("a token GitHub refused was accepted:\n%s", said)
	}
	if !strings.Contains(err.Error(), "did not work") {
		t.Errorf("err = %v, want it to say the token was the problem", err)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("it wrote a configuration despite the token being refused")
	}
}

// TestItSaysWhoTheTokenIs, because a reader who pasted the wrong one of three
// finds out here rather than from the data a week later.
func TestItSaysWhoTheTokenIs(t *testing.T) {
	base := githubStub(t, "someone-else", http.StatusOK)
	path := filepath.Join(t.TempDir(), "config.yaml")
	said, _ := converse(t, "ghp_atoken\n\n5\nn\n", path, base)
	if !strings.Contains(said, "belongs to someone-else") {
		t.Errorf("output = %q, want it to name the account the token is", said)
	}
}

// TestItWillNotReplaceAConfigurationWithoutBeingAsked.
func TestItWillNotReplaceAConfigurationWithoutBeingAsked(t *testing.T) {
	base := githubStub(t, "octocat", http.StatusOK)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("mine: keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// ... and the answer to "replace it?" is the default, which is no.
	_, err := converse(t, "ghp_atoken\n\n5\nn\n\n", path, base)
	if err == nil {
		t.Fatal("it replaced a configuration that was already there")
	}
	kept, _ := os.ReadFile(path)
	if string(kept) != "mine: keep\n" {
		t.Errorf("the file was changed anyway: %q", kept)
	}
}

// TestStdoutNeedsNoDashboard: the question is skipped for the two destinations
// Grafana cannot read, rather than asked and then ignored.
func TestStdoutNeedsNoDashboard(t *testing.T) {
	base := githubStub(t, "octocat", http.StatusOK)
	path := filepath.Join(t.TempDir(), "config.yaml")
	said, err := converse(t, "ghp_atoken\n\n5\nn\n", path, base)
	if err != nil {
		t.Fatalf("%v\n%s", err, said)
	}
	if strings.Contains(said, "Grafana") {
		t.Errorf("output = %q, want no dashboard question for stdout", said)
	}
}

// TestASecretIsNeverPrinted. Every other question shows its default so a
// person knows what enter will do; doing that for a credential would put the
// token already in the environment on the screen and into the scrollback of
// whatever window this ran in.
func TestASecretIsNeverPrinted(t *testing.T) {
	const secret = "ghp_the_one_already_in_the_environment"
	var said strings.Builder
	ask := newAsker(strings.NewReader("\n"), &said)
	got, err := ask.secret("  token", secret)
	if err != nil {
		t.Fatal(err)
	}
	if got != secret {
		t.Errorf("answer = %q, want the fallback kept on an empty line", got)
	}
	if strings.Contains(said.String(), secret) {
		t.Errorf("the prompt printed the secret: %q", said.String())
	}
	if !strings.Contains(said.String(), "keep the one already set") {
		t.Errorf("prompt = %q, want it to offer keeping it without showing it", said.String())
	}
}

// TestTheConfigurationNamesCredentialsRatherThanCarryingThem, for every
// destination that has one.
func TestTheConfigurationNamesCredentialsRatherThanCarryingThem(t *testing.T) {
	t.Parallel()
	for _, answers := range []setupAnswers{
		{User: "u", Sink: "influxdb", SinkURL: "http://influx:8181", Bucket: "github", SinkKey: "influxsecret"},
		{User: "u", Sink: "postgres", SinkURL: "postgres://user:pgsecret@host:5432/db"},
		{User: "u", Sink: "elasticsearch", SinkURL: "http://es:9200", SinkKey: "essecret"},
		{User: "u", Sink: "stdout", Dashboard: true, GrafanaURL: "http://g:3000", GrafanaToken: "grafanasecret"},
	} {
		written := setupConfig(answers, time.Now())
		for _, secret := range []string{"influxsecret", "pgsecret", "essecret", "grafanasecret"} {
			if strings.Contains(written, secret) {
				t.Errorf("%s configuration carries %s:\n%s", answers.Sink, secret, written)
			}
		}
		if !strings.Contains(written, "${GITHUB_TOKEN}") {
			t.Errorf("%s configuration does not read the token from the environment:\n%s",
				answers.Sink, written)
		}
	}
}

// TestTheCredentialsGoBesideTheConfigurationAndNotInIt, which is the other
// half: naming them is only useful if something writes them somewhere.
func TestTheCredentialsGoBesideTheConfigurationAndNotInIt(t *testing.T) {
	t.Parallel()
	env, hasAny := setupEnvironment(setupAnswers{
		Token: "ghp_token", Sink: "influxdb", SinkKey: "influxsecret",
		Dashboard: true, GrafanaToken: "grafanasecret",
	})
	if !hasAny {
		t.Fatal("nothing to write, with three credentials answered")
	}
	for _, want := range []string{
		"GITHUB_TOKEN=ghp_token",
		"INFLUX_TOKEN=influxsecret",
		"GRAFANA_TOKEN=grafanasecret",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("the environment file does not carry %q:\n%s", want, env)
		}
	}
}

// TestItWillNotReplaceAServiceThatIsAlreadyThere. A machine that already runs
// this has a unit at that path, and a guided setup run to try something out
// must not take down the thing it was being tried against. This one was
// learned by doing it: a test run as root overwrote a live unit, and the
// service stayed up only because systemd had the old one loaded.
func TestItWillNotReplaceAServiceThatIsAlreadyThere(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "ghchronicle.service")
	const mine = "[Service]\nExecStart=/usr/local/bin/ghchronicle -config /etc/ghchronicle/config.yaml\n"
	if err := os.WriteFile(existing, []byte(mine), serviceMode); err != nil {
		t.Fatal(err)
	}
	var said strings.Builder
	// The answer to "replace it?" is the default, which is no.
	ask := newAsker(strings.NewReader("\n"), &said)
	if err := writeServiceAt(ask, setupAnswers{Token: "t"},
		filepath.Join(dir, "config.yaml"), existing); err != nil {
		t.Fatal(err)
	}
	kept, _ := os.ReadFile(existing)
	if string(kept) != mine {
		t.Errorf("the unit was replaced anyway:\n%s", kept)
	}
	if !strings.Contains(said.String(), "Left it alone") {
		t.Errorf("output = %q, want it to say it did not touch it", said.String())
	}
}

// TestRunningOutOfAnswersSaysSo. A person pressing ctrl-D, or a script piped
// in with fewer lines than there are questions, used to get "EOF", which says
// neither what happened nor whether anything was written.
func TestRunningOutOfAnswersSaysSo(t *testing.T) {
	t.Parallel()
	var said strings.Builder
	ask := newAsker(strings.NewReader(""), &said)
	_, err := ask.text("something", "")
	if err == nil {
		t.Fatal("an answer was produced from nothing")
	}
	if !strings.Contains(err.Error(), "no answer") {
		t.Errorf("err = %v, want it to say there was no answer", err)
	}
	if strings.EqualFold(err.Error(), "EOF") {
		t.Error("the error is still the bare EOF")
	}
}

// TestEveryDestinationAsksForWhatItNeeds, and writes it where the parser looks
// for it. Each of these is a different shape of answer: a connection string, an
// address and a key, a path, and nothing at all.
func TestEveryDestinationAsksForWhatItNeeds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers string
		wants   []string
	}{
		{
			"postgres", "2\npostgres://u:p@db:5432/metrics\nn\nn\n",
			[]string{"postgres:", "dsn: ${DATABASE_URL}"},
		},
		{
			"elasticsearch", "3\nhttp://es:9200\nkey\nn\nn\n",
			[]string{"elasticsearch:", "url: http://es:9200", "api_key: ${ES_API_KEY}"},
		},
		{
			"a file", "4\n/tmp/points.lp\nn\n",
			[]string{"file:", "path: /tmp/points.lp"},
		},
		{
			"the screen", "5\nn\n",
			[]string{"stdout: true"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := githubStub(t, "octocat", http.StatusOK)
			path := filepath.Join(t.TempDir(), "config.yaml")
			said, err := converse(t, "ghp_atoken\n\n"+tc.answers, path, base)
			if err != nil {
				t.Fatalf("%v\n%s", err, said)
			}
			written, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			for _, want := range tc.wants {
				if !strings.Contains(string(written), want) {
					t.Errorf("the configuration does not carry %q:\n%s", want, written)
				}
			}
		})
	}
}

// TestTheDashboardIsAskedAboutAndWrittenIn, with Grafana checked on the way.
func TestTheDashboardIsAskedAboutAndWrittenIn(t *testing.T) {
	base := githubStub(t, "octocat", http.StatusOK)
	grafana := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"name":"Main Org."}`))
	}))
	t.Cleanup(grafana.Close)
	path := filepath.Join(t.TempDir(), "config.yaml")
	said, err := converse(t,
		"ghp_atoken\n\n1\nhttp://influx:8181\ngithub\ninfluxsecret\ny\n"+grafana.URL+"\ngrafanasecret\nn\n",
		path, base)
	if err != nil {
		t.Fatalf("%v\n%s", err, said)
	}
	if !strings.Contains(said, "it answers") {
		t.Errorf("output = %q, want it to say Grafana answered", said)
	}
	written, _ := os.ReadFile(path)
	for _, want := range []string{"grafana:", "publish_on_start: true", "token: ${GRAFANA_TOKEN}"} {
		if !strings.Contains(string(written), want) {
			t.Errorf("the configuration does not carry %q:\n%s", want, written)
		}
	}
	if strings.Contains(string(written), "grafanasecret") {
		t.Errorf("the Grafana token was written into the configuration:\n%s", written)
	}
}

// TestAQuestionWithNoDefaultIsAskedAgain rather than accepted empty, because
// the rest of the setup cannot go on without it.
func TestAQuestionWithNoDefaultIsAskedAgain(t *testing.T) {
	t.Parallel()
	var said strings.Builder
	ask := newAsker(strings.NewReader("\n\nfinally\n"), &said)
	answer, err := ask.required("what")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "finally" {
		t.Errorf("answer = %q, want the one that was not empty", answer)
	}
	if strings.Count(said.String(), "that one is needed") != 2 {
		t.Errorf("output = %q, want it to have said so for each empty answer", said.String())
	}
}

// TestTheServiceFileAndItsCredentialsLandWithTheRightPermissions. The unit is
// read by the init system and names nothing secret; the file beside it holds
// every credential and is readable by nobody else.
func TestTheServiceFileAndItsCredentialsLandWithTheRightPermissions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	unit := filepath.Join(dir, "ghchronicle.service")
	var said strings.Builder
	ask := newAsker(strings.NewReader(""), &said)
	err := writeServiceAt(ask, setupAnswers{Token: "ghp_token", Sink: "influxdb", SinkKey: "influxsecret"},
		filepath.Join(dir, "config.yaml"), unit)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{
		unit:                                  serviceMode,
		filepath.Join(dir, "ghchronicle.env"): 0o600,
	} {
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatalf("%s: %v", path, statErr)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s is %v, want %v", filepath.Base(path), info.Mode().Perm(), want)
		}
	}
	env, _ := os.ReadFile(filepath.Join(dir, "ghchronicle.env"))
	if !strings.Contains(string(env), "INFLUX_TOKEN=influxsecret") {
		t.Errorf("the credentials file does not carry the sink's token:\n%s", env)
	}
	if !strings.Contains(said.String(), "readable by you alone") {
		t.Errorf("output = %q, want it to say what it just wrote and who can read it", said.String())
	}
}
