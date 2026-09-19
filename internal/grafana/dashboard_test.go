package grafana

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// answering is a Grafana that replies as told and remembers every path it was
// asked for, so a test can say what was and was not called.
type answering struct {
	reply  func(w http.ResponseWriter, r *http.Request)
	asked  []string
	bodies []map[string]any
}

func (a *answering) serve(t *testing.T) Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.asked = append(a.asked, r.Method+" "+r.URL.Path)
		if r.Body != nil {
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) == nil {
				a.bodies = append(a.bodies, body)
			}
		}
		a.reply(w, r)
	}))
	t.Cleanup(srv.Close)
	return Client{URL: srv.URL, Token: "t"}
}

// TestHealthTellsTheThreeAnswersApart. A datasource that answers, one that
// cannot reach its store, and a plugin with no health check at all are three
// different things, and calling the third one a pass would defeat the check.
func TestHealthTellsTheThreeAnswersApart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		ok     bool
		says   string
	}{
		{"it answers", http.StatusOK, `{"status":"OK","message":"1 measurement"}`, true, "1 measurement"},
		{
			"it cannot reach the store", http.StatusBadRequest,
			`{"status":"ERROR","message":"dial tcp: no such host"}`, false, "no such host",
		},
		{"the plugin has no check", http.StatusNotFound, `{}`, true, "no health check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &answering{reply: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}}
			ok, message, err := a.serve(t).DatasourceHealth(t.Context(), "uid", 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tc.ok {
				t.Errorf("ok = %v, want %v (%s)", ok, tc.ok, message)
			}
			if !strings.Contains(message, tc.says) {
				t.Errorf("message = %q, want it to carry %q", message, tc.says)
			}
		})
	}
}

// TestEnsureFolderComparesTheTitleRatherThanTakingTheFirstHit: Grafana's
// search matches loosely, so the folder called "GitHub Chronicle" comes back
// for a search for "GitHub" and is not it.
func TestEnsureFolderComparesTheTitleRatherThanTakingTheFirstHit(t *testing.T) {
	t.Parallel()
	a := &answering{}
	a.reply = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `[{"uid":"loose","title":"GitHub Chronicle"}]`)
			return
		}
		_, _ = io.WriteString(w, `{"uid":"made"}`)
	}
	client := a.serve(t)
	uid, err := client.EnsureFolder(t.Context(), "GitHub", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if uid != "made" {
		t.Errorf("uid = %q, want the folder it created rather than the loose match", uid)
	}
}

// TestEnsureFolderTakesTheOneThatIsAlreadyThere, and makes nothing.
func TestEnsureFolderTakesTheOneThatIsAlreadyThere(t *testing.T) {
	t.Parallel()
	a := &answering{reply: func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[{"uid":"exact","title":"GitHub"}]`)
	}}
	uid, err := a.serve(t).EnsureFolder(t.Context(), "GitHub", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if uid != "exact" {
		t.Errorf("uid = %q, want the folder that was there", uid)
	}
	for _, call := range a.asked {
		if strings.HasPrefix(call, http.MethodPost) {
			t.Errorf("it created a folder anyway: %v", a.asked)
		}
	}
}

// TestNoFolderAsksNothing: the default folder has no uid, and a call to find
// it would be a call about nothing.
func TestNoFolderAsksNothing(t *testing.T) {
	t.Parallel()
	a := &answering{reply: func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	}}
	client := a.serve(t)
	uid, err := client.EnsureFolder(t.Context(), "", 5*time.Second)
	if err != nil || uid != "" {
		t.Fatalf("uid = %q, err = %v, want both empty", uid, err)
	}
	if len(a.asked) != 0 {
		t.Errorf("it called %v for a folder nobody named", a.asked)
	}
}

// TestPublishDashboardReportsWhatGrafanaRefused rather than reporting success
// off a reply that never said so.
func TestPublishDashboardReportsWhatGrafanaRefused(t *testing.T) {
	t.Parallel()
	a := &answering{reply: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPreconditionFailed)
		_, _ = io.WriteString(w, `{"status":"version-mismatch","message":"someone else saved it"}`)
	}}
	_, err := a.serve(t).PublishDashboard(t.Context(),
		map[string]any{"uid": "x"}, "", "message", 5*time.Second)
	if err == nil {
		t.Fatal("a refused publish was reported as a success")
	}
	if !strings.Contains(err.Error(), "someone else saved it") {
		t.Errorf("err = %v, want it to carry what Grafana said", err)
	}
}
