package sink

import (
	"strings"
	"testing"
	"time"
)

func TestLineProtocolEscapesRatherThanRewrites(t *testing.T) {
	// A space in a tag value has to survive as a space. Replacing it with an
	// underscore, which is the tempting shortcut, silently forks the series
	// away from every other writer's.
	p := Point{
		Measurement: "gh_repo",
		Tags:        map[string]string{"owner": "jmrplens", "license": "MIT License", "empty": ""},
		Fields:      map[string]any{"stars": 28},
		Time:        time.Unix(0, 1700000000000000000),
	}
	got := LineProtocol(p)
	want := `gh_repo,license=MIT\ License,owner=jmrplens stars=28i 1700000000000000000`
	if got != want {
		t.Fatalf("\n got: %s\nwant: %s", got, want)
	}
	if strings.Contains(got, "empty=") {
		t.Error("an empty tag must be dropped, not written as an empty value")
	}
}

func TestLineProtocolTypesAndSkips(t *testing.T) {
	p := Point{
		Measurement: "gh_x",
		Fields: map[string]any{
			"count": 3, "ratio": 1.5, "on": true, "name": "v1.2.0",
			"nothing": nil, "blank": "", "zerotime": time.Time{},
		},
		Time: time.Unix(0, 1),
	}
	got := LineProtocol(p)
	for _, want := range []string{"count=3i", "ratio=1.5", "on=true", `name="v1.2.0"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	for _, unwanted := range []string{"nothing", "blank", "zerotime"} {
		if strings.Contains(got, unwanted+"=") {
			t.Errorf("%s should have been skipped: %s", unwanted, got)
		}
	}
}

func TestLineProtocolRejectsPointWithoutFields(t *testing.T) {
	// Writing a point whose fields all resolved to nothing would be a line
	// InfluxDB rejects, failing the whole batch with it.
	p := Point{Measurement: "gh_x", Fields: map[string]any{"a": nil}, Time: time.Unix(0, 1)}
	if got := LineProtocol(p); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

func TestLineProtocolSurvivesAMultiLineName(t *testing.T) {
	// A workflow step with no `name:` is named after its `run:` block, so a
	// multi-line block becomes a multi-line name. Line protocol has no escape
	// for a newline in a tag: the record would split in two and InfluxDB would
	// reject the whole batch, not just the line.
	line := LineProtocol(Point{
		Measurement: "gh_workflow_step",
		Tags:        map[string]string{"step": "echo one\necho two", "job": "build"},
		Fields:      map[string]any{"duration_seconds": 3},
		Time:        time.Unix(0, 1700000000000000000),
	})
	if strings.Contains(line, "\n") {
		t.Fatalf("the record must stay on one line: %q", line)
	}
	if !strings.Contains(line, `step=echo\ one\ echo\ two`) {
		t.Errorf("the words must survive the fold: %q", line)
	}
}

func TestLineProtocolFoldsControlCharactersInAStringField(t *testing.T) {
	line := LineProtocol(Point{
		Measurement: "gh_commit",
		Tags:        map[string]string{"sha": "abc"},
		Fields:      map[string]any{"headline": "first\r\nsecond"},
		Time:        time.Unix(0, 1700000000000000000),
	})
	if strings.Contains(line, "\n") || strings.Contains(line, "\r") {
		t.Fatalf("field strings must not carry a line break: %q", line)
	}
}

func TestLineProtocolNeverUsesOneNameForBothATagAndAField(t *testing.T) {
	// InfluxDB rejects a line where a name is both, and rejects the whole
	// batch with it. This caught a real one: a deploy key carried `read_only`
	// as a tag and as a field.
	line := LineProtocol(Point{
		Measurement: "m",
		Tags:        map[string]string{"read_only": "false"},
		Fields:      map[string]any{"read_only": false, "keys": 1},
		Time:        time.Unix(0, 1),
	})
	tags, fields, _ := strings.Cut(line, " ")
	fields, _, _ = strings.Cut(fields, " ")
	for _, name := range []string{"read_only"} {
		inTags := strings.Contains(tags, name+"=")
		inFields := strings.Contains(fields, name+"=")
		if inTags && inFields {
			t.Errorf("%q is both a tag and a field in %q", name, line)
		}
	}
}

func TestLineProtocolKeepsAFieldWhoseTagNameIsEmpty(t *testing.T) {
	// An empty tag is no tag, so a field of the same name has nothing to
	// clash with and must be written; otherwise the point loses a value to
	// a tag the line never carries.
	line := LineProtocol(Point{
		Measurement: "m",
		Tags:        map[string]string{"state": ""},
		Fields:      map[string]any{"state": "open"},
		Time:        time.Unix(0, 1),
	})
	if line != `m state="open" 1` {
		t.Errorf("line = %q, want the field written", line)
	}
}

func TestLineProtocolWritesEveryFieldType(t *testing.T) {
	// Each type has its own syntax, and one written in another's makes
	// InfluxDB refuse the batch or store the wrong column type.
	line := LineProtocol(Point{
		Measurement: "m",
		Fields: map[string]any{
			"a_int": 3, "b_int64": int64(-4), "c_float": 2.5, "d_bool": false,
			"e_time": time.Unix(1600000000, 0), "f_text": "say \"hi\" \\ bye", "g_slice": []int{1},
		},
		Time: time.Unix(0, 7),
	})
	want := `m a_int=3i,b_int64=-4i,c_float=2.5,d_bool=false,e_time=1600000000i,f_text="say \"hi\" \\ bye" 7`
	if line != want {
		t.Errorf("\n got: %s\nwant: %s", line, want)
	}
}

func TestOneLineFoldsEveryControlCharacterItNames(t *testing.T) {
	// Each of the five splits a record or a column in some reader, so each
	// one has to become a space, not only the line feed the others resemble.
	if got := oneLine("a\nb\rc\td\ve\ff"); got != "a b c d e f" {
		t.Errorf("oneLine = %q", got)
	}
}
