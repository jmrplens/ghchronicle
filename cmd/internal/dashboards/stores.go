package dashboards

import (
	"encoding/json"
	"maps"
	"strings"
)

// Store is everything that differs between the five dashboards but the panels:
// the placeholder datasource an export carries, the uid, the prose at the top,
// and the repository variable each store has to be asked for in its own
// language.
type Store struct {
	Name        string
	DS          string
	UID         string
	File        string
	Title       string
	Description string
	Inputs      []any
	Requires    []any
	Variable    map[string]any
}

// esFind is the Elasticsearch variable query: which field to list the terms
// of, and how many.
type esFind struct {
	Field string `json:"field"`
	Find  string `json:"find"`
	Query string `json:"query"`
	Size  int    `json:"size"`
}

func grafanaRequires(id, name string) []any {
	return []any{
		map[string]any{"type": "grafana", "id": "grafana", "name": "Grafana", "version": "11.0.0"},
		map[string]any{"type": "datasource", "id": id, "name": name, "version": "1.0.0"},
	}
}

func input(name, label, description, pluginID, pluginName string) []any {
	return []any{map[string]any{
		"name": name, "label": label, "description": description,
		"type": "datasource", "pluginId": pluginID, "pluginName": pluginName,
	}}
}

func variable(ds any, definition string, query any, extra map[string]any) map[string]any {
	v := map[string]any{
		"name": "repo", "label": "Repository", "type": "query",
		"datasource": ds, "multi": true, "includeAll": true,
		"refresh": 1, "sort": 1, "definition": definition, "query": query,
		"current": map[string]any{
			"selected": true, "text": []any{"All"}, "value": []any{"$__all"},
		},
		"options": []any{}, "hide": 0,
	}
	maps.Copy(v, extra)
	return v
}

// storeDescLead opens the description of all five dashboards: only the tail,
// naming the store each one reads, differs.
const storeDescLead = "Every metric GitHub will give about an account, kept with the date it "

// AllStores is every dashboard this generates, in the order the files are
// written.
func AllStores() []Store {
	influxRepos := "SELECT DISTINCT repo FROM gh_repo WHERE time > now() - INTERVAL '7 days'"
	pgRepos := "SELECT DISTINCT repo FROM gh_repo WHERE time > now() - INTERVAL '7 days' ORDER BY 1"
	promRepos := "label_values(github_repo_stars, repo)"
	// The repository node of gh_repo: every other node is a wildcard, and the
	// find query returns the names at the last one.
	grRepos := gp("gh_repo", "*")
	if i := strings.LastIndex(grRepos, "."); i >= 0 {
		grRepos = grRepos[:i]
		if j := strings.LastIndex(grRepos, "."); j >= 0 {
			grRepos = grRepos[:j]
		}
	}
	// A named type rather than a map, so the encoder is given nothing it can
	// refuse. The field order is the order encoding/json emits a map's keys
	// in, which is what the committed dashboards carry.
	esRepos, err := json.Marshal(esFind{
		Field: "repo.keyword", Find: "terms",
		Query: "_index:" + idx("gh_repo"), Size: 500,
	})
	if err != nil {
		// Four strings and an int: there is nothing here the encoder can
		// refuse, so this is a programming error like the others below.
		panic("encoding the Elasticsearch variable query: " + err.Error())
	}

	return []Store{
		{
			Name: "influxdb", DS: "${DS_INFLUXDB}", UID: "ghchronicle-influxdb",
			File: "ghchronicle-influxdb.json", Title: "GitHub Chronicle (InfluxDB)",
			Description: storeDescLead +
				"happened. Collected by ghchronicle.",
			Inputs: input("DS_INFLUXDB", "InfluxDB",
				"The InfluxDB 3 database ghchronicle writes to, queried with SQL.",
				"influxdb", "InfluxDB"),
			Requires: grafanaRequires("influxdb", "InfluxDB"),
			// A variable is not a panel target, and the InfluxDB plugin reads
			// the opposite key here: its metricFindQuery builds the request
			// out of the variable object's `query` and ignores `rawSql`
			// (grafana/grafana 13.2.1, public/plugins/influxdb/module.js:
			// `{refId: "metricFindQuery", query: e.query, rawQuery: true,
			// ...(SQL ? {rawSql: e.query, format: Table} : {})}`). Dropping
			// `query` here empties the repository list with no error, so both
			// keys stay: `query` because the plugin reads it, `rawSql` because
			// that is the shape its own variable editor writes back.
			Variable: variable("${DS_INFLUXDB}", influxRepos, map[string]any{
				"query": influxRepos, "rawQuery": true, "rawSql": influxRepos, "format": "table",
			}, nil),
		},
		{
			Name: "prometheus", DS: "${DS_PROMETHEUS}", UID: "ghchronicle-prometheus",
			File: "ghchronicle-prometheus.json", Title: "GitHub Chronicle (Prometheus)",
			Description: "Every metric GitHub will give about an account, from ghchronicle's " +
				"exporter. Prometheus stamps samples at scrape time, so dated history starts " +
				"the day the exporter did; each panel says what that means for it.",
			Inputs: input("DS_PROMETHEUS", "Prometheus",
				"The Prometheus that scrapes ghchronicle's exporter.",
				"prometheus", "Prometheus"),
			Requires: grafanaRequires("prometheus", "Prometheus"),
			Variable: variable("${DS_PROMETHEUS}", promRepos,
				map[string]any{"query": promRepos, "refId": "repo"},
				map[string]any{"allValue": ".*"}),
		},
		{
			Name: "postgres", DS: "${DS_POSTGRES}", UID: "ghchronicle-postgres",
			File: "ghchronicle-postgres.json", Title: "GitHub Chronicle (PostgreSQL)",
			Description: storeDescLead +
				"happened, in the tables the ghchronicle SQL sink writes.",
			Inputs: input("DS_POSTGRES", "PostgreSQL",
				"The PostgreSQL or TimescaleDB database the SQL sink's statements were "+
					"piped into.",
				"grafana-postgresql-datasource", "PostgreSQL"),
			Requires: grafanaRequires("grafana-postgresql-datasource", "PostgreSQL"),
			Variable: variable("${DS_POSTGRES}", pgRepos, map[string]any{
				"rawSql": pgRepos, "rawQuery": true, "format": "table",
				"editorMode": "code", "refId": "repo",
			}, nil),
		},
		{
			Name: "graphite", DS: "${DS_GRAPHITE}", UID: "ghchronicle-graphite",
			File: "ghchronicle-graphite.json", Title: "GitHub Chronicle (Graphite)",
			Description: storeDescLead +
				"happened, from the paths the ghchronicle Graphite sink writes.",
			Inputs: input("DS_GRAPHITE", "Graphite",
				"The Graphite the sink writes to, with the default prefix `github`.",
				"graphite", "Graphite"),
			Requires: grafanaRequires("graphite", "Graphite"),
			// `multiFormat` is the pre-scenes name of the same choice: several
			// selected repositories become one brace list in the path.
			Variable: variable("${DS_GRAPHITE}", grRepos, grRepos,
				map[string]any{"allValue": "*", "multiFormat": "glob"}),
		},
		{
			Name: "elasticsearch", DS: "${DS_ELASTICSEARCH}", UID: "ghchronicle-elasticsearch",
			File: "ghchronicle-elasticsearch.json", Title: "GitHub Chronicle (Elasticsearch)",
			Description: storeDescLead +
				"happened, from the documents the ghchronicle Elasticsearch sink writes.",
			Inputs: input("DS_ELASTICSEARCH", "Elasticsearch",
				"The Elasticsearch or OpenSearch datasource pointing at `ghchronicle-*`, "+
					"with `@timestamp` as its time field.",
				"elasticsearch", "Elasticsearch"),
			Requires: grafanaRequires("elasticsearch", "Elasticsearch"),
			Variable: variable("${DS_ELASTICSEARCH}", string(esRepos), string(esRepos),
				map[string]any{"allValue": "*"}),
		},
	}
}

// Build renders one store's whole dashboard, bound to its metrics store
// alone: the shape the exported files have.
func (s *Store) Build(ds any) map[string]any {
	return s.BuildWith(ds, nil)
}

// BuildWith renders the dashboard with a log store beside the metrics store:
// `logs` is the Loki datasource the job log panel is drawn from, or nil for
// the text panel in its place. Nothing else in the document changes, so a
// dashboard published with a Loki lays out exactly as the file does.
func (s *Store) BuildWith(ds, logs any) map[string]any {
	if ds == nil {
		ds = s.DS
	}
	v := map[string]any{}
	maps.Copy(v, s.Variable)
	v["datasource"] = ds
	return Dashboard(s, ds, logs, v)
}

// ByName is the store of that name, and whether there is one. Every command
// that takes a store on the command line looks it up this way, so they all
// spell the name the same and none of them copies the list to search it.
func ByName(name string) (*Store, bool) {
	stores := AllStores()
	for i := range stores {
		if stores[i].Name == name {
			return &stores[i], true
		}
	}
	return nil, false
}

// Names lists every store, in the order the files are written, for the usage
// lines that offer the choice.
func Names() []string {
	stores := AllStores()
	out := make([]string, len(stores))
	for i := range stores {
		out[i] = stores[i].Name
	}
	return out
}
