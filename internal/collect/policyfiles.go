package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// PolicyFiles records which governance files a repository carries and when
// each of them last changed.
//
// gh_repo_policy already says whether most of these exist right now. What
// exists nowhere is when they changed, and .github/dependabot.yml is in no
// measurement at all, which is the gap that matters: a repository with
// Dependabot security updates switched off and no dependabot.yml receives no
// dependency updates by either route, and nothing collected today can ask that
// question. Five of the eighteen repositories are in that state.
//
// This is the only collector here that reaches the birth of a repository on
// the first sweep of a fresh install, without a backfill: the file history is
// a totalCount over the whole life of the path, and the date it carries is the
// last commit that touched it, however long ago that was.
//
// Cost, measured on 2026-09-10 over five repositories in one query: HTTP 200,
// rateLimit cost 1, 25 nodes, 2.5 s. The blob text is asked for only where it
// is parsed, because the same expression against copilot-instructions.md
// returns 25 KB.
type PolicyFiles struct {
	Repos []Repo
	// Batch is how many repositories go into one query. Zero means five.
	Batch int
}

// policyFile is one governance file, the paths GitHub honors for it, and
// whether its contents are worth carrying back.
type policyFile struct {
	name string
	// paths in GitHub's own precedence order. The first that exists is the one
	// in force; the rest are still asked about, because a file at the wrong
	// path is a file that does nothing.
	paths []string
	text  bool
}

// policyFileSet is the set asked about, and the order of the aliases in the
// query. CODEOWNERS is the reason a file has more than one path: GitHub reads
// it from .github/ first and from the root second, and only one of the two is
// in force.
var policyFileSet = []policyFile{
	{name: "dependabot", paths: []string{".github/dependabot.yml"}, text: true},
	{name: "codeowners", paths: []string{".github/CODEOWNERS", "CODEOWNERS"}},
	{name: "security", paths: []string{"SECURITY.md"}},
	{name: "funding", paths: []string{".github/FUNDING.yml"}},
}

// policyBlob is one path: the blob if it is there now, and the history of the
// path whether it is or not.
type policyBlob struct {
	path    string
	present bool
	bytes   int
	text    string
	changes int
	changed time.Time
}

// policyResponse is one repository's answer, indexed by the position of the
// path in the flattened list the aliases follow. The two halves are separate
// because the blob comes from the tree and the history comes from the branch
// tip: a path that was deleted has the second and not the first.
type policyResponse struct {
	files   map[int]*policyBlobNode
	history map[int]*policyHistoryNode
}

type policyBlobNode struct {
	ByteSize    int    `json:"byteSize"`
	Text        string `json:"text"`
	IsTruncated bool   `json:"isTruncated"`
}

type policyHistoryNode struct {
	TotalCount int `json:"totalCount"`
	Nodes      []struct {
		CommittedDate time.Time `json:"committedDate"`
	} `json:"nodes"`
}

// parsePolicyResponse reads the flat f<n> and h<n> aliases. They are siblings
// of nameWithOwner rather than a list, so the node arrives as raw keys and is
// walked here against the path list that generated them.
func parsePolicyResponse(raw map[string]json.RawMessage) policyResponse {
	res := policyResponse{files: map[int]*policyBlobNode{}, history: map[int]*policyHistoryNode{}}
	for i := range policyPaths() {
		var blob policyBlobNode
		if v, ok := raw["f"+strconv.Itoa(i)]; ok && string(v) != "null" {
			if err := json.Unmarshal(v, &blob); err == nil {
				res.files[i] = &blob
			}
		}
	}
	var branch *struct {
		Target map[string]*policyHistoryNode `json:"target"`
	}
	if v, ok := raw["defaultBranchRef"]; ok {
		if err := json.Unmarshal(v, &branch); err != nil || branch == nil {
			return res
		}
		for i := range policyPaths() {
			if h := branch.Target["h"+strconv.Itoa(i)]; h != nil {
				res.history[i] = h
			}
		}
	}
	return res
}

func (pf PolicyFiles) Collect(ctx context.Context, c *ghapi.Client, now time.Time) ([]sink.Point, error) {
	if len(pf.Repos) == 0 {
		return nil, nil
	}
	fragment := policyFragment()
	build := func(batch []Repo) string {
		return aliasQuery(batch, func(int) string { return "...policyfiles" }, fragment)
	}
	var points []sink.Point
	err := aliasBatch(ctx, c, pf.Repos, pf.Batch, build, func(repo Repo, node map[string]json.RawMessage) {
		points = append(points, policyPoints(repo, parsePolicyResponse(node), now)...)
	})
	if len(points) == 0 && err != nil {
		return nil, err
	}
	return points, nil
}

// policyFragment writes the fragment once for the paths in policyFileSet.
//
// The history field is written with a space before its arguments on purpose:
// the end-to-end fake picks a fixture by the first marker that appears in the
// query text, and `history(` is the commit walk's marker. This query is not
// that one.
func policyFragment() string {
	var b strings.Builder
	b.WriteString("\nfragment policyfiles on Repository {\n  nameWithOwner\n")
	for i, path := range policyPaths() {
		selection := "byteSize isTruncated"
		if path.text {
			selection += " text"
		}
		fmt.Fprintf(&b, "  f%d: object(expression: %q) { ... on Blob { %s } }\n", i, "HEAD:"+path.path, selection)
	}
	b.WriteString("  defaultBranchRef { target { ... on Commit {\n")
	for i, path := range policyPaths() {
		fmt.Fprintf(&b, "    h%d: history (first: 1, path: %q) { totalCount nodes { committedDate } }\n", i, path.path)
	}
	b.WriteString("  } } }\n}")
	return b.String()
}

// flatPath is one entry of the flattened path list the aliases follow.
type flatPath struct {
	file int
	path string
	text bool
}

func policyPaths() []flatPath {
	var out []flatPath
	for i, f := range policyFileSet {
		for _, p := range f.paths {
			out = append(out, flatPath{file: i, path: p, text: f.text})
		}
	}
	return out
}

func policyPoints(repo Repo, res policyResponse, now time.Time) []sink.Point {
	base := map[string]string{"owner": repo.Owner, "repo": repo.Name, "full_name": repo.FullName}
	blobs := readPolicyBlobs(res)

	var points []sink.Point
	for i, file := range policyFileSet {
		chosen := choosePolicyBlob(blobs[i])
		// A path that never existed has no date of its own. The start of the
		// UTC day is the same convention an open item uses: stamping it at the
		// instant of the sweep would write a fresh copy of "still absent"
		// every few hours.
		stamp := now.UTC().Truncate(24 * time.Hour)
		if !chosen.changed.IsZero() {
			stamp = chosen.changed
		}
		fields := map[string]any{
			"present": chosen.present,
			"bytes":   chosen.bytes,
			// How many times the path was touched in the whole life of the
			// repository. A file that is gone but has a history was deleted,
			// and this row is dated at the deletion.
			"changes": chosen.changes,
			// Where the file is, or where it would go: an absent file still
			// names the path it is absent from.
			"path": chosen.path,
		}
		if chosen.present {
			setNonEmpty(fields, "url", githubPage(repo.FullName, "blob", "HEAD", chosen.path))
		}
		if file.name == "dependabot" {
			ecosystems := dependabotEcosystems(chosen.text)
			fields["blocks"] = totalBlocks(ecosystems)
			fields["ecosystems"] = len(distinctEcosystems(ecosystems))
			points = append(points, ecosystemPoints(base, ecosystems, stamp)...)
		}
		points = append(points, sink.Point{
			Measurement: "gh_policy_file",
			Tags:        merge(base, map[string]string{"file": file.name}),
			Fields:      fields,
			Time:        stamp,
		})
	}
	return points
}

// readPolicyBlobs pairs each path with its blob and its history, grouped by
// the file they belong to.
func readPolicyBlobs(res policyResponse) map[int][]policyBlob {
	out := map[int][]policyBlob{}
	for i, path := range policyPaths() {
		found := policyBlob{path: path.path}
		if blob := res.files[i]; blob != nil {
			found.present, found.bytes = true, blob.ByteSize
			if !blob.IsTruncated {
				found.text = blob.Text
			}
		}
		if history := res.history[i]; history != nil {
			found.changes = history.TotalCount
			if len(history.Nodes) > 0 {
				found.changed = history.Nodes[0].CommittedDate
			}
		}
		out[path.file] = append(out[path.file], found)
	}
	return out
}

// choosePolicyBlob picks the path that speaks for the file.
//
// The one that exists wins, in GitHub's own precedence order. When none does,
// the one with a history wins, because that is a file that was deleted and the
// deletion is the fact worth dating: Cloudflare-DNS-Updater has no
// .github/dependabot.yml and two commits against that path, the last on
// 2025-12-29.
func choosePolicyBlob(found []policyBlob) policyBlob {
	if len(found) == 0 {
		return policyBlob{}
	}
	best := found[0]
	for _, b := range found[1:] {
		switch {
		case b.present && !best.present:
			best = b
		case b.present == best.present && b.changes > best.changes:
			best = b
		}
	}
	return best
}

// dependabotEcosystem is one update block's identity: what it updates and how
// often.
type dependabotEcosystem struct {
	ecosystem string
	interval  string
}

// dependabotEcosystems counts the update blocks of a dependabot.yml by
// ecosystem and schedule.
//
// Blocks are counted rather than listed because one ecosystem appears more
// than once when it is configured per directory: the busiest of these
// repositories has seven blocks across five ecosystems, all weekly.
func dependabotEcosystems(text string) map[dependabotEcosystem]int {
	if trimSpace(text) == "" {
		return nil
	}
	var doc struct {
		Updates []struct {
			PackageEcosystem string `yaml:"package-ecosystem"`
			Schedule         struct {
				Interval string `yaml:"interval"`
			} `yaml:"schedule"`
		} `yaml:"updates"`
	}
	if err := yaml.Unmarshal([]byte(text), &doc); err != nil {
		// A file that does not parse is a file Dependabot is not acting on
		// either, and the row above already records that it is there.
		return nil
	}
	out := map[dependabotEcosystem]int{}
	for _, u := range doc.Updates {
		out[dependabotEcosystem{
			ecosystem: orNone(u.PackageEcosystem),
			interval:  orNone(u.Schedule.Interval),
		}]++
	}
	return out
}

func ecosystemPoints(base map[string]string, ecosystems map[dependabotEcosystem]int, stamp time.Time) []sink.Point {
	points := make([]sink.Point, 0, len(ecosystems))
	for key, blocks := range ecosystems {
		points = append(points, sink.Point{
			Measurement: "gh_dependabot_ecosystem",
			Tags: merge(base, map[string]string{
				"ecosystem": key.ecosystem,
				"interval":  key.interval,
			}),
			Fields: map[string]any{"blocks": blocks},
			Time:   stamp,
		})
	}
	return points
}

func totalBlocks(ecosystems map[dependabotEcosystem]int) int {
	total := 0
	for _, n := range ecosystems {
		total += n
	}
	return total
}

func distinctEcosystems(ecosystems map[dependabotEcosystem]int) map[string]bool {
	out := map[string]bool{}
	for key := range ecosystems {
		out[key.ecosystem] = true
	}
	return out
}
