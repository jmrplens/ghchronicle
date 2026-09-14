package sink

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// pushURL is the endpoint a sink is about to post to, parsed and checked.
//
// Every HTTP sink takes its destination from the configuration file and leaves
// the field exported afterwards, so the check belongs here at the push rather
// than in a constructor that a later assignment would walk straight past. Two
// things are ruled out. A typo, which otherwise surfaces several layers down as
// `unsupported protocol scheme ""` and names neither the sink nor the setting.
// And a scheme these sinks were never meant to speak: they post to an HTTP
// endpoint an operator named, and a value that reached file:// or a bare host
// is not that endpoint.
func pushURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("endpoint %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("endpoint %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("endpoint %q: names no host", raw)
	}
	return u.String(), nil
}

// httpError is the one-line failure every HTTP sink reports: which sink, the
// status, and enough of the body to say why. Five hundred bytes is plenty for
// an error message and too little for a stack trace to swamp the log.
func httpError(sinkName string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("%s write: %s: %s", sinkName, resp.Status, bytes.TrimSpace(b))
}

// scalar is numeric plus the one type the line protocol also writes as a
// number. A time becomes Unix seconds, so a "created at" survives in a store
// that takes nothing but numbers.
func scalar(v any) (float64, bool) {
	if t, ok := v.(time.Time); ok {
		if t.IsZero() {
			return 0, false
		}
		return float64(t.Unix()), true
	}
	return numeric(v)
}

func formatFloat(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// presentTags is the tag set the line protocol writes: an empty value is not
// a tag there, so it is not part of a point's identity anywhere else either.
func presentTags(tags map[string]string) map[string]string {
	out := make(map[string]string, len(tags))
	for k, v := range tags {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// stampOf is the point's time, or now for a point that was never dated.
func stampOf(p Point) time.Time {
	if p.Time.IsZero() {
		return time.Now()
	}
	return p.Time
}
