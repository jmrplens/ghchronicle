package dashboards

// The tag keys each collector sets, sorted, which is the node order of the
// sink's path: <prefix>.<measurement without gh_>.<tag values>.<field>. Every
// key is written even when its value is empty (as `none`), so a measurement's
// depth is fixed and the position of `repo` decides where the variable goes.
// Mirrors internal/collect/*.go; a new tag on a collector changes this table.
//
// Only tags a collector writes on every point belong here. A tag written under
// an `if` moves every node after it by one on the points that carry it, so
// listing it would misplace `repo` on the points that do not, and omitting it
// misplaces `repo` on the points that do: the repair is in the collector,
// which has to write a fallback value, not in this table. Three such tags
// existed and each was already costing a Graphite panel, which is what this
// rule is for. `seconds_to_resolve` is written only on a closed alert and
// closing is what set `reason`, so the alert-resolution panel read a path one
// node short of every point it wanted; and every labeled or milestoned issue
// event was one node too deep for the panel that counts them. Both collectors
// now write the tag always, with `open` and `none` where GitHub says nothing,
// and the three tags are listed below like any other.
//
// A tag whose value moves after the row's own date is not in this table
// because it is not a tag any more: an artifact's expiry, a commit's gate,
// an alert's state and the reason it closed, an issue's resolution, holder,
// milestone and parent, whether a pull request is a draft and what its
// reviewers decided, whether a discussion has its answer. Each of those opened
// a second series at the same instant the day it changed, and each is a field
// now, under a new name; a Graphite panel that wants one of them cannot have
// it by path, and says so.
//
// A measurement that names a repository names it with the same three keys
// everywhere: `owner`, the short `repo`, and `full_name`. Twelve of them used
// to carry `repo` alone with the full name inside it, which is what made a
// filter written against one group match nothing at all in the other. Nothing
// else in the build reads this table against the collectors' tag sets, so
// TestEveryRepositoryTagSetIsTheSameShape holds the entries to it here.
//
// One measurement is absent from this table altogether. gh_job_log is absent
// because no panel queries it, so it has no node index to get wrong; a panel
// that wanted it would have to add its tags here first. gh_event is listed
// like the rest since its collector writes every tag on every row.
var tags = map[string][]string{
	"gh_account":                  {"user"},
	"gh_achievement":              {"achievement", "user"},
	"gh_achievement_progress":     {"achievement", "user"},
	"gh_account_total":            {"user"},
	"gh_actions_cache":            {"full_name", "owner", "repo"},
	"gh_actions_cache_entry":      {"cache", "full_name", "owner", "ref", "repo"},
	"gh_actions_policy":           {"full_name", "owner", "permissions", "repo"},
	"gh_artifact":                 {"artifact", "full_name", "owner", "repo"},
	"gh_artifact_total":           {"full_name", "owner", "repo"},
	"gh_billing_usage":            {"full_name", "owner", "product", "repo", "sku", "unit", "user"},
	"gh_branch":                   {"branch", "full_name", "is_default", "owner", "repo"},
	"gh_branch_protection":        {"full_name", "owner", "pattern", "repo"},
	"gh_code_scanning_alert":      {"full_name", "owner", "repo", "severity", "tool"},
	"gh_code_scanning_alert_item": {"category", "full_name", "number", "owner", "path", "ref", "repo", "rule", "severity", "tool"},
	"gh_code_scanning_analysis":   {"category", "full_name", "owner", "ref", "repo", "tool", "version"},
	"gh_code_scanning_setup":      {"full_name", "owner", "query_suite", "repo", "schedule", "state"},
	"gh_collector_family":         {"family", "full_name", "owner", "reason", "repo", "scope"},
	"gh_commit":                   {"author", "branch", "full_name", "owner", "repo", "sha", "signature"},
	"gh_commit_check":             {"app", "check", "conclusion", "full_name", "owner", "repo", "sha"},
	"gh_commit_punchcard":         {"full_name", "hour", "owner", "repo", "weekday"},
	"gh_commits_week":             {"full_name", "owner", "repo"},
	"gh_contribution_day":         {"user"},
	"gh_contribution_day_repo":    {"full_name", "own", "owner", "private", "repo", "user"},
	"gh_contribution_repo":        {"full_name", "kind", "owner", "repo", "user"},
	"gh_contribution_year":        {"user", "year"},
	"gh_contributions_total":      {"user"},
	"gh_dependabot_alert":         {"ecosystem", "full_name", "owner", "repo", "severity"},
	"gh_dependabot_ecosystem":     {"ecosystem", "full_name", "interval", "owner", "repo"},
	"gh_dependabot_alert_item":    {"ecosystem", "full_name", "ghsa", "manifest", "number", "owner", "package", "relationship", "repo", "scope", "severity"},
	"gh_dependency":               {"ecosystem", "full_name", "owner", "repo"},
	"gh_dependency_change":        {"change", "ecosystem", "full_name", "owner", "repo"},
	"gh_dependency_license":       {"full_name", "license", "owner", "repo"},
	"gh_deploy_key":               {"full_name", "key", "owner", "read_only", "repo"},
	"gh_deployment":               {"deployment", "environment", "full_name", "owner", "repo", "task"},
	"gh_discussion":               {"answerable", "author", "category", "full_name", "number", "owner", "repo"},
	"gh_discussion_comment":       {"author", "comment", "full_name", "is_answer", "is_reply", "number", "own", "owner", "repo", "user"},
	"gh_environment":              {"environment", "full_name", "owner", "repo"},
	"gh_external_contribution":    {"full_name", "kind", "number", "owner", "repo", "state", "user"},
	"gh_fork":                     {"by", "full_name", "owner", "repo"},
	"gh_gist":                     {"gist", "public", "user"},
	"gh_event":                    {"action", "full_name", "owner", "ref_type", "repo", "type"},
	"gh_issue":                    {"author", "full_name", "number", "owner", "repo", "state"},
	"gh_issue_comment":            {"full_name", "number", "own", "owner", "repo", "user"},
	"gh_issue_event":              {"actor", "bot", "event", "full_name", "kind", "label", "mentioned", "milestone", "owner", "repo", "requested_reviewer", "review_requester"},
	"gh_key":                      {"key", "kind", "user"},
	"gh_label":                    {"full_name", "label", "owner", "repo"},
	"gh_milestone":                {"full_name", "milestone", "owner", "repo", "state"},
	"gh_notification":             {"full_name", "owner", "private", "reason", "repo", "subject_type"},
	"gh_package":                  {"full_name", "owner", "package", "repo", "type", "user", "visibility"},
	"gh_package_version":          {"full_name", "owner", "package", "repo", "tag", "type", "user", "visibility"},
	"gh_pinned_item":              {"full_name", "owner", "repo", "user"},
	"gh_policy_file":              {"file", "full_name", "owner", "repo"},
	"gh_profile_flag":             {"flag", "user"},
	"gh_pull_request":             {"author", "full_name", "number", "owner", "repo", "state"},
	"gh_pull_request_review":      {"author", "bot", "full_name", "number", "owner", "repo", "reviewer", "self"},
	"gh_rate_limit":               {"resource"},
	"gh_release":                  {"draft", "full_name", "owner", "prerelease", "repo", "tag"},
	"gh_release_asset":            {"asset", "full_name", "owner", "repo", "tag"},
	"gh_repo":                     {"archived", "default_branch", "fork", "full_name", "language", "license", "owner", "repo", "visibility"},
	"gh_repo_activity":            {"activity", "actor", "full_name", "owner", "repo"},
	"gh_repo_community":           {"full_name", "owner", "repo"},
	"gh_repo_created":             {"fork", "full_name", "owner", "repo", "user"},
	"gh_repo_language":            {"full_name", "language", "owner", "repo"},
	"gh_repo_archived":            {"full_name", "owner", "repo"},
	"gh_repo_policy":              {"full_name", "owner", "repo"},
	"gh_repo_topic":               {"full_name", "owner", "repo", "topic"},
	"gh_repo_total":               {"archived", "fork", "full_name", "owner", "repo", "visibility"},
	"gh_review_thread":            {"author", "bot", "full_name", "number", "owner", "repo", "thread"},
	"gh_ruleset":                  {"enforcement", "full_name", "owner", "repo", "ruleset", "target"},
	"gh_ruleset_rule":             {"full_name", "owner", "repo", "rule", "ruleset"},
	"gh_ruleset_version":          {"actor_type", "full_name", "owner", "repo", "ruleset", "target"},
	"gh_secret":                   {"full_name", "kind", "owner", "repo", "secret"},
	"gh_security_feature":         {"feature", "full_name", "owner", "repo"},
	"gh_security_setting":         {"full_name", "owner", "repo", "setting", "status"},
	"gh_social_account":           {"provider", "user"},
	"gh_sponsors_listing":         {"user"},
	"gh_sponsors_tier":            {"tier", "user"},
	"gh_sponsorship":              {"direction", "sponsorable", "user"},
	"gh_star":                     {"full_name", "owner", "repo", "user"},
	"gh_star_given":               {"full_name", "language", "owner", "repo", "user"},
	"gh_star_list":                {"list", "user"},
	"gh_traffic":                  {"full_name", "kind", "owner", "repo"},
	"gh_traffic_path":             {"full_name", "owner", "path", "repo"},
	"gh_traffic_referrer":         {"full_name", "owner", "referrer", "repo"},
	"gh_webhook":                  {"active", "full_name", "hook", "host", "owner", "repo"},
	"gh_webhook_delivery":         {"code", "event", "full_name", "hook", "host", "ok", "owner", "repo", "status"},
	"gh_workflow":                 {"full_name", "owner", "path", "repo", "state", "workflow"},
	"gh_workflow_job":             {"attempt", "conclusion", "full_name", "job_name", "labels", "owner", "repo", "runner_group", "workflow"},
	"gh_workflow_run":             {"actor", "conclusion", "event", "full_name", "owner", "repo", "workflow"},
	"gh_workflow_run_total":       {"full_name", "owner", "repo"},
	"gh_workflow_step":            {"attempt", "conclusion", "full_name", "job_name", "owner", "repo", "step", "workflow"},
}
