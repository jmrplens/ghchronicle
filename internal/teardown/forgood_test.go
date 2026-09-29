package teardown

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
)

// influx3Release is an InfluxDB 3 of one release that holds tables, some of
// them deleted already, each with the hard deletion time its system table
// lists or none. It answers /ping the way 3.0.0 to 3.4.0 do, with the
// version in a header and no product name, renames a table it is told to
// delete, and, like 3.0.0 to 3.1.0, has no system table before 3.2 (all
// measured).
func influx3Release(t *testing.T, q *requests, release string, deleted map[string]string,
	tables ...string,
) *config.InfluxSink {
	t.Helper()
	var mu sync.Mutex
	before32 := strings.HasPrefix(release, "3.0.") || strings.HasPrefix(release, "3.1.")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q.note(r)
		mu.Lock()
		defer mu.Unlock()
		sql := r.URL.Query().Get("q")
		switch {
		case r.URL.Path == "/ping":
			w.Header().Set("X-Influxdb-Build", "Core")
			w.Header().Set("X-Influxdb-Version", release)
			_, _ = io.WriteString(w, `{"version":"`+release+`"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v3/configure/table":
			i := slices.Index(tables, r.URL.Query().Get("table"))
			if i < 0 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			tables[i] += "-20260929T101012"
		case r.URL.Query().Get("db") == "_internal" && before32:
			http.Error(w, "table 'public.system.tables' not found", http.StatusBadRequest)
		case r.URL.Query().Get("db") == "_internal" && sql == "SELECT table_name, hard_deletion_time FROM "+
			"system.tables WHERE database_name = 'github' AND deleted":
			rows := []map[string]string{}
			for name, until := range deleted {
				row := map[string]string{"table_name": name}
				if until != "" {
					row["hard_deletion_time"] = until
				}
				rows = append(rows, row)
			}
			_ = json.NewEncoder(w).Encode(rows)
		case r.URL.Path == "/api/v3/query_sql" && strings.HasPrefix(sql, "SELECT table_name FROM information_schema.tables"):
			rows := []map[string]string{}
			for _, name := range tables {
				rows = append(rows, map[string]string{"table_name": name})
			}
			_ = json.NewEncoder(w).Encode(rows)
		default:
			t.Errorf("InfluxDB %s was asked %s %s, which the fake does not answer", release, r.Method, r.URL)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	t.Cleanup(srv.Close)
	return &config.InfluxSink{URL: srv.URL, Token: "t", Bucket: "github"}
}

// TestOnlyAnInfluxDBBefore32KeepsWhatItDeletesForGood: the release /ping
// names decides it, whichever way the server names itself.
func TestOnlyAnInfluxDBBefore32KeepsWhatItDeletesForGood(t *testing.T) {
	t.Parallel()
	for server, want := range map[string]bool{
		"InfluxDB 3 Core 3.0.0":        true,
		"InfluxDB 3 Core 3.0.3":        true,
		"InfluxDB 3 Core 3.1.0":        true,
		"InfluxDB 3 3.1.0":             true,
		"InfluxDB 3 Core 3.2.0":        false,
		"InfluxDB 3 Core 3.10.0":       false,
		"InfluxDB 3 Enterprise 3.11.5": false,
		"InfluxDB 2 OSS 2.7.12":        false,
		"":                             false,
	} {
		if got := KeepsForGood(server); got != want {
			t.Errorf("KeepsForGood(%q) = %v, want %v", server, got, want)
		}
	}
}

// TestAnInfluxDBBefore32SaysTheCopyStaysAndWhatRemovesIt: 3.1.0 renames the
// table as every release does and never purges it, so the copy is the
// server's for good, and the request that removes it once the server runs a
// release that takes one is there to copy. No hard deletion time is asked of
// a server that has none.
func TestAnInfluxDBBefore32SaysTheCopyStaysAndWhatRemovesIt(t *testing.T) {
	t.Parallel()
	q := &requests{}
	sink := influx3Release(t, q, "3.1.0", nil, "gh_discussion_comment", "payments")
	store := &influx{sink: sink}
	aside, held, err := store.Clear(t.Context(), "gh_discussion_comment", clearedAt)
	if err != nil || !held {
		t.Fatalf("Clear = %+v, %v, %v", aside, held, err)
	}
	if aside.Name != "gh_discussion_comment-20260929T101012" || !aside.ByServer || !aside.ForGood || !aside.Until.IsZero() {
		t.Errorf("aside = %+v, want the copy kept for good by the server, with no purge time", aside)
	}
	for _, want := range []string{
		"InfluxDB 3 Core 3.1.0 has no hard deletion, so it never purges a table it deleted",
		"A release from 3.2 to 3.9 removes it when told to",
		"curl -X DELETE '" + sink.URL + "/api/v3/configure/table?db=github&table=gh_discussion_comment-20260929T101012" +
			"&hard_delete_at=now' -H 'Authorization: Bearer <token>'",
	} {
		if !strings.Contains(aside.Stays, want) {
			t.Errorf("Stays = %q, want it to say %q", aside.Stays, want)
		}
	}
	for _, r := range q.seen {
		if strings.Contains(r, "_internal") {
			t.Errorf("a server with no system table was asked %s", r)
		}
	}
}

// TestUnpurgedIsWhatNothingWillPurge: before 3.2 every copy stays; from 3.2
// on only a copy the system table lists with no hard deletion time, which an
// earlier release set aside, and what removes it depends on the release that
// answers now: from 3.2 to 3.9 a delete with hard_delete_at=now, from 3.10 on
// nothing. A table that only starts like a measurement, and one that is not
// a copy, are never listed.
func TestUnpurgedIsWhatNothingWillPurge(t *testing.T) {
	t.Parallel()
	const (
		legacy    = "gh_discussion_comment-20260901T000000"
		scheduled = "gh_discussion_comment-20260929T101012"
	)
	deleted := map[string]string{legacy: "", scheduled: "2026-10-02T10:10:12Z"}
	tables := []string{"gh_discussion_comment", legacy, scheduled, "gh_discussion_comment_x-20260901T000000", "payments"}
	for _, c := range []struct {
		release string
		want    []string
		says    string
		command bool
	}{
		{"3.1.0", []string{legacy, scheduled}, "InfluxDB 3 Core 3.1.0 has no hard deletion", true},
		{"3.4.0", []string{legacy}, "a release before 3.2 set it aside, so nothing is scheduled to purge it. " +
			"InfluxDB 3 Core 3.4.0 removes it when told to: ", true},
		{"3.11.2", []string{legacy}, "InfluxDB 3 Core 3.11.2 refuses to be told to, with or without hard_delete_at", false},
	} {
		t.Run(c.release, func(t *testing.T) {
			t.Parallel()
			q := &requests{}
			sink := influx3Release(t, q, c.release, deleted, slices.Clone(tables)...)
			found, err := (&influx{sink: sink}).Unpurged(t.Context(), []string{"gh_discussion_comment"})
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, a := range found {
				names = append(names, a.Name)
				if !a.ByServer || !a.ForGood || a.Measurement != "gh_discussion_comment" || a.At.IsZero() {
					t.Errorf("%s listed as %+v", a.Name, a)
				}
				if !strings.Contains(a.Stays, c.says) || strings.Contains(a.Stays, "hard_delete_at=now'") != c.command {
					t.Errorf("%s stays %q, want %q, with a command %v", a.Name, a.Stays, c.says, c.command)
				}
			}
			if !slices.Equal(names, c.want) {
				t.Errorf("unpurged %v, want %v", names, c.want)
			}
			if got := q.changes(); len(got) != 0 {
				t.Errorf("listing what stays changed %v", got)
			}
		})
	}
}

// TestAStayReadsWholeWhateverTheRelease: the sentence ends in the request or
// in a full stop, and a server whose release cannot be read is told what the
// releases measured do.
func TestAStayReadsWholeWhateverTheRelease(t *testing.T) {
	t.Parallel()
	sink := &config.InfluxSink{URL: "http://influx:8181/", Bucket: "git hub"}
	if got, want := InfluxStay("InfluxDB 3 Core 3.11.5", sink, "c").String(), "a release before 3.2 set it aside, "+
		"so nothing is scheduled to purge it. InfluxDB 3 Core 3.11.5 refuses to be told to, with or without "+
		"hard_delete_at, as every release from 3.10.0 to 3.11.5 was measured to, so no request removes it on this "+
		"release."; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got := InfluxStay("InfluxDB 3 Core 3.9.13", sink, "c").Command; got != "curl -X DELETE "+
		"'http://influx:8181/api/v3/configure/table?db=git+hub&table=c&hard_delete_at=now' -H 'Authorization: Bearer <token>'" {
		t.Errorf("Command = %q", got)
	}
	if got, want := InfluxStay("InfluxDB 3 Enterprise", sink, "c").String(), "a release before 3.2 set it aside, "+
		"so nothing is scheduled to purge it. A release from 3.2 to 3.9 removes it when told to: curl -X DELETE "+
		"'http://influx:8181/api/v3/configure/table?db=git+hub&table=c&hard_delete_at=now' -H 'Authorization: Bearer "+
		"<token>'"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
