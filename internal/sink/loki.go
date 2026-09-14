package sink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Loki writes the events, not the numbers.
//
// Some of what GitHub reports is a measurement and some of it is an event. "The
// repository has 283 stars" is a measurement. "Someone starred it at 03:03,
// this release was published, that workflow failed on main, this alert was
// raised" are events: each happened once, at a known moment, and what you want
// later is to read them in order and search them, not to average them.
//
// Metrics stores answer the first kind badly for the second. A log store
// answers it directly, and the two sit side by side in the same Grafana. So
// the measurements go to InfluxDB and Prometheus, and the events come here.
//
// Only measurements with a rule are sent. Everything else is a gauge in
// disguise and would turn the log into a slow, expensive copy of the metrics.
type Loki struct {
	// URL is the push endpoint, for example http://loki:3100/loki/api/v1/push
	URL string
	// TenantID sets X-Scope-OrgID for a multi-tenant Loki.
	TenantID string
	// Labels are added to every stream. Keep them few: Loki indexes labels and
	// a high cardinality label costs far more than a wide log line.
	Labels map[string]string
	Batch  int
	// MaxAge is how far behind an entry may be before it is left out.
	//
	// Two Loki limits make this necessary, and the second is the one that
	// actually bites. `reject_old_samples_max_age` refuses anything older than
	// a week, and half of what this collects is older than that by design: a
	// star from 2020, a pull request from 2024. But Loki also refuses an entry
	// more than its out-of-order window behind the newest entry already in
	// that stream, which defaults to about two hours. Measured against a real
	// Loki 3: once a stream held an entry from 19:14, one from 00:35 the same
	// day came back as "entry too far behind".
	//
	// So the horizon is applied twice: against the wall clock, and against the
	// newest entry in each stream, both within this batch and across pushes.
	// Zero means one hour, which is inside the default window.
	MaxAge time.Duration

	// watermarks remembers the newest entry sent per stream, so the second
	// push is judged by the same rule Loki will judge it by.
	wmMu       sync.Mutex
	watermarks map[string]time.Time

	client *http.Client
}

func NewLoki(url, tenant string, labels map[string]string, batch int, maxAge, timeout time.Duration) *Loki {
	if batch <= 0 {
		batch = 1000
	}
	// An hour rather than a day: the binding limit is the out-of-order window,
	// about two hours, not reject_old_samples_max_age. A day would let a push
	// carry an entry Loki refuses, and Loki refuses the whole push, so the
	// cost of guessing high is every entry in the batch and not just the old
	// one. Zero is the config layer saying nothing, which lands here.
	if maxAge <= 0 {
		maxAge = time.Hour
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if labels == nil {
		labels = map[string]string{}
	}
	if _, ok := labels["job"]; !ok {
		labels["job"] = "ghchronicle"
	}
	return &Loki{
		URL: url, TenantID: tenant, Labels: labels, Batch: batch, MaxAge: maxAge,
		watermarks: map[string]time.Time{}, client: &http.Client{Timeout: timeout},
	}
}

func (l *Loki) Name() string { return "loki" }
func (l *Loki) Close() error { return nil }

// lokiEvent says how one measurement becomes a log line.
type lokiEvent struct {
	// kind is the stream label, the thing you filter on first.
	kind string
	// message builds the human-readable half of the line. The structured half
	// is every tag and field, attached as JSON.
	message func(p Point) string
}

// A tag or field is quoted into the message by name. The rendering is
// deliberately plain: a line has to be readable in a terminal tail as well as
// in Grafana.
func tagOf(p Point, k string) string { return p.Tags[k] }

func fieldOf(p Point, k string) string {
	v, ok := p.Fields[k]
	if !ok {
		return ""
	}
	return fmt.Sprint(v)
}

var lokiEvents = map[string]lokiEvent{
	"gh_star": {kind: "star", message: func(p Point) string {
		return fmt.Sprintf("%s starred %s", tagOf(p, "user"), tagOf(p, "full_name"))
	}},
	"gh_star_given": {kind: "star_given", message: func(p Point) string {
		return fmt.Sprintf("%s starred %s", tagOf(p, "user"), tagOf(p, "repo"))
	}},
	"gh_fork": {kind: "fork", message: func(p Point) string {
		return fmt.Sprintf("%s forked %s", tagOf(p, "by"), tagOf(p, "full_name"))
	}},
	"gh_release": {kind: "release", message: func(p Point) string {
		return fmt.Sprintf("release %s of %s, %s downloads",
			tagOf(p, "tag"), tagOf(p, "full_name"), fieldOf(p, "downloads"))
	}},
	"gh_package_version": {kind: "package", message: func(p Point) string {
		return fmt.Sprintf("published %s:%s", tagOf(p, "package"), tagOf(p, "tag"))
	}},
	"gh_pull_request": {kind: "pull_request", message: func(p Point) string {
		return fmt.Sprintf("pull request %s#%s %s by %s, %s lines changed",
			tagOf(p, "full_name"), tagOf(p, "number"), strings.ToLower(tagOf(p, "state")),
			tagOf(p, "author"), fieldOf(p, "churn"))
	}},
	// The state is a field: a dismissal moves it after the review's own date.
	"gh_pull_request_review": {kind: "review", message: func(p Point) string {
		return fmt.Sprintf("%s reviewed %s#%s: %s", tagOf(p, "reviewer"),
			tagOf(p, "full_name"), tagOf(p, "number"), strings.ToLower(fieldOf(p, "review_state")))
	}},
	"gh_issue": {kind: "issue", message: func(p Point) string {
		return fmt.Sprintf("issue %s#%s %s by %s", tagOf(p, "full_name"),
			tagOf(p, "number"), strings.ToLower(tagOf(p, "state")), tagOf(p, "author"))
	}},
	"gh_commit": {kind: "commit", message: func(p Point) string {
		return fmt.Sprintf("%s %s: %s (+%s -%s)", tagOf(p, "sha"), tagOf(p, "author"),
			fieldOf(p, "headline"), fieldOf(p, "additions"), fieldOf(p, "deletions"))
	}},
	// The workflow tag is the file path now, not the human name, and the
	// sentence keeps it. The name is one fieldOf away, but it is the value the
	// collectors just demoted for being unstable: 35 distinct names for 7
	// workflows on one repository, and on a dynamic run it is the pull request
	// title, so "PR #495 on o/r: failure" would name the pull request and not
	// the workflow that failed. A log line is read by tailing and grepping, and
	// the path is what makes both work. Nothing is lost either way: logLine
	// appends every tag and field, so name="CI" is already on the same line.
	"gh_workflow_run": {kind: "workflow_run", message: func(p Point) string {
		return fmt.Sprintf("%s on %s: %s after %ss", tagOf(p, "workflow"),
			tagOf(p, "full_name"), tagOf(p, "conclusion"), fieldOf(p, "duration_seconds"))
	}},
	"gh_repo_activity": {kind: "repo_activity", message: func(p Point) string {
		return fmt.Sprintf("%s %s on %s by %s", tagOf(p, "activity"), fieldOf(p, "ref_name"),
			tagOf(p, "full_name"), tagOf(p, "actor"))
	}},
	"gh_dependabot_alert_item": {kind: "alert", message: func(p Point) string {
		return fmt.Sprintf("%s alert on %s in %s (%s), state %s", tagOf(p, "severity"),
			tagOf(p, "package"), tagOf(p, "full_name"), tagOf(p, "ecosystem"), fieldOf(p, "alert_state"))
	}},
	"gh_code_scanning_analysis": {kind: "code_scanning", message: func(p Point) string {
		return fmt.Sprintf("%s analyzed %s (%s): %s results", tagOf(p, "tool"),
			tagOf(p, "full_name"), tagOf(p, "ref"), fieldOf(p, "results"))
	}},
	// No actor: the feed is the account's own, so the actor is the login on
	// every row and the collector stopped writing it.
	"gh_event": {kind: "event", message: func(p Point) string {
		return fmt.Sprintf("%s on %s", tagOf(p, "type"), tagOf(p, "repo"))
	}},
	"gh_notification": {kind: "notification", message: func(p Point) string {
		return fmt.Sprintf("%s: %s (%s)", tagOf(p, "repo"), fieldOf(p, "title"), tagOf(p, "reason"))
	}},
	"gh_discussion": {kind: "discussion", message: func(p Point) string {
		return fmt.Sprintf("discussion in %s (%s), answered %s", tagOf(p, "full_name"),
			tagOf(p, "category"), fieldOf(p, "has_answer"))
	}},
	"gh_webhook_delivery": {kind: "webhook", message: func(p Point) string {
		return fmt.Sprintf("%s delivered %s to %s: HTTP %s in %ss", tagOf(p, "hook"),
			tagOf(p, "event"), tagOf(p, "host"), tagOf(p, "code"),
			fieldOf(p, "duration_seconds"))
	}},
	"gh_job_log": {kind: "job_log", message: func(p Point) string {
		return fieldOf(p, "line")
	}},
	"gh_external_contribution": {kind: "external_contribution", message: func(p Point) string {
		return fmt.Sprintf("%s merged %s#%s", tagOf(p, "user"), tagOf(p, "repo"), tagOf(p, "number"))
	}},
	// The environment is what a reader is looking for here, so it goes in the
	// sentence rather than only in the logfmt tail: "which of my environments
	// moved, and did it come up" is the question a deployment log answers.
	// `state` is the collector's own stable outcome and not the raw enum,
	// which becomes INACTIVE the moment the next deployment replaces this one
	// and would rewrite the past of a line that was true when it was written.
	// The outcome is a field: it moves after the deployment's own date.
	"gh_deployment": {kind: "deployment", message: func(p Point) string {
		return fmt.Sprintf("%s deployed %s to %s: %s", fieldOf(p, "creator"),
			tagOf(p, "full_name"), tagOf(p, "environment"), fieldOf(p, "outcome"))
	}},
	// A thread is dated at its first comment, which is when the objection was
	// raised, so the line reads as the objection and not as its resolution:
	// `resolved` and `outdated` move for weeks afterwards and ride in the
	// logfmt tail, where a reader can see they are the state at collection
	// time rather than at the moment the line describes.
	"gh_review_thread": {kind: "review_thread", message: func(p Point) string {
		return fmt.Sprintf("%s opened a thread on %s#%s (%s), %s comments",
			tagOf(p, "author"), tagOf(p, "full_name"), tagOf(p, "number"),
			fieldOf(p, "path"), fieldOf(p, "comments"))
	}},
	// The moment a protection changed, which is the one thing gh_ruleset's
	// days_since_change cannot say afterwards. GitHub names the actor by type
	// and id only, so the line does too; the login is not in the response.
	"gh_ruleset_version": {kind: "ruleset", message: func(p Point) string {
		return fmt.Sprintf("ruleset %s of %s saved by %s %s", tagOf(p, "ruleset"),
			tagOf(p, "full_name"), tagOf(p, "actor_type"), fieldOf(p, "actor_id"))
	}},
}

// lokiNotEvents is the other half of the decision lokiEvents records: the
// dated measurements that are deliberately not rendered, and why.
//
// eventsByStream drops a measurement with no rule silently, which is the right
// behavior for a gauge and the wrong one for an event nobody has got round
// to: gh_deployment and gh_review_thread were dated events for months with no
// line, and nothing anywhere said so. So the reduction's own list of dated
// items is the domain, and every one of them has to appear here or in
// lokiEvents. TestEveryDatedItemIsRenderedOrRefused is what makes absence
// impossible; a reason is what makes the refusal readable.
//
// Every entry here can be reversed by writing a rendering and deleting the
// line, which is the point of writing the reason down.
var lokiNotEvents = map[string]string{
	"gh_workflow_job": "the run is the event and it is rendered; a run of a dozen jobs " +
		"would be a dozen lines saying what the run's own line says once, and which job " +
		"was slow is a question for the metrics",
	"gh_commit_check": "the commit is rendered and carries the gate's verdict in `checks`; " +
		"a line per checking app repeats that verdict once per app",
	"gh_code_scanning_alert_item": "the analysis that raised it is rendered and carries how " +
		"many results each scan found, which is the line a reader tails; Dependabot has no " +
		"analysis measurement, which is why its alerts are rendered and these are not",
	"gh_discussion_comment": "the collector counts comments and never reads their text, so " +
		"the line would carry nothing the discussion's own line does not",
	"gh_issue_comment": "the same as the discussion comment: a count with no text behind it",
	"gh_issue_event": "the account's own event feed is rendered and carries these transitions " +
		"while they are recent; this measurement exists to hold them after the feed has " +
		"forgotten them, which is history rather than a tail",
	"gh_repo_created": "the repository list is re-emitted whole on every sweep, so this is " +
		"inventory dated at its creation rather than an event arriving once",
	"gh_repo_archived": "the twin of gh_repo_created and out for the same reason: every sweep " +
		"re-emits every archived repository",
	"gh_sponsorship": "a payment rather than something that happened in a repository, a " +
		"handful a year, and every value a line would carry is already a gauge",
}

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

type lokiPush struct {
	Streams []lokiStream `json:"streams"`
}

// lokiEntry is one rendered log line and the moment it describes.
type lokiEntry struct {
	at   time.Time
	line string
}

// A write is three steps, and the middle one is the reason this is not a
// single loop: what Loki accepts depends on the newest entry in each stream,
// which is not known until every point has been read.
func (l *Loki) Write(ctx context.Context, points []Point) error {
	grouped, newest, old := l.eventsByStream(points)
	values, behind := l.admit(grouped, newest)
	dropped := old + behind

	if len(values) > 0 {
		if err := l.push(ctx, values); err != nil {
			return err
		}
	}
	if dropped > 0 {
		return &DroppedError{N: dropped, Older: l.MaxAge}
	}
	return nil
}

// eventsByStream turns the points that have an event rule into log lines,
// keyed by the stream they belong to, and reports the newest entry each stream
// carries in this batch.
//
// Grouping is not an optimization: a Loki stream is defined by its label set,
// and pushing one stream per line would be pathological. Anything already past
// the wall-clock horizon is counted and left out, because the dated history
// belongs in the metrics store and what a log answers is "what happened
// recently, in order".
func (l *Loki) eventsByStream(points []Point) (grouped map[string][]lokiEntry, newest map[string]time.Time, dropped int) {
	grouped = map[string][]lokiEntry{}
	newest = map[string]time.Time{}
	wall := time.Now().Add(-l.MaxAge)
	for _, p := range points {
		ev, ok := lokiEvents[p.Measurement]
		if !ok {
			continue
		}
		stamp := stampOf(p)
		if stamp.Before(wall) {
			dropped++
			continue
		}
		grouped[ev.kind] = append(grouped[ev.kind], lokiEntry{stamp, logLine(ev.message(p), p)})
		if stamp.After(newest[ev.kind]) {
			newest[ev.kind] = stamp
		}
	}
	return grouped, newest, dropped
}

// admit drops what Loki would refuse for being too far behind the newest entry
// in its own stream, and renders the survivors as the timestamp and line pairs
// the push API takes.
//
// The high-water mark is the newest of this batch and everything already sent,
// so the rule is applied within a push as well as across pushes: Loki judges
// the rest of a push against its own newest entry, and a batch spanning a day
// fails on its very first push otherwise.
func (l *Loki) admit(grouped map[string][]lokiEntry, newest map[string]time.Time) (values map[string][][2]string, dropped int) {
	values = map[string][][2]string{}
	l.wmMu.Lock()
	defer l.wmMu.Unlock()
	for kind, entries := range grouped {
		high := newest[kind]
		if wm := l.watermarks[kind]; wm.After(high) {
			high = wm
		}
		floor := high.Add(-l.MaxAge)
		for _, e := range entries {
			if e.at.Before(floor) {
				dropped++
				continue
			}
			values[kind] = append(values[kind], [2]string{
				strconv.FormatInt(e.at.UnixNano(), 10), e.line,
			})
			if e.at.After(l.watermarks[kind]) {
				l.watermarks[kind] = e.at
			}
		}
	}
	return values, dropped
}

// push sends the streams, flushing once a batch has filled. Streams go in name
// order so two runs over the same points make the same requests.
func (l *Loki) push(ctx context.Context, values map[string][][2]string) error {
	kinds := make([]string, 0, len(values))
	for k := range values {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)

	var streams []lokiStream
	batched := 0
	flush := func() error {
		if len(streams) == 0 {
			return nil
		}
		err := l.post(ctx, lokiPush{Streams: streams})
		streams, batched = nil, 0
		return err
	}
	for _, kind := range kinds {
		entries := values[kind]
		// Loki rejects a stream whose entries are not in ascending time order.
		sort.Slice(entries, func(i, j int) bool { return entries[i][0] < entries[j][0] })
		labels := map[string]string{"kind": kind}
		maps.Copy(labels, l.Labels)
		streams = append(streams, lokiStream{Stream: labels, Values: entries})
		batched += len(entries)
		if batched >= l.Batch {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// DroppedError reports entries a sink refused to send. It is not a failure: the
// caller wrote everything it could. It exists so a silent horizon does not
// become an afternoon spent wondering where the old stars went.
type DroppedError struct {
	N     int
	Older time.Duration
}

func (d *DroppedError) Error() string {
	return fmt.Sprintf("%d entries older than %s were not sent, which Loki would have rejected outright", d.N, d.Older)
}

// logLine puts the human sentence first and the structured data after it, in
// logfmt. Loki parses that on read, so the same line is both greppable and
// queryable without a second copy of the data.
func logLine(msg string, p Point) string {
	var b strings.Builder
	b.WriteString(msg)
	for _, k := range sortedKeys2(p.Tags) {
		fmt.Fprintf(&b, " %s=%q", k, p.Tags[k])
	}
	for _, k := range sortedKeys(p.Fields) {
		switch v := p.Fields[k].(type) {
		case string:
			fmt.Fprintf(&b, " %s=%q", k, v)
		default:
			fmt.Fprintf(&b, " %s=%v", k, v)
		}
	}
	return b.String()
}

func (l *Loki) post(ctx context.Context, body lokiPush) error {
	endpoint, err := pushURL(l.URL)
	if err != nil {
		return fmt.Errorf("loki push: %w", err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.TenantID != "" {
		req.Header.Set("X-Scope-OrgID", l.TenantID)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("loki push: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return nil
}
