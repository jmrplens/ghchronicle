package run

import (
	"bufio"
	"cmp"
	"compress/gzip"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
	"github.com/jmrplens/ghchronicle/v2/internal/config"
	"github.com/jmrplens/ghchronicle/v2/internal/ghapi"
)

// The cache file is what a process learned about GitHub that makes its next
// pass cheap, kept for the process after it: the conditional cache of the
// client, the workflow runs whose jobs were written, the refusals with the
// instant each one's day ends, and the page sizes the totals family set.
//
// All four used to live in the process alone, and every restart paid for
// them again. Measured on the production service's proxy log: the restart of
// 2026-09-26 19:52Z made 130 requests in its first sweep and not one was
// answered 304, and in its first 38 minutes it spent 1,092 charged core
// requests on passes that cost about 66 warm, 269 of them listing the jobs of
// runs the process before it had already written; the start of 2026-09-25
// 12:58Z spent about 1,350 on first passes that cost about 64 warm. The
// journal holds ten starts between 2026-09-18 and 2026-09-26, most of them
// upgrades, and a daily family in a process that did not live a day never got
// a 304 at all.
//
// It is not the state file, and on purpose. Everything here only makes a
// pass cheaper: deleting it costs one pass of each family at the price of a
// restart before this file existed, and loses nothing, where the state file
// holds last_head, whose loss loses data. It is also a megabyte and a half of
// compressed binary per thousand answers, which a file a reader opens to see
// when a family last ran should not carry.

// cacheMagic opens the file, and its digit is the version of what follows. A
// file that does not start with it is not read, which is how a binary that
// writes another version and one that reads this one keep out of each
// other's way: each starts cold once, and nothing is misread.
const cacheMagic = "GHCACHE1\n"

// cacheSaveEvery is the least time between two saves while running, the
// written-points ledger's own interval, for the same reason: what the file
// is for is surviving a restart, and a lost few minutes of it cost those
// minutes' answers once more. A stop saves it whatever the interval says.
const cacheSaveEvery = 5 * time.Minute

// The bounds on what the file keeps of the conditional cache, on top of the
// horizon: an answer larger than cacheMaxAnswer is left out, and the answers
// stop, least recently used first, once they come to cacheMaxBytes. Both
// count the bytes an answer carries, the body being nearly all of it.
//
// Measured on 2026-09-27 with this code against the account the production
// service collects, 37 repositories and every family its configuration runs,
// in one sweep that ran every family from a copy of the service's state file,
// which is the shape of the first sweep after a restart: 1,065 answers and
// 10.3 MB of bodies, none over a megabyte, the largest the account's event
// feed at 404 KB and then the failed runs of the last month and the releases
// of the busiest repositories at 285 and 262 KB. The dependency graph's
// SBOMs, up to 786 KB each as GitHub sends them once uncompressed, are kept
// as what their collector decodes of them, 1.4 to 12 KB for the four that
// sweep stored. Written by writeCache, that is 1.6 MB on disk, written and
// flushed to the NVMe disk of the machine the service runs on in 35 to 39 ms
// and read back in 33 to 35, five times each.
//
// Over days the cache grows by the answers that carry a moving window in the
// query string, which is what the horizon is for. The production proxy's log
// of the process that ran from 2026-09-19 18:22Z to 2026-09-25 09:41Z gives
// the URLs that process held, 6,695 at the end. Priced at what the sweep
// above kept of each kind of URL against the uncompressed size the log gives
// for its last 200, they come to about 99 MB, which is what the cache of a
// process that lives a week holds in memory, under the client's own bound;
// the 2,517 asked for in the last 48 hours, the horizon at the default
// cadences, come to about 31 MB, and none of them to more than 389 KB.
//
// So a megabyte keeps every answer measured, and bounds what one list
// grown past anything seen here can take of the file to a sixty-fourth of
// it; and sixty-four megabytes is those 31 twice over, about ten megabytes on
// disk at the ratio above. An answer left out costs one charged request after
// the next restart, which is what it cost before this file existed.
const (
	cacheMaxAnswer = 1 << 20
	cacheMaxBytes  = 64 << 20
)

// cacheHeader is the first thing in the file, and says how many answers
// follow it.
type cacheHeader struct {
	// Written is when the file was saved.
	Written  time.Time
	Counts   map[string]collect.ItemCounts
	Refusals map[string][]collect.Refused
	Expanded []expandedRun
	// Stores is the sinks the jobs of Expanded were offered to, by name.
	Stores  []string
	Answers int
}

// expandedRun is one workflow run attempt whose jobs were written, and when
// a sweep last listed it, in Unix seconds.
type expandedRun struct {
	ID      int64
	Attempt int
	Listed  int64
}

// cacheHorizon is how long the file keeps what nothing has asked for: twice
// the longest cadence this configuration runs a family at, and never under a
// day.
//
// A family asks for the same URLs on every pass, so what it has not asked
// for in longer than its own cadence is a URL it has moved past: a window in
// a query string, a page a list no longer reaches, a repository renamed. The
// longest cadence is the only one that has to fit, since every entry belongs
// to some family, and twice it leaves room for one pass that was skipped for
// want of budget or failed without its answers being dropped. Measured on the
// production proxy's log over the process that ran from 2026-09-19 18:22Z to
// 2026-09-25 09:41Z, whose slowest families ran daily: the conditional cache
// held 6,695 entries at the end, and 2,517 of them had been asked for in the
// 48 hours before it.
func (r *Runner) cacheHorizon() time.Duration {
	longest := time.Duration(0)
	for _, name := range config.Families() {
		if every, enabled := r.Cfg.Interval(name); enabled && every > longest {
			longest = every
		}
	}
	return max(2*longest, 24*time.Hour)
}

// loadCache reads CacheFile into this runner and its client, once, at the
// first sweep. A file that does not load in full is not read at all: a pass
// with nothing remembered costs quota, and one answered from a half read
// file could replay the wrong body.
func (r *Runner) loadCache(now time.Time) {
	if r.cacheLoaded || r.CacheFile == "" {
		return
	}
	// As good as saved: what is in memory now is what the file holds, or
	// nothing, and the first save is due an interval from here rather than
	// after the first family.
	r.cacheLoaded, r.cacheSaved = true, time.Now()
	head, answers, err := readCache(r.CacheFile)
	if errors.Is(err, fs.ErrNotExist) {
		r.Log.Debug("no cache file yet, the first pass of each family pays in full", "file", r.CacheFile)
		return
	}
	if err != nil {
		r.Log.Warn("cache file not read, the first pass of each family pays in full",
			"file", r.CacheFile, "err", err)
		return
	}
	taken := r.API.Restore(answers)
	if len(head.Counts) > 0 {
		r.counts = maps.Clone(head.Counts)
	}
	refused := 0
	for family, list := range head.Refusals {
		// Nil for a backfill, which asks everything and so recalls nothing.
		m := r.refusalsFor(family)
		m.Recall(list)
		refused += len(m.Standing())
	}
	// The runs are a claim that every store holds their jobs. A store being
	// filled again does not (see Refill), and nor does one added since the
	// file was written, which the ledger, keyed by sink, offers everything.
	r.expanded = make(map[collect.RunKey]time.Time, len(head.Expanded))
	switch stores := r.storeNames(); {
	case len(head.Expanded) == 0:
	case r.Refill && r.RefillEveryStart:
		r.Log.Debug("not every store keeps a write ledger, listing the jobs of the runs the cache file remembers again",
			"runs", len(head.Expanded))
	case r.Refill:
		r.Log.Info("the write ledger remembers nothing, listing the jobs of the runs the cache file remembers again",
			"runs", len(head.Expanded))
	case !subset(stores, head.Stores):
		r.Log.Info("a store was added since the cache file was written, listing the jobs of the runs it remembers again",
			"stores", strings.Join(stores, ","), "written_to", strings.Join(head.Stores, ","), "runs", len(head.Expanded))
	default:
		for _, run := range head.Expanded {
			r.expanded[collect.RunKey{ID: run.ID, Attempt: run.Attempt}] = time.Unix(run.Listed, 0)
		}
	}
	r.expandedKept = maps.Clone(r.expanded)
	r.Log.Info("cache file read", "file", r.CacheFile, "written", head.Written.Format(time.RFC3339),
		"answers", taken, "runs", len(r.expanded), "refusals", refused, "page_sizes", len(r.counts),
		"age", now.Sub(head.Written).Round(time.Second).String())
}

// settleExpanded ends an actions pass: the runs whose jobs it listed become
// the ones the cache file keeps when every sink took its rows, and are
// forgotten when one did not.
//
// The collector adds a run the moment its jobs are in hand, which is before
// any sink has seen them. A sink that fails then, or a shutdown that cancels
// the write, leaves runs remembered whose jobs no store holds, and a memory
// that crosses a restart would keep them that way for good, since no sweep
// lists the jobs of a run it remembers. Forgotten, the next pass lists them
// again, a free 304 where the listing is still cached, and writes them. A
// stop in the middle of the pass never reaches here at all, and the file
// keeps what the pass before it settled.
func (r *Runner) settleExpanded(delivered bool) {
	if delivered {
		r.expandedKept = maps.Clone(r.expanded)
		return
	}
	r.expanded = maps.Clone(r.expandedKept)
}

// saveCacheSoon saves the cache file if a family has run since the last save
// and the last save is at least cacheSaveEvery old.
//
// A save that failed waits the interval too, as if it had worked: a disk that
// refuses one save refuses the next family's as well, and asking after every
// family would put a line per family in the log, every sweep, for one full
// disk.
func (r *Runner) saveCacheSoon() {
	if !r.cacheDirty || time.Since(r.cacheSaved) < cacheSaveEvery {
		return
	}
	if err := r.saveCache(); err != nil {
		r.cacheSaved = time.Now()
		r.Log.Warn("cache file not saved", "file", r.CacheFile, "err", err)
	}
}

// SaveCache writes the cache file now, if anything has run since it was last
// written. The command calls it on the way out, however the run ended, which
// is what makes the file the last thing the process knew rather than what it
// knew five minutes before it stopped.
func (r *Runner) SaveCache() error {
	if !r.cacheDirty {
		return nil
	}
	return r.saveCache()
}

// saveCache writes what the file keeps.
//
// A card-only run writes nothing, for the reason it leaves the state file
// alone: the runs in expanded are a claim that their jobs reached a store,
// and a card-only run's reached none.
//
// A backfill reads the file and writes nothing to it either. What it asks
// for is every page of every list, pages no sweep asks for, and each of them
// is more recently used than anything the sweeps before it stored, so the
// file's total would keep the walk's pages and leave out the sweeps' own: a
// service started after a backfill would start colder than it stopped. Its
// runs are none, since it lists the jobs of every run, and it asks every
// refused path again rather than recalling one.
func (r *Runner) saveCache() error {
	if r.CacheFile == "" || r.CardOnly || r.Backfill {
		return nil
	}
	wall, now := time.Now(), r.clock()
	horizon := r.cacheHorizon()
	answers := keptAnswers(r.API.Answers(), wall.Add(-horizon).Unix(), cacheMaxAnswer, cacheMaxBytes)
	head := cacheHeader{
		Written:  now,
		Counts:   r.counts,
		Refusals: r.standingRefusals(),
		Expanded: listedSince(r.expandedKept, now.Add(-horizon)),
		Stores:   r.storeNames(),
	}
	if err := writeCache(r.CacheFile, &head, answers); err != nil {
		return err
	}
	r.cacheSaved, r.cacheDirty = wall, false
	r.Log.Debug("cache file saved", "file", r.CacheFile, "answers", len(answers),
		"runs", len(head.Expanded), "took", time.Since(wall).Round(time.Millisecond).String())
	return nil
}

// storeNames is the names of the sinks this runner writes to, sorted, each
// once.
func (r *Runner) storeNames() []string {
	names := make([]string, 0, len(r.Sinks))
	for _, s := range r.Sinks {
		names = append(names, s.Name())
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// subset reports whether every name in some is in all.
func subset(some, all []string) bool {
	for _, name := range some {
		if !slices.Contains(all, name) {
			return false
		}
	}
	return true
}

// standingRefusals is every family's refusals whose day has not ended.
func (r *Runner) standingRefusals() map[string][]collect.Refused {
	out := map[string][]collect.Refused{}
	for family, m := range r.refusals {
		if standing := m.Standing(); len(standing) > 0 {
			out[family] = standing
		}
	}
	return out
}

// keptAnswers is what the file keeps of the cache: answers asked for since
// the horizon, each no larger than one, in the order the cache holds them,
// until they come to total.
//
// all is most recently used first, so what the total leaves out is always
// the least recently used, which is what the cache itself would evict first.
func keptAnswers(all []ghapi.Answer, since int64, one, total int) []ghapi.Answer {
	kept := make([]ghapi.Answer, 0, len(all))
	used := 0
	for i := range all {
		a := &all[i]
		if a.Used < since {
			continue
		}
		size := len(a.URL) + len(a.Type) + len(a.ETag) + len(a.Link) + len(a.Body)
		if size > one {
			continue
		}
		if used+size > total {
			break
		}
		used += size
		kept = append(kept, *a)
	}
	return kept
}

// listedSince is the expanded runs a sweep listed at or after since, sorted
// so that two saves of the same memory write the same bytes.
func listedSince(expanded map[collect.RunKey]time.Time, since time.Time) []expandedRun {
	out := make([]expandedRun, 0, len(expanded))
	for key, listed := range expanded {
		if listed.Before(since) {
			continue
		}
		out = append(out, expandedRun{ID: key.ID, Attempt: key.Attempt, Listed: listed.Unix()})
	}
	slices.SortFunc(out, func(a, b expandedRun) int {
		if a.ID != b.ID {
			return cmp.Compare(a.ID, b.ID)
		}
		return cmp.Compare(a.Attempt, b.Attempt)
	})
	return out
}

// writeCache writes the file: the magic, then one gzip stream holding the
// header and each answer as a gob message of its own, so an answer is
// encoded while the one before it is already on its way to the disk rather
// than all of them being held in one buffer.
func writeCache(path string, head *cacheHeader, answers []ghapi.Answer) error {
	head.Answers = len(answers)
	return replaceFileWith(path, func(w io.Writer) error {
		if _, err := io.WriteString(w, cacheMagic); err != nil {
			return err
		}
		z, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			return err
		}
		if encoded := encodeCache(z, head, answers); encoded != nil {
			return encoded
		}
		return z.Close()
	})
}

// encodeCache is the part of the file inside the gzip stream.
func encodeCache(w io.Writer, head *cacheHeader, answers []ghapi.Answer) error {
	enc := gob.NewEncoder(w)
	if err := enc.Encode(head); err != nil {
		return err
	}
	for i := range answers {
		if err := enc.Encode(&answers[i]); err != nil {
			return err
		}
	}
	return nil
}

// readCache reads a file writeCache wrote, whole or not at all.
//
// The gzip trailer carries a checksum of everything before it and is read
// only at the end of the stream, so the stream is read to its end after the
// last answer: a file cut short, or damaged anywhere, is refused there rather
// than half believed.
func readCache(path string) (*cacheHeader, []ghapi.Answer, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = f.Close() }()
	in := bufio.NewReaderSize(f, 1<<16)
	magic := make([]byte, len(cacheMagic))
	if _, err = io.ReadFull(in, magic); err != nil || string(magic) != cacheMagic {
		return nil, nil, errors.New("not a cache file of this version")
	}
	z, err := gzip.NewReader(in)
	if err != nil {
		return nil, nil, err
	}
	// Buffered here so the decoder reads from this reader rather than from
	// one of its own, and whatever it did not need is still here to drain.
	stream := bufio.NewReaderSize(z, 1<<16)
	dec := gob.NewDecoder(stream)
	var head cacheHeader
	if err = dec.Decode(&head); err != nil {
		return nil, nil, fmt.Errorf("header: %w", err)
	}
	if head.Answers < 0 {
		return nil, nil, fmt.Errorf("header: %d answers", head.Answers)
	}
	// Not sized by the header up front: a count nothing has checked yet is
	// no reason to allocate for it.
	var answers []ghapi.Answer
	for i := range head.Answers {
		var a ghapi.Answer
		if err = dec.Decode(&a); err != nil {
			return nil, nil, fmt.Errorf("answer %d of %d: %w", i+1, head.Answers, err)
		}
		answers = append(answers, a)
	}
	extra, err := io.Copy(io.Discard, stream)
	if err != nil {
		return nil, nil, err
	}
	if extra > 0 {
		return nil, nil, fmt.Errorf("%d bytes after the last answer", extra)
	}
	return &head, answers, nil
}

// pageSizesUnknown reports, once per process, whether its first sweep has no
// counts to size the pull request page from and asks for them first: see
// sizeFirst. A backfill does not size the page, and a configuration that
// does not collect pull requests does not need it sized.
func (r *Runner) pageSizesUnknown() bool {
	if r.countsAsked {
		return false
	}
	r.countsAsked = true
	_, issues := r.Cfg.Interval("issues")
	return issues && !r.Backfill && len(r.counts) == 0
}
