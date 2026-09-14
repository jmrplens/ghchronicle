package sink

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The documents that state, in prose, how many measurements Loki gets a
// rendering for. The number was written into five files by hand and had
// drifted in all five at once: they said sixteen, one of them said
// twenty-one, and lokiEvents held twenty-two. Nothing failed, because
// TestEveryDatedItemIsRenderedOrRefused pins the table to promRules and
// nothing pinned the prose to the table.
//
// Raising the count means changing lokiEventCount and, if it crosses into a
// word neither language has here, lokiEventWords. The failure names the file
// and the word it is still carrying.
const lokiEventCount = 22

// How each language spells lokiEventCount. English hyphenates, Spanish does
// not, and both are compared literally rather than spelled by a helper: a
// speller for two words is more code than the two words.
var lokiEventWords = map[string]string{"en": "twenty-two", "es": "veintidós"}

// lokiEventClaims is the written claim in each document, as a pattern whose
// one group is the number word.
var lokiEventClaims = []struct {
	path   string
	locale string
	claim  *regexp.Regexp
}{
	{
		path:   filepath.Join("..", "..", "site", "src", "content", "docs", "sinks", "loki.mdx"),
		locale: "en",
		claim:  regexp.MustCompile(`\*\*([A-Za-z-]+) measurements have an event rendering\*\*`),
	},
	{
		path:   filepath.Join("..", "..", "site", "src", "content", "docs", "sinks", "loki.mdx"),
		locale: "en",
		claim:  regexp.MustCompile(`description: The ([a-z-]+) measurements that are events`),
	},
	{
		path:   filepath.Join("..", "..", "site", "src", "content", "docs", "es", "sinks", "loki.mdx"),
		locale: "es",
		claim:  regexp.MustCompile(`\*\*(\S+) medidas tienen una representación de evento\*\*`),
	},
	{
		path:   filepath.Join("..", "..", "site", "src", "content", "docs", "es", "sinks", "loki.mdx"),
		locale: "es",
		claim:  regexp.MustCompile(`description: Las (\S+) medidas que son eventos`),
	},
	{
		path:   filepath.Join("..", "..", "site", "src", "content", "docs", "how", "index.mdx"),
		locale: "en",
		claim:  regexp.MustCompile(`the ([a-z-]+) event renderings`),
	},
	{
		path:   filepath.Join("..", "..", "site", "src", "content", "docs", "es", "how", "index.mdx"),
		locale: "es",
		// Anchored on the mermaid node rather than on the words after the
		// number, which the spell checker reads as English and mangles.
		claim: regexp.MustCompile(`Loki<br/>las (\S+) `),
	},
	{
		path:   filepath.Join("..", "..", "config.example.yaml"),
		locale: "en",
		claim:  regexp.MustCompile(`([A-Za-z-]+) measurements have an\n\s*# event rendering`),
	},
	// The card on the page a reader chooses a store from, which is where the
	// number is read before any of the above. It was still saying sixteen
	// after the other five were corrected, because nothing held it either.
	{
		path:   filepath.Join("..", "..", "site", "src", "content", "docs", "sinks", "index.mdx"),
		locale: "en",
		claim:  regexp.MustCompile(`Loki turns ([a-z-]+) of the measurements`),
	},
	{
		path:   filepath.Join("..", "..", "site", "src", "content", "docs", "es", "sinks", "index.mdx"),
		locale: "es",
		claim:  regexp.MustCompile(`Loki convierte (\S+) de las medidas`),
	},
}

// TestTheWrittenLokiEventCountMatchesTheTable fails when a document states a
// number of event renderings that lokiEvents no longer holds.
func TestTheWrittenLokiEventCountMatchesTheTable(t *testing.T) {
	if lokiEventCount != len(lokiEvents) {
		t.Fatalf("lokiEvents holds %d renderings, the documents are written for %d: "+
			"raise lokiEventCount and rewrite the sentence in each file below",
			len(lokiEvents), lokiEventCount)
	}
	for _, c := range lokiEventClaims {
		text, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		m := c.claim.FindSubmatch(text)
		if m == nil {
			t.Errorf("%s no longer states how many measurements have an event rendering "+
				"(looked for %s)", c.path, c.claim)
			continue
		}
		// The sentence starts with the word in some of these files and
		// carries it mid-sentence in others, so the capital is not the claim.
		if got, want := string(m[1]), lokiEventWords[c.locale]; !strings.EqualFold(got, want) {
			t.Errorf("%s says %q event renderings, lokiEvents holds %d (%q)",
				c.path, got, len(lokiEvents), want)
		}
	}
}

// The two pages that publish a CREATE TABLE and an INSERT as "the schema is
// the contract, so here it is exactly". They were not exactly it: the sample
// had been typed by hand and had lost two tags and a field, so a reader who
// prepared a database from it was short three columns.
var sqlSamplePages = []string{
	filepath.Join("..", "..", "site", "src", "content", "docs", "sinks", "postgres.mdx"),
	filepath.Join("..", "..", "site", "src", "content", "docs", "es", "sinks", "postgres.mdx"),
}

// TestTheDocumentedSQLSampleIsTheSinksOwnOutput writes one gh_traffic point
// through the SQL sink and requires both pages to carry exactly what came
// out. The point is the one the sample shows: the demonstration account, the
// four tags the collector sets and the three fields it writes.
func TestTheDocumentedSQLSampleIsTheSinksOwnOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.sql")
	s := NewSQL("postgres", path, 1<<20, 2)
	point := Point{
		Measurement: "gh_traffic",
		Tags: map[string]string{
			"owner":     "acme",
			"repo":      "telemetry",
			"full_name": "acme/telemetry",
			"kind":      "views",
		},
		Fields: map[string]any{
			"count":   41,
			"uniques": 12,
			"url":     "https://github.com/acme/telemetry/graphs/traffic",
		},
		Time: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
	}
	if err := s.Write(context.Background(), []Point{point}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	emitted, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	sample := strings.TrimSpace(string(emitted))
	for _, page := range sqlSamplePages {
		text, readErr := os.ReadFile(page)
		if readErr != nil {
			t.Fatalf("%s: %v", page, readErr)
		}
		if !strings.Contains(string(text), sample) {
			t.Errorf("%s does not publish what the sink emits. The sink writes:\n%s",
				page, sample)
		}
	}
}

// The pages that publish one NDJSON line as what the file sink writes. The
// published line had the keys in the wrong order and was missing a tag, which
// a reader writing a parser against it would have discovered on their first
// real file.
var jsonSamplePages = []string{
	filepath.Join("..", "..", "site", "src", "content", "docs", "sinks", "file.mdx"),
	filepath.Join("..", "..", "site", "src", "content", "docs", "es", "sinks", "file.mdx"),
}

// TestTheDocumentedJSONSampleIsTheSinksOwnOutput writes the same gh_traffic
// point through the file sink in JSON and requires both pages to carry the
// line it produced.
func TestTheDocumentedJSONSampleIsTheSinksOwnOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sample.ndjson")
	f := NewFile(path, "json", 1<<20, 2)
	point := Point{
		Measurement: "gh_traffic",
		Tags: map[string]string{
			"owner":     "acme",
			"repo":      "telemetry",
			"full_name": "acme/telemetry",
			"kind":      "views",
		},
		Fields: map[string]any{"count": 220, "uniques": 131},
		Time:   time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
	}
	if err := f.Write(context.Background(), []Point{point}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	emitted, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	line := strings.TrimSpace(string(emitted))
	for _, page := range jsonSamplePages {
		text, readErr := os.ReadFile(page)
		if readErr != nil {
			t.Fatalf("%s: %v", page, readErr)
		}
		if !strings.Contains(string(text), line) {
			t.Errorf("%s does not publish what the sink writes. The sink writes:\n%s",
				page, line)
		}
	}

	// The same point in the other format, in the tab beside it. Its timestamp
	// had drifted a year away from the JSON twin's.
	lpPath := filepath.Join(t.TempDir(), "sample.lp")
	lp := NewFile(lpPath, "influx", 1<<20, 2)
	if writeErr := lp.Write(context.Background(), []Point{point}); writeErr != nil {
		t.Fatalf("write line protocol: %v", writeErr)
	}
	if closeErr := lp.Close(); closeErr != nil {
		t.Fatalf("close line protocol: %v", closeErr)
	}
	emittedLP, lpErr := os.ReadFile(lpPath)
	if lpErr != nil {
		t.Fatalf("read back line protocol: %v", lpErr)
	}
	protocol := strings.TrimSpace(string(emittedLP))
	for _, page := range jsonSamplePages {
		text, readErr := os.ReadFile(page)
		if readErr != nil {
			t.Fatalf("%s: %v", page, readErr)
		}
		if !strings.Contains(string(text), protocol) {
			t.Errorf("%s does not publish the line protocol the sink writes:\n%s",
				page, protocol)
		}
	}
}
