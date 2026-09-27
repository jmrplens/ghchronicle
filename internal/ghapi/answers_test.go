package ghapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestARestoredCacheAsksConditionallyAndReplaysItsLink is the restart the
// cache file exists for, from the client's side: what one process stored,
// handed to a new client, makes that client's first request conditional, and
// the 304 is answered from the restored body with the Link of the page it
// came from, which the 304 does not repeat.
func TestARestoredCacheAsksConditionallyAndReplaysItsLink(t *testing.T) {
	t.Parallel()
	const pages = `<https://api.github.com/repositories/1/stargazers?page=2>; rel="next", ` +
		`<https://api.github.com/repositories/1/stargazers?page=7>; rel="last"`
	var conditional atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			conditional.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Link", pages)
		_, _ = w.Write([]byte(`[{"week":7,"total":3,"ignored":"x"}]`))
	}))
	t.Cleanup(srv.Close)
	client := func() *Client {
		c := New("token", 5*time.Second)
		c.SetBaseURL(srv.URL)
		return c
	}
	type week struct {
		Week  int `json:"week"`
		Total int `json:"total"`
	}
	const path = "/repos/o/n/stargazers/history"

	first := client()
	var stored []week
	if _, _, err := first.GetJSON(context.Background(), path, &stored, ""); err != nil {
		t.Fatal(err)
	}
	kept := first.Answers()
	if len(kept) != 1 {
		t.Fatalf("the first client holds %d answers, want the one it was given", len(kept))
	}

	second := client()
	if n := second.Restore(kept); n != 1 {
		t.Fatalf("Restore took %d answers, want 1", n)
	}
	var replayed []week
	link, cached, err := second.GetJSON(context.Background(), path, &replayed, "")
	if err != nil {
		t.Fatal(err)
	}
	if conditional.Load() != 1 || !cached {
		t.Fatalf("the restored client's first request: conditional %d, cached %v; want one conditional request answered from the cache",
			conditional.Load(), cached)
	}
	if !reflect.DeepEqual(replayed, stored) {
		t.Errorf("replayed %+v, want the %+v the first client decoded", replayed, stored)
	}
	if link != pages {
		t.Errorf("the 304 reported Link %q, want the one the stored page came with", link)
	}
}

// TestRestoreGoesBehindWhatIsHeldAndStopsAtTheLimit: a file is older than
// anything the process has stored itself, so a URL both hold keeps the
// process's pair, the file's answers queue behind in their own order, and
// once the limit is reached the rest are left out rather than evicting
// what is newer. Half a pair is refused, as put refuses it.
func TestRestoreGoesBehindWhatIsHeldAndStopsAtTheLimit(t *testing.T) {
	t.Parallel()
	c := New("token", 0)
	c.cache.put(cacheKey{url: "/held"}, `"new"`, []byte(`{"a":1}`), "")
	file := []Answer{
		{URL: "/held", ETag: `"old"`, Body: []byte(`{"a":0}`), Used: 5},
		{URL: "/newer", ETag: `"n"`, Body: []byte(`{}`), Used: 4},
		{URL: "/no-etag", Body: []byte(`{}`), Used: 3},
		{URL: "/no-body", ETag: `"e"`, Used: 3},
		{URL: "/older", ETag: `"o"`, Body: []byte(`{}`), Used: 2},
	}
	if n := c.Restore(file); n != 2 {
		t.Errorf("Restore took %d answers, want the two that were whole and not already held", n)
	}
	var order []string
	for _, a := range c.Answers() {
		order = append(order, a.URL+" "+a.ETag)
	}
	if want := []string{`/held "new"`, `/newer "n"`, `/older "o"`}; !reflect.DeepEqual(order, want) {
		t.Errorf("the cache holds %q, want %q", order, want)
	}

	small := New("token", 0)
	small.SetCacheLimit(2 * (cacheEntryOverhead + 10))
	fits := Answer{URL: "/a", ETag: `"1"`, Body: []byte("{}"), Used: 2}
	if n := small.Restore([]Answer{fits, {URL: "/b", ETag: `"2"`, Body: []byte("{}")}, {URL: "/c", ETag: `"3"`, Body: []byte("{}")}}); n != 2 {
		t.Errorf("Restore took %d answers into room for two", n)
	}
	if st := small.CacheStats(); st.Evicted != 0 || st.Bytes > st.Limit {
		t.Errorf("Restore evicted %d entries or went past the limit: %+v", st.Evicted, st)
	}
}

// TestAnAnswerSaysWhenItWasLastAskedFor: the stamp is what lets a file leave
// out what nothing has asked for in days, so it moves on every use, a hit as
// much as a store.
func TestAnAnswerSaysWhenItWasLastAskedFor(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 9, 27, 3, 0, 0, 0, time.UTC)
	k := newCache(1 << 20)
	k.clock = func() time.Time { return clock }
	key := cacheKey{url: "/x"}
	k.put(key, `"v"`, []byte("{}"), "")
	stamp := func() int64 { return pairAt(k.entries[key]).used }
	if got := stamp(); got != clock.Unix() {
		t.Errorf("a stored entry is stamped %d, want %d", got, clock.Unix())
	}
	clock = clock.Add(time.Hour)
	k.get(key)
	if got := stamp(); got != clock.Unix() {
		t.Errorf("a hit left the stamp at %d, want %d", got, clock.Unix())
	}
}

// TestADecodingTypeIsNamedByWhatItDecodes: the name a cache entry carries
// into a file changes when what the type keeps changes, and only then. A
// release that adds a field to a collector's type must not have its 304s
// answered from bodies stored without that field.
func TestADecodingTypeIsNamedByWhatItDecodes(t *testing.T) {
	t.Parallel()
	type repo struct {
		Name string `json:"name"`
	}
	type sameShape struct {
		Name string `json:"name"`
	}
	type tree struct {
		Name     string `json:"name"`
		Children []tree `json:"children"`
	}
	// The releases after it, each declaring a type of the same package and
	// the same name, which is what a collector's type is across an upgrade:
	// a name alone would give every one of them the name of the first.
	var withField, retagged, retyped any
	func() {
		type repo struct {
			Name  string `json:"name"`
			Stars int    `json:"stargazers_count"`
		}
		withField = &repo{}
	}()
	func() {
		type repo struct {
			Name string `json:"full_name"`
		}
		retagged = &repo{}
	}()
	func() {
		type repo struct {
			Name int `json:"name"`
		}
		retyped = &repo{}
	}()
	name := func(v any) string { return decodingName(reflect.TypeOf(v)) }
	if reflect.TypeOf(withField).String() != reflect.TypeFor[*repo]().String() {
		t.Fatalf("the later release's type is %s and the first's %s, so this tells nothing apart",
			reflect.TypeOf(withField), reflect.TypeFor[*repo]())
	}

	// Described again rather than read back from the memory, so this is the
	// name a new process would give it.
	remembered := name(&repo{})
	decodingNames.Delete(reflect.TypeFor[*repo]())
	if again := name(&repo{}); again != remembered {
		t.Errorf("one type was named %s and then %s", remembered, again)
	}
	for label, other := range map[string]any{
		"a field added":             withField,
		"a tag changed":             retagged,
		"a field's type changed":    retyped,
		"another type of one shape": &sameShape{},
		"the value, not a pointer":  repo{},
		"a slice of it":             &[]repo{},
	} {
		if name(other) == name(&repo{}) {
			t.Errorf("%s keeps the name %s", label, name(other))
		}
	}
	if name(&tree{}) == "" {
		t.Error("a type that refers to itself has no name")
	}
	if decodingName(nil) != "" {
		t.Errorf("the nil type is named %q, want the empty name of a call that decodes nothing", decodingName(nil))
	}

	// A type that decodes itself is its name: time.Time's fields are the
	// runtime's own, and a Go release that moved them would otherwise cost
	// every entry that holds a date.
	var b strings.Builder
	describe(&b, reflect.TypeFor[time.Time](), map[reflect.Type]bool{})
	if b.String() != "time.Time" {
		t.Errorf("time.Time is described as %q, want its name alone", b.String())
	}
	// Whatever package the toolchain declares it in: in Go 1.27, which this
	// was written with, it is an alias of encoding/json/jsontext.Value.
	raw := reflect.TypeFor[json.RawMessage]()
	b.Reset()
	describe(&b, raw, map[reflect.Type]bool{})
	if want := raw.PkgPath() + "." + raw.Name(); b.String() != want {
		t.Errorf("json.RawMessage is described as %q, want its name alone, %q", b.String(), want)
	}
}

// TestTheNameOfATypeIsTheSameInEveryProcess pins one name to its value. The
// name is half the key of every entry in a file another process reads, so a
// change to how names are made turns every entry in every deployment's file
// into one nothing asks for, and each pays its first pass in full once. That
// can be the right trade, and this is where it is made on purpose.
func TestTheNameOfATypeIsTheSameInEveryProcess(t *testing.T) {
	t.Parallel()
	type pinned struct {
		ID   int64     `json:"id"`
		When time.Time `json:"created_at"`
	}
	const want = "faaf058b06ed00d55570393b83f92b52"
	if got := decodingName(reflect.TypeFor[*[]pinned]()); got != want {
		t.Errorf("the name is %s, it was %s: see the comment above before changing it", got, want)
	}
}
