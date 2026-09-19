package grafana

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// PublishDashboard writes one dashboard document, overwriting whatever is at
// its uid. The document is expected to be one a server can take: every
// datasource chosen, and neither of the blocks that ask an importer to choose.
//
// It returns the path Grafana says the dashboard is at, which is what a person
// wants printed.
func (c Client) PublishDashboard(ctx context.Context, doc map[string]any,
	folderUID, message string, timeout time.Duration,
) (string, error) {
	body := map[string]any{
		"dashboard": doc,
		"overwrite": true,
		"message":   message,
	}
	if folderUID != "" {
		body["folderUid"] = folderUID
	}
	res, err := c.Do(ctx, http.MethodPost, "/api/dashboards/db", body, timeout)
	if err != nil {
		return "", err
	}
	if field(res.Body, "status") != "success" {
		return "", fmt.Errorf("publishing the dashboard: %s", answerText(res))
	}
	return field(res.Body, "url"), nil
}

// EnsureFolder turns a folder's title into its uid, making the folder when
// there is none by that title. An empty title means the default folder, which
// has no uid and needs no call.
//
// Grafana matches a search loosely, so the titles are compared here rather
// than the first hit being taken: a folder called "GitHub Chronicle" is not
// the folder called "GitHub" that somebody asked for.
func (c Client) EnsureFolder(ctx context.Context, title string,
	timeout time.Duration,
) (string, error) {
	if title == "" {
		return "", nil
	}
	res, err := c.Do(ctx, http.MethodGet, "/api/search?type=dash-folder&query="+
		url.QueryEscape(title), nil, timeout)
	if err != nil {
		return "", err
	}
	for _, raw := range res.List {
		hit, ok := raw.(map[string]any)
		if !ok || field(hit, "title") != title {
			continue
		}
		return field(hit, "uid"), nil
	}
	made, err := c.Do(ctx, http.MethodPost, "/api/folders",
		map[string]any{"title": title}, timeout)
	if err != nil {
		return "", err
	}
	if made.Status < 200 || made.Status > 299 {
		return "", fmt.Errorf("creating folder %q: %s", title, answerText(made))
	}
	return field(made.Body, "uid"), nil
}

// Exists says whether something answers at a uid, for dashboards and for
// datasources alike. A 404 is the ordinary answer and not an error.
func (c Client) Exists(ctx context.Context, path string, timeout time.Duration) (bool, error) {
	res, err := c.Do(ctx, http.MethodGet, path, nil, timeout)
	if err != nil {
		return false, err
	}
	switch {
	case res.Status == http.StatusNotFound:
		return false, nil
	case res.Status >= 200 && res.Status <= 299:
		return true, nil
	default:
		return false, fmt.Errorf("asking about %s: %s", path, answerText(res))
	}
}

// DashboardPath is where a dashboard answers to its uid.
func DashboardPath(uid string) string { return "/api/dashboards/uid/" + uid }

// DatasourcePath is where a datasource answers to its uid.
func DatasourcePath(uid string) string { return datasourceByUID + uid }
