package collect

import (
	"slices"
	"strings"
)

// Where a row's `url` field comes from, and the one rule every one of them
// obeys.
//
// Around fifty measurements carry a `url`, and it is the only field in this
// tree whose whole purpose is to be clicked: every dashboard table selects it
// as a hidden Link column and hangs the row's link on its first column,
// reading the value back through `${__data.fields.Link}`. A value that is not
// a url is a link to the wrong place, and a relative one sends the reader's
// browser to the Grafana host rather than to GitHub, with no error anywhere
// to say so.
//
// Most of these urls are not returned by GitHub, they are built by appending a
// path to something GitHub returned, and what GitHub returns is not always
// there: a repository body with no `html_url` turns `html_url + "/community"`
// into `/community`, which is exactly that link. So the concatenation happens
// here and nowhere else, and every function below answers with the empty
// string when any part it needs is missing. Callers pair that with
// setNonEmpty, so a row either carries a page or says nothing about one.

// pageURL is a page below one GitHub gave us: pageURL(repo.HTMLURL,
// "community"). Segments are joined with the separator, so a caller passes the
// path in pieces or in one piece, whichever reads better.
func pageURL(base string, path ...string) string {
	if base == "" || len(path) == 0 || slices.Contains(path, "") {
		return ""
	}
	return base + "/" + strings.Join(path, "/")
}

// githubPage is a page on github.com addressed by the path below the host:
// githubPage(repo.FullName, "security", "dependabot"). An empty segment makes
// the whole url empty rather than a shorter one, since a missing owner would
// otherwise silently address a different page.
func githubPage(path ...string) string {
	return pageURL("https://github.com", path...)
}

// githubRootedPage is a page GitHub named by an absolute path of its own, as
// the traffic API does with "/owner/repo/blob/main/README.md". The leading
// slash is what says the value is a path at all; without it there is nothing
// to build a url out of.
func githubRootedPage(path string) string {
	if !strings.HasPrefix(path, "/") {
		return ""
	}
	return "https://github.com" + path
}

// withURL writes the row's page onto its fields and writes nothing when there
// is no page. It returns the map so it can wrap a field literal where the
// point is built, rather than forcing a variable on every collector.
func withURL(fields map[string]any, url string) map[string]any {
	setNonEmpty(fields, "url", url)
	return fields
}
