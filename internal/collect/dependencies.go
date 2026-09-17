package collect

import (
	"context"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/internal/ghapi"
	"github.com/jmrplens/ghchronicle/internal/sink"
)

// Dependencies collects what a repository depends on, and what changed.
//
// Two endpoints that answer different questions:
//
//   - The SBOM is a photograph: every package present today, with its license.
//     It is one call and 1.8 MB on a repository with two thousand packages, so
//     only the aggregate is kept. A row per package would be a series per
//     package version and would say nothing a license histogram does not.
//   - The dependency diff is the change: which packages entered and left
//     between two commits, with the license each brought and the advisory each
//     one carried. A single dependency bump measured 370 changes, one of them
//     removing a package with a known advisory. Again only the aggregate is
//     kept, six rows per range rather than three hundred and seventy.
//
// Both are off by default. The SBOM has its own rate limit bucket of a hundred
// an hour, and neither is worth a call on an account that does not care about
// its dependency graph.
type Dependencies struct {
	// Base is the commit the diff starts from, usually the head of the last
	// sweep. Empty means no diff, only the photograph.
	Base string
	// Head is where the diff ends. Empty means the default branch.
	Head string
	// SBOM asks for the photograph as well as the diff.
	SBOM bool
	// ResolvedHead is the commit the diff ended at, for the caller to store as
	// the base of the next one.
	ResolvedHead string
	// Refusals remembers the repositories whose dependency graph is off, so
	// the SBOM's 404 is paid once a day from its own bucket of a hundred a
	// minute rather than on every sweep. The compare endpoint is not
	// remembered: its path names a fresh range each time. Nil asks every time.
	Refusals *Refusals
}

func (d *Dependencies) Collect(ctx context.Context, c *ghapi.Client, repo Repo, now time.Time) ([]sink.Point, error) {
	base := repoTags(repo.Owner, repo.Name)
	var points []sink.Point

	head := d.Head
	if head == "" {
		resolved, err := defaultBranchHead(ctx, c, repo)
		if err != nil {
			return points, err
		}
		head = resolved
	}
	moved := d.Base == "" || head == "" || d.Base != head

	// The photograph is read when the default branch moved, and on the first
	// sweep, which has no base to compare against. GitHub regenerates the SBOM
	// on every request, so its ETag never matches and each read is charged
	// from that bucket of a hundred a minute, while the head is a 40 byte
	// answer that is a free 304 when nothing changed: a repository without a
	// commit since the last sweep has the same packages it had, and asking
	// again would only spend the bucket to be told so. ResolvedHead stays
	// empty until the photograph is taken, so a failed read is asked again
	// next sweep.
	if d.SBOM && moved {
		pts, err := sbomPoints(ctx, c, d.Refusals, repo, base, now.UTC().Truncate(24*time.Hour))
		if err != nil {
			return points, err
		}
		points = append(points, pts...)
	}

	d.ResolvedHead = head
	if head == "" {
		// The head could not be read, so there is no range and nothing to
		// say about one.
		return points, nil
	}
	if d.Base == "" || !moved {
		// No range yet, or nothing has moved since the last one: a row that
		// says so, rather than no row, for the reason on noDependencyChange.
		return append(points, noDependencyChange(base, d.Base, head, now)), nil
	}
	changes, err := dependencyChangePoints(ctx, c, repo, base, d.Base, head, now)
	if err != nil {
		return points, err
	}
	return append(points, changes...), nil
}

// noDependencyChange is the row a sweep writes when there is nothing to
// report: no range yet, a head that did not move, or a compare that came
// back empty.
//
// A table that is only ever written when something changed does not exist
// until something does, and InfluxDB 3 answers a query naming a table it
// has not seen with an error, not with no rows. Measured on 2026-09-12: the
// Dependency changes panel was an error on every time range after a day of
// sweeps, because no default branch had carried a dependency change yet. One
// zero row per sweep is what makes the table exist from the first sweep,
// under a `change` of (none) the panels leave out. It costs no request: two
// of the three cases never reach the compare endpoint at all.
func noDependencyChange(base map[string]string, from, to string, now time.Time) sink.Point {
	fields := map[string]any{"packages": 0, "vulnerable": 0, "head": to}
	// The first sweep has no base to name; a field left out is what the
	// sinks write for a string they do not have.
	setNonEmpty(fields, "base", from)
	return sink.Point{
		Measurement: "gh_dependency_change",
		Tags:        merge(base, map[string]string{"change": noneTag, "ecosystem": noneTag}),
		Fields:      fields,
		Time:        now,
	}
}

// sbomPackage is one entry of the repository's software bill of materials.
type sbomPackage struct {
	Name             string `json:"name"`
	VersionInfo      string `json:"versionInfo"`
	LicenseConcluded string `json:"licenseConcluded"`
	ExternalRefs     []struct {
		ReferenceLocator string `json:"referenceLocator"`
	} `json:"externalRefs"`
}

// sbomPoints is the photograph: how many packages there are, by ecosystem and
// by license. A repository with no dependency graph yields none of it.
func sbomPoints(ctx context.Context, c *ghapi.Client, m *Refusals, repo Repo, base map[string]string, day time.Time) ([]sink.Point, error) {
	var res struct {
		SBOM struct {
			Packages []sbomPackage `json:"packages"`
		} `json:"sbom"`
	}
	if _, _, err := m.GetJSON(ctx, c, repoPathPrefix+repo.FullName+"/dependency-graph/sbom", &res, ""); err != nil {
		if isSkippable(err) {
			return nil, nil
		}
		return nil, err
	}
	byEcosystem, byLicense := map[string]int{}, map[string]int{}
	for i := range res.SBOM.Packages {
		p := &res.SBOM.Packages[i]
		byEcosystem[ecosystemOf(p.ExternalRefs)]++
		license := p.LicenseConcluded
		if license == "" || license == "NOASSERTION" {
			// Not "unknown license" as a fact about the package: it is what
			// GitHub says when it could not determine one, and counting them
			// is how a reader spots a blind spot.
			license = "undetermined"
		}
		byLicense[license]++
	}
	var points []sink.Point
	for ecosystem, n := range byEcosystem {
		points = append(points, sink.Point{
			Measurement: "gh_dependency",
			Tags:        merge(base, map[string]string{"ecosystem": ecosystem}),
			Fields:      map[string]any{"packages": n},
			Time:        day,
		})
	}
	for license, n := range byLicense {
		points = append(points, sink.Point{
			Measurement: "gh_dependency_license",
			Tags:        merge(base, map[string]string{"license": license}),
			Fields:      map[string]any{"packages": n},
			Time:        day,
		})
	}
	return points, nil
}

// shaMediaType asks the commit endpoint for the bare SHA. It is the one
// narrow representation REST offers: the default form of a single commit is
// its author, tree, stats and file diff, and the one-commit listing this used
// to read was five and a half kilobytes for forty characters.
const shaMediaType = "application/vnd.github.sha"

// defaultBranchHead is the head of the default branch, which is both the end
// of this range and the start of the next one.
//
// One call, and the only way to know where the diff stopped: the compare
// endpoint answers with changes, not with refs. HEAD is the default branch on
// GitHub's side, so the branch name need not be known here. An empty answer
// means the range cannot be computed yet, which is not a failure.
func defaultBranchHead(ctx context.Context, c *ghapi.Client, repo Repo) (string, error) {
	sha, err := c.GetTextAs(ctx, repoPathPrefix+repo.FullName+"/commits/HEAD", shaMediaType)
	if err != nil {
		if isSkippable(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(sha), nil
}

// dependencyChangePoints is what the dependency graph says moved between two
// commits.
//
// Aggregated by what changed and where, never a row per package: one
// dependency bump is three hundred and seventy of them.
func dependencyChangePoints(ctx context.Context, c *ghapi.Client, repo Repo,
	base map[string]string, from, to string, now time.Time,
) ([]sink.Point, error) {
	var changes []struct {
		ChangeType      string `json:"change_type"`
		Ecosystem       string `json:"ecosystem"`
		Name            string `json:"name"`
		License         string `json:"license"`
		Scope           string `json:"scope"`
		Vulnerabilities []struct {
			Severity string `json:"severity"`
		} `json:"vulnerabilities"`
	}
	path := repoPathPrefix + repo.FullName + "/dependency-graph/compare/" + from + "..." + to
	if _, _, err := c.GetJSON(ctx, path, &changes, ""); err != nil {
		if isSkippable(err) {
			return nil, nil
		}
		return nil, err
	}
	type key struct{ change, ecosystem string }
	counts := map[key]int{}
	vulnerable := map[key]int{}
	for _, ch := range changes {
		k := key{ch.ChangeType, ch.Ecosystem}
		counts[k]++
		if len(ch.Vulnerabilities) > 0 {
			vulnerable[k]++
		}
	}
	if len(counts) == 0 {
		// A real range with no dependency in it, which is most commits.
		return []sink.Point{noDependencyChange(base, from, to, now)}, nil
	}
	var points []sink.Point
	for k, n := range counts {
		points = append(points, sink.Point{
			Measurement: "gh_dependency_change",
			Tags: merge(base, map[string]string{
				"change": k.change, "ecosystem": k.ecosystem,
			}),
			Fields: map[string]any{
				"packages": n, "vulnerable": vulnerable[k],
				"base": from, "head": to,
			},
			Time: now,
		})
	}
	return points, nil
}

// ecosystemOf reads the package manager out of an SPDX purl, which is the only
// place the SBOM says it: "pkg:npm/left-pad@1.3.0" is npm.
func ecosystemOf(refs []struct {
	ReferenceLocator string `json:"referenceLocator"`
},
) string {
	for _, r := range refs {
		if rest, ok := strings.CutPrefix(r.ReferenceLocator, "pkg:"); ok {
			if name, _, found := strings.Cut(rest, "/"); found {
				return name
			}
			return rest
		}
	}
	return noneTag
}
