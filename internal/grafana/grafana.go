// Package grafana is the little bit of Grafana this repository needs: where
// the server is, how to post a query to it, how to walk a dashboard document
// panel by panel, and how to render a panel's query the way a dashboard render
// would before posting it.
//
// Its readers are the collector itself, which publishes the dashboard and the
// datasource it reads from, the development commands under cmd/, and the
// containerised end-to-end suite under test/, which runs those same panels
// against real stores in Docker.
//
// The dashboards are built in memory by internal/dashboards and committed
// as JSON under dashboards/, so a panel arrives either as a map the builder
// produced or as one encoding/json decoded. Everything here therefore works on
// `any` and accepts both shapes: the []map[string]any the builder produces and
// the []any a decoded file produces.
package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// DefaultURL is the Grafana these commands talk to unless GRAFANA_URL says
// otherwise. Grafana's own default address, because the only thing a default
// can know is what the software it talks to ships with: a Grafana that lives
// anywhere else is named by the environment variable, which is the whole
// reason it exists.
const DefaultURL = "http://localhost:3000"

// Client is one Grafana, and what opens it.
type Client struct {
	URL   string
	Token string
	// User and Password are the other way in, for a Grafana that has no
	// service account yet. A server brought up beside this one by a compose
	// file is the case: it exists for the first time when the collector first
	// asks, so there was nobody to make a token, and its admin credentials are
	// the only thing that can be arranged in advance. A token is preferred
	// wherever there is one, because it can be scoped and this cannot.
	User     string
	Password string
}

// New reads the address and the token from the environment. The token is only
// required by the calls that need it, so a missing one is not an error here.
func New() Client {
	url := os.Getenv("GRAFANA_URL")
	if url == "" {
		url = DefaultURL
	}
	return Client{URL: url, Token: os.Getenv("GRAFANA_TOKEN")}
}

// Post sends one JSON request and decodes the answer. A 4xx or 5xx is not an
// error: Grafana reports a rejected query in the body, and that body is what
// the caller wants to read.
//
// The timeout is the request's whole life, deadline and all, carried on the
// context rather than on a client of its own, so a caller that gives up early
// takes the request down with it.
func (c Client) Post(ctx context.Context, path string, body any, timeout time.Duration) (map[string]any, error) {
	res, err := c.Do(ctx, http.MethodPost, path, body, timeout)
	return res.Body, err
}

// Response is one answer, with the status the server gave it. Post throws the
// status away because the calls it serves read their outcome out of the body,
// but the datasource calls cannot: a datasource that is not there answers 404
// with a message shaped like any other failure, and "not there" is the case
// that decides between creating one and correcting one.
type Response struct {
	Status int
	Body   map[string]any
	// List holds the answer instead when the server sent an array. /api/search
	// is one: a reply that is a list at the top level decodes into no map at
	// all, and reading it as a failed decode would turn every search into an
	// error about a body that is perfectly well formed.
	List []any
}

// Do sends one JSON request under any method and hands back both halves of the
// answer. A 4xx or 5xx is not an error here, for Post's reason: Grafana says
// what it refused in the body, and that body is what the caller wants to read.
func (c Client) Do(ctx context.Context, method, path string, body any,
	timeout time.Duration,
) (Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return Response{}, err
		}
		reader = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, reader)
	if err != nil {
		return Response{}, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case c.Token != "":
		req.Header.Set("Authorization", "Bearer "+c.Token)
	case c.User != "":
		req.SetBasicAuth(c.User, c.Password)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return Response{}, err
	}
	defer res.Body.Close()
	answer, err := io.ReadAll(res.Body)
	if err != nil {
		return Response{}, err
	}
	if len(bytes.TrimSpace(answer)) == 0 {
		return Response{Status: res.StatusCode, Body: map[string]any{}}, nil
	}
	var out any
	if json.Unmarshal(answer, &out) != nil {
		return Response{Status: res.StatusCode},
			fmt.Errorf("%s: %s", res.Status, trim(string(answer), 200))
	}
	switch decoded := out.(type) {
	case map[string]any:
		return Response{Status: res.StatusCode, Body: decoded}, nil
	case []any:
		return Response{Status: res.StatusCode, Body: map[string]any{}, List: decoded}, nil
	default:
		return Response{Status: res.StatusCode, Body: map[string]any{}}, nil
	}
}

// Query posts one datasource query.
func (c Client) Query(ctx context.Context, from, to string, query map[string]any,
	timeout time.Duration,
) (map[string]any, error) {
	return c.Queries(ctx, from, to, []any{query}, timeout)
}

// Queries posts a whole panel's worth the way a render does: one request
// carrying every target, because a server-side expression target reads the
// others out of the same request by refId and fails on its own.
func (c Client) Queries(ctx context.Context, from, to string, queries []any,
	timeout time.Duration,
) (map[string]any, error) {
	return c.Post(ctx, "/api/ds/query", map[string]any{
		"from": from, "to": to, "queries": queries,
	}, timeout)
}

// Panel is one panel of a dashboard and one of its targets.
type Panel struct {
	Title  string
	Target map[string]any
}

// Walk yields every target of every panel, descending into collapsed rows,
// which carry their own panels.
func Walk(panels any) []Panel {
	var out []Panel
	for _, raw := range list(panels) {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if p["type"] == "row" {
			out = append(out, Walk(p["panels"])...)
			continue
		}
		title, _ := p["title"].(string)
		for _, t := range list(p["targets"]) {
			if tgt, isObject := t.(map[string]any); isObject {
				out = append(out, Panel{Title: title, Target: tgt})
			}
		}
	}
	return out
}

// list accepts either of the two shapes a panel list arrives in.
func list(v any) []any {
	switch l := v.(type) {
	case []any:
		return l
	case []map[string]any:
		out := make([]any, len(l))
		for i, m := range l {
			out[i] = m
		}
		return out
	default:
		return nil
	}
}

// Frames counts the rows of every frame one query returned, and reports the
// error Grafana attached to it, wherever it put it.
func Frames(res map[string]any, ref string) (rows int, errText string) {
	results, _ := res["results"].(map[string]any)
	a, _ := results[ref].(map[string]any)
	// Grafana documents this field as a string, but a datasource that hands
	// back a structured one must still be reported as a failure: a checker
	// that calls an error it cannot read a success is the one wrong answer.
	if v := a["error"]; truthy(v) {
		errText = text(v)
	} else if m := res["message"]; truthy(m) {
		errText = text(m)
	}
	for _, f := range list(a["frames"]) {
		frame, _ := f.(map[string]any)
		data, _ := frame["data"].(map[string]any)
		values := list(data["values"])
		if len(values) > 0 {
			rows += len(list(values[0]))
		}
	}
	return rows, errText
}

// Column reads one column of the first frame a query returned, as strings.
func Column(res map[string]any, ref string, n int) ([]string, error) {
	results, _ := res["results"].(map[string]any)
	a, _ := results[ref].(map[string]any)
	frames := list(a["frames"])
	if len(frames) == 0 {
		return nil, errors.New("no frames in the answer")
	}
	frame, _ := frames[0].(map[string]any)
	data, _ := frame["data"].(map[string]any)
	values := list(data["values"])
	if n >= len(values) {
		return nil, fmt.Errorf("column %d is not in the answer", n)
	}
	var out []string
	for _, v := range list(values[n]) {
		out = append(out, fmt.Sprint(v))
	}
	return out, nil
}

// truthy is the test the Python these commands replace applied to the error
// field: an absent, empty or zero value falls through to the next candidate.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case float64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	default:
		return true
	}
}

// text renders whatever the field held, so a structured error still says
// something rather than nothing.
func text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// trim shortens a message for a one-line report. It counts characters and not
// bytes, so a message with an accent in it is cut where it reads and never in
// the middle of a character.
func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Trim is trim, for the commands that print what Grafana said.
func Trim(s string, n int) string { return trim(s, n) }
