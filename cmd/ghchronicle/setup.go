package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/grafana"
)

// The guided setup.
//
// It exists because the shortest honest description of getting started used to
// be: read four pages, write a YAML file by hand, find out at the first sweep
// whether the token has the right scopes, and then write a service unit. Every
// one of those steps is one this can do, and the last two are ones it can
// check rather than hope.
//
// So every answer is tried against the thing it names before the next question
// is asked. A token that cannot read the account says so here, not in an hour.
// A database nobody is listening on says so here, not as an empty dashboard.

// setupAnswers is what the conversation collects.
type setupAnswers struct {
	Token   string
	User    string
	Sink    string
	SinkURL string
	SinkKey string
	Bucket  string

	Dashboard    bool
	GrafanaURL   string
	GrafanaToken string

	Service bool
}

// setupSinks are the destinations the guided setup offers. The whole list is
// eleven and most of them are for somebody who already knows they want one, so
// this is the short list: the reference store, the two other databases a
// person is likely to already run, and the two that need nothing at all.
func setupSinks() []choice {
	return []choice{
		{"influxdb", "the reference store, and what the dashboard is built against"},
		{"postgres", "a PostgreSQL you already run"},
		{"elasticsearch", "an Elasticsearch or OpenSearch you already run"},
		{"file", "a file on disk, to look at before deciding"},
		{"stdout", "the screen, to see it work and nothing else"},
	}
}

// runSetup is the whole conversation. It takes its streams rather than the
// process's so a test can hold both ends of it.
func runSetup(ctx context.Context, ask *asker, path, apiBase string, now time.Time) error {
	ask.sayf("This writes a configuration that works, asking only what it cannot")
	ask.sayf("work out, and checking each answer before going on.")
	ask.sayf("")

	answers := setupAnswers{}
	if err := askGitHub(ctx, ask, &answers, apiBase); err != nil {
		return err
	}
	if err := askSink(ctx, ask, &answers); err != nil {
		return err
	}
	if err := askDashboard(ctx, ask, &answers); err != nil {
		return err
	}
	if err := askService(ask, &answers); err != nil {
		return err
	}
	return writeSetup(ask, answers, path, now)
}

// askGitHub takes the token and the account, and proves the first can see the
// second.
func askGitHub(ctx context.Context, ask *asker, answers *setupAnswers, apiBase string) error {
	ask.sayf("A GitHub token. Create one at https://github.com/settings/tokens")
	ask.sayf("with repo, read:org, read:user, read:packages, security_events,")
	ask.sayf("read:public_key and read:gpg_key.")
	token, err := ask.secret("  token", os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("a token is needed: everything this collects is behind one")
	}
	login, err := whoAmI(ctx, token, apiBase)
	if err != nil {
		return fmt.Errorf("that token did not work: %w", err)
	}
	ask.sayf("  the token belongs to %s.", login)
	answers.Token = token

	user, err := ask.text("Which account to collect", login)
	if err != nil {
		return err
	}
	answers.User = user
	ask.sayf("")
	return nil
}

// whoAmI asks GitHub who the token is, which is the cheapest question that
// proves it works at all.
func whoAmI(ctx context.Context, token, apiBase string) (string, error) {
	api := ghapi.New(token, 30*time.Second)
	if apiBase != "" {
		api.SetBaseURL(apiBase)
	}
	var who struct {
		Login string `json:"login"`
	}
	if _, _, err := api.GetJSON(ctx, "/user", &who, ""); err != nil {
		return "", err
	}
	if who.Login == "" {
		return "", errors.New("GitHub answered without a login, which a valid token never does")
	}
	return who.Login, nil
}

// askSink takes the destination and whatever it needs, and checks it answers.
func askSink(ctx context.Context, ask *asker, answers *setupAnswers) error {
	sink, err := ask.pick("Where should the numbers go?", setupSinks())
	if err != nil {
		return err
	}
	answers.Sink = sink
	switch sink {
	case "influxdb":
		if answers.SinkURL, err = ask.text("  its address", "http://localhost:8181"); err != nil {
			return err
		}
		if answers.Bucket, err = ask.text("  the database to write to", "github"); err != nil {
			return err
		}
		if answers.SinkKey, err = ask.secret("  its token", os.Getenv("INFLUX_TOKEN")); err != nil {
			return err
		}
		reach(ctx, ask, answers.SinkURL+"/health")
	case "postgres":
		if answers.SinkURL, err = ask.required(
			"  its connection string, postgres://user:pass@host:5432/db",
		); err != nil {
			return err
		}
	case "elasticsearch":
		if answers.SinkURL, err = ask.text("  its address", "http://localhost:9200"); err != nil {
			return err
		}
		if answers.SinkKey, err = ask.secret("  an api key, or nothing", ""); err != nil {
			return err
		}
		reach(ctx, ask, answers.SinkURL)
	case "file":
		if answers.SinkURL, err = ask.text("  where to write it",
			"/var/lib/ghchronicle/points.lp"); err != nil {
			return err
		}
	}
	ask.sayf("")
	return nil
}

// reach says whether something answered, and does not stop the setup when it
// did not: a database that is not up yet is an ordinary thing to be setting
// this up before, and the line is the warning.
func reach(ctx context.Context, ask *asker, url string) {
	if err := probe(ctx, url); err != nil {
		ask.sayf("  note: nothing answered at %s yet (%v).", url, err)
		ask.sayf("  the configuration is written anyway; the first sweep will need it up.")
		return
	}
	ask.sayf("  it answers.")
}

// askDashboard takes Grafana, when it is wanted, and proves the token works.
func askDashboard(ctx context.Context, ask *asker, answers *setupAnswers) error {
	if answers.Sink == "stdout" || answers.Sink == "file" {
		return nil
	}
	wanted, err := ask.yes("Publish the Grafana dashboard for it?", true)
	if err != nil || !wanted {
		ask.sayf("")
		return err
	}
	answers.Dashboard = true
	if answers.GrafanaURL, err = ask.text("  Grafana's address", "http://localhost:3000"); err != nil {
		return err
	}
	ask.sayf("  a service account token. Publishing needs an Editor; letting this")
	ask.sayf("  make the datasource as well needs permission to create one.")
	if answers.GrafanaToken, err = ask.secret("  token", os.Getenv("GRAFANA_TOKEN")); err != nil {
		return err
	}
	client := grafana.Client{URL: answers.GrafanaURL, Token: answers.GrafanaToken}
	if _, err = client.Do(ctx, "GET", "/api/org", nil, 15*time.Second); err != nil {
		ask.sayf("  note: Grafana did not answer (%v). Written anyway.", err)
	} else {
		ask.sayf("  it answers.")
	}
	ask.sayf("")
	return nil
}

// askService asks the last question, which only means anything where this can
// write something that runs it.
func askService(ask *asker, answers *setupAnswers) error {
	kind := serviceKind()
	if kind == "" {
		return nil
	}
	wanted, err := ask.yes(fmt.Sprintf("Set it up to run by itself, as %s?", kind), true)
	if err != nil {
		return err
	}
	answers.Service = wanted
	ask.sayf("")
	return nil
}

// writeSetup puts the configuration where it was asked for, and says what to
// do next.
func writeSetup(ask *asker, answers setupAnswers, path string, now time.Time) error {
	if path == "" {
		path = defaultConfigPath()
	}
	if _, err := os.Stat(path); err == nil {
		keep, askErr := ask.yes(path+" is already there. Replace it?", false)
		if askErr != nil {
			return askErr
		}
		if !keep {
			return errors.New("nothing was written")
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), configDirMode); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(setupConfig(answers, now)), 0o600); err != nil {
		return err
	}
	ask.sayf("Wrote %s", path)

	if answers.Service {
		if err := writeService(ask, answers, path); err != nil {
			return err
		}
	}
	ask.sayf("")
	ask.sayf("Try it once, before anything runs on a timer:")
	ask.sayf("")
	ask.sayf("  %s -config %s -once", exeName(), path)
	return nil
}

// exeName is how to call this from a shell, which is its name and not its
// path once it is installed.
func exeName() string {
	if path, err := os.Executable(); err == nil {
		return filepath.Base(path)
	}
	return "ghchronicle"
}

// reported wiring, so -setup can be run the way -list and -backfill-status are.
func setupFrom(ctx context.Context, o options, stdin *os.File, stdout io.Writer) error {
	if !interactive(stdin) {
		return errNotATerminal
	}
	ask := newAsker(stdin, stdout)
	ask.hidden = hiddenReader(stdin)
	return runSetup(ctx, ask, strings.TrimSpace(o.path), "", time.Now())
}
