package sink

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Telegraf posts line protocol to Telegraf's http_listener_v2 input.
//
// It is the sink for reaching everything Telegraf can reach. Telegraf has an
// output for Kafka, Datadog, New Relic, Graphite, Loki, Elasticsearch and a
// hundred more, and rather than grow a sink here for each of them, the points
// go to Telegraf as the line protocol it already speaks and its own
// configuration routes them onward. Telegraf keeps the timestamps as given, so
// the dated history survives as far as its outputs allow.
type Telegraf struct {
	// URL is the listener, path included. A bare host gets Telegraf's
	// default path, /telegraf.
	URL      string
	Username string
	Password string
	Batch    int
	client   *http.Client
}

// NewTelegraf returns a sink. A zero batch means 5000 lines per request.
func NewTelegraf(rawURL, username, password string, batch int, timeout time.Duration) *Telegraf {
	if batch <= 0 {
		batch = 5000
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &Telegraf{
		URL: withDefaultPath(rawURL, "/telegraf"), Username: username, Password: password,
		Batch: batch, client: &http.Client{Timeout: timeout},
	}
}

// withDefaultPath fills in the path when the URL names only the host, because
// a listener answers 404 to "/" and the message does not say why.
func withDefaultPath(rawURL, path string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = path
	}
	return u.String()
}

func (t *Telegraf) Name() string { return "telegraf" }
func (t *Telegraf) Close() error { return nil }

func (t *Telegraf) Write(ctx context.Context, points []Point) error {
	lines := make([]string, 0, len(points))
	for _, p := range points {
		if l := LineProtocol(p); l != "" {
			lines = append(lines, l)
		}
	}
	for start := 0; start < len(lines); start += t.Batch {
		end := min(start+t.Batch, len(lines))
		if err := t.post(ctx, strings.Join(lines[start:end], "\n")); err != nil {
			return err
		}
	}
	return nil
}

func (t *Telegraf) post(ctx context.Context, body string) error {
	listener, err := pushURL(t.URL)
	if err != nil {
		return fmt.Errorf("telegraf write: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, listener, bytes.NewReader([]byte(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if t.Username != "" {
		req.SetBasicAuth(t.Username, t.Password)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return httpError("telegraf", resp)
	}
	return nil
}
