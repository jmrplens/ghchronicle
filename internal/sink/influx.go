package sink

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Influx writes line protocol to InfluxDB 2 or 3 over the v2 write endpoint.
//
// Rewriting a point that already exists is not an error and not a duplicate:
// InfluxDB keys a point by measurement, tag set and timestamp, so replaying the
// same 14-day traffic window every few hours converges instead of accumulating.
// The whole backfill design rests on that.
type Influx struct {
	URL    string
	Token  string
	Org    string
	Bucket string
	Batch  int
	// OnReject is called with any line the server refuses to parse, once the
	// batch has been bisected down to it.
	OnReject func(line string)
	// Exclude names measurements this sink drops. A log line has no business
	// in a metrics database.
	Exclude map[string]bool
	client  *http.Client
	// filtered counts the points this sink dropped instead of writing, which
	// is what makes the difference between what it was offered and what the
	// database holds readable from outside. See Filtering.
	filtered atomic.Uint64
}

// NewInflux returns a sink. A zero batch means 5000 lines per request.
func NewInflux(url, token, org, bucket string, batch int, timeout time.Duration) *Influx {
	if batch <= 0 {
		batch = 5000
	}
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	return &Influx{
		URL: strings.TrimRight(url, "/"), Token: token, Org: org, Bucket: bucket,
		Batch: batch, client: &http.Client{Timeout: timeout},
	}
}

func (i *Influx) Name() string { return "influxdb" }
func (i *Influx) Close() error { return nil }

// Filtered counts the points this sink has dropped rather than written: a
// measurement named by Exclude, and a point carrying no field the line
// protocol can render. Both are decisions this sink makes after the point was
// handed to it, so nothing outside can count them.
func (i *Influx) Filtered() uint64 { return i.filtered.Load() }

func (i *Influx) Write(ctx context.Context, points []Point) error {
	lines := make([]string, 0, len(points))
	for _, p := range points {
		if i.Exclude[p.Measurement] {
			continue
		}
		if l := LineProtocol(p); l != "" {
			lines = append(lines, l)
		}
	}
	if dropped := len(points) - len(lines); dropped > 0 {
		i.filtered.Add(uint64(dropped))
	}
	rejected := 0
	for start := 0; start < len(lines); start += i.Batch {
		end := min(start+i.Batch, len(lines))
		batch := lines[start:end]
		err := i.post(ctx, strings.Join(batch, "\n"))
		if err == nil {
			continue
		}
		if !isParseRejection(err) {
			return err
		}
		// One unparseable line makes InfluxDB refuse the whole write, and the
		// message names no line. Rather than lose several thousand good points
		// to one bad one, the batch is halved until the offender is alone, and
		// it is then reported by content so the bug can be fixed at the source.
		n, rerr := i.bisect(ctx, batch)
		if rerr != nil {
			return rerr
		}
		rejected += n
	}
	if rejected > 0 {
		return &RejectedError{N: rejected}
	}
	return nil
}

// RejectedError reports lines the server refused to parse. Everything else was
// written, so this is a warning rather than a failure.
type RejectedError struct{ N int }

func (r *RejectedError) Error() string {
	return fmt.Sprintf("%d lines were rejected as unparseable and skipped; everything else was written", r.N)
}

func isParseRejection(err error) bool {
	return err != nil && strings.Contains(err.Error(), "parse failed")
}

// bisect writes what it can and returns how many lines were dropped.
func (i *Influx) bisect(ctx context.Context, lines []string) (int, error) {
	if len(lines) == 1 {
		// Down to one: it is the offender. Log-worthy, and skipped.
		i.reject(lines[0])
		return 1, nil
	}
	half := len(lines) / 2
	dropped := 0
	for _, part := range [][]string{lines[:half], lines[half:]} {
		err := i.post(ctx, strings.Join(part, "\n"))
		if err == nil {
			continue
		}
		if !isParseRejection(err) {
			return dropped, err
		}
		n, rerr := i.bisect(ctx, part)
		if rerr != nil {
			return dropped, rerr
		}
		dropped += n
	}
	return dropped, nil
}

// reject records a line the server would not parse. It is kept short: the
// point of it is to name the value that broke, not to reproduce the record.
func (i *Influx) reject(line string) {
	if i.OnReject == nil {
		return
	}
	if len(line) > 300 {
		line = line[:300] + "..."
	}
	i.OnReject(line)
}

func (i *Influx) post(ctx context.Context, body string) error {
	server, err := pushURL(i.URL)
	if err != nil {
		return fmt.Errorf("influx write: %w", err)
	}
	// Nanosecond precision: the traffic points are days and the workflow ones
	// are seconds, and one precision has to cover both.
	write := fmt.Sprintf("%s/api/v2/write?org=%s&bucket=%s&precision=ns", server, i.Org, i.Bucket)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, write, bytes.NewReader([]byte(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Token "+i.Token)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := i.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("influx write: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return nil
}
