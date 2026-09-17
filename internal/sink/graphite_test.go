package sink

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// graphiteServer accepts connections and keeps what each one sent.
type graphiteServer struct {
	ln    net.Listener
	mu    sync.Mutex
	conns []string
	wg    sync.WaitGroup
}

func newGraphiteServer(t *testing.T) *graphiteServer {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &graphiteServer{ln: ln}
	// One goroutine, connections read one after another: the tests write
	// serially and close each socket before the next, and a serial reader
	// keeps the wait group free of a concurrent Add.
	s.wg.Go(s.accept)
	return s
}

// accept records every connection until the listener is closed.
func (s *graphiteServer) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		raw, _ := io.ReadAll(c)
		// The far side has already finished; a close error here says
		// nothing the recorded payload does not.
		_ = c.Close()
		s.mu.Lock()
		s.conns = append(s.conns, string(raw))
		s.mu.Unlock()
	}
}

// received waits for want connections to arrive, then closes the listener and
// returns what each one carried.
//
// Waiting first is the whole point. Closing the listener discards anything
// still in the accept backlog, so a sink that had already dialed could have
// its connection thrown away before the accept loop ever saw it. That is what
// made this test fail about one run in three.
func (s *graphiteServer) received(t *testing.T, want int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		n := len(s.conns)
		s.mu.Unlock()
		if n >= want || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// Closing the listener is how the accept loop is told to stop, so its
	// error is the signal rather than a failure.
	_ = s.ln.Close()
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

func TestGraphitePathIsTheContract(t *testing.T) {
	p := Point{
		Measurement: "gh_repo", Tags: map[string]string{
			"repo": "ghchronicle", "full_name": "jmrplens/ghchronicle",
			"language": "Jupyter Notebook", "license": "",
		},
		Fields: map[string]any{"stars": 3},
	}
	// Nodes in tag key order, whatever order the map came in: full_name,
	// language, license, repo. The slash and the space become underscores,
	// the empty license is "none" so the depth is stable.
	want := "github.repo.jmrplens_ghchronicle.Jupyter_Notebook.none.ghchronicle.stars"
	if got := graphitePath("github", p, "stars"); got != want {
		t.Errorf("path = %q\nwant %q", got, want)
	}
	if got := graphiteNode("Cloudflare, Inc."); got != "Cloudflare__Inc_" {
		t.Errorf("node = %q", got)
	}
}

func TestGraphiteWritesPlaintextLines(t *testing.T) {
	srv := newGraphiteServer(t)
	g := NewGraphite(srv.ln.Addr().String(), "", 0, 0)
	_, err := g.Write(context.Background(), []Point{{
		Measurement: "gh_traffic", Tags: map[string]string{"repo": "a", "kind": "views"},
		Fields: map[string]any{"count": 10, "uniques": 2.5, "note": "a string", "kind": 99},
		Time:   time.Unix(1700000000, 0),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got := srv.received(t, 1)
	if len(got) != 1 {
		t.Fatalf("connections = %d, want 1", len(got))
	}
	// One line per numeric field, the string skipped, and the field that
	// shares its name with a tag skipped too: the tag wins, as everywhere.
	want := "github.traffic.views.a.count 10 1700000000\n" +
		"github.traffic.views.a.uniques 2.5 1700000000\n"
	if got[0] != want {
		t.Errorf("wire = %q\nwant %q", got[0], want)
	}
}

func TestGraphiteReconnectsWhenTheSocketIsGone(t *testing.T) {
	srv := newGraphiteServer(t)
	g := NewGraphite(srv.ln.Addr().String(), "", 0, 0)
	p := Point{
		Measurement: "gh_repo", Tags: map[string]string{"repo": "a"},
		Fields: map[string]any{"stars": 1}, Time: time.Unix(1700000000, 0),
	}
	if _, err := g.Write(context.Background(), []Point{p}); err != nil {
		t.Fatal(err)
	}
	// The connection dies underneath the sink. The next write must notice
	// and dial again rather than fail for the rest of the process's life.
	g.mu.Lock()
	// Killing the socket is the point of the test; whether the close itself
	// succeeded says nothing about what the next write must do.
	_ = g.conn.Close()
	g.mu.Unlock()
	if _, err := g.Write(context.Background(), []Point{p}); err != nil {
		t.Fatalf("write after a dead socket: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	got := srv.received(t, 2)
	if len(got) != 2 {
		t.Fatalf("connections = %d, want 2 (one before the drop, one after)", len(got))
	}
	for i, body := range got {
		if !strings.HasPrefix(body, "github.repo.a.stars 1 1700000000") {
			t.Errorf("connection %d carried %q", i, body)
		}
	}
}

func TestGraphiteNamesItselfWhenUnreachable(t *testing.T) {
	// Bind a port and give it straight back, so the address is one nothing is
	// listening on rather than one that might be in use.
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err = ln.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = NewGraphite(addr, "", 0, time.Second).Write(context.Background(), []Point{{
		Measurement: "m", Fields: map[string]any{"v": 1}, Time: time.Now(),
	}})
	if err == nil || !strings.HasPrefix(err.Error(), "graphite write:") {
		t.Errorf("err = %v", err)
	}
}

// TestGraphiteNodeKeepsExactlyTheCharactersAPathAllows checks every edge of
// the allowed ranges and the character just outside each one, since a range
// off by one either splits a node or renames a series.
func TestGraphiteNodeKeepsExactlyTheCharactersAPathAllows(t *testing.T) {
	if got := graphiteNode("azAZ09_-:"); got != "azAZ09_-:" {
		t.Errorf("node = %q, want every allowed character kept", got)
	}
	// Just before and after a-z, A-Z and 0-9 (the colon after 9 is allowed on
	// its own), then a dot, a slash, a space and a letter outside ASCII.
	if got := graphiteNode("`{@[/;. é"); got != "_________" {
		t.Errorf("node = %q, want every other character replaced", got)
	}
}

// TestNewGraphiteFillsInOnlyWhatWasLeftOut gives a zero prefix, batch and
// timeout their documented values and keeps the ones that were set.
func TestNewGraphiteFillsInOnlyWhatWasLeftOut(t *testing.T) {
	g := NewGraphite("graphite:2003", "", 0, 0)
	if g.Prefix != "github" || g.Batch != 1000 || g.Timeout != 30*time.Second || g.Name() != "graphite" {
		t.Errorf("defaults = prefix %q, batch %d, timeout %v, name %q", g.Prefix, g.Batch, g.Timeout, g.Name())
	}
	g = NewGraphite("graphite:2003", "gh", 2, time.Second)
	if g.Prefix != "gh" || g.Batch != 2 || g.Timeout != time.Second {
		t.Errorf("given = prefix %q, batch %d, timeout %v, want gh, 2 and 1s kept", g.Prefix, g.Batch, g.Timeout)
	}
	if err := g.Close(); err != nil {
		t.Errorf("Close before any write = %v, want nil", err)
	}
}

// TestGraphiteDoesNotDialForABatchWithNoNumber sends nothing when no field is
// a number, so a batch of text does not even need the server to be up.
func TestGraphiteDoesNotDialForABatchWithNoNumber(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err = ln.Close(); err != nil {
		t.Fatal(err)
	}
	g := NewGraphite(addr, "", 0, time.Second)
	if _, err = g.Write(context.Background(), []Point{{
		Measurement: "m", Fields: map[string]any{"note": "text"}, Time: time.Unix(1, 0),
	}}); err != nil {
		t.Errorf("Write = %v, want nothing to send and so nothing to fail", err)
	}
}

// TestGraphiteWritesADateAsSecondsAndSkipsAnUnsetOne sends a time field as its
// Unix seconds, the one number the line protocol also writes for it, and
// leaves out a time that was never set.
func TestGraphiteWritesADateAsSecondsAndSkipsAnUnsetOne(t *testing.T) {
	srv := newGraphiteServer(t)
	g := NewGraphite(srv.ln.Addr().String(), "", 1, 0)
	_, err := g.Write(context.Background(), []Point{{
		Measurement: "gh_repo",
		Fields:      map[string]any{"pushed_at": time.Unix(1600000000, 0), "never": time.Time{}, "stars": 2},
		Time:        time.Unix(1700000000, 0),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Close(); err != nil {
		t.Fatal(err)
	}
	got := srv.received(t, 1)
	// Carbon parses a value as a float, so the exponent form is a number to it.
	want := "github.repo.pushed_at 1.6e+09 1700000000\ngithub.repo.stars 2 1700000000\n"
	if len(got) != 1 || got[0] != want {
		t.Errorf("wire = %q, want %q in batches of one over one connection", got, want)
	}
}
