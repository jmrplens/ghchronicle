package sink

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

// Graphite writes the plaintext protocol over TCP: one "path value timestamp"
// line per numeric field.
//
// Graphite keeps a point at the time it was given, so the raw dated points go
// out as they are and the traffic window lands on its own days. There is no
// idempotency to speak of: writing the same point twice overwrites the same
// slot of the same whisper file, which is what a rewrite of the window wants.
//
// The metric path is the dashboard's contract, so it is fixed:
//
//	<prefix>.<measurement without gh_>.<tag values, in key order>.<field>
//
// Every tag value is one node. They are ordered by tag key, never by the
// order a collector happened to set them, and an empty value is written as
// "none" so a measurement's depth never changes from one point to the next.
// A node keeps ASCII letters, digits, "_", "-" and ":"; everything else,
// the dot, the space and the slash in "owner/repo" included, becomes "_",
// because a dot would split the node and a slash would nest a directory.
type Graphite struct {
	Addr   string
	Prefix string
	Batch  int
	// Timeout bounds the dial and each write.
	Timeout time.Duration

	mu   sync.Mutex
	conn net.Conn
}

// NewGraphite returns a sink. A zero batch means 1000 lines per write.
func NewGraphite(addr, prefix string, batch int, timeout time.Duration) *Graphite {
	if prefix == "" {
		prefix = "github"
	}
	if batch <= 0 {
		batch = 1000
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Graphite{Addr: addr, Prefix: prefix, Batch: batch, Timeout: timeout}
}

func (g *Graphite) Name() string { return "graphite" }

func (g *Graphite) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.conn == nil {
		return nil
	}
	err := g.conn.Close()
	g.conn = nil
	return err
}

func (g *Graphite) Write(ctx context.Context, points []Point) error {
	var lines []string
	for _, p := range points {
		stamp := stampOf(p).Unix()
		for _, field := range sortedKeys(p.Fields) {
			if _, clash := p.Tags[field]; clash {
				continue // the same rule as the line protocol: the tag wins
			}
			v, ok := scalar(p.Fields[field])
			if !ok {
				continue // a string is not a metric
			}
			lines = append(lines, fmt.Sprintf("%s %s %d\n", graphitePath(g.Prefix, p, field), formatFloat(v), stamp))
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for start := 0; start < len(lines); start += g.Batch {
		end := min(start+g.Batch, len(lines))
		if err := g.send(ctx, strings.Join(lines[start:end], "")); err != nil {
			return err
		}
	}
	return nil
}

// send writes one payload, reconnecting once if the socket has gone.
//
// A write to a socket the far side has already closed succeeds the first time
// and fails on the next, when the reset has come back. So one retry on a fresh
// connection is part of the protocol here, not optimism.
func (g *Graphite) send(ctx context.Context, payload string) error {
	var last error
	for range 2 {
		if err := g.dial(ctx); err != nil {
			return fmt.Errorf("graphite write: %w", err)
		}
		_ = g.conn.SetWriteDeadline(time.Now().Add(g.Timeout))
		_, err := io.WriteString(g.conn, payload)
		if err == nil {
			return nil
		}
		last = err
		_ = g.conn.Close()
		g.conn = nil
	}
	return fmt.Errorf("graphite write: %w", last)
}

func (g *Graphite) dial(ctx context.Context) error {
	if g.conn != nil {
		return nil
	}
	conn, err := (&net.Dialer{Timeout: g.Timeout}).DialContext(ctx, "tcp", g.Addr)
	if err != nil {
		return err
	}
	g.conn = conn
	return nil
}

func graphitePath(prefix string, p Point, field string) string {
	parts := make([]string, 0, len(p.Tags)+3)
	parts = append(parts, prefix, graphiteNode(strings.TrimPrefix(p.Measurement, "gh_")))
	for _, k := range sortedKeys2(p.Tags) {
		parts = append(parts, graphiteNode(p.Tags[k]))
	}
	parts = append(parts, graphiteNode(field))
	return strings.Join(parts, ".")
}

func graphiteNode(s string) string {
	if s == "" {
		return "none"
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '_', r == '-', r == ':':
			return r
		}
		return '_'
	}, s)
}
