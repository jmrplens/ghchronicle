// Command probe runs collectors against one repository and prints the line
// protocol they would write. Development aid: it writes nothing anywhere.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// defaultRepo is the repository probed when none is named.
const defaultRepo = "jmrplens/Cloudflare-DNS-Updater"

func main() {
	c := ghapi.New(os.Getenv("GITHUB_TOKEN"), 0)
	// Nothing here cancels the run, but every collector is started under one
	// context so that a caller that wanted to could.
	os.Exit(run(context.Background(), c, os.Args[1:], os.Getenv("GHC_DUMP"), os.Stdout, os.Stderr))
}

// run probes the repository args name, or defaultRepo, through c and prints one
// line per collector. The family named by dump also has every point it
// collected printed in full. It answers with the status to exit with: a
// collector that fails is reported and the run goes on, so only an argument
// it cannot read fails it.
func run(ctx context.Context, c *ghapi.Client, args []string, dump string, stdout, stderr io.Writer) int {
	full := defaultRepo
	if len(args) > 0 {
		full = args[0]
	}
	owner, name, ok := strings.Cut(full, "/")
	if !ok || owner == "" || name == "" {
		fmt.Fprintf(stderr, "usage: probe [owner/name]\n%q is not a repository\n", full)
		return 2
	}
	repo := collect.Repo{Owner: owner, Name: name, FullName: full}
	now := time.Now()

	for _, col := range collectors(ctx, c, repo, now) {
		pts, err := col.run()
		if err != nil {
			fmt.Fprintf(stdout, "%-11s ERROR %v\n", col.name, err)
			continue
		}
		if dump == col.name {
			for _, pt := range pts {
				fmt.Fprintln(stdout, sink.LineProtocol(pt))
			}
		}
		fmt.Fprintf(stdout, "%-11s %4d points", col.name, len(pts))
		if len(pts) > 0 {
			fmt.Fprintf(stdout, "  | %s", truncate(sink.LineProtocol(pts[0]), 120))
		}
		fmt.Fprintln(stdout)
	}
	r := c.Rate()
	fmt.Fprintf(stdout, "\nquota %s: %d/%d\n", r.Resource, r.Remaining, r.Limit)
	return 0
}

// probed is one collector, under the name its output line and GHC_DUMP use.
type probed struct {
	name string
	run  func() ([]sink.Point, error)
}

// collectors is every collector the probe runs, in the order it prints them,
// each set up the way a short look at one repository wants it.
func collectors(ctx context.Context, c *ghapi.Client, repo collect.Repo, now time.Time) []probed {
	return []probed{
		{"traffic", func() ([]sink.Point, error) { return collect.Traffic{}.Collect(ctx, c, repo, now) }},
		{"repo", func() ([]sink.Point, error) { return collect.RepoCore{}.Collect(ctx, c, repo, now) }},
		{"stars", func() ([]sink.Point, error) {
			return collect.Stargazers{Full: true}.Collect(ctx, c, repo, now)
		}},
		{"starhistory", func() ([]sink.Point, error) {
			return collect.StarHistory{Walk: collect.Unbounded}.Collect(ctx, c, repo, now)
		}},
		{"account", func() ([]sink.Point, error) {
			return collect.Account{Login: repo.Owner}.Collect(ctx, c, now)
		}},
		{"pulls", func() ([]sink.Point, error) { return collect.Pulls{}.Collect(ctx, c, repo, now) }},
		{"actions", func() ([]sink.Point, error) {
			return collect.Actions{Since: now.AddDate(0, 0, -7), Jobs: true, MaxJobRuns: 3}.Collect(ctx, c, repo, now)
		}},
		{"artifacts", func() ([]sink.Point, error) { return collect.Artifacts{}.Collect(ctx, c, repo, now) }},
		{"activity", func() ([]sink.Point, error) {
			return collect.RepoActivity{}.Collect(ctx, c, repo, now)
		}},
		{"discuss", func() ([]sink.Point, error) { return collect.Discussions{}.Collect(ctx, c, repo, now) }},
		{"billing", func() ([]sink.Point, error) {
			return collect.Billing{Login: repo.Owner, Months: 2}.Collect(ctx, c, now)
		}},
		{"profile", func() ([]sink.Point, error) {
			return collect.Profile{Login: repo.Owner}.Collect(ctx, c, now)
		}},
		{"commits", func() ([]sink.Point, error) {
			return collect.Commits{Since: now.AddDate(0, 0, -30)}.Collect(ctx, c, repo, now)
		}},
		{"activity2", func() ([]sink.Point, error) {
			return collect.RepoActivityLog{}.Collect(ctx, c, repo, now)
		}},
		{"analyses", func() ([]sink.Point, error) { return collect.Analyses{}.Collect(ctx, c, repo, now) }},
		{"forks", func() ([]sink.Point, error) { return collect.Forks{}.Collect(ctx, c, repo, now) }},
		{"planning", func() ([]sink.Point, error) { return collect.Planning{}.Collect(ctx, c, repo, now) }},
		{"outbound", func() ([]sink.Point, error) {
			return collect.Outbound{Login: repo.Owner}.Collect(ctx, c, now)
		}},
		{"history", func() ([]sink.Point, error) {
			return collect.History{Login: repo.Owner}.Collect(ctx, c, now)
		}},
		{"settings", func() ([]sink.Point, error) { return collect.Settings{}.Collect(ctx, c, repo, now) }},
		{"rulesets", func() ([]sink.Point, error) {
			return collect.RulesetHistory{}.Collect(ctx, c, repo, now)
		}},
		{"joblogs", func() ([]sink.Point, error) {
			return collect.JobLogs{Since: now.AddDate(0, 0, -30)}.Collect(ctx, c, repo, now)
		}},
		{"events", func() ([]sink.Point, error) {
			return (&collect.Events{Login: repo.Owner}).Collect(ctx, c, now)
		}},
		{"notifs", func() ([]sink.Point, error) {
			return (&collect.Notifications{All: true}).Collect(ctx, c, now)
		}},
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
