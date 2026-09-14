package fakegh

import (
	"slices"

	"github.com/jmrplens/ghchronicle/internal/config"
)

// NotCollected is every family the suites deliberately leave out of the sweep,
// and why. A name here is checked against internal/config, so an entry that
// stops describing a family fails rather than quietly excusing nothing.
//
// It is the only hand-written half of Families: the rest is derived, because a
// list of families that mirrors the runner is the kind that goes stale. This
// one is a subset with a reason, so it has to be written down, and the tests
// beside it fail when a new family is neither collected nor excused here.
var NotCollected = map[string]string{
	"deps": "there is no SBOM fixture to answer the dependency graph with, and no " +
		"commits/HEAD route either, which is what gh_dependency_change writes its zero " +
		"row against: a head the fake cannot answer is a range that never opens",
}

// Families is every family the suites switch on, so a config can put each one
// on a cadence short enough that a fresh state file collects all of them.
//
// Derived from internal/config rather than typed out: a family added to the
// runner is collected here the day it is added, and one that is deliberately
// left out says so in NotCollected.
func Families() []string {
	out := make([]string, 0, len(config.Families()))
	for _, f := range config.Families() {
		if _, skipped := NotCollected[f]; skipped {
			continue
		}
		out = append(out, f)
	}
	return out
}

// Measurements names at least one measurement per collected family, so a
// family that ran and answered nothing cannot hide behind the others. That is
// the assertion a forgotten route fails: the family is scheduled, the fake
// four-oh-fours every request it makes, and the only visible symptom is a
// table that never gets created.
//
// Hand-written and checked both ways against Families: which measurement
// stands for a family is a fact about the collector, not something the config
// knows.
var Measurements = map[string]string{
	"account":      "gh_account",
	"achievements": "gh_achievement",
	"actions":      "gh_workflow_run",
	"activity":     "gh_repo_activity",
	"analyses":     "gh_code_scanning_analysis",
	"artifacts":    "gh_artifact_total",
	"billing":      "gh_billing_usage",
	"branches":     "gh_branch",
	"commits":      "gh_commit",
	"deployments":  "gh_deployment",
	"discussions":  "gh_discussion",
	"events":       "gh_event",
	"forks":        "gh_fork",
	"history":      "gh_contribution_year",
	"inventory":    "gh_secret",
	"issueevents":  "gh_issue_event",
	"issues":       "gh_pull_request",
	"joblogs":      "gh_job_log",
	"keys":         "gh_key",
	"notifs":       "gh_notification",
	"outbound":     "gh_external_contribution",
	"planning":     "gh_milestone",
	"policyfiles":  "gh_policy_file",
	"profile":      "gh_package",
	"ratelimit":    "gh_rate_limit",
	"repo":         "gh_repo",
	"rulesets":     "gh_ruleset_version",
	"security":     "gh_security_feature",
	"settings":     "gh_webhook",
	"stars":        "gh_star",
	"stats":        "gh_commits_week",
	"totals":       "gh_account_total",
	"traffic":      "gh_traffic",
}

// Unanswered is every collected family this fixture set has no response for,
// and why, so Measurements can be asserted on the rest without a blanket
// excuse.
//
// Both are deliberate rather than forgotten, and the dashboard suite already
// records the same decision: dashboardMissing lists gh_issue_event and gh_key
// among "families this account has nothing in, which is the ordinary case for
// most accounts". Adding a fixture for either would create the table and make
// those two entries describe nothing, which that test fails on, so the two
// halves have to move together.
var Unanswered = map[string]string{
	"issueevents": "the timeline query is answered by the pull request fixture, which carries no " +
		"typed timeline items, and no fixture answers /repos/{repo}/issues/{n}/events, so the family runs and " +
		"collects nothing, which is what an account with no transitions looks like",
	"keys": "no fixture answers /user/keys or /user/gpg_keys, so the family runs and " +
		"collects nothing, which is what an account with no SSH or GPG key looks like",
}

// Answering is the families whose measurement a sweep of this fixture set must
// actually produce.
func Answering() []string {
	out := make([]string, 0, len(Measurements))
	for _, f := range Families() {
		if _, quiet := Unanswered[f]; quiet {
			continue
		}
		out = append(out, f)
	}
	slices.Sort(out)
	return out
}
