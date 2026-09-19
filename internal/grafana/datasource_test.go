package grafana

import (
	"encoding/json"
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

// asExisting is what Grafana would return for a datasource this had made.
func asExisting(d, secretOf Datasource) map[string]any {
	settings := map[string]any{}
	maps.Copy(settings, d.JSON)
	settings[fingerprintField] = fingerprint(secretOf.Secret)
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
	f := &dsServer{existing: asExisting(w, w)}
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
		{"the token, which is never readable", func(d *Datasource) {
			d.Secret = map[string]string{"token": "rotated"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stale := want()
			fresh := want()
			tc.break_(&fresh)
			f := &dsServer{existing: asExisting(stale, stale)}
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

// TestTheFingerprintIsNotTheToken: it has to change with the secret and it has
// to not carry it, since jsonData is readable by anyone who can read the
// datasource.
func TestTheFingerprintIsNotTheToken(t *testing.T) {
	t.Parallel()
	one := fingerprint(map[string]string{"token": "a-real-looking-secret"})
	two := fingerprint(map[string]string{"token": "a-real-looking-secre"})
	if one == two {
		t.Error("two different tokens fingerprint the same, so a rotation is invisible")
	}
	if strings.Contains(one, "a-real-looking") || len(one) != 12 {
		t.Errorf("fingerprint = %q, want twelve characters carrying none of the token", one)
	}
	if fingerprint(nil) != "" {
		t.Error("no secret should fingerprint as nothing, or every secretless datasource differs from itself")
	}
	// Moving the same value to another field is a change, because the plugin
	// reads the two fields for different things.
	if fingerprint(map[string]string{"a": "x"}) == fingerprint(map[string]string{"b": "x"}) {
		t.Error("the field a secret sits in is part of what it means")
	}
}
