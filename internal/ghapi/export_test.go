package ghapi

// CacheEntry is one entry of the conditional-request cache, as the live
// measurement harness in livesweep_test.go needs to see it: the charged size
// is what the bound is enforced against, and the body size is what a sweep
// actually moved. The two differ by the per-entry overhead, which is most of
// an entry when the answer is small.
type CacheEntry struct {
	URL     string
	ETag    int
	Body    int
	Charged int
}

// CacheEntries lists what the cache holds. Test-only: the shipped surface is
// CacheStats, which reports the totals without naming any URL.
func CacheEntries(c *Client) []CacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CacheEntry, 0, len(c.cache.entries))
	for key, el := range c.cache.entries {
		e := el.Value.(*conditional)
		out = append(out, CacheEntry{URL: key.url, ETag: len(e.etag), Body: len(e.body), Charged: e.size()})
	}
	return out
}
