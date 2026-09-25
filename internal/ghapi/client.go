// Package ghapi talks to GitHub's REST and GraphQL APIs.
//
// It exists to keep three concerns out of the collectors: not exceeding the
// rate limit, not paying for responses that have not changed, and telling a
// disabled feature apart from a real failure.
package ghapi

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/httpx"
)

const (
	defaultBase = "https://api.github.com"
	userAgent   = "ghchronicle"

	authScheme      = "Bearer "
	userAgentHeader = "User-Agent"
	graphqlPath     = "/graphql"
)

// Client is safe for concurrent use.
type Client struct {
	http  *http.Client
	token string
	// base is the API root: api.github.com, a GitHub Enterprise host, or a
	// test server. GraphQL lives at base + "/graphql" on all three.
	base string

	// cache holds the ETag, the body and the Link header of the URLs
	// fetched, so a repeat request can ask GitHub "only if it changed". A 304
	// answer costs no rate limit at all and the stored body is replayed, which
	// is what lets the collectors run often without paying for it. It is
	// bounded; see cache.
	mu    sync.Mutex
	cache *cache

	// reserve is how much of a bucket is never spent; wait says whether to
	// block until the window turns over instead of refusing.
	reserve int
	wait    bool
	// OnWait is called before the client blocks, so the caller can say so.
	OnWait func(bucket string, d time.Duration)

	// rates mirrors the x-ratelimit-* headers per bucket.
	//
	// One field would be wrong, and was: GitHub runs fifteen independent
	// budgets and names the one it charged in x-ratelimit-resource. Search has
	// a limit of thirty against core's five thousand, so after one search the
	// single field read "29 of 30 left" and a brake comparing that against a
	// reserve of five hundred stopped the entire sweep.
	rates map[string]RateState
	last  string

	// spend accumulates the cost GraphQL reports for each query, because no
	// endpoint reports it: `GET /rate_limit` does not even see this budget.
	spend GraphQLSpend
}

// RateState is the budget as GitHub last reported it.
//
// Used is the whole token's spend in the current window, which is not the
// same as this process's: the token is shared with whatever else runs beside
// it. GraphQLSpend is the part ghchronicle answers for.
type RateState struct {
	Limit     int
	Used      int
	Remaining int
	Reset     time.Time
	Resource  string
}

// GraphQLSpend is what this process has spent on the GraphQL budget, priced
// by GitHub itself rather than by subtracting two readings.
//
// The subtraction cannot be trusted: the buckets are per token, and a token
// shared with another tool moves between two of this client's own requests.
// The rateLimit block every query carries reports the cost of that query and
// nothing else.
type GraphQLSpend struct {
	// Queries is how many charged GraphQL requests this process has made.
	Queries int
	// Cost is what those queries cost in GraphQL points.
	Cost int
}

// New returns a client. A zero timeout means 30s.
func New(token string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	// Its own transport rather than the shared default one. Two clients that
	// share http.DefaultTransport share a connection pool, and closing one
	// side's idle connections then fails a request in flight on the other with
	// "http: CloseIdleConnections called", which is how this surfaced: one
	// test server shutting down broke another test's request about one run in
	// twenty.
	return &Client{
		http:  &http.Client{Timeout: timeout, Transport: httpx.OwnTransport()},
		token: token,
		base:  defaultBase,
		cache: newCache(DefaultCacheBytes),
		rates: map[string]RateState{},
	}
}

// SetBaseURL points the client at a different API root: a GitHub Enterprise
// instance, or a test server. Trailing slashes are dropped so paths join
// cleanly.
func (c *Client) SetBaseURL(base string) {
	if base != "" {
		c.base = strings.TrimRight(base, "/")
	}
}

// Rate returns the budget of the bucket most recently charged.
func (c *Client) Rate() RateState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rates[c.last]
}

// RateFor returns one bucket's budget, and whether anything has been seen for
// it yet. "core", "graphql" and "search" are the ones that matter here.
func (c *Client) RateFor(resource string) (RateState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.rates[resource]
	return r, ok
}

// Rates returns a copy of every bucket seen so far.
func (c *Client) Rates() map[string]RateState {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]RateState, len(c.rates))
	maps.Copy(out, c.rates)
	return out
}

// DefaultCacheBytes is what the conditional-request cache is allowed to hold.
//
// Measured rather than picked, by TestLiveSweepCacheFootprint against the
// account this was written for: 52 repositories with every family forced on
// every sweep, which is the shape the bound has to survive. The first sweep
// leaves 915 entries and 114,662,719 bytes charged; three sweeps leave 922 and
// 114,807,379, which is the high-water mark the test pins. Those were charged
// before an entry held its Link and at an overhead of 200 (see
// cacheEntryOverhead). The same entries now charge 56 bytes more each, 51,632
// across the 922, plus the Link of each paginated answer, at most four page
// URLs: under one percent of the total even if every entry carried one, which
// moves nothing below. Each sweep after the first adds only two to five
// entries and 18 to 124 KB, because nearly every URL a sweep asks for is the
// one it asked for last time; the growth is the few that carry a moving
// window in the query string. At the one minute cadence this is meant for
// that is 1440 sweeps a day, so three to seven thousand entries and 26 to 178
// MB a day accumulating on top of the sweep's own footprint with nothing to
// remove any of it. A fortnight of that is several gigabytes of bodies held
// for answers no collector will ask for again.
//
// 256 MB is 2.3 times the measured sweep, and the headroom is the whole point.
// A limit under one sweep's live set is not a smaller cache but no cache:
// every sweep would evict what the next one is about to ask for, and this
// tool's affordability rests on that not happening. In that run the second
// sweep revalidated 920 entries for 55 charged core requests, which is the
// mechanism the bound exists to protect. The first sweep paid for at least the
// 915 bodies it stored, every one of which arrived as a 200: that count, and
// not the harness's core counter, is the evidence for what a cold sweep costs,
// because the counter reports the whole token's spend in the window running
// now and so undercounts any sweep long enough to cross a reset.
//
// It is not a worst case, it is the steady state. An LRU against a key space
// that keeps growing fills to its bound and stays there, so a larger default
// is not free, it is resident memory. A deployment larger than that account
// raises it with SetCacheLimit and watches CacheStats().Evicted, which stays
// at zero for as long as the live set fits.
const DefaultCacheBytes = 256 << 20

// cacheEntryOverhead is charged against the limit on top of the bytes an entry
// carries, so that a cache full of small answers is accounted at what it
// really costs rather than at a fraction of it. Plenty of what a sweep stores
// is a few hundred bytes of body behind a ninety byte URL, and without this
// the bound would be measuring about half of that entry's memory.
//
// Measured, not guessed, on 2026-09-25 with Go 1.27.1: entries whose ETag,
// body and Link are empty, keyed by URLs allocated before the count started,
// cost between 198 and 253 bytes each in HeapAlloc across forty-one sizes
// from 100 to 235,000 entries, and 251 at the 922 one sweep holds, the spread
// being where the map's capacity happens to land. That is the map slot, whose
// key is a string and a reflect.Type, the container/list element, and the
// conditional struct, whose 88 bytes are allocated one at a time and so round
// up to a 96 byte size class. The URL, the ETag, the body and the Link are
// counted separately below. Two hundred and fifty-six rounds the measured
// range up, so the accounting errs towards charging more than the entry costs
// and never less.
//
// It was two hundred, measured when the key was the URL alone and the struct
// held no Link. The type in the key had already taken the real cost past it
// at most sizes, to between 182 and 237 at the same forty-one, and the Link's
// string header is the other sixteen bytes.
const cacheEntryOverhead = 256

// cacheKey names an entry: the URL, and the type its body was decoded into.
//
// The type is part of the key because the body is that type's encoding of
// the answer and nothing else (see replayable). One URL is decoded into two
// types when a repository is named in targets.repos: discovery reads four
// flags out of GET /repos/{owner}/{repo} and the repo family reads forty
// fields out of the same URL. Keyed by URL alone, the entry discovery wrote
// would answer the repo family's 304 with nothing but those four flags, and
// a repository with hundreds of stars would be recorded with none. Keyed by
// both, each caller has an entry of its own, each repeat is still
// conditional, and the only cost is the second entry's bytes.
//
// A call that decodes nothing keys on the nil type, which is the wire body's
// own entry.
type cacheKey struct {
	url string
	typ reflect.Type
}

// conditional is the ETag and the body of one URL, for one decoding type. The
// body is what a 304 is answered with: the decoded value encoded again, not
// the bytes GitHub sent (see replayable), so it is a fraction of the wire
// size.
//
// The Link header the body came with is kept beside it, because a 304 does
// not repeat it. Measured on 2026-09-25 against /stargazers/history: the 200
// for page 1 carries rel="next" and rel="last", and the 304 for the same page
// carries no Link at all. Every walk that finds its next page there, a page
// number or a cursor, would otherwise read a page 1 answered from memory as a
// list with no second page.
//
// One entry rather than two maps because the two are only useful together. An
// ETag whose body has been dropped still asks GitHub "only if it changed", and
// when GitHub answers 304 there is nothing to replay: GetJSON returns no rows
// and no error, and no collector in this repository looks at the cached flag
// to tell that apart from an endpoint that had nothing to say. Keeping the
// pair in one entry makes storing one without the other unrepresentable, and
// put refuses an entry with either half missing, so an ETag that is asked with
// always has a body to answer with.
type conditional struct {
	key  cacheKey
	etag string
	body []byte
	link string
}

func (e *conditional) size() int {
	return len(e.key.url) + len(e.etag) + len(e.body) + len(e.link) + cacheEntryOverhead
}

// cache is a byte-bounded LRU over those pairs.
//
// Bounded by bytes and not by entries because the entries are not comparable.
// The measured sweep stored 922 of them: the smallest body was 2 bytes and the
// largest 4.3 MB, a median of 2952 against a mean of 124165. The 62 entries
// over a megabyte held 77% of the total, and the 323 under a kilobyte held
// less than a thousandth of it. An entry count that leaves room for the large
// ones is no bound on memory at all, and one that bounds memory throws away
// the small ones for nothing.
//
// No cap on a single body either, though dropping those 62 would take the
// footprint from 115 MB to 26. Quota is charged per request and not per byte,
// so a 1.5 MB page of workflow runs and a 200 byte answer cost exactly the
// same one, and refusing to store the large ones buys memory with the only
// thing that is actually scarce: 62 unconditional requests every sweep is
// 89,000 a day against a budget of 120,000, and 127 GB of transfer. Memory is
// the cheap side of that trade by a wide margin.
//
// Least recently used, because the access pattern names the victim by itself.
// A sweep asks for the URLs that are still being collected, so anything that
// has not been asked for in a while is a URL the collectors have moved past: a
// page that has since shifted, a workflow run that has aged out of the window,
// a repository that was renamed. Those are exactly the entries that will never
// be hit again, and they are the whole of the growth.
type cache struct {
	limit   int
	bytes   int
	evicted int
	// order is most recently used at the front, so the victim is the back.
	order   *list.List
	entries map[cacheKey]*list.Element
}

// CacheStats is what the conditional-request cache currently holds.
//
// Evicted is the count since the client was made, and is the number that says
// whether the limit is set sensibly: a sweep that fits leaves it at zero
// forever, because every sweep asks for the same URLs and the only entries
// dropped are ones nothing asks for any more. A number that climbs by
// thousands per sweep means the limit is under one sweep's own footprint and
// the cache is evicting entries it is about to need.
type CacheStats struct {
	Entries int
	Bytes   int
	Limit   int
	Evicted int
}

func newCache(limit int) *cache {
	if limit <= 0 {
		limit = DefaultCacheBytes
	}
	return &cache{limit: limit, order: list.New(), entries: map[cacheKey]*list.Element{}}
}

// pairAt returns the pair an element of the recency order holds, or nil.
//
// put is the only thing that adds to the order and it only ever adds a
// *conditional, so nil means that stopped being true. Every caller then
// treats the element as holding nothing, which is the safe direction for this
// cache: a URL with no pair is asked unconditionally, and bytes that are never
// subtracted make the bound evict early rather than late.
func pairAt(el *list.Element) *conditional {
	e, ok := el.Value.(*conditional)
	if !ok {
		return nil
	}
	return e
}

// get returns the pair stored for key, and the Link header its body came
// with, and marks it as the most recently used.
func (k *cache) get(key cacheKey) (etag string, body []byte, link string) {
	el, ok := k.entries[key]
	if !ok {
		return "", nil, ""
	}
	e := pairAt(el)
	if e == nil {
		return "", nil, ""
	}
	k.order.MoveToFront(el)
	return e.etag, e.body, e.link
}

// put stores the pair for key, with the Link header the body came with, and
// evicts until the total fits again. The Link is not half of the pair: an
// answer with no Link is a list of one page, or no list, and is stored as
// such, which also forgets a Link an older body of the same URL came with.
func (k *cache) put(key cacheKey, etag string, body []byte, link string) {
	if etag == "" || len(body) == 0 {
		// Half a pair is worse than none. An ETag with no body would be asked
		// with and then answered by a 304 carrying nothing to replay, which
		// every caller reads as an endpoint that had nothing to say; a body
		// with no ETag can never be asked for. Either way the old entry, if
		// there is one, describes an answer this URL no longer gives.
		k.drop(key)
		return
	}
	e := &conditional{key: key, etag: etag, body: body, link: link}
	if e.size() > k.limit {
		// Nothing else would fit beside it, so keeping it would mean emptying
		// the cache for one page. Paying for that page again is the cheaper
		// half of that trade. Whatever was stored for this URL goes too: its
		// ETag still describes the body stored beside it, so it is answered
		// correctly and can now only ever be answered 200. It is memory held
		// for a hit that will never come.
		k.drop(key)
		return
	}
	if el, ok := k.entries[key]; ok {
		if old := pairAt(el); old != nil {
			k.bytes -= old.size()
		}
		el.Value = e
		k.bytes += e.size()
		k.order.MoveToFront(el)
	} else {
		k.entries[key] = k.order.PushFront(e)
		k.bytes += e.size()
	}
	for k.bytes > k.limit && k.order.Len() > 0 {
		k.evict()
	}
}

// drop forgets one entry. Forgetting is always safe: it costs one
// unconditional request, which is what the request would have cost with no
// cache at all.
func (k *cache) drop(key cacheKey) {
	el, ok := k.entries[key]
	if !ok {
		return
	}
	if e := pairAt(el); e != nil {
		k.bytes -= e.size()
	}
	delete(k.entries, key)
	k.order.Remove(el)
}

// evict forgets the least recently used entry.
func (k *cache) evict() {
	el := k.order.Back()
	if el == nil {
		return
	}
	k.order.Remove(el)
	k.evicted++
	if e := pairAt(el); e != nil {
		k.bytes -= e.size()
		delete(k.entries, e.key)
		return
	}
	// An element with no pair carries no key to delete by, and an index entry
	// left pointing at it would hold a put for that URL outside the recency
	// order, where no eviction could ever reach it. So the index is searched
	// for it instead, which costs a scan on a path that never runs.
	maps.DeleteFunc(k.entries, func(_ cacheKey, v *list.Element) bool { return v == el })
}

// setLimit changes the bound, evicting straight away when it shrinks.
func (k *cache) setLimit(limit int) {
	if limit <= 0 {
		limit = DefaultCacheBytes
	}
	k.limit = limit
	for k.bytes > k.limit && k.order.Len() > 0 {
		k.evict()
	}
}

// SetCacheLimit bounds the conditional-request cache to n bytes. Zero restores
// the default. A limit below one sweep's own footprint is not a smaller cache
// but no cache: every sweep would evict what the next one is about to ask for,
// and CacheStats().Evicted is where that shows.
func (c *Client) SetCacheLimit(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache.setLimit(n)
}

// CacheStats reports what the conditional-request cache holds.
func (c *Client) CacheStats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{
		Entries: len(c.cache.entries),
		Bytes:   c.cache.bytes,
		Limit:   c.cache.limit,
		Evicted: c.cache.evicted,
	}
}

// UnavailableError marks a feature that is switched off for this repository rather
// than an error: Dependabot, the dependency graph and code scanning all answer
// 403 or 404 when disabled. Collectors record it and move on instead of
// retrying in a loop.
type UnavailableError struct {
	Path   string
	Status int
	Reason string
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("%s: not available (%d): %s", e.Path, e.Status, e.Reason)
}

// TooLargeError means the GraphQL gateway gave up on a query before finishing it.
// Measured: an HTML 502 at about ten seconds, independent of the point cost.
// The remedy is a smaller page, not a retry of the same one.
type TooLargeError struct{ Status int }

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("graphql: the gateway gave up (%d); the query is too large for one request", e.Status)
}

// RateLimitedError means the budget for a bucket is spent.
//
// GitHub reports it as 403, the same status a switched-off feature answers
// with, and telling them apart matters more than it looks: a collector treats
// "this feature is off" as nothing to collect and moves on, so a rate limit
// misread as a feature flag makes a sweep report success while silently
// collecting nothing at all.
type RateLimitedError struct {
	Path     string
	Resource string
	Reset    time.Time
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("%s: the %s budget is spent until %s", e.Path, e.Resource, e.Reset.Format(time.TimeOnly))
}

// StatusError is an answer this package has no meaning of its own for: any
// status at or above 300 that is not one of the four above.
//
// It exists so the status is a number a caller can group by rather than only
// three words inside a message. Measured on the author's own account on
// 2026-09-16: one transient 502 on /repos/<repo>/actions/runs/<id>/jobs, once
// per repository, is what cost five repositories their entire workflow run and
// job history, and the only record of it was a line in the journal that read
// like every other line. The message is what it always was, so anything that
// reads the text, isPaginationLimit for one, reads the same text.
type StatusError struct {
	// Path is the request, relative to the REST base.
	Path string
	// Code is the status as a number, for a caller that wants to group by it.
	Code int
	// Status is what the response called it, "502 Bad Gateway".
	Status string
	// Body is what came with it, trimmed and bounded, empty where the answer
	// carried none.
	Body string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: %s", e.Path, e.Status)
	}
	return fmt.Sprintf("%s: %s: %s", e.Path, e.Status, e.Body)
}

// NotReadyError means GitHub accepted the request and is computing the answer.
// The four /stats/* endpoints do this: the first call returns 202 with an
// empty body and the numbers appear on a later call. Collectors skip the
// metric this round rather than blocking.
type NotReadyError struct{ Path string }

func (e *NotReadyError) Error() string { return e.Path + ": still being computed by GitHub (202)" }

// GetJSON fetches path (relative to the REST base) into out.
//
// It returns the response's Link header so a caller can paginate, the one
// stored with the body when the answer is a 304 that brought none, and
// reports whether the answer came from the ETag cache.
func (c *Client) GetJSON(ctx context.Context, path string, out any, accept string) (link string, cached bool, err error) {
	url := path
	if strings.HasPrefix(path, "/") {
		url = c.base + path
	}
	if stopped := c.brake(ctx, path); stopped != nil {
		return "", false, stopped
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", authScheme+c.token)
	req.Header.Set(userAgentHeader, userAgent)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)

	// The ETag and the body it belongs to are read together, once, and the
	// body is held for the rest of the call. Asking conditionally is a promise
	// that a 304 can be answered from memory, and the cache evicts: reading
	// the body only after the response arrived would let another goroutine
	// drop it in between, and a 304 with nothing to replay is indistinguishable
	// at every call site from an endpoint that answered with nothing.
	//
	// The entry is the one this caller's type wrote: a body stored by another
	// type decoding the same URL holds only what that type kept.
	key := cacheKey{url: url, typ: reflect.TypeOf(out)}
	c.mu.Lock()
	tag, saved, savedLink := c.cache.get(key)
	c.mu.Unlock()
	if len(saved) > 0 {
		req.Header.Set("If-None-Match", tag)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	c.readRate(resp)

	switch resp.StatusCode {
	case http.StatusNotModified:
		// Nothing changed and no quota was spent. Replay the body stored with
		// the ETag this asked with, so the caller still gets a value to
		// publish. A 304 nothing was stored for can only come from an
		// If-None-Match this client did not send.
		if len(saved) == 0 {
			return "", true, nil
		}
		// GitHub's 304 carries no Link, so the one read with the body is
		// replayed with it. A 304 that did carry one would be the server's
		// newer word on the stored answer, which RFC 9111 has a cache take
		// over the stored header, so that one is reported instead.
		link = resp.Header.Get("Link")
		if link == "" {
			link = savedLink
		}
		if out == nil {
			return link, true, nil
		}
		return link, true, json.Unmarshal(saved, out)
	case http.StatusAccepted:
		return "", false, &NotReadyError{Path: path}
	case http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if limit := c.rateLimited(resp, path); limit != nil {
			return "", false, limit
		}
		return "", false, &UnavailableError{Path: path, Status: resp.StatusCode, Reason: reasonOf(b)}
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", false, &StatusError{
			Path: path, Code: resp.StatusCode, Status: resp.Status,
			Body: string(bytes.TrimSpace(b)),
		}
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", false, err
	}
	if out != nil && len(raw) > 0 {
		if err = json.Unmarshal(raw, out); err != nil {
			return "", false, fmt.Errorf("%s: %w", path, err)
		}
	}
	link = resp.Header.Get("Link")
	if fresh := resp.Header.Get("ETag"); fresh != "" {
		c.mu.Lock()
		c.cache.put(key, fresh, replayable(raw, out), link)
		c.mu.Unlock()
	}
	return link, false, nil
}

// replayable is the body stored beside an ETag: what the caller decoded,
// serialized again, rather than the bytes GitHub sent.
//
// The two answer a 304 identically, because a 304 is decoded into the same
// type that was decoded on the 200 (the entry is keyed by that type, see
// cacheKey), and encoding/json is symmetric for every type the collectors
// decode into. What differs is the size. Measured on
// 2026-09-11 against jmrplens/jmrp.io: a page of a hundred workflow runs is
// 1,347,668 bytes on the wire and 58,521 stored, one twenty-third; across a
// sweep of eighteen repositories the raw bodies held 67.6 MB where the
// decoded values re-encoded hold about 7.6 MB, and every 304 then decodes a
// ninth of the bytes it used to. The bound is charged the stored size either
// way, so a sweep's live set now fits a smaller limit.
//
// The symmetry is a promise the decoding type makes: that it encodes every
// field it decodes. A `json:"-"` field keeps it, since it is decoded on
// neither path, and so does a json.RawMessage, which encodes as the bytes it
// holds. What breaks it is a custom UnmarshalJSON whose MarshalJSON twin
// writes less than it read, and a 304 then answers with less than the 200
// did. Nothing in this repository decodes into such a type, and the collect
// package's conditional parity test is what keeps that true: it collects
// every REST family twice against a fake that answers the repeat 304 and
// fails on the first point that differs.
//
// A value that cannot be encoded, or a call that decoded nothing, keeps the
// raw body: the cache is then no worse than it was, and only that answer
// stays at its wire size.
func replayable(raw []byte, out any) []byte {
	if out == nil || len(raw) == 0 {
		return raw
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return encoded
}

// GetText fetches a path that answers with plain text rather than JSON.
//
// Only the Actions job log does this, and it does it through a redirect to
// object storage. The Authorization header must not follow: the signed URL
// carries its own credentials and the storage endpoint rejects a request that
// arrives with both.
func (c *Client) GetText(ctx context.Context, path string) (string, error) {
	return c.GetTextAs(ctx, path, "")
}

// GetTextAs is GetText with a media type of the caller's choosing, for the
// one endpoint whose narrow representation is not JSON: a commit asked for
// as application/vnd.github.sha answers with its forty hex digits and nothing
// else, where the JSON form is kilobytes of author, tree and file diff. Empty
// means the API's default.
//
// An answer that came straight from the API is cached by its ETag like a
// JSON one, so the repeat is a free 304: measured on 2026-09-11, the bare
// SHA carries an ETag and a second request with it is answered 304 with
// nothing charged. The one that came through a redirect is not: the log
// blob is up to eight megabytes, is read once per failed job, and its
// storage ETag is not the API's to honor.
func (c *Client) GetTextAs(ctx context.Context, path, accept string) (string, error) {
	url := path
	if strings.HasPrefix(path, "/") {
		url = c.base + path
	}
	if err := c.brake(ctx, path); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", authScheme+c.token)
	req.Header.Set(userAgentHeader, userAgent)
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)

	// The text has no decoding type, and the nil type is a key no GetJSON
	// caller writes under: GetJSON stores a nil out's body under the type of
	// nil too, but nothing asks the same URL both ways.
	key := cacheKey{url: url}
	c.mu.Lock()
	tag, saved, _ := c.cache.get(key)
	c.mu.Unlock()
	if len(saved) > 0 {
		req.Header.Set("If-None-Match", tag)
	}

	client := &http.Client{
		Timeout: c.http.Timeout,
		// The same pool as the client this request belongs to, rather than a
		// nil Transport, which would send it through the process-wide
		// default: a new client is built here for every request that may
		// redirect, and each one would be borrowing connections from
		// whatever else in the process is using that default.
		Transport: c.http.Transport,
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			r.Header.Del("Authorization")
			// The redirect itself is the API's answer and carries the rate
			// headers; the final response comes from storage and carries
			// none. Go only returns the final one, so the budget is read
			// here, on the way through.
			if r.Response != nil {
				c.readRate(r.Response)
			}
			if len(via) >= 5 {
				return fmt.Errorf("%s: too many redirects", path)
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	c.readRate(resp)

	switch resp.StatusCode {
	case http.StatusNotModified:
		// Only ever asked for with a body to replay, as in GetJSON.
		return string(saved), nil
	case http.StatusForbidden, http.StatusNotFound, http.StatusGone, http.StatusTooManyRequests:
		if limit := c.rateLimited(resp, path); limit != nil {
			return "", limit
		}
		return "", &UnavailableError{Path: path, Status: resp.StatusCode, Reason: "no log"}
	}
	if resp.StatusCode >= 300 {
		return "", &StatusError{Path: path, Code: resp.StatusCode, Status: resp.Status}
	}
	// Bounded: a job can print hundreds of megabytes and only the tail is kept
	// anyway, so reading it all would be paying for nothing.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	// Straight from the API, not from the storage a redirect led to: the
	// final request is the one asked for exactly when nothing redirected.
	if fresh := resp.Header.Get("ETag"); fresh != "" && resp.Request.URL.String() == url {
		// No Link: GetText returns none, so storing one would charge the
		// bound for a header nothing can read back.
		c.mu.Lock()
		c.cache.put(key, fresh, b, "")
		c.mu.Unlock()
	}
	return string(b), nil
}

// GraphQL runs one query and unmarshals data into out.
func (c *Client) GraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	return c.graphql(ctx, query, vars, out, true)
}

// GraphQLRate reads the GraphQL budget with a query that is not charged for it.
//
// It exists because the endpoint that reports every other bucket does not
// report this one. Measured on 2026-09-08 against api.github.com with the
// token this runs under: `GET /rate_limit` answered the graphql bucket
// used=0, remaining=5000 in the same minute two ordinary queries here moved
// the real counter from 161 to 162.
//
// The query itself is free. Measured the same day: two consecutive
// `{ rateLimit { ... } }` calls both answered used=167, while an ordinary
// query moved used by one every time. The block reports cost 1 all the same,
// which is why this path is left out of GraphQLSpend.
//
// It also goes around the brake, because a client refusing to spend its
// reserve must still be able to say how little is left.
func (c *Client) GraphQLRate(ctx context.Context) (RateState, error) {
	if err := c.graphql(ctx, BudgetQuery, nil, nil, false); err != nil {
		return RateState{}, err
	}
	st, _ := c.RateFor("graphql")
	return st, nil
}

// GraphQLSpend returns what this process has spent on GraphQL so far.
func (c *Client) GraphQLSpend() GraphQLSpend {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.spend
}

// graphql sends one query. charged says whether it spends the budget, which
// decides both whether the brake applies and whether the cost is counted.
func (c *Client) graphql(ctx context.Context, query string, vars map[string]any, out any, charged bool) error {
	if charged {
		if err := c.brake(ctx, graphqlPath); err != nil {
			return err
		}
	}
	query, hasRate := withRateLimit(query)
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+graphqlPath, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authScheme+c.token)
	req.Header.Set(userAgentHeader, userAgent)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	c.readRate(resp)

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if resp.StatusCode >= 500 || !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		// The gateway answers a query it cannot finish in about ten seconds
		// with an HTML 502, whatever the point cost. That is a request too
		// large, not a server down, and the caller should shrink it.
		return &TooLargeError{Status: resp.StatusCode}
	}
	if err = json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("graphql: %w", err)
	}
	// Before the errors below, because a query that half worked was charged
	// in full: a ten-repository batch with one repository renamed away answers
	// 200 with the data, the block, `cost: 1`, and a NOT_FOUND for that one
	// alias. Reading the block only on the clean path left exactly those
	// points uncounted, which is the common case and not the rare one.
	if hasRate && len(envelope.Data) > 0 {
		c.readGraphQLRate(envelope.Data, charged)
	}
	// GraphQL answers 200 with an errors array, so the status alone says
	// nothing. A missing field on one repository must not fail the sweep.
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("graphql: %s: %s", envelope.Errors[0].Type, envelope.Errors[0].Message)
	}
	if out == nil {
		return nil
	}
	if len(envelope.Data) == 0 {
		// A body with neither data nor errors is not a GraphQL answer at all.
		// It is what the REST 404 page looks like when a base URL is wrong,
		// and decoding it reported "unexpected end of JSON input", which says
		// nothing about what happened.
		return fmt.Errorf("graphql: %s: no data in the response", resp.Status)
	}
	return json.Unmarshal(envelope.Data, out)
}

// rateLimited reports a spent budget, or nil when the refusal was about
// something else. The headers are the evidence, not the message: GitHub sets
// x-ratelimit-remaining to zero, and on a secondary limit sends Retry-After
// instead.
func (c *Client) rateLimited(resp *http.Response, path string) error {
	resource := resp.Header.Get("x-ratelimit-resource")
	if resource == "" {
		resource = "core"
	}
	if resp.Header.Get("x-ratelimit-remaining") == "0" {
		reset := time.Now().Add(time.Minute)
		if sec, err := strconv.Atoi(resp.Header.Get("x-ratelimit-reset")); err == nil && sec > 0 {
			reset = time.Unix(int64(sec), 0)
		}
		return &RateLimitedError{Path: path, Resource: resource, Reset: reset}
	}
	if v := resp.Header.Get("retry-after"); v != "" {
		sec, err := strconv.Atoi(v)
		if err != nil || sec <= 0 {
			sec = 60
		}
		return &RateLimitedError{Path: path, Resource: "secondary", Reset: time.Now().Add(time.Duration(sec) * time.Second)}
	}
	return nil
}

// Reserve is how much of a bucket the client refuses to spend, and Wait says
// what to do when it gets there.
//
// This lives in the client rather than in the sweep loop because the sweep
// loop only sees the boundaries between repositories and families. One
// repository's workflow runs can be a thousand requests on their own, and a
// brake checked only at the boundary is a brake that is never reached in time.
func (c *Client) SetReserve(n int, wait bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reserve, c.wait = n, wait
}

// brake blocks or refuses when the bucket this request will charge is spent.
//
// The bucket is inferred from the path, which is exact for the three that
// matter: GraphQL has its own, anything under /search has its own, and
// everything else is core.
func (c *Client) brake(ctx context.Context, path string) error {
	bucket := "core"
	switch {
	case strings.HasPrefix(path, "/search"):
		bucket = "search"
	case strings.HasSuffix(path, graphqlPath):
		bucket = "graphql"
	}
	for {
		c.mu.Lock()
		reserve, wait := c.reserve, c.wait
		st, seen := c.rates[bucket]
		c.mu.Unlock()
		if reserve <= 0 || !seen || st.Limit == 0 {
			return nil
		}
		// Scaled to the bucket: search allows thirty requests a minute against
		// core's five thousand, so a flat reserve would make it unusable.
		if fifth := st.Limit / 5; fifth < reserve {
			reserve = fifth
		}
		if st.Remaining > reserve {
			return nil
		}
		until := time.Until(st.Reset)
		if until <= 0 {
			// The window has turned over; the next response will say so.
			c.mu.Lock()
			st.Remaining = st.Limit
			c.rates[bucket] = st
			c.mu.Unlock()
			return nil
		}
		if !wait {
			return &RateLimitedError{Path: path, Resource: bucket, Reset: st.Reset}
		}
		if c.OnWait != nil {
			c.OnWait(bucket, until)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(until + time.Second):
		}
	}
}

func (c *Client) readRate(resp *http.Response) {
	atoi := func(h string) int { n, _ := strconv.Atoi(resp.Header.Get(h)); return n }
	c.mu.Lock()
	defer c.mu.Unlock()
	if resp.Header.Get("x-ratelimit-limit") != "" {
		resource := resp.Header.Get("x-ratelimit-resource")
		if resource == "" {
			resource = "core"
		}
		st := RateState{
			Limit:     atoi("x-ratelimit-limit"),
			Used:      atoi("x-ratelimit-used"),
			Remaining: atoi("x-ratelimit-remaining"),
			Resource:  resource,
		}
		if sec := atoi("x-ratelimit-reset"); sec > 0 {
			st.Reset = time.Unix(int64(sec), 0)
		}
		c.rates[resource] = st
		c.last = resource
	}
}

// RateLimitAlias is the key the budget block lands under in the answer. An
// alias rather than the bare field so it can collide with nothing: not with a
// selection a collector already makes, and not with the r0..rN aliases the
// batched queries give their repositories.
//
// Exported for the same reason BudgetQuery is: the end-to-end fake GitHub
// puts a budget block under this key in every answer, so a sweep against it
// is priced the way a sweep against api.github.com is.
const RateLimitAlias = "ghcRateLimit"

// rateLimitBlock is added to the root of every query, and BudgetQuery is that
// block on its own.
//
// BudgetQuery is exported because the end-to-end fake GitHub has to recognize
// it, and it can only do so by equality: the same block is injected into every
// other query, so no substring of it identifies the probe. Matching the
// constant rather than a copy of its text means a change to the block reaches
// the fake instead of quietly turning the probe into a query nothing answers.
const (
	rateLimitBlock = RateLimitAlias + ": rateLimit { limit cost used remaining resetAt }"
	BudgetQuery    = "query { " + rateLimitBlock + " }"
)

// withRateLimit adds the budget block to the root selection set of the first
// query operation in the document, and reports whether the document carries
// the block afterwards.
//
// It happens here, once per request, rather than in each collector: three of
// the queries are built as a batch of aliased repositories, and a block per
// alias would be the same five numbers repeated ten times for the same money.
// Measured on 2026-09-08, that money is nothing: the same query was billed
// cost 1 with the block and without it.
//
// A document this cannot place the block in confidently is sent unchanged. A
// missing budget reading is a missing row; a mangled query loses everything
// the request was for.
func withRateLimit(query string) (string, bool) {
	if strings.Contains(query, RateLimitAlias) {
		// Already carries it: the budget query itself.
		return query, true
	}
	var s rootScan
	for i := 0; i < len(query); i++ {
		switch ch := query[i]; {
		case ch == '#':
			i = endOfComment(query, i)
		case ch == '"':
			i = endOfString(query, i)
		case isNameByte(ch):
			end := endOfName(query, i)
			s.name(query[i : end+1])
			i = end
		case ch == '{' && !s.inValue():
			if s.atRootQuery() {
				return query[:i+1] + " " + rateLimitBlock + query[i+1:], true
			}
			s.depth++
		default:
			s.punctuation(ch)
		}
	}
	return query, false
}

// rootScan is where withRateLimit's walk through a document stands: how deep
// in selection sets, how deep in argument lists and list values, and which
// definition it is inside.
type rootScan struct {
	depth, paren, bracket int
	// word is the keyword the current definition opened with, which is what
	// tells a query from a fragment or a mutation. The empty string is the
	// anonymous shorthand, which is a query too.
	word string
}

// inValue reports whether the walk is inside an argument list or a list
// value. A brace there is an input object, a value and not a selection set.
// Counting it as structure would make `mutation($i: In = {a: 1}) { ... }`
// look like a definition that opened and closed before the root brace, and
// the block would go into the mutation, where rateLimit does not exist.
func (s *rootScan) inValue() bool { return s.paren > 0 || s.bracket > 0 }

// atRootQuery reports whether a structural brace here opens the root
// selection set of a query.
func (s *rootScan) atRootQuery() bool {
	return s.depth == 0 && (s.word == "" || s.word == "query")
}

// name records the first name of a definition, which is its keyword.
func (s *rootScan) name(word string) {
	if s.depth == 0 && s.paren == 0 && s.word == "" {
		s.word = word
	}
}

// punctuation follows the brackets that decide what a brace means, and the
// closing brace that ends a definition. An opening brace reaches it only
// inside a value, where it is not structure and changes nothing.
func (s *rootScan) punctuation(ch byte) {
	switch ch {
	case '(':
		s.paren++
	case ')':
		s.paren--
	case '[':
		s.bracket++
	case ']':
		s.bracket--
	case '}':
		if s.inValue() {
			return
		}
		s.depth--
		if s.depth <= 0 {
			// The definition ended; the next name opens the next one.
			s.depth, s.word = 0, ""
		}
	}
}

// endOfComment returns the index of the newline that ends the comment
// starting at i, so the loop's own step lands past it. A comment with no
// newline after it runs to the end of the document.
func endOfComment(q string, i int) int {
	if nl := strings.IndexByte(q[i:], '\n'); nl >= 0 {
		return i + nl
	}
	return len(q)
}

// endOfName returns the index of the last byte of the name starting at i.
func endOfName(q string, i int) int {
	for i+1 < len(q) && isNameByte(q[i+1]) {
		i++
	}
	return i
}

// endOfString returns the index of the quote that closes the string starting
// at i, so braces inside a literal are not read as structure.
func endOfString(q string, i int) int {
	if strings.HasPrefix(q[i:], `"""`) {
		if end := strings.Index(q[i+3:], `"""`); end >= 0 {
			return i + 3 + end + 2
		}
		return len(q)
	}
	for j := i + 1; j < len(q); j++ {
		switch q[j] {
		case '\\':
			j++
		case '"':
			return j
		}
	}
	return len(q)
}

func isNameByte(ch byte) bool {
	return ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
}

// readGraphQLRate takes the budget out of an answer that carries the block.
//
// This is the only honest reading of the GraphQL budget there is, so it wins
// over the x-ratelimit-* headers of the same response. Measured on
// 2026-09-08 the two agreed exactly, down to the reset instant; the block is
// the counter GitHub documents, so it is the one kept if they ever drift.
//
// The answer is parsed twice, once here for five numbers and once by the
// caller for its own type. That is cheaper than making every collector's
// struct carry a field it did not ask for.
func (c *Client) readGraphQLRate(data json.RawMessage, charged bool) {
	var body struct {
		// The key is RateLimitAlias, which a struct tag cannot name.
		Rate *struct {
			Limit     int       `json:"limit"`
			Cost      int       `json:"cost"`
			Used      int       `json:"used"`
			Remaining int       `json:"remaining"`
			ResetAt   time.Time `json:"resetAt"`
		} `json:"ghcRateLimit"`
	}
	if err := json.Unmarshal(data, &body); err != nil || body.Rate == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rates["graphql"] = RateState{
		Limit:     body.Rate.Limit,
		Used:      body.Rate.Used,
		Remaining: body.Rate.Remaining,
		Reset:     body.Rate.ResetAt,
		Resource:  "graphql",
	}
	c.last = "graphql"
	if charged {
		c.spend.Queries++
		c.spend.Cost += body.Rate.Cost
	}
}

func reasonOf(body []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &m) == nil && m.Message != "" {
		return m.Message
	}
	return string(bytes.TrimSpace(body))
}
