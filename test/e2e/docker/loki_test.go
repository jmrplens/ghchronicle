//go:build dockere2e

package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// What a real Loki does with the event lines.
//
// This sink is the one that does not send everything: a measurement is sent
// only if it has an event rule, because everything else is a gauge in disguise
// and would turn the log into a slow copy of the metrics store. So there are
// two questions here. Whether Loki kept each entry at the nanosecond the thing
// happened, which is the dating rule again and which no capture server can
// answer. And whether the streams that exist are the ones that should, which
// is the sink's own selection, checked against the store rather than against
// itself.
//
// Two properties of Loki shape every query below, and both were measured here
// rather than assumed. An entry is not queryable when the push is answered: it
// becomes queryable once its chunk is flushed, which is why every read polls.
// And a query is split into subqueries by time, so a range of a few days costs
// tens of them and a range of years costs thousands: every query here is
// bounded to the days it is asking about.

// lokiKinds mirrors lokiEvents in internal/sink/loki.go: the measurements that
// are events, and the stream each becomes. It is the sink's selection written
// out, so that this suite can ask the store for exactly the streams that
// should exist and no others. A new event rule adds a line here, and the
// closure test below is what says so.
var lokiKinds = map[string]string{
	"gh_star":                   "star",
	"gh_star_given":             "star_given",
	"gh_fork":                   "fork",
	"gh_release_published":      "release",
	"gh_package_version":        "package",
	"gh_pull_request":           "pull_request",
	"gh_pull_request_review":    "review",
	"gh_issue":                  "issue",
	"gh_commit":                 "commit",
	"gh_workflow_run":           "workflow_run",
	"gh_repo_activity":          "repo_activity",
	"gh_dependabot_alert_item":  "alert",
	"gh_code_scanning_analysis": "code_scanning",
	"gh_event":                  "event",
	"gh_notification":           "notification",
	"gh_discussion":             "discussion",
	"gh_webhook_delivery":       "webhook",
	"gh_job_log":                "job_log",
	"gh_external_contribution":  "external_contribution",
	"gh_deployment":             "deployment",
	"gh_review_thread":          "review_thread",
	"gh_ruleset_version":        "ruleset",
}

func TestLokiKeepsTheNanosecondTheEventHappened(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)

	t.Run("a star from two years ago", func(t *testing.T) {
		entry := lokiAwaitOne(ctx, t, s, "star", starGivenAt)
		// The sentence a human reads when tailing, then every tag and field
		// in logfmt so the same line is queryable without a second copy.
		for _, want := range []string{
			"alice starred octocat/hello-world",
			`user="alice"`, `repo="hello-world"`, "starred=1",
		} {
			if !strings.Contains(entry, want) {
				t.Errorf("the line does not carry %q:\n%s", want, entry)
			}
		}
	})

	t.Run("a workflow run", func(t *testing.T) {
		entry := lokiAwaitOne(ctx, t, s, "workflow_run", runFinishedAt)
		if !strings.Contains(entry, "duration_seconds=220") {
			t.Errorf("the line does not carry the duration:\n%s", entry)
		}
	})

	t.Run("every run of the fixtures is in the stream, each at its own moment", func(t *testing.T) {
		// The whole of one stream, over the days its entries belong to, which
		// is the cheapest window that can hold them all. Loki keeping the
		// dates is one thing; keeping all of them is another, and a sink that
		// dropped an entry for being behind the newest one would show up
		// here and nowhere else.
		want := lokiStamps(t, sweep, "gh_workflow_run")
		got := lokiAwaitStamps(ctx, t, s, "workflow_run", want)
		if !slices.Equal(got, want) {
			t.Errorf("the workflow_run stream holds %v, want %v", lokiReadable(got), lokiReadable(want))
		}
	})

	// The negative of the three above, which none of them can make on its own.
	// Each asks whether an entry sits at the moment it belongs to, and that
	// stays true when the sink additionally writes it at the moment of the
	// sweep, and it stays true on a stack an earlier run already filled, which
	// `make e2e-docker-up` and a second `go test` produce. A dated stream must
	// hold nothing at all from the moment this sweep began.
	t.Run("nothing dated was restamped with the sweep's clock", func(t *testing.T) {
		// release joined these when its line moved from gh_release, stamped at
		// the sweep, to the publication: before that, this is exactly what
		// its stream held.
		for _, kind := range []string{"star", "workflow_run", "traffic", "release"} {
			reads, err := lokiRange(ctx, s, kind, sweep.Started, time.Now().UTC().Add(time.Minute))
			if err != nil {
				t.Fatalf("%s: %v", kind, err)
			}
			for _, r := range reads {
				t.Errorf("the %s stream holds an entry stamped %s, inside the sweep that collected it: %s",
					kind, r.at.Format(time.RFC3339Nano), r.line)
			}
		}
	})
}

func TestLokiHoldsTheEventsAndNothingElse(t *testing.T) {
	s := Start(t)
	ctx := t.Context()
	sweep := pushSweepRun(ctx, t, s)

	// The streams that should exist: one per measurement of the sweep that
	// has an event rule.
	want := map[string]bool{}
	for measurement := range pushPointsByMeasurement(t, sweep) {
		if kind, ok := lokiKinds[measurement]; ok {
			want[kind] = true
		}
	}
	got := lokiAwaitKinds(ctx, t, s, want)

	t.Run("every event measurement became a stream", func(t *testing.T) {
		for kind := range want {
			if !slices.Contains(got, kind) {
				t.Errorf("no stream named %q, though the sweep produced its measurement", kind)
			}
		}
	})

	t.Run("nothing else did", func(t *testing.T) {
		// A gauge that grew an event rule, or a stream from a measurement
		// this suite does not know about. Either is a change of what the log
		// is for, and it should be made deliberately.
		for _, kind := range got {
			if !want[kind] {
				t.Errorf("the store holds a stream named %q, which no measurement of this sweep should have produced", kind)
			}
		}
	})

	t.Run("the sink dropped nothing on the way", func(t *testing.T) {
		// The sink applies its own horizon before Loki sees anything, twice:
		// against the wall clock and against the newest entry in each stream.
		// The sweep opens it wide on purpose, so a drop here would mean the
		// horizon is being applied where it should not be.
		if strings.Contains(sweep.Log, "were not sent") {
			t.Errorf("the sink held entries back:\n%s", tail(sweep.Log))
		}
	})
}

// TestLokiIsNotThrottledByTheHostsDisk keeps the suite's Loki from answering
// by how full the machine's disk is. Loki's write-ahead log refuses every push
// once the filesystem under it passes wal.disk_full_threshold, 90% by default,
// and that filesystem is the host's. On 2026-09-29 the suite passed at 02:40
// UTC and failed at 05:10 and 05:25 with nothing changed but the host's disk,
// which had reached 90.2%: every Loki test timed out on an empty store and the
// Elasticsearch suite failed on the Loki sink's "sink write failed". Either the
// log is off, which config/loki.yaml does because nothing here outlives the
// run, or its threshold is zero, which is Loki's word for never.
//
// It reads the file rather than the stack, so it needs no containers.
func TestLokiIsNotThrottledByTheHostsDisk(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("config", "loki.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Ingester struct {
			WAL struct {
				Enabled   *bool    `yaml:"enabled"`
				Threshold *float64 `yaml:"disk_full_threshold"`
			} `yaml:"wal"`
		} `yaml:"ingester"`
	}
	if err = yaml.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	wal := cfg.Ingester.WAL
	off := wal.Enabled != nil && !*wal.Enabled
	unthrottled := wal.Threshold != nil && *wal.Threshold == 0
	if !off && !unthrottled {
		t.Error("config/loki.yaml leaves Loki's write-ahead log on with its disk threshold: " +
			"on a host whose disk is past it, every push is refused with \"Ingester is shutting down\"; " +
			"set ingester.wal.enabled to false, or ingester.wal.disk_full_threshold to 0")
	}
}

// TestTheHarnessRefusesALokiThatTakesNoWrite is the other half of that guard:
// whatever the next reason a Loki takes no write, the stack does not start on
// it. The answers are the ones Loki 3.7.7 gave with the log throttled: /ready
// said ready, and the push was a 500.
func TestTheHarnessRefusesALokiThatTakesNoWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name: "a throttled ingester", status: http.StatusInternalServerError,
			body: "rpc error: code = Unknown desc = Ingester is shutting down\n",
			want: "Ingester is shutting down",
		},
		{name: "a loki that takes the write", status: http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pushed := 0
			loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/ready":
					_, _ = io.WriteString(w, "ready\n")
				case "/loki/api/v1/push":
					pushed++
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				default:
					http.NotFound(w, r)
				}
			}))
			defer loki.Close()
			err := lokiReady(t.Context(), strings.TrimPrefix(loki.URL, "http://"))
			if pushed != 1 {
				t.Errorf("the probe pushed %d times, want once: /ready is not the question", pushed)
			}
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("a Loki that took the write was refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("err = %v, want Loki's own reason, %q", err, tc.want)
			}
		})
	}
}

// ── Reading Loki back ───────────────────────────────────────────────────────

// errNotYet is what a poll returns while the chunk it is waiting for has not
// been flushed, and it says how much of the stream is visible so far.
func errNotYet(reads []lokiRead) error {
	return fmt.Errorf("%d entries visible so far", len(reads))
}

// lokiRead is one line and the moment it describes.
type lokiRead struct {
	at   time.Time
	line string
}

// lokiRange queries one stream between two moments.
//
// The window is always the caller's, never "everything": Loki splits a range
// query into subqueries by time, so asking from 2017 to 2030 costs thousands
// of them and answers no faster for it.
func lokiRange(ctx context.Context, s *Stack, kind string, from, until time.Time) ([]lokiRead, error) {
	query := url.Values{}
	query.Set("query", `{job="ghchronicle",kind="`+kind+`"}`)
	query.Set("start", strconv.FormatInt(from.UnixNano(), 10))
	query.Set("end", strconv.FormatInt(until.UnixNano(), 10))
	query.Set("limit", "5000")
	query.Set("direction", "forward")
	var out struct {
		Data struct {
			Result []struct {
				Values [][2]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	where := s.LokiURL + "/loki/api/v1/query_range?" + query.Encode()
	if err := storeJSON(ctx, http.MethodGet, where, nil, &out); err != nil {
		return nil, err
	}
	var reads []lokiRead
	for _, stream := range out.Data.Result {
		for _, v := range stream.Values {
			nanos, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				continue
			}
			reads = append(reads, lokiRead{at: time.Unix(0, nanos).UTC(), line: v[1]})
		}
	}
	slices.SortFunc(reads, func(a, b lokiRead) int { return a.at.Compare(b.at) })
	return reads, nil
}

// lokiAwaitOne polls until the entry stamped at one moment is queryable, and
// returns its line. A push Loki answered 204 is not yet a query result: the
// entry becomes visible when its chunk is flushed.
func lokiAwaitOne(ctx context.Context, t *testing.T, s *Stack, kind, stamp string) string {
	t.Helper()
	at := lokiWhen(t, stamp)
	var line string
	err := WaitUntil(ctx, "the "+kind+" entry", 2*time.Minute, func(ctx context.Context) error {
		reads, err := lokiRange(ctx, s, kind, at.Add(-time.Minute), at.Add(time.Minute))
		if err != nil {
			return err
		}
		for _, r := range reads {
			if r.at.Equal(at) {
				line = r.line
				return nil
			}
		}
		return errNotYet(reads)
	})
	if err != nil {
		t.Fatalf("no %s entry is stamped %s: %v", kind, stamp, err)
	}
	return line
}

// lokiAwaitStamps polls until a stream holds at least as many entries as the
// oracle says it should, and returns the moments it holds.
func lokiAwaitStamps(ctx context.Context, t *testing.T, s *Stack, kind string, want []time.Time) []time.Time {
	t.Helper()
	if len(want) == 0 {
		t.Fatalf("the sweep produced nothing for the %s stream, so there is nothing to check", kind)
	}
	from, until := want[0].Add(-time.Minute), want[len(want)-1].Add(time.Minute)
	var got []time.Time
	err := WaitUntil(ctx, "the "+kind+" stream", 2*time.Minute, func(ctx context.Context) error {
		reads, err := lokiRange(ctx, s, kind, from, until)
		if err != nil {
			return err
		}
		got = got[:0]
		for _, r := range reads {
			got = append(got, r.at)
		}
		if len(got) < len(want) {
			return errNotYet(reads)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the %s stream never filled: %v", kind, err)
	}
	return got
}

// lokiAwaitKinds polls the label values until every stream the sweep should
// have created is there, and returns every stream the store holds.
func lokiAwaitKinds(ctx context.Context, t *testing.T, s *Stack, want map[string]bool) []string {
	t.Helper()
	var got []string
	err := WaitUntil(ctx, "loki's streams", 2*time.Minute, func(ctx context.Context) error {
		var out struct {
			Data []string `json:"data"`
		}
		// The label API reads the index rather than the chunks, so a wide
		// window costs nothing here. It is bounded all the same, because the
		// fixtures reach back to 2020 and forward to today.
		query := url.Values{}
		query.Set("start", strconv.FormatInt(time.Now().AddDate(-10, 0, 0).UnixNano(), 10))
		query.Set("end", strconv.FormatInt(time.Now().Add(time.Hour).UnixNano(), 10))
		if err := storeJSON(ctx, http.MethodGet,
			s.LokiURL+"/loki/api/v1/label/kind/values?"+query.Encode(), nil, &out); err != nil {
			return err
		}
		got = out.Data
		for kind := range want {
			if !slices.Contains(got, kind) {
				return errNotYet(nil)
			}
		}
		return nil
	})
	if err != nil {
		// Reported rather than fatal: which stream is missing is the finding,
		// and the subtests below name it.
		t.Errorf("not every stream became queryable: %v", err)
	}
	slices.Sort(got)
	return got
}

// lokiStamps is the moments the oracle says one measurement's points carry.
func lokiStamps(t *testing.T, sweep *pushSweep, measurement string) []time.Time {
	t.Helper()
	var out []time.Time
	for _, p := range pushPointsByMeasurement(t, sweep)[measurement] {
		out = append(out, lokiWhen(t, p.Time))
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out
}

func lokiWhen(t *testing.T, stamp string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatalf("the point's date %q: %v", stamp, err)
	}
	return at.UTC()
}

// lokiReadable renders a set of moments for a failure message.
func lokiReadable(stamps []time.Time) []string {
	out := make([]string, 0, len(stamps))
	for _, at := range stamps {
		out = append(out, at.Format(time.RFC3339Nano))
	}
	return out
}
