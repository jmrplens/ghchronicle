package grafana

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// dsServer answers the datasource calls and records what it was asked to
// write, which is the half of reconciling that matters: whether a write
// happened at all, and under which verb.
type dsServer struct {
	existing map[string]any // what GET returns, nil for a 404
	// listed is what GET /api/datasources returns, and listStatus replaces it
	// with a refusal when it is not zero.
	listed     []map[string]any
	listStatus int
	writes     []dsWrite
	asked      []string
}

type dsWrite struct {
	Method string
	Path   string
	Body   map[string]any
}

func (f *dsServer) serve(t *testing.T) Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.asked = append(f.asked, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet && r.URL.Path == "/api/datasources" {
			if f.listStatus != 0 {
				w.WriteHeader(f.listStatus)
				_, _ = io.WriteString(w, `{"message":"You'll need additional permissions to `+
					`perform this action. Permissions needed: datasources:read"}`)
				return
			}
			listed := f.listed
			if listed == nil {
				listed = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(listed)
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/health") {
			_, _ = io.WriteString(w, `{"status":"OK","message":"1 measurement found"}`)
			return
		}
		if r.Method == http.MethodGet {
			if f.existing == nil {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"message":"Data source not found"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(f.existing)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.writes = append(f.writes, dsWrite{Method: r.Method, Path: r.URL.Path, Body: body})
		_, _ = io.WriteString(w, `{"datasource":{"uid":"x"},"message":"ok"}`)
	}))
	t.Cleanup(srv.Close)
	return Client{URL: srv.URL, Token: "t"}
}

// want is the datasource the tests reconcile towards.
func want() Datasource {
	return Datasource{
		UID: "ghchronicle-influxdb", Name: "ghchronicle-influxdb",
		Type: "influxdb", URL: "http://influx:8181", Database: "github",
		JSON:   map[string]any{"version": "SQL"},
		Secret: map[string]string{"token": "first"},
	}
}

// asExisting is what Grafana would return for a datasource this had made. The
// secret is not in it, because Grafana never hands one back.
func asExisting(d Datasource) map[string]any {
	settings := map[string]any{}
	maps.Copy(settings, d.JSON)
	return map[string]any{
		"uid": d.UID, "name": d.Name, "type": d.Type,
		"url": d.URL, "database": d.Database, "jsonData": settings,
	}
}

// TestEnsureDatasourceCreatesTheOneThatIsNotThere: a 404 is not a failure, it
// is the case this exists for, and what it posts is a complete datasource.
func TestEnsureDatasourceCreatesTheOneThatIsNotThere(t *testing.T) {
	t.Parallel()
	f := &dsServer{}
	client := f.serve(t)
	outcome, err := client.EnsureDatasource(t.Context(), want(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Created {
		t.Errorf("outcome = %s, want created", outcome)
	}
	if len(f.writes) != 1 || f.writes[0].Method != http.MethodPost {
		t.Fatalf("writes = %+v, want one POST", f.writes)
	}
	body := f.writes[0].Body
	if body["uid"] != "ghchronicle-influxdb" || body["access"] != "proxy" {
		t.Errorf("body = %v, want the uid asked for and proxy access", body)
	}
	secret, _ := body["secureJsonData"].(map[string]any)
	if secret["token"] != "first" {
		t.Errorf("the token was not sent: %v", body["secureJsonData"])
	}
}

// TestEnsureDatasourceLeavesTheOneThatAlreadySaysThis alone. A reconciler that
// writes every time is a reconciler that bumps a version on every start and
// tells a reader nothing by doing it.
func TestEnsureDatasourceLeavesTheOneThatAlreadySaysThis(t *testing.T) {
	t.Parallel()
	w := want()
	w.Secret = nil
	f := &dsServer{existing: asExisting(w)}
	client := f.serve(t)
	outcome, err := client.EnsureDatasource(t.Context(), w, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Unchanged {
		t.Errorf("outcome = %s, want unchanged", outcome)
	}
	if len(f.writes) != 0 {
		t.Errorf("it wrote anyway: %+v", f.writes)
	}
}

// TestEnsureDatasourceCorrectsWhatDiffers, one field at a time, including the
// one Grafana never hands back. A rotated token with every visible field the
// same is the case the fingerprint is for: without it the datasource goes on
// using a credential the config has already replaced.
func TestEnsureDatasourceCorrectsWhatDiffers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		break_ func(*Datasource)
	}{
		{"the address", func(d *Datasource) { d.URL = "http://elsewhere:8181" }},
		{"the database", func(d *Datasource) { d.Database = "other" }},
		{"a plugin setting", func(d *Datasource) { d.JSON = map[string]any{"version": "Flux"} }},
		{"the name", func(d *Datasource) { d.Name = "renamed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stale := want()
			stale.Secret = nil
			fresh := want()
			fresh.Secret = nil
			tc.break_(&fresh)
			f := &dsServer{existing: asExisting(stale)}
			client := f.serve(t)
			outcome, err := client.EnsureDatasource(t.Context(), fresh, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if outcome != Updated {
				t.Fatalf("outcome = %s, want updated", outcome)
			}
			if len(f.writes) != 1 || f.writes[0].Method != http.MethodPut {
				t.Fatalf("writes = %+v, want one PUT", f.writes)
			}
		})
	}
}

// TestADatasourceWithASecretIsWrittenEveryTime. Grafana reports which secret
// keys are set and never their values, so a token rotated in the config while
// every visible field stayed the same is invisible from here. Writing it every
// time is what keeps the credential current; the reconcile runs at most once
// per process start, so the saved request was never worth the alternative.
func TestADatasourceWithASecretIsWrittenEveryTime(t *testing.T) {
	t.Parallel()
	w := want()
	f := &dsServer{existing: asExisting(w)}
	client := f.serve(t)
	outcome, err := client.EnsureDatasource(t.Context(), w, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Updated {
		t.Errorf("outcome = %s, want updated: the secret cannot be compared, so it is sent", outcome)
	}
	if len(f.writes) != 1 {
		t.Fatalf("writes = %+v, want the secret written", f.writes)
	}
	secret, _ := f.writes[0].Body["secureJsonData"].(map[string]any)
	if secret["token"] != "first" {
		t.Errorf("secureJsonData = %v, want the token sent again", secret)
	}
}

// TestNothingDerivedFromTheSecretIsWrittenWhereItCanBeRead. jsonData is
// readable by anyone who can read the datasource, so a hash of the token kept
// there to detect a rotation would put something derived from a credential in
// front of every viewer. CodeQL called an earlier version of this out and it
// was right to.
func TestNothingDerivedFromTheSecretIsWrittenWhereItCanBeRead(t *testing.T) {
	t.Parallel()
	const token = "a-distinctive-token-value"
	w := want()
	w.Secret = map[string]string{"token": token}
	f := &dsServer{}
	client := f.serve(t)
	if _, err := client.EnsureDatasource(t.Context(), w, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	settings, _ := f.writes[0].Body["jsonData"].(map[string]any)
	for key, value := range settings {
		text := fmt.Sprint(value)
		if strings.Contains(text, token) {
			t.Errorf("jsonData.%s carries the token itself", key)
		}
		for _, digest := range []string{
			hex.EncodeToString(sha256Of(token)),
			hex.EncodeToString(sha256Of("ghchronicle datasource secret\x00token\x00" + token + "\x00")),
		} {
			if strings.Contains(text, digest[:12]) {
				t.Errorf("jsonData.%s carries a digest of the token", key)
			}
		}
	}
}

// sha256Of is the digest an earlier version of this stored, kept only so the
// test above can look for it.
func sha256Of(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// lokiWant is a Loki datasource with no tenant, which is its address and
// nothing else: the one kind this adopts.
func lokiWant() Datasource {
	return Datasource{
		UID: "ghchronicle-loki", Name: "ghchronicle-loki", Type: "loki", URL: "http://loki:3100",
	}
}

// theirs is a datasource somebody else made, as the list endpoint shows it.
func theirs(uid, kind, address string, settings map[string]any) map[string]any {
	if settings == nil {
		settings = map[string]any{}
	}
	return map[string]any{
		"uid": uid, "name": "their-" + kind, "type": kind, "url": address,
		"access": "proxy", "jsonData": settings,
	}
}

// TestADatasourceAlreadyAtTheSameAddressIsAdopted rather than joined by a
// second one, and adopting writes nothing, so a token that may read
// datasources and may not create them gets as far as this. The trailing slash
// is how Grafana's own form often saves an address, and it is the same server.
func TestADatasourceAlreadyAtTheSameAddressIsAdopted(t *testing.T) {
	t.Parallel()
	f := &dsServer{listed: []map[string]any{
		theirs("pr0m", "prometheus", "http://loki:3100", nil),
		theirs("cfbntsncufta8f", "loki", "http://LOKI:3100/", nil),
	}}
	uid, outcome, err := f.serve(t).EnsureDatasourceOrAdopt(t.Context(), lokiWant(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Adopted || uid != "cfbntsncufta8f" {
		t.Errorf("got %s %q, want the existing Loki datasource adopted", outcome, uid)
	}
	if len(f.writes) != 0 {
		t.Errorf("adopting wrote %+v, want nothing written", f.writes)
	}
}

// TestOnlyADatasourceThatSaysTheSameIsAdopted. The production case that
// started this had a Loki datasource at the address Grafana reaches Loki by on
// its container network, while the sink pushed to a published port: another
// address, so another datasource as far as anything here can tell, and made.
func TestOnlyADatasourceThatSaysTheSameIsAdopted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		there map[string]any
		want  func() Datasource
	}{
		{
			"another address",
			theirs("cfbntsncufta8f", "loki", "http://192.168.0.40:50104", nil),
			lokiWant,
		},
		{"another type", theirs("pr0m", "prometheus", "http://loki:3100", nil), lokiWant},
		{
			"a setting this would write, set otherwise",
			theirs("pr0m", "prometheus", "http://prometheus:9090", map[string]any{"httpMethod": "GET"}),
			func() Datasource {
				return Datasource{
					UID: "ghchronicle-prometheus", Name: "ghchronicle-prometheus",
					Type: "prometheus", URL: "http://prometheus:9090",
					JSON: map[string]any{"httpMethod": "POST"},
				}
			},
		},
		{"no uid to adopt", theirs("", "loki", "http://loki:3100", nil), lokiWant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &dsServer{listed: []map[string]any{tc.there}}
			want := tc.want()
			uid, outcome, err := f.serve(t).EnsureDatasourceOrAdopt(t.Context(), want, 5*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if outcome != Created || uid != want.UID {
				t.Errorf("got %s %q, want %s created", outcome, uid, want.UID)
			}
			if len(f.writes) != 1 || f.writes[0].Method != http.MethodPost {
				t.Errorf("writes = %+v, want one POST", f.writes)
			}
		})
	}
}

// TestADatasourceThatNeedsASecretIsNeverAdopted. Grafana never hands a secret
// back, and a secret is where two datasources at one address differ: a Loki
// tenant, a token with other rights. So it is made, and the list is not even
// asked for.
func TestADatasourceThatNeedsASecretIsNeverAdopted(t *testing.T) {
	t.Parallel()
	want := lokiWant()
	want.JSON = map[string]any{"httpHeaderName1": "X-Scope-OrgID"}
	want.Secret = map[string]string{"httpHeaderValue1": "tenant-one"}
	f := &dsServer{listed: []map[string]any{
		theirs("cfbntsncufta8f", "loki", "http://loki:3100", map[string]any{
			"httpHeaderName1": "X-Scope-OrgID",
		}),
	}}
	_, outcome, err := f.serve(t).EnsureDatasourceOrAdopt(t.Context(), want, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Created {
		t.Errorf("outcome = %s, want created", outcome)
	}
	if slices.Contains(f.asked, "GET /api/datasources") {
		t.Errorf("it listed datasources for one it could never adopt: %v", f.asked)
	}
}

// TestAListThatIsRefusedFallsThroughToCreating, which is what ran before
// anything looked. Whatever the create meets, it reports itself.
func TestAListThatIsRefusedFallsThroughToCreating(t *testing.T) {
	t.Parallel()
	f := &dsServer{listStatus: http.StatusForbidden}
	uid, outcome, err := f.serve(t).EnsureDatasourceOrAdopt(t.Context(), lokiWant(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Created || uid != "ghchronicle-loki" {
		t.Errorf("got %s %q, want ghchronicle-loki created", outcome, uid)
	}
}

// TestTheDatasourceThisMadeIsKeptOverOneAtTheSameAddress. Only a datasource
// this has not made yet is looked for elsewhere, so one somebody adds later at
// the same address never takes the dashboard over from it.
func TestTheDatasourceThisMadeIsKeptOverOneAtTheSameAddress(t *testing.T) {
	t.Parallel()
	own := lokiWant()
	f := &dsServer{
		existing: asExisting(own),
		listed:   []map[string]any{theirs("cfbntsncufta8f", "loki", own.URL, nil)},
	}
	uid, outcome, err := f.serve(t).EnsureDatasourceOrAdopt(t.Context(), own, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Unchanged || uid != own.UID {
		t.Errorf("got %s %q, want its own left unchanged", outcome, uid)
	}
	if slices.Contains(f.asked, "GET /api/datasources") {
		t.Errorf("it looked elsewhere for a datasource it already has: %v", f.asked)
	}
}

// TestTwoThatWouldDoAdoptTheSameOneEveryRun, so consecutive restarts do not
// repoint the dashboard between them in whatever order Grafana lists them.
func TestTwoThatWouldDoAdoptTheSameOneEveryRun(t *testing.T) {
	t.Parallel()
	for _, order := range [][]string{{"zeta", "alpha"}, {"alpha", "zeta"}} {
		f := &dsServer{listed: []map[string]any{
			theirs(order[0], "loki", "http://loki:3100", nil),
			theirs(order[1], "loki", "http://loki:3100", nil),
		}}
		uid, _, err := f.serve(t).EnsureDatasourceOrAdopt(t.Context(), lokiWant(), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if uid != "alpha" {
			t.Errorf("listed %v, adopted %q, want alpha whatever the order", order, uid)
		}
	}
}

// TestARefusalNamesThePermissionTheCallNeeded. The three were measured on
// Grafana 13.2.1 with an Editor service account and one with no role: a read
// of a datasource wants datasources:read, a create datasources:create and an
// update datasources:write. Anything else that fails is not a refusal, since
// no permission would have changed it.
func TestARefusalNamesThePermissionTheCallNeeded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		existing   map[string]any
		refuse     string
		status     int
		permission string
	}{
		{"reading", nil, http.MethodGet, http.StatusForbidden, "datasources:read"},
		{"creating", nil, http.MethodPost, http.StatusForbidden, "datasources:create"},
		{"updating", asExisting(want()), http.MethodPut, http.StatusForbidden, "datasources:write"},
		{"a conflict, which is no refusal", nil, http.MethodPost, http.StatusConflict, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &answering{reply: func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == tc.refuse:
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, `{"message":"You'll need additional permissions"}`)
				case r.Method == http.MethodGet && tc.existing == nil:
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"message":"Data source not found"}`)
				case r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(tc.existing)
				default:
					_, _ = io.WriteString(w, `{"message":"ok"}`)
				}
			}}
			_, err := a.serve(t).EnsureDatasource(t.Context(), want(), 5*time.Second)
			if err == nil {
				t.Fatal("the refusal was not reported")
			}
			var refused *RefusedError
			if got := errors.As(err, &refused); got != (tc.permission != "") {
				t.Fatalf("err = %v (%T), want a refusal: %v", err, err, tc.permission != "")
			}
			if refused != nil && refused.Permission != tc.permission {
				t.Errorf("permission = %q, want %q", refused.Permission, tc.permission)
			}
			if !strings.Contains(err.Error(), "additional permissions") {
				t.Errorf("err = %v, want Grafana's own words kept", err)
			}
		})
	}
}

// TestDatasourcesReadsTheList in the fields a comparison needs.
func TestDatasourcesReadsTheList(t *testing.T) {
	t.Parallel()
	f := &dsServer{listed: []map[string]any{
		theirs("cfbntsncufta8f", "loki", "http://loki:3100", map[string]any{"maxLines": 1000}),
	}}
	all, err := f.serve(t).Datasources(t.Context(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("read %d datasources, want 1", len(all))
	}
	got := all[0]
	if got.UID != "cfbntsncufta8f" || got.Name != "their-loki" || got.Type != "loki" ||
		got.URL != "http://loki:3100" || fmt.Sprint(got.JSON["maxLines"]) != "1000" {
		t.Errorf("read %+v", got)
	}
	f = &dsServer{listStatus: http.StatusForbidden}
	_, err = f.serve(t).Datasources(t.Context(), 5*time.Second)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Permission != "datasources:read" {
		t.Errorf("err = %v, want a refusal naming datasources:read", err)
	}
}

// TestSameAddressReadsAnAddressTheWayAPersonWould.
func TestSameAddressReadsAnAddressTheWayAPersonWould(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"http://loki:3100", "http://loki:3100", true},
		{"http://loki:3100/", "http://loki:3100", true},
		{"HTTP://Loki:3100", "http://loki:3100", true},
		{"http://loki:3100/prefix/", "http://loki:3100/prefix", true},
		{"http://loki:3100", "https://loki:3100", false},
		{"http://loki:3100", "http://loki:3101", false},
		{"http://loki:3100", "http://192.168.0.40:50104", false},
		{"http://loki:3100/a", "http://loki:3100/b", false},
		// A PostgreSQL datasource's address is host:port with no scheme, which
		// url.Parse reads as a scheme and an opaque port.
		{"db:5432", "db:5432", true},
		{"db:5432", "db:5433", false},
		{"", "", false},
	} {
		if got := sameAddress(tc.a, tc.b); got != tc.same {
			t.Errorf("sameAddress(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}
