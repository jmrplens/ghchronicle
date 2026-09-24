package dashboards

import "fmt"

// ── Continuous integration ──────────────────────────────────────────────────

// The three measurements this section reads, the paths its Graphite panels
// take from them, and the Prometheus counter behind the same numbers. Every
// part of the section below needs some of these.
const (
	ciRun  = "gh_workflow_run"
	ciJob  = "gh_workflow_job"
	ciStep = "gh_workflow_step"
)

// The coverage every job and step number is computed over. A sweep expands
// the jobs of the newest runs only, so these describe the runs that were
// expanded, never every run the run count includes; said on each panel, since
// a median over a tenth of the runs read as a median over all of them.
//
// A backfill expands every run GitHub still lists, and GitHub serves a run's
// jobs for as long as it holds the run: measured on 2026-09-24, a run 278
// days old listed every job with its times and runner, while job logs were a
// 410 from ninety days. The runs themselves follow the retention period only
// from 1 October 2026. Steps go sooner: the same day, every run created
// before 12 April, about five and a half months back, listed its jobs with no
// steps, and every later one with them. So the step panels say that they
// reach less far back than the job panels.
const expanded = "Over the runs whose jobs were expanded, which is the newest runs of each " +
	"sweep and, after a backfill, every run GitHub still holds, which from 1 October 2026 " +
	"means the repository's retention period, ninety days by default; not every run the " +
	"run count includes."

// expandedSteps is expanded for the panels drawn from steps.
const expandedSteps = expanded + " Steps reach less far back than jobs: GitHub stops " +
	"serving a job's steps before the job itself, and measured in September 2026 it " +
	"served none for runs older than about five and a half months."

// declaredWorkflows is the declarations a run table joins to for the
// workflow's own page: a run carries the file path under `workflow`, and the
// declaration carries that path under `path` beside its url. Collapsed to one
// row per file before the join, because a join that matches a run twice
// doubles its run count: the url carries the default branch, so a renamed
// branch is a second distinct row under the same path in the range.
const declaredWorkflows = "SELECT repo, path, MAX(url) AS url FROM gh_workflow" +
	ciInRange + RF + " GROUP BY 1, 2"

// ciInRange bounds a query by the dashboard's range and leaves the AND open for
// the repository filter that follows it everywhere. The inventory and security
// sections bound their own queries with the same clause, and ciFromRuns is the
// whole tail for the measurement most of this section reads.
const (
	ciInRange  = " WHERE $__timeFilter(time) AND "
	ciFromRuns = " FROM " + ciRun + ciInRange
)

// ciOnWorkflowPath is how a run meets its declaration: the run carries the file
// under `workflow` and declaredWorkflows carries it under `path`.
const ciOnWorkflowPath = " ON w.repo = r.repo AND w.path = r.workflow"

// What this section calls each of its numbers. The stat, the Prometheus query,
// the Graphite one and the Elasticsearch frame behind one value all have to
// spell the name alike, and the thresholds and units below match a field by it.
const (
	ciRunCount        = "Workflow runs"
	ciSuccessRate     = "Success rate"
	ciUndecidedRuns   = "Undecided runs"
	ciRunTime         = "Run duration"
	ciQueueWait       = "Queue wait"
	ciArtifactStorage = "Artifact storage walked"
	ciCacheSize       = "Actions cache"
	ciTimesRun        = "Times run"
	ciLiveSize        = "Live size"
	ciLiveCount       = "Live"
)

// GitHub spells this conclusion the British way and the linter's dictionary
// is American, so the value is assembled rather than written: a query that
// compared against the American spelling would match no run.
var cancelledRun = "cancel" + "led"

var (
	runSeconds    = rp(ciRun, "duration_seconds")
	jobQueued     = rp(ciJob, "queued_seconds")
	jobSeconds    = rp(ciJob, "duration_seconds")
	stepSeconds   = rp(ciStep, "duration_seconds")
	artifactBytes = rp("gh_artifact_total", "live_bytes")
	promRuns      = fmt.Sprintf("github_workflow_runs_total{%s}", PF)
)

// ci is Actions, in four questions: whether the runs passed, where their time
// went, what keeps failing, and what the runs left behind.
func ci(b *builder) []Panel {
	out := runOutcomes(b)
	out = append(out, whereTheTimeGoes(b)...)
	out = append(out, whatKeepsFailing(b)...)
	return append(out, artifactStorage(b)...)
}

// runOutcomes is the shape of the whole account's continuous integration:
// how many runs, how many passed, how long they took to start and to finish,
// what they are storing, and each of those again per day.
func runOutcomes(b *builder) []Panel {
	runs := "SELECT COUNT(*) AS value FROM gh_workflow_run WHERE $__timeFilter(time) AND " + RF
	// Success over success plus failure. A run canceled by concurrency or
	// skipped by its own condition did not fail, and on this account those
	// were sixteen per cent of a week: counted as failures the rate read 76.7
	// in red where the runs that ran passed 91 times in a hundred.
	decided := "conclusion IN ('success', 'failure')"
	success := "SELECT 100.0 * SUM(CASE WHEN conclusion = 'success' THEN 1 ELSE 0 END)" +
		" / NULLIF(SUM(CASE WHEN " + decided + " THEN 1 ELSE 0 END), 0) AS value" +
		ciFromRuns + RF
	undecided := "SELECT COUNT(*) AS value FROM gh_workflow_run WHERE $__timeFilter(time) AND " + RF +
		" AND conclusion IN ('" + cancelledRun + "', 'skipped')"
	dur := "SELECT approx_percentile_cont(duration_seconds, 0.5) AS value FROM gh_workflow_run" +
		ciInRange + RF
	queue := "SELECT approx_percentile_cont(queued_seconds, 0.5) AS value FROM gh_workflow_job" +
		ciInRange + RF
	perDay := flowSelect + timeBin + ", conclusion AS series," +
		" COUNT(*) AS runs FROM gh_workflow_run WHERE $__timeFilter(time) AND " + RF +
		" GROUP BY 1, 2 ORDER BY 1"
	durTS := flowSelect + timeBin + "," +
		` approx_percentile_cont(duration_seconds, 0.5) AS "Median",` +
		` approx_percentile_cont(duration_seconds, 0.95) AS "95th percentile"` +
		ciFromRuns + RF + " GROUP BY 1 ORDER BY 1"
	queueTS := flowSelect + timeBin + "," +
		` approx_percentile_cont(queued_seconds, 0.5) AS "Median queue",` +
		` MAX(queued_seconds) AS "Worst queue" FROM gh_workflow_job` +
		ciInRange + RF + " GROUP BY 1 ORDER BY 1"
	// A snapshot per repository: the value of a bucket is its newest row, never
	// a sum of the sweeps in it. The eight repositories holding the most at
	// their newest reading are named, the rest are one `other` series, and a
	// repository holding nothing is not a legend entry.
	storage := "SELECT time, CASE WHEN rk <= 8 THEN repo ELSE 'other' END AS series," +
		" SUM(live_bytes) AS bytes FROM (SELECT time, repo, live_bytes," +
		" DENSE_RANK() OVER (ORDER BY newest DESC, repo) AS rk FROM (" +
		flowSelect + timeBin + ", repo, live_bytes," +
		" ROW_NUMBER() OVER (PARTITION BY $__dateBin(time), repo ORDER BY time DESC) AS rn," +
		" FIRST_VALUE(live_bytes) OVER (PARTITION BY repo ORDER BY time DESC) AS newest" +
		" FROM gh_artifact_total WHERE $__timeFilter(time) AND " + RF + ") y WHERE rn = 1) x" +
		" GROUP BY 1, 2 HAVING SUM(live_bytes) > 0 ORDER BY 1"

	return []Panel{
		// Seven values in one panel rather than seven tiles: each tile was
		// its own 144 pixel panel on a phone and the seven opened the section
		// with nothing else on the screen. The success rate keeps its colors
		// by naming its own thresholds on its own field, since a stat's
		// colors are otherwise the whole panel's and 88.5% in orange painted
		// the run count and the queue wait beside it.
		statGroup("Runs in range", box{W: 24, H: 5, X: 0, Y: 0}, []Target{
			sqlT(namedValue(runs, ciRunCount)),
			{Kind: "sql", Format: "table", Ref: "B", SQL: namedValue(success, ciSuccessRate)},
			{Kind: "sql", Format: "table", Ref: "C", SQL: namedValue(undecided, ciUndecidedRuns)},
			{Kind: "sql", Format: "table", Ref: "D", SQL: namedValue(dur, ciRunTime)},
			{Kind: "sql", Format: "table", Ref: "E", SQL: namedValue(queue, ciQueueWait)},
			{Kind: "sql", Format: "table", Ref: "F", SQL: namedValue(
				latestSumSQL("gh_artifact_total", "live_bytes"), ciArtifactStorage,
			)},
			{Kind: "sql", Format: "table", Ref: "G", SQL: namedValue(
				latestSumSQL("gh_actions_cache", "size_bytes"), ciCacheSize,
			)},
		}, &P{
			Prom: []Target{
				promNamed("A", ciRunCount, fmt.Sprintf("sum(increase(%s[$__range]))", promRuns)),
				promNamed("B", ciSuccessRate, fmt.Sprintf(
					`100 * sum(increase(github_workflow_runs_total{conclusion="success",%s}[$__range]))`+
						` / sum(increase(github_workflow_runs_total{conclusion=~"success|failure",%s}[$__range]))`,
					PF, PF,
				)),
				promNamed("C", ciUndecidedRuns, fmt.Sprintf(
					`sum(increase(github_workflow_runs_total{conclusion=~"%s|skipped",%s}[$__range]))`,
					cancelledRun, PF,
				)),
				promNamed("D", ciRunTime, fmt.Sprintf(
					"avg(github_workflow_runs_duration_seconds_mean{%s})", PF,
				)),
				promNamed("E", ciQueueWait, fmt.Sprintf(
					"avg(github_workflow_jobs_queued_seconds_mean{%s})", PF,
				)),
				promNamed("F", ciArtifactStorage, fmt.Sprintf(
					"sum(github_artifact_total_live_bytes{%s})", PF,
				)),
				promNamed("G", ciCacheSize, fmt.Sprintf(
					"sum(github_actions_cache_size_bytes{%s})", PF,
				)),
			},
			Desc: "How many runs the range holds and how they ended. The success rate is " +
				"the runs that succeeded as a share of the runs that succeeded or failed: " +
				"a run canceled by a newer push under a concurrency group, or skipped by " +
				"its own condition, decided nothing and is counted beside it as undecided " +
				"rather than as a failure. The run duration is the median over the range " +
				"and the queue wait is the time a job spent waiting for a runner, which " +
				"only exists at job level, the run-level number folding the wait into the " +
				"duration. " + expanded + " Last come the bytes the runs left behind, which " +
				"are the artifacts the walk reached: GitHub lists thousands of them per " +
				"repository and the walk stops at five hundred, so this is a floor wherever " +
				"it did. \"Artifact storage counted\" below puts the two counts beside it.",
			PromDesc: sinceStart + " " + lastSweep,
			GR: []Target{
				grNamed("A", ciRunCount, total(countOf(runSeconds))),
				grNamed("B", ciSuccessRate, fmt.Sprintf("asPercent(%s, %s)",
					total(countOf(rp(ciRun, "duration_seconds", "conclusion", "success"))),
					total(countOf(rp(ciRun, "duration_seconds", "conclusion", "{success,failure}"))))),
				grNamed("C", ciUndecidedRuns, total(countOf(
					rp(ciRun, "duration_seconds", "conclusion", "{"+cancelledRun+",skipped}"),
				))),
				grNamed("D", ciRunTime, medianTotal(runSeconds)),
				grNamed("E", ciQueueWait, medianTotal(jobQueued)),
				grNamed("F", ciArtifactStorage, latestSum(artifactBytes)),
				grNamed("G", ciCacheSize, latestSum(rp("gh_actions_cache", "size_bytes"))),
			},
			GRDesc: grSlot,
			ES: func() []Target {
				out := []Target{
					esRef("A", b.esTotal(ciRun, b.mCount(), ESF)),
					esRef("B", b.esTotal(ciRun, b.mAvg("success"), ESF, "conclusion:(success OR failure)")),
					esRef("C", b.esTotal(ciRun, b.mCount(), ESF, "conclusion:("+cancelledRun+" OR skipped)")),
					esRef("D", b.esTotal(ciRun, b.mPct("duration_seconds", 50), ESF)),
					esRef("E", b.esTotal(ciJob, b.mPct("queued_seconds", 50), ESF)),
				}
				out = append(out, esRefs("F", b.esLatestSum("gh_artifact_total", "live_bytes"))...)
				return append(out, esRefs("G", b.esLatestSum("gh_actions_cache", "size_bytes"))...)
			}(),
			ESOver: []any{
				frameName("A", ciRunCount), frameName("B", ciSuccessRate),
				frameName("C", ciUndecidedRuns), frameName("D", ciRunTime),
				frameName("E", ciQueueWait), frameName("F", ciArtifactStorage),
				frameName("G", ciCacheSize),
				fieldThresholds(ciSuccessRate, "percentunit", fractionOf(rateThresholds)),
			},
			// The two byte totals are a sum over the newest reading of each
			// series, and the five before them are single values a sum leaves
			// alone.
			ESOpts: Opts{"calc": "sum"},
			ESDesc: "In Elasticsearch the success rate is the mean of the boolean `success` " +
				"field over those runs, as a fraction.",
			Opts: Opts{"thresholds": plainSteps},
			Overrides: []any{
				fieldThresholds(ciSuccessRate, "percent", rateThresholds),
				unitOf(ciRunTime, "s", 0), unitOf(ciQueueWait, "s", 0),
				unitOf(ciArtifactStorage, "bytes", 0), unitOf(ciCacheSize, "bytes", 0),
			},
		}),
		panel("timeseries", "Runs by outcome over time", box{W: 12, H: 8, X: 0, Y: 5}, []Target{sqlTS(perDay)}, &P{
			Prom:     []Target{daily(fmt.Sprintf("sum by (conclusion) (increase(%s[1d]))", promRuns), "{{conclusion}}")},
			PromDesc: sinceStart,
			Opts:     mergeOpts(Opts{"bars": true, "stack": true}, dayBins),
			SQLOpts:  seriesOpts,
			// The words are the tag's own, so the colors hold in every store.
			Overrides: []any{
				colorOf("success", "green"), colorOf("failure", "red"),
				colorOf(cancelledRun, "orange"), colorOf("skipped", "blue"),
				colorOf("action_required", "purple"), colorOf("timed_out", "dark-red"),
			},
			GR:   []Target{grq(perBucket("isNonNull("+runSeconds+")", gn(ciRun, "conclusion")))},
			ES:   []Target{b.esDaily(ciRun, b.mCount(), "conclusion", "", []string{ESF}, "")},
			Desc: bucketFollowsRange,
		}),
		panel("timeseries", "Run duration over time", box{W: 12, H: 8, X: 12, Y: 5}, []Target{sqlTS(durTS)}, &P{
			Prom: []Target{promq(fmt.Sprintf("avg(github_workflow_runs_duration_seconds_mean{%s})", PF),
				legend("Mean of the sweep"))},
			Opts:     mergeOpts(Opts{"unit": "s"}, dayBins),
			PromDesc: lastSweep,
			GR: []Target{
				grq(fmt.Sprintf(`alias(%s, "Median")`, medianBucket(runSeconds)), "A"),
				grq(fmt.Sprintf(`alias(%s, "Worst")`, worstBucket(runSeconds)), "B"),
			},
			GRDesc: grWorst + " " + grSlot,
			ES:     []Target{esq(ciRun, []any{b.mPct("duration_seconds", 50, 95)}, []any{b.dh()}, "A", []string{ESF}, "")},
			Desc:   bucketFollowsRange,
		}),
		panel("timeseries", "Queue wait over time", box{W: 12, H: 8, X: 0, Y: 13}, []Target{sqlTS(queueTS)}, &P{
			Prom: []Target{
				promq(fmt.Sprintf("avg(github_workflow_jobs_queued_seconds_mean{%s})", PF),
					withRef("A"), legend("Mean queue")),
				promq(fmt.Sprintf("max(github_workflow_jobs_queued_seconds_mean{%s})", PF),
					withRef("B"), legend("Worst job")),
			},
			Opts:     mergeOpts(Opts{"unit": "s"}, hourBins),
			Desc:     expanded + " " + bucketFollowsRange,
			PromDesc: lastSweep,
			GR: []Target{
				grq(fmt.Sprintf(`alias(%s, "Median queue")`, medianBucket(jobQueued, "1h")), "A"),
				grq(fmt.Sprintf(`alias(%s, "Worst queue")`, worstBucket(jobQueued, "1h")), "B"),
			},
			GRDesc: grSlot,
			ES: []Target{
				esq(ciJob, []any{b.mPct("queued_seconds", 50)}, []any{b.dh("1h")}, "A", []string{ESF}, "Median queue"),
				esq(ciJob, []any{b.mMax("queued_seconds")}, []any{b.dh("1h")}, "B", []string{ESF}, "Worst queue"),
			},
		}),
		panel("timeseries", "Artifact storage over time", box{W: 12, H: 8, X: 12, Y: 13}, []Target{sqlTS(storage)}, &P{
			Prom: []Target{promq(fmt.Sprintf("sum by (repo) (github_artifact_total_live_bytes{%s}) > 0", PF),
				legend("{{repo}}"))},
			Opts:    mergeOpts(Opts{"unit": "bytes"}, hourBins),
			SQLOpts: seriesOpts,
			Desc: "Artifacts that have not expired, over the ones the walk reached, which " +
				"is a floor on any repository with more than five hundred: " +
				"\"Artifact storage counted\" further down has the counts that say which " +
				"those are. GitHub deletes artifacts on their own " +
				"schedule, which is why this falls without anyone doing anything. A " +
				"reading taken at each sweep, so the curve starts the day the collector " +
				"did: there is no history of it to rebuild. The eight repositories holding " +
				"the most are named; the rest are `other`. " + bucketFollowsRange,
			GR: []Target{grq(fmt.Sprintf("removeEmptySeries(removeBelowValue(%s, 1))",
				perBucket(artifactBytes, gn("gh_artifact_total", "repo"), "1h", "max")))},
			ES: []Target{b.esDaily("gh_artifact_total", b.mMax("live_bytes"), "repo", "1h", []string{ESF}, "")},
		}),
	}
}

// whereTheTimeGoes ranks the work itself: which workflows run most and take
// longest, and which jobs and steps inside them are the slow ones.
func whereTheTimeGoes(b *builder) []Panel {
	// The file name without its directory: every workflow lives under
	// .github/workflows/, and the column showed that prefix and nothing else.
	// The workflow's own page comes from gh_workflow, joined on the file
	// path the way "Workflows that never ran" joins it; a run carries the
	// file and never the declaration's url.
	// In each of the three the column the table is sorted by comes second,
	// beside the name: on a phone the two are what fits on the screen.
	byWF := `SELECT REPLACE(workflow, '.github/workflows/', '') AS "Workflow", COUNT(*) AS "Runs",` +
		` r.repo AS "Repository",` +
		` SUM(CASE WHEN conclusion <> 'success' THEN 1 ELSE 0 END) AS "Not successful",` +
		` approx_percentile_cont(duration_seconds, 0.5) AS "Duration", MAX(w.url) AS "Link"` +
		" FROM gh_workflow_run r LEFT JOIN (" + declaredWorkflows + ") w" +
		ciOnWorkflowPath +
		" WHERE $__timeFilter(time) AND r." + RF +
		" GROUP BY 1, 3 ORDER BY 2 DESC LIMIT 30"
	jobs := `SELECT job_name AS "Job", approx_percentile_cont(duration_seconds, 0.5) AS "Duration",` +
		` repo AS "Repository", COUNT(*) AS "Times run",` +
		` MAX(duration_seconds) AS "Worst" FROM gh_workflow_job` +
		ciInRange + RF + " GROUP BY 1, 3 ORDER BY 2 DESC LIMIT 30"
	steps := `SELECT step AS "Step", approx_percentile_cont(duration_seconds, 0.5) AS "Duration",` +
		` job_name AS "Job", repo AS "Repository", COUNT(*) AS "Times run",` +
		` MAX(duration_seconds) AS "Worst" FROM gh_workflow_step` +
		ciInRange + RF + " GROUP BY 1, 3, 4" +
		" ORDER BY 2 DESC LIMIT 30"
	// HAVING drops repositories with no live artifacts: a series pinned at
	// zero is a legend entry and nothing else.

	wfGR, wfGRtf := gTbl(fmt.Sprintf(`limit(sortBy(groupByNodes(%s, "avg", %d, %d), "count", true), 30)`,
		runSeconds, gn(ciRun, "workflow"), gn(ciRun, "repo")), "Workflow",
		[]col{{"count", "Runs"}, {"median", "Duration"}})
	wfES, wfEStf := esTbl(ciRun, []any{b.tm("workflow", 30), b.tm("repo", 50)},
		[]any{b.mCount(), b.mAvg("success"), b.mPct("duration_seconds", 50)},
		[]named{
			{"workflow.keyword", "Workflow"},
			{inventoryRepoTerm, "Repository"},
			{"n", "Runs"},
			{"s", ciSuccessRate},
			{"d", "Duration"},
		}, []string{ESF})

	jobsGR, jobsGRtf := gTbl(fmt.Sprintf(`limit(sortBy(groupByNodes(%s, "avg", %d, %d), "median", true), 30)`,
		jobSeconds, gn(ciJob, "job_name"), gn(ciJob, "repo")), "Job",
		[]col{{"count", ciTimesRun}, {"median", "Duration"}, {"max", "Worst"}})
	jobsES, jobsEStf := esTbl(ciJob, []any{b.tm("job_name", 30), b.tm("repo", 50)},
		[]any{b.mCount(), b.mPct("duration_seconds", 50), b.mMax("duration_seconds")},
		[]named{
			{"job_name.keyword", "Job"},
			{inventoryRepoTerm, "Repository"},
			{"n", ciTimesRun},
			{"d", "Duration"},
			{"w", "Worst"},
		}, []string{ESF})

	stepsGR, stepsGRtf := gTbl(fmt.Sprintf(
		`limit(sortBy(groupByNodes(%s, "avg", %d, %d, %d), "median", true), 30)`,
		stepSeconds, gn(ciStep, "step"), gn(ciStep, "job_name"), gn(ciStep, "repo"),
	), "Step",
		[]col{{"count", ciTimesRun}, {"median", "Duration"}, {"max", "Worst"}})
	stepsES, stepsEStf := esTbl(ciStep, []any{b.tm("step", 30), b.tm("job_name", 50), b.tm("repo", 50)},
		[]any{b.mCount(), b.mPct("duration_seconds", 50), b.mMax("duration_seconds")},
		[]named{
			{"step.keyword", "Step"},
			{"job_name.keyword", "Job"},
			{inventoryRepoTerm, "Repository"},
			{"n", ciTimesRun},
			{"d", "Duration"},
			{"w", "Worst"},
		}, []string{ESF})
	return []Panel{
		panel("table", "Workflows", box{W: 12, H: 9, X: 0, Y: 21}, []Target{sqlT(byWF)}, &P{
			Prom: func() []Target {
				rank := fmt.Sprintf("sum by (repo, workflow) (increase(%s[$__range]))", promRuns)
				return []Target{
					promTbl(promTop(30, rank), "A"),
					promTbl(promWithin(30, fmt.Sprintf(
						`sum by (repo, workflow) (increase(github_workflow_runs_total{conclusion!="success",%s}[$__range]))`,
						PF,
					), rank, "repo", "workflow"), "B"),
					promTbl(promWithin(30, fmt.Sprintf(
						"avg by (repo, workflow) (github_workflow_runs_duration_seconds_mean{%s})",
						PF,
					), rank, "repo", "workflow"), "C"),
				}
			}(),
			PromTF: merged(map[string]string{
				"workflow": "Workflow", "repo": "Repository", inventoryValueCol + "A": "Runs",
				inventoryValueCol + "B": "Not successful", inventoryValueCol + "C": "Duration",
			}, nil, map[string]int{"workflow": 0, "repo": 1}),
			Opts:     Opts{"sort": "Runs"},
			PromDesc: sinceStart + " " + lastSweep,
			Overrides: []any{
				repoColumn(),
				barCell("Runs", "short", 110), width("Not successful", 130),
				unitOf("Duration", "s", 130), linkOn("Workflow"),
			},
			GR: wfGR, GRTF: wfGRtf,
			GRDesc: "Graphite names each row workflow and repository from the path; the failures are in the chart above. " + grRows,
			ES:     wfES, ESTF: wfEStf,
			ESDesc: "In Elasticsearch the failures are a success rate, the mean of the boolean `success` field.",
			ESOver: []any{unitOf(ciSuccessRate, "percentunit", 110)},
		}),
		panel("table", "Slowest jobs", box{W: 12, H: 9, X: 12, Y: 21}, []Target{sqlT(jobs)}, &P{
			Prom: func() []Target {
				rank := fmt.Sprintf("avg by (repo, job_name) (github_workflow_jobs_duration_seconds_mean{%s})", PF)
				return []Target{
					promTbl(promTop(30, rank), "A"),
					promTbl(promWithin(30, fmt.Sprintf(
						"sum by (repo, job_name) (github_workflow_jobs_count{%s})", PF,
					),
						rank, "repo", "job_name"), "B"),
				}
			}(),
			PromTF: merged(map[string]string{
				"job_name": "Job", "repo": "Repository", inventoryValueCol + "A": "Duration",
				inventoryValueCol + "B": ciTimesRun,
			}, nil, map[string]int{"job_name": 0, "repo": 1}),
			Opts:     Opts{"sort": "Duration"},
			Desc:     expanded,
			PromDesc: lastSweep + " " + sweepCount,
			Overrides: []any{
				repoColumn(), width(ciTimesRun, 90),
				unitOf("Duration", "s", 100), unitOf("Worst", "s", 90),
			},
			GR: jobsGR, GRTF: jobsGRtf,
			GRDesc: "Graphite names each row job and repository from the path. " + grSlot,
			ES:     jobsES, ESTF: jobsEStf,
		}),
		panel("table", "Slowest steps", box{W: 24, H: 8, X: 0, Y: 30}, []Target{sqlT(steps)}, &P{
			PromNote: cannot("the thirty slowest steps by median duration, with the job and "+
				"repository each belongs to and its worst time.",
				"The exporter skips `gh_workflow_step`: per-step timings are a "+
					"series per step of every job, and their mean would say nothing "+
					"about which step to look at. The jobs are in the table above."),
			Opts: Opts{"sort": "Duration"},
			Desc: expandedSteps,
			// The first column has no width: with all six fixed the table used
			// seven hundred pixels of a full-width panel and left the rest blank.
			Overrides: []any{
				width("Job", 220), repoColumn(),
				width(ciTimesRun, 90), unitOf("Duration", "s", 90), unitOf("Worst", "s", 90),
			},
			GR: stepsGR, GRTF: stepsGRtf,
			GRDesc: "Graphite names each row step, job and repository from the path. " + grSlot,
			ES:     stepsES, ESTF: stepsEStf,
		}),
	}
}

// whatKeepsFailing is the cost of the failures: the minutes spent on runs
// that failed, the workflows and steps that keep failing, and the workflows
// that never ran at all.
func whatKeepsFailing(b *builder) []Panel {
	wastedGR, wastedGRtf := gTbl(fmt.Sprintf(`groupByNode(%s, %d, "sum")`,
		rp(ciRun, "duration_seconds"), gn(ciRun, "repo")), "Repository", []col{{"sum", "Total"}})
	wastedES, wastedEStf := esTbl(ciRun, []any{b.tm("repo", 50)},
		[]any{b.mSum("duration_seconds"), b.mAvg("success")},
		[]named{{inventoryRepoTerm, "Repository"}, {"t", "Total"}, {"s", ciSuccessRate}},
		[]string{ESF})

	failingGR, failingGRtf := gTbl(fmt.Sprintf(
		`limit(sortBy(groupByNodes(%s, "sum", %d, %d), "sum", true), 20)`,
		countOf(rp(ciRun, "duration_seconds", "conclusion", "failure")),
		gn(ciRun, "repo"), gn(ciRun, "workflow"),
	), "Repository, workflow", []col{{"sum", "Failures"}})
	failingES, failingEStf := esTbl(ciRun, []any{b.tm("repo", 50), b.tm("workflow", 30)},
		[]any{b.mCount(), b.mAvg("success")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"workflow.keyword", "Workflow"},
			{"n", "Runs"},
			{"s", ciSuccessRate},
		}, []string{ESF})

	failStepsGR, failStepsGRtf := gTbl(fmt.Sprintf(
		`limit(sortBy(groupByNodes(%s, "sum", %d, %d), "sum", true), 20)`,
		countOf(rp(ciStep, "duration_seconds", "conclusion", "failure")),
		gn(ciStep, "step"), gn(ciStep, "repo"),
	), "Step, repository", []col{{"sum", "Failures"}})
	failStepsES, failStepsEStf := esTbl(ciStep, []any{b.tm("step", 20), b.tm("repo", 50)},
		[]any{b.mCount()},
		[]named{{"step.keyword", "Step"}, {inventoryRepoTerm, "Repository"}, {"n", "Failures"}},
		[]string{ESF, "conclusion:failure"})

	return []Panel{
		panel("table", "Minutes spent on failed runs", box{W: 12, H: 8, X: 0, Y: 38}, []Target{sqlT(
			`SELECT repo AS "Repository",` +
				` SUM(CASE WHEN conclusion <> 'success' THEN duration_seconds ELSE 0 END) AS "Wasted",` +
				` SUM(duration_seconds) AS "Total",` +
				` 100.0 * SUM(CASE WHEN conclusion <> 'success' THEN duration_seconds ELSE 0 END)` +
				` / NULLIF(SUM(duration_seconds), 0) AS "Share"` +
				ciFromRuns + RF +
				" GROUP BY 1 ORDER BY 2 DESC",
		)}, &P{
			Prom: []Target{
				promTbl(fmt.Sprintf(`sum by (repo) (increase(github_workflow_runs_total{conclusion!="success",%s}[$__range])) * on (repo) group_left avg by (repo) (github_workflow_runs_duration_seconds_mean{%s})`, PF, PF), "A"),
				promTbl(fmt.Sprintf(`sum by (repo) (increase(github_workflow_runs_total{%s}[$__range])) * on (repo) group_left avg by (repo) (github_workflow_runs_duration_seconds_mean{%s})`, PF, PF), "B"),
			},
			PromTF: merged(map[string]string{
				"repo": "Repository", inventoryValueCol + "A": "Wasted", inventoryValueCol + "B": "Total",
			}, nil, nil),
			Opts: Opts{"sort": "Wasted"},
			Desc: "Minutes that produced nothing. On the account this was written against, one " +
				"repository burned 8,530 of its 15,111 minutes in a week on runs that failed.",
			PromDesc: sinceStart + " " + lastSweep + " The product of a count and a mean, " +
				"so it is an estimate rather than the sum InfluxDB adds up.",
			Overrides: []any{
				unitOf("Wasted", "s", 130), unitOf("Total", "s", 120),
				unitOf("Share", "percent", 100),
			},
			GR: wastedGR, GRTF: wastedGRtf,
			GRDesc: "Graphite has no conclusion to filter a sum by here, so this is the whole " +
				"time rather than the wasted part. " + grRows,
			ES: wastedES, ESTF: wastedEStf,
			ESDesc: "In Elasticsearch the wasted share is the complement of the success rate.",
			ESOver: []any{unitOf(ciSuccessRate, "percentunit", 120)},
		}),
		panel("table", "Workflows that keep failing", box{W: 12, H: 8, X: 12, Y: 38}, []Target{sqlT(
			`SELECT r.repo AS "Repository",` +
				` SUM(CASE WHEN conclusion <> 'success' THEN 1 ELSE 0 END) AS "Failures",` +
				` workflow AS "Workflow", COUNT(*) AS "Runs",` +
				` 100.0 * SUM(CASE WHEN conclusion <> 'success' THEN 1 ELSE 0 END)` +
				` / COUNT(*) AS "Failure rate", MAX(w.url) AS "Link"` +
				" FROM gh_workflow_run r LEFT JOIN (" + declaredWorkflows + ") w" +
				ciOnWorkflowPath +
				" WHERE $__timeFilter(time) AND r." + RF +
				" GROUP BY 1, 3 HAVING SUM(CASE WHEN conclusion <> 'success' THEN 1 ELSE 0 END) > 3" +
				" ORDER BY 5 DESC, 2 DESC LIMIT 20",
		)}, &P{
			Prom: func() []Target {
				rank := fmt.Sprintf(
					`sum by (repo, workflow) (increase(github_workflow_runs_total{conclusion!="success",%s}[$__range])) > 3`, PF,
				)
				return []Target{
					promTbl(promTop(20, rank), "A"),
					promTbl(promWithin(20, fmt.Sprintf(
						"sum by (repo, workflow) (increase(github_workflow_runs_total{%s}[$__range]))", PF,
					),
						rank, "repo", "workflow"), "B"),
				}
			}(),
			PromTF: merged(map[string]string{
				"repo": "Repository", "workflow": "Workflow",
				inventoryValueCol + "A": "Failures", inventoryValueCol + "B": "Runs",
			}, nil, map[string]int{"repo": 0, "workflow": 1}),
			Opts: Opts{"sort": "Failures"},
			Desc: "Not the ones that fail sometimes: the ones nobody has switched off. Two " +
				"workflows here fail on every single run, 108 of 108 and 86 of 86.",
			PromDesc: sinceStart,
			Overrides: []any{
				unitOf("Failure rate", "percent", 120), barCell("Failures", "short", 110),
				linkOn("Repository"),
			},
			GR: failingGR, GRTF: failingGRtf, GRDesc: grSlot,
			ES: failingES, ESTF: failingEStf,
			ESOver: []any{unitOf(ciSuccessRate, "percentunit", 120)},
		}),
		panel("table", "Steps that fail", box{W: 12, H: 8, X: 0, Y: 46}, []Target{sqlT(
			`SELECT step AS "Step", COUNT(*) AS "Failures", repo AS "Repository"` +
				" FROM gh_workflow_step WHERE $__timeFilter(time) AND " + RF +
				" AND conclusion = 'failure' GROUP BY 1, 3 ORDER BY 2 DESC LIMIT 20",
		)}, &P{
			PromNote: cannot("which step failed and how often, rather than which job.",
				"The exporter skips gh_workflow_step: a gauge per step of every "+
					"job of every workflow is a series count in the thousands, and "+
					"the InfluxDB dashboard draws the same thing from rows."),
			Opts: Opts{"sort": "Failures"},
			Desc: "Which step, not which job. This is the question that otherwise gets answered " +
				"by reading job logs, and it comes from a table already collected. " + expandedSteps,
			PromDesc:  sinceStart,
			Overrides: []any{width("Step", 200), barCell("Failures", "short", 120)},
			GR:        failStepsGR, GRTF: failStepsGRtf, GRDesc: grSlot,
			ES: failStepsES, ESTF: failStepsEStf,
		}),
		// Joined on the file path, not on the name. gh_workflow tags both: its
		// `workflow` is the human name the listing gives and its `path` is the
		// file. gh_workflow_run tags only the file, under the name `workflow`,
		// because a run's own name is the pull request title on a dynamic run.
		// Joining name to path matched nothing, and a LEFT JOIN that matches
		// nothing reports every workflow in the account as never run, which
		// looks like data rather than like a broken query.
		panel("table", "Workflows that never ran", box{W: 12, H: 8, X: 12, Y: 46}, []Target{sqlT(
			`SELECT w.repo AS "Repository", w.workflow AS "Workflow",` +
				` w.state AS "State", f.fork AS "Fork", w.url AS "Link" FROM (` +
				"SELECT DISTINCT repo, workflow, path, state, url FROM gh_workflow" +
				ciInRange + RF + ") w" +
				" LEFT JOIN (SELECT DISTINCT repo, workflow FROM gh_workflow_run" +
				ciInRange + RF + ") r" +
				ciOnWorkflowPath +
				repoFlagsJoin("w.repo") +
				" WHERE r.workflow IS NULL AND " + notArchived + " ORDER BY 1, 2",
		)}, &P{
			PromNote: cannot("the workflows that are declared in a repository and did not run "+
				"in the range, which is a join between two measurements.",
				"The exporter keeps one series per label set and no way to subtract "+
					"one set from another: `unless` compares label sets that would have "+
					"to be identical. Both sides still carry a `workflow` label and it no "+
					"longer holds the same string on each, the run's being the workflow "+
					"file and the declaration's the human name, and the file path that "+
					"would match is not exported at all: the declaration keeps `repo`, "+
					"`workflow` and `state` and nothing else."),
			GRNote: cannot("the workflows declared and never run, a join between two "+
				"measurements.",
				"Graphite has paths, not rows, and no operation that removes the "+
					"series present in one tree from another.", "graphite"),
			ESNote: cannot("the workflows declared and never run, a join between two "+
				"measurements.",
				"Elasticsearch aggregations run inside one index, and these are two.",
				"elasticsearch"),
			Desc: "A workflow that never runs is either dead or waiting on a trigger that " +
				"no longer fires, and nothing else in the dashboard would say so. " +
				archivedLeftOut + " A workflow in an archived repository cannot run at all, " +
				"which is why every row of this table was one of those until they came out. " +
				"A fork's workflows stay, since a fork can be given one that runs, and Fork " +
				"says which they are.",
			Overrides: []any{
				repoColumn(), width("Workflow", 200), width("Fork", 70),
				linkOn("Workflow"),
			},
		}),
	}
}

// artifactStorage is what the runs left behind: how much is uploaded per day,
// and how much of what is stored GitHub will still account for.
func artifactStorage(b *builder) []Panel {
	walkedGR, walkedGRtf := gTbl(rowsOf(fmt.Sprintf("keepLastValue(%s)",
		rp("gh_artifact_total", "live_bytes")), gn("gh_artifact_total", "repo")),
		"Repository", []col{{"lastNotNull", ciLiveSize}})
	walkedES, walkedEStf := esTbl("gh_artifact_total", []any{b.tm("repo", 50)},
		[]any{b.mNewest("count", "walked", "live_count", "live_bytes")},
		[]named{
			{inventoryRepoTerm, "Repository"},
			{"count", "Declared"},
			{"walked", "Walked"},
			{"live_count", ciLiveCount},
			{"live_bytes", ciLiveSize},
		}, []string{ESF})

	return []Panel{
		panel("timeseries", "Artifacts created over time", box{W: 12, H: 8, X: 0, Y: 54}, []Target{sqlTS(
			topSeries("gh_artifact", "repo", "size_bytes", "bytes", RF),
		)}, &P{
			Opts:    mergeOpts(Opts{"bars": true, "stack": true, "unit": "bytes"}, dayBins),
			SQLOpts: seriesOpts,
			Desc: "What the workflows produced, at the resolution of one artifact. Seven hundred " +
				"and ninety three artifacts and thirteen gigabytes in a single day here, which " +
				"is where the storage bill comes from. The eight repositories producing the " +
				"most in the range are named; the rest are `other`. " + bucketFollowsRange,
			PromDesc: sinceStart,
			PromNote: cannot("artifact bytes created per day, one bar per repository.",
				"The exporter reduces the per-artifact rows to a count and a mean "+
					"size, so the bytes created in a day cannot be recovered from it."),
			GR: []Target{grq(perBucket(rp("gh_artifact", "size_bytes"), gn("gh_artifact", "repo")))},
			ES: []Target{b.esDaily("gh_artifact", b.mSum("size_bytes"), "repo", "", []string{ESF}, "")},
		}),
		panel("table", "Artifact storage counted", box{W: 12, H: 8, X: 12, Y: 54},
			[]Target{sqlT(`SELECT repo AS "Repository", MAX(count) AS "Declared",` +
				` MAX(walked) AS "Walked", MAX(live_count) AS "` + ciLiveCount + `",` +
				` MAX(live_bytes) AS "Live size"` +
				" FROM gh_artifact_total WHERE $__timeFilter(time) AND " + RF +
				" GROUP BY 1 ORDER BY 2 DESC")}, &P{
				Prom: []Target{
					promTbl(fmt.Sprintf("max by (repo) (github_artifact_total_count{%s})", PF), "A"),
					promTbl(fmt.Sprintf("max by (repo) (github_artifact_total_walked{%s})", PF), "B"),
					promTbl(fmt.Sprintf("max by (repo) (github_artifact_total_live_bytes{%s})", PF), "C"),
					promTbl(fmt.Sprintf("max by (repo) (github_artifact_total_live_count{%s})", PF), "D"),
				},
				PromTF: merged(map[string]string{
					"repo": "Repository", inventoryValueCol + "A": "Declared", inventoryValueCol + "B": "Walked",
					inventoryValueCol + "C": ciLiveSize, inventoryValueCol + "D": ciLiveCount,
				}, nil, nil),
				Opts: Opts{"sort": "Declared"},
				Desc: "Artifact storage, and how much of it was counted. Three counts, because " +
					"the live size is on neither of the other two: Declared is GitHub's own " +
					"total and counts the artifacts it has already expired, Walked is how far " +
					"the page cap let the walk go, and Live is the artifacts still held among " +
					"those, which is what the live size is the size of. When Walked is lower " +
					"than Declared the live size is a floor rather than a total: here it is " +
					"short by a factor of fifty six, and without these columns the tile above " +
					"would say so nowhere.",
				Overrides: []any{
					unitOf(ciLiveSize, "bytes", 120), width("Declared", 110),
					width("Walked", 100), width(ciLiveCount, 90),
				},
				GR: walkedGR, GRTF: walkedGRtf, GRDesc: grSlot,
				ES: walkedES, ESTF: walkedEStf,
			}),
	}
}
