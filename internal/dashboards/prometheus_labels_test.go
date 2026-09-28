package dashboards

import (
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/sink"
)

// TestPrometheusPanelsGroupByLabelsTheExporterKeeps is "Languages starred" in
// the 2.6.1 review: the panel summed github_stars_given_total by language,
// the exporter kept `user` alone on that measurement, and a grouping by a
// label no series carries is one series with no label at all, which the bar
// chart refused with "Bar charts require a string or time field". Nothing
// else would have caught it: the expression parses, and Prometheus answers
// it.
//
// So every label a Prometheus panel groups or matches by has to be one the
// exporter keeps on every metric of that expression. The exporter's own label
// sets are read off sink.Summarize, fed one point of every measurement with
// every tag the tag table lists and, as a field, every label the dashboard
// names, which is how a rule that reads a field as a label is given the
// chance to.
func TestPrometheusPanelsGroupByLabelsTheExporterKeeps(t *testing.T) {
	t.Parallel()
	exprs := prometheusExprs(t)
	named := map[string]bool{}
	for _, expr := range exprs {
		for _, l := range labelsNamed(expr) {
			named[l] = true
		}
	}
	families := exporterLabels(named)
	checked := 0
	for _, expr := range exprs {
		metrics := metricRef.FindAllString(expr, -1)
		for _, label := range labelsNamed(expr) {
			for _, m := range metrics {
				family, found := familyOf(families, m)
				if !found {
					continue // a metric no rule makes, which check_prometheus names
				}
				checked++
				if !families[family][label] {
					t.Errorf("%s is grouped or matched by %q, which the exporter does not keep on "+
						"it (it keeps %v), so the panel reads one series with no %s:\n%s",
						m, label, slices.Sorted(maps.Keys(families[family])), label, expr)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Prometheus panel groups by a label, so this checked nothing")
	}
}

var (
	metricRef = regexp.MustCompile(`\bgithub_[a-z0-9_]+`)
	// A label list after by, without, on, ignoring or group_left, and a
	// matcher inside a selector's braces.
	labelList = regexp.MustCompile(`\b(?:by|on|without|ignoring|group_left|group_right) \(([^)]*)\)`)
	matcher   = regexp.MustCompile(`([a-z_]+)\s*(?:=~|!~|!=|=)\s*"`)
	selector  = regexp.MustCompile(`\{[^}]*\}`)
)

// labelsNamed is every label an expression groups, joins or matches by.
func labelsNamed(expr string) []string {
	seen := map[string]bool{}
	for _, m := range labelList.FindAllStringSubmatch(expr, -1) {
		for l := range strings.SplitSeq(m[1], ",") {
			if l = strings.TrimSpace(l); l != "" {
				seen[l] = true
			}
		}
	}
	for _, braces := range selector.FindAllString(expr, -1) {
		for _, m := range matcher.FindAllStringSubmatch(braces, -1) {
			seen[m[1]] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}

// prometheusExprs is every expression of the Prometheus dashboard.
func prometheusExprs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, p := range renderedPanels(t, "prometheus") {
		targets, _ := p["targets"].([]any)
		for _, raw := range targets {
			target, _ := raw.(map[string]any)
			if expr, _ := target["expr"].(string); expr != "" {
				out = append(out, expr)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("the Prometheus dashboard has no expression")
	}
	return out
}

// exporterLabels is, per metric family, the name every metric of one reduced
// measurement starts with, the labels the exporter serves it with.
func exporterLabels(named map[string]bool) map[string]map[string]bool {
	var points []sink.Point
	for m, keys := range tags {
		p := sink.Point{
			Measurement: m, Tags: map[string]string{}, Fields: map[string]any{"value": 1.0},
			Time: time.Now(),
		}
		for _, k := range keys {
			p.Tags[k] = "v"
		}
		for l := range named {
			if _, isTag := p.Tags[l]; !isTag {
				p.Fields[l] = "v"
			}
		}
		points = append(points, p)
	}
	out := map[string]map[string]bool{}
	for _, g := range sink.Summarize(points) {
		family := "github_" + strings.TrimPrefix(g.Measurement, "gh_")
		if out[family] == nil {
			out[family] = map[string]bool{}
		}
		for k := range g.Tags {
			out[family][k] = true
		}
	}
	return out
}

// familyOf is the longest family a metric name belongs to: github_repo_total
// and not github_repo for github_repo_total_stars.
func familyOf(families map[string]map[string]bool, metric string) (string, bool) {
	best := ""
	for f := range families {
		if strings.HasPrefix(metric, f+"_") && len(f) > len(best) {
			best = f
		}
	}
	return best, best != ""
}
