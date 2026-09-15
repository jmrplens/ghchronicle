package sink

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Elasticsearch writes documents through the _bulk API, to Elasticsearch or
// OpenSearch, which share it.
//
// One index per measurement, one document per point, with the tags and fields
// as top-level keys and the time as @timestamp, which is what Kibana, the
// OpenSearch dashboards and Grafana's datasource all look for first.
//
// The document id is derived from the point's identity, the measurement, the
// tag set and the timestamp, and the action is "index" rather than "create".
// So writing the same fourteen-day traffic window every six hours replaces
// fourteen documents instead of adding fourteen more, which is the same
// convergence InfluxDB gives for free.
type Elasticsearch struct {
	// URL is the cluster, for example http://elasticsearch:9200.
	URL string
	// Prefix starts every index name: <prefix>-<measurement>.
	Prefix   string
	Username string
	Password string
	// APIKey is sent as "Authorization: ApiKey" and wins over basic auth.
	APIKey string
	Batch  int
	// OnReject is called with the reason for each document the cluster
	// refused, once per document. A bulk request answers 200 even then.
	OnReject func(reason string)
	client   *http.Client
}

// NewElasticsearch returns a sink. A zero batch means 1000 documents per request.
func NewElasticsearch(url, prefix, username, password, apiKey string, batch int, timeout time.Duration) *Elasticsearch {
	if prefix == "" {
		prefix = "ghchronicle"
	}
	if batch <= 0 {
		batch = 1000
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &Elasticsearch{
		URL: strings.TrimRight(url, "/"), Prefix: prefix, Username: username, Password: password,
		APIKey: apiKey, Batch: batch, client: &http.Client{Timeout: timeout},
	}
}

func (e *Elasticsearch) Name() string { return "elasticsearch" }
func (e *Elasticsearch) Close() error { return nil }

type esAction struct {
	Index struct {
		Index string `json:"_index"`
		ID    string `json:"_id"`
	} `json:"index"`
}

func (e *Elasticsearch) Write(ctx context.Context, points []Point) error {
	var body bytes.Buffer
	n, rejected := 0, 0
	flush := func() error {
		if n == 0 {
			return nil
		}
		r, err := e.post(ctx, body.Bytes())
		body.Reset()
		n = 0
		rejected += r
		return err
	}
	for _, p := range points {
		doc := esDocument(p)
		if doc == nil {
			continue
		}
		var action esAction
		action.Index.Index = e.indexFor(p.Measurement)
		action.Index.ID = esID(p)
		a, err := json.Marshal(action)
		if err != nil {
			return err
		}
		d, err := json.Marshal(doc)
		if err != nil {
			return err
		}
		body.Write(a)
		body.WriteByte('\n')
		body.Write(d)
		body.WriteByte('\n')
		n++
		if n >= e.Batch {
			if flushErr := flush(); flushErr != nil {
				return flushErr
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if rejected > 0 {
		return &RejectedError{N: rejected}
	}
	return nil
}

// indexFor lowercases, because an index name may not carry an upper-case
// letter and the failure names the whole bulk request rather than the name.
func (e *Elasticsearch) indexFor(measurement string) string {
	return strings.ToLower(e.Prefix + "-" + measurement)
}

// esID is the identity of a point: the measurement, the tags that are set,
// and the timestamp. Hashed, because a tag value can be anything and an id
// cannot be longer than 512 bytes.
func esID(p Point) string {
	sum := sha256.Sum256([]byte(p.Measurement + "|" + tagKey(presentTags(p.Tags)) + "|" +
		strconv.FormatInt(stampOf(p).UnixNano(), 10)))
	return hex.EncodeToString(sum[:])
}

// esDocument flattens a point. It returns nil for a point with no usable
// field, which is not a point, the same rule the line protocol applies.
func esDocument(p Point) map[string]any {
	doc := map[string]any{
		"@timestamp":  stampOf(p).UTC().Format(time.RFC3339Nano),
		"measurement": p.Measurement,
	}
	tags := presentTags(p.Tags)
	for k, v := range tags {
		doc[k] = v
	}
	fields := 0
	for k, v := range p.Fields {
		if _, clash := tags[k]; clash {
			continue // the tag wins, as in the line protocol
		}
		switch t := v.(type) {
		case nil:
			continue
		case string:
			if t == "" {
				continue
			}
			doc[k] = t
		case time.Time:
			if t.IsZero() {
				continue
			}
			doc[k] = t.UTC().Format(time.RFC3339Nano)
		case int, int64, float64, bool:
			doc[k] = v
		default:
			continue
		}
		fields++
	}
	if fields == 0 {
		return nil
	}
	return doc
}

// The part of a bulk response that says what went wrong, per item.
type esBulkResponse struct {
	Errors bool `json:"errors"`
	Items  []struct {
		Index esBulkIndex `json:"index"`
	} `json:"items"`
}

// esBulkIndex is how one indexing action of a batch ended: where it went, the
// status it came back with, and the error naming why it was refused.
type esBulkIndex struct {
	Index  string `json:"_index"`
	Status int    `json:"status"`
	Error  *struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	} `json:"error"`
}

// post sends one bulk request and returns how many of its items were refused.
func (e *Elasticsearch) post(ctx context.Context, body []byte) (int, error) {
	cluster, err := pushURL(e.URL)
	if err != nil {
		return 0, fmt.Errorf("elasticsearch write: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cluster+"/_bulk", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	switch {
	case e.APIKey != "":
		req.Header.Set("Authorization", "ApiKey "+e.APIKey)
	case e.Username != "":
		req.SetBasicAuth(e.Username, e.Password)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return 0, httpError("elasticsearch", resp)
	}
	// A bulk request is 200 even when every item failed; the verdict is
	// per item, inside the body.
	var out esBulkResponse
	if json.NewDecoder(resp.Body).Decode(&out) != nil || !out.Errors {
		return 0, nil
	}
	rejected := 0
	for _, it := range out.Items {
		if it.Index.Error == nil {
			continue
		}
		rejected++
		if e.OnReject != nil {
			e.OnReject(fmt.Sprintf("%s: %s: %s", it.Index.Index, it.Index.Error.Type, it.Index.Error.Reason))
		}
	}
	return rejected, nil
}
