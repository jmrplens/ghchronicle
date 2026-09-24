package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
)

// TestListMarksTheArchivedRepositoriesSetAside is the -list half of the
// archive: the repositories the filter set aside for being archived follow
// the collected ones, marked, because a sweep still writes the one row each
// has and a backfill collects them in full. An archived fork under the
// default fork rule is not set aside, so it is not listed either.
func TestListMarksTheArchivedRepositoriesSetAside(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/user/repos" || r.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"name":"n","full_name":"o/n","owner":{"login":"o"}},` +
			`{"name":"old","full_name":"o/old","archived":true,"owner":{"login":"o"}},` +
			`{"name":"oldfork","full_name":"o/oldfork","archived":true,"fork":true,"owner":{"login":"o"}}]`))
	}))
	t.Cleanup(gh.Close)
	api := ghapi.New("test-token", 10*time.Second)
	api.SetBaseURL(gh.URL)
	cfg := &config.Config{Targets: config.Targets{User: "o"}}

	var out bytes.Buffer
	if err := listRepositories(t.Context(), api, cfg, &out); err != nil {
		t.Fatal(err)
	}
	want := "o/n\no/old (archived: the archive date only; a backfill collects it)\n"
	if out.String() != want {
		t.Errorf("-list printed:\n%s\nwant:\n%s", out.String(), want)
	}
}

// TestListCollectsThePrivateRepositoriesUnlessRefused is the sweep's half of
// the same discovery: private repositories are collected unless the config
// refuses them. A bare bool made the absent key mean false, and an account
// whose work is mostly private collected a fraction of itself with nothing to
// say so.
func TestListCollectsThePrivateRepositoriesUnlessRefused(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/user/repos" || r.URL.Query().Get("page") != "1" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"name":"n","full_name":"o/n","owner":{"login":"o"}},` +
			`{"name":"hidden","full_name":"o/hidden","private":true,"owner":{"login":"o"}}]`))
	}))
	t.Cleanup(gh.Close)
	api := ghapi.New("test-token", 10*time.Second)
	api.SetBaseURL(gh.URL)
	refused := false

	for _, tc := range []struct {
		name    string
		targets config.Targets
		want    string
	}{
		{"the key absent", config.Targets{User: "o"}, "o/n\no/hidden\n"},
		{"include_private: false", config.Targets{User: "o", IncludePrivate: &refused}, "o/n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := listRepositories(t.Context(), api, &config.Config{Targets: tc.targets}, &out); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Errorf("-list printed:\n%swant:\n%s", out.String(), tc.want)
			}
		})
	}
}
