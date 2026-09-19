package grafana

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// dsServer answers the datasource calls and records what it was asked to
// write, which is the half of reconciling that matters: whether a write
// happened at all, and under which verb.
type dsServer struct {
	existing map[string]any // what GET returns, nil for a 404
	writes   []dsWrite
}

type dsWrite struct {
	Method string
	Path   string
	Body   map[string]any
}

func (f *dsServer) serve(t *testing.T) Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
