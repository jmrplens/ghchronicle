package ghapi

import (
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"sync"
)

// Answer is one entry of the conditional-request cache in the form a file
// keeps it between two processes.
//
// It exists because the cache is what makes a pass cheap and a restart used
// to throw it away. Measured on the production service's proxy log: in the 38
// minutes after the restart of 2026-09-26 19:52Z the service spent 1,092
// charged core requests on passes that cost about 66 with the cache warm, and
// a family whose cadence is longer than the time between two restarts never
// got a 304 at all. The run package writes these beside the state file and
// hands them back to Restore at the next start.
type Answer struct {
	// URL is what was asked, the API root included.
	URL string
	// Type is the decodingName of what the body was decoded into, which is
	// half of the entry's key: see cacheKey.
	Type string
	ETag string
	// Link is the header the body came with, which a 304 does not repeat.
	Link string
	Body []byte
	// Used is when a request last asked for the entry, in Unix seconds.
	Used int64
}

// Answers returns what the cache holds, most recently used first.
//
// The bodies are the cache's own and are not copied: nothing writes to a
// stored body, which put replaces whole rather than editing, so a caller that
// only reads them can hold them while the cache moves on.
func (c *Client) Answers() []Answer {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Answer, 0, len(c.cache.entries))
	for el := c.cache.order.Front(); el != nil; el = el.Next() {
		e := pairAt(el)
		if e == nil {
			continue
		}
		out = append(out, Answer{
			URL: e.key.url, Type: e.key.typ, ETag: e.etag, Link: e.link, Body: e.body, Used: e.used,
		})
	}
	return out
}

// Restore puts back answers an earlier process kept, in the order Answers
// gave them, and reports how many it took.
//
// Behind whatever this process already holds, and never over it: an entry
// this process stored is newer than anything a file can say about the same
// URL. It stops at the limit rather than evicting for room, because what is
// left of the list is older than what is already in, and put would evict the
// newer to make room for the older. An answer with half a pair is refused
// for the reason put gives.
func (c *Client) Restore(answers []Answer) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	taken := 0
	for i := range answers {
		a := &answers[i]
		if a.ETag == "" || len(a.Body) == 0 {
			continue
		}
		key := cacheKey{url: a.URL, typ: a.Type}
		if _, held := c.cache.entries[key]; held {
			continue
		}
		e := &conditional{key: key, etag: a.ETag, body: a.Body, link: a.Link, used: a.Used}
		if c.cache.bytes+e.size() > c.cache.limit {
			break
		}
		c.cache.entries[key] = c.cache.order.PushBack(e)
		c.cache.bytes += e.size()
		taken++
	}
	return taken
}

// decodingNames holds the name of every type decodingName has already
// described, because the description walks the whole type and GetJSON asks
// for the name on every call.
var decodingNames sync.Map

// decodingName is what an entry's decoding type is called in the key, and so
// in a file another process reads: a digest of everything that decides what
// encoding/json keeps of an answer when it decodes one into that type.
//
// Not the type's name, because a type keeps its name across a change that
// changes what it keeps. An entry's body is the decoded value encoded again
// (see replayable), so it holds exactly the fields its type had when it was
// stored. A release that adds a field to a collector's type and then answered
// a 304 from a body a release before it stored would hand the collector that
// field empty, on every URL that had not changed, for as long as it did not
// change. Named by what decodes, the type with the new field is a different
// key: its first request goes out unconditionally, like any request with
// nothing stored, and an upgrade costs exactly the entries whose types moved.
//
// The nil type, a call that decodes nothing, is the empty name.
func decodingName(t reflect.Type) string {
	if t == nil {
		return ""
	}
	if name, ok := decodingNames.Load(t); ok {
		if s, isString := name.(string); isString {
			return s
		}
	}
	var b strings.Builder
	describe(&b, t, map[reflect.Type]bool{})
	sum := sha256.Sum256([]byte(b.String()))
	name := hex.EncodeToString(sum[:16])
	decodingNames.Store(t, name)
	return name
}

var (
	jsonDecoder = reflect.TypeFor[json.Unmarshaler]()
	textDecoder = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// describe writes t out as encoding/json sees it when it decodes: every
// field's name, tag and type, all the way down.
//
// A named type is written by its package path and name as well as its shape,
// so two types that happen to have one shape are still two keys. It is
// expanded once: a type met again, which is how a type that refers to itself
// ends, is its name alone. So is a type that decodes itself, time.Time and
// json.RawMessage in what the collectors decode: its fields say nothing about
// what its decoder keeps, and time.Time's are the runtime's own and move
// between Go releases while its JSON does not. No type in this repository
// decodes itself; replayable says why that matters.
func describe(b *strings.Builder, t reflect.Type, seen map[reflect.Type]bool) {
	if t.Name() != "" {
		b.WriteString(t.PkgPath())
		b.WriteByte('.')
		b.WriteString(t.Name())
		if seen[t] || decodesItself(t) {
			return
		}
		seen[t] = true
		b.WriteByte('=')
	}
	switch t.Kind() {
	case reflect.Pointer:
		b.WriteByte('*')
		describe(b, t.Elem(), seen)
	case reflect.Slice:
		b.WriteString("[]")
		describe(b, t.Elem(), seen)
	case reflect.Array:
		b.WriteString("[" + strconv.Itoa(t.Len()) + "]")
		describe(b, t.Elem(), seen)
	case reflect.Map:
		b.WriteString("map[")
		describe(b, t.Key(), seen)
		b.WriteByte(']')
		describe(b, t.Elem(), seen)
	case reflect.Struct:
		b.WriteString("struct{")
		for f := range t.Fields() {
			b.WriteString(f.Name)
			if f.Anonymous {
				b.WriteString(" embedded")
			}
			b.WriteString(" " + strconv.Quote(string(f.Tag)) + " ")
			describe(b, f.Type, seen)
			b.WriteByte(';')
		}
		b.WriteByte('}')
	default:
		b.WriteString(t.Kind().String())
	}
}

// decodesItself reports whether encoding/json hands t's decoding to t.
func decodesItself(t reflect.Type) bool {
	p := reflect.PointerTo(t)
	return p.Implements(jsonDecoder) || p.Implements(textDecoder)
}
