package grafana

import (
	"encoding/json"
	"errors"
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

// TestAHealthCheckTheTokenMayNotMakeIsARefusal, not a datasource that cannot
// reach its store: that answer sends the reader to change an address that was
// right all along.
func TestAHealthCheckTheTokenMayNotMakeIsARefusal(t *testing.T) {
	t.Parallel()
	a := &answering{reply: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"You'll need additional permissions to perform `+
			`this action. Permissions needed: datasources:query"}`)
	}}
	ok, _, err := a.serve(t).DatasourceHealth(t.Context(), "uid", 5*time.Second)
	refused, isRefused := errors.AsType[*RefusedError](err)
	if ok || !isRefused {
		t.Fatalf("ok = %v, err = %v, want a refusal", ok, err)
	}
	if refused.Permission != "datasources:query" {
		t.Errorf("permission = %q, want datasources:query", refused.Permission)
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

// TestOutcomeReadsAsALogLineWants.
func TestOutcomeReadsAsALogLineWants(t *testing.T) {
	t.Parallel()
	for outcome, want := range map[Outcome]string{
		Created: "created", Updated: "updated", Unchanged: "unchanged", Adopted: "adopted",
		Outcome(9): "unchanged",
	} {
		if got := outcome.String(); got != want {
			t.Errorf("Outcome(%d) = %q, want %q", outcome, got, want)
		}
	}
}

// TestExistsTellsThereFromNotThereFromCannotTell. A 404 is the ordinary answer
// and not a failure; anything else that is not a success is, because treating
// a server error as "not there" would report a leftover as gone.
func TestExistsTellsThereFromNotThereFromCannotTell(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  int
		want    bool
		wantErr bool
	}{
		{"it is there", http.StatusOK, true, false},
		{"it is not", http.StatusNotFound, false, false},
		{"the server could not say", http.StatusInternalServerError, false, true},
		// A reply with nothing to quote still has to name the failure, or the
		// note printed about it says only that something went wrong.
		{"and could not say why", http.StatusBadGateway, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := `{"message":"said"}`
			if tc.status == http.StatusBadGateway {
				body = `{}`
			}
			a := &answering{reply: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, body)
			}}
			got, err := a.serve(t).Exists(t.Context(), DashboardPath("uid"), 5*time.Second)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want an error: %v", err, tc.wantErr)
			}
			if tc.status == http.StatusBadGateway && !strings.Contains(err.Error(), "502") {
				t.Errorf("err = %v, want the status where there is no message to quote", err)
			}
			if got != tc.want {
				t.Errorf("exists = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestThePathsAreTheOnesGrafanaAnswersAt, spelled once so the leftover check
// and the reconciler cannot drift apart.
func TestThePathsAreTheOnesGrafanaAnswersAt(t *testing.T) {
	t.Parallel()
	if got := DashboardPath("x"); got != "/api/dashboards/uid/x" {
		t.Errorf("DashboardPath = %q", got)
	}
	if got := DatasourcePath("x"); got != "/api/datasources/uid/x" {
		t.Errorf("DatasourcePath = %q", got)
	}
}

// TestAServerThatIsNotThereIsReportedRatherThanMistakenForAnAnswer.
func TestAServerThatIsNotThereIsReportedRatherThanMistakenForAnAnswer(t *testing.T) {
	t.Parallel()
	client := Client{URL: "http://127.0.0.1:1", Token: "t"}
	if _, err := client.EnsureFolder(t.Context(), "GitHub", time.Second); err == nil {
		t.Error("EnsureFolder reported a folder from a server that is not listening")
	}
	if _, err := client.PublishDashboard(t.Context(),
		map[string]any{}, "", "m", time.Second); err == nil {
		t.Error("PublishDashboard reported a success from a server that is not listening")
	}
	if _, err := client.EnsureDatasource(t.Context(),
		Datasource{UID: "u", Type: "influxdb"}, time.Second); err == nil {
		t.Error("EnsureDatasource reported an outcome from a server that is not listening")
	}
	if _, _, err := client.DatasourceHealth(t.Context(), "u", time.Second); err == nil {
		t.Error("DatasourceHealth reported health from a server that is not listening")
	}
}

// TestADatasourceGrafanaRefusesToWriteIsReported, not swallowed into an
// outcome that says it was created.
func TestADatasourceGrafanaRefusesToWriteIsReported(t *testing.T) {
	t.Parallel()
	a := &answering{reply: func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"not found"}`)
			return
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"message":"data source with the same name already exists"}`)
	}}
	_, err := a.serve(t).EnsureDatasource(t.Context(),
		Datasource{UID: "u", Type: "influxdb", Name: "n"}, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "same name already exists") {
		t.Errorf("err = %v, want what Grafana refused", err)
	}
}

// TestADatasourceNeedsAUIDAndAType before anything is sent, since a request
// without them would create something nothing here could find again.
func TestADatasourceNeedsAUIDAndAType(t *testing.T) {
	t.Parallel()
	a := &answering{reply: func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}}
	client := a.serve(t)
	for _, want := range []Datasource{{Type: "influxdb"}, {UID: "u"}} {
		if _, err := client.EnsureDatasource(t.Context(), want, 5*time.Second); err == nil {
			t.Errorf("%+v was accepted", want)
		}
	}
	if len(a.asked) != 0 {
		t.Errorf("it called %v before checking what it had", a.asked)
	}
}

// TestRemovingSomethingThatIsAlreadyGoneIsNotAFailure. Deleting is how the
// collector clears a dashboard that a rename left behind, and the run that
// clears it happens to be the one that already cleared it on the previous
// start. A 404 there is the wanted end state, not an error to stop on.
func TestRemovingSomethingThatIsAlreadyGoneIsNotAFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"it was there", http.StatusOK, false},
		{"it had already gone", http.StatusNotFound, false},
		{"Grafana would not", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := &answering{reply: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"message":"no"}`)
			}}
			err := g.serve(t).Delete(t.Context(), DatasourcePath("ghchronicle-influx"),
				2*time.Second)
			if tc.wantErr != (err != nil) {
				t.Fatalf("Delete() error = %v, want an error: %v", err, tc.wantErr)
			}
			if len(g.asked) != 1 ||
				!strings.HasPrefix(g.asked[0], "DELETE ") ||
				!strings.HasSuffix(g.asked[0], "ghchronicle-influx") {
				t.Errorf("it asked for %v, want one DELETE of that datasource", g.asked)
			}
		})
	}
}
