package sink

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Ledger remembers the last value written for every point, so a sweep can
// offer the same history again and only what changed goes out.
//
// Rewriting a point that already exists is not a duplicate: a store that keys
// a row by measurement, tag set and timestamp overwrites it, which is what
// makes re-collection converge instead of accumulating and lets a sweep
// rewrite GitHub's fourteen-day traffic window in full.
//
// It is not free either. InfluxDB 3 Core writes one Parquet file per partition
// per write request and never compacts them, so a sweep that offers a year of
// dated history every hour leaves one file per day per sweep. Measured on the
// account this was developed against: gh_notification held 27,131 rows in
// 17,270 files, and a query over fourteen days was refused outright with
// "Query would scan 10000 Parquet files, exceeding the file limit". The rows
// were right; the number of files they arrived in was not.
//
// Aggregating harder in the query does not help, because the limit counts the
// files the planner has to open, before any aggregation happens. The fix has
// to be at the write: a point whose fields have not moved since the last time
// it was written carries no new information, so it is not written.
//
// The ledger keys on measurement, tags, timestamp and sink name, and stores a
// hash of the fields. A changed field is a different hash, so it goes out. Two
// sinks each get their own answer, because one of them may have been down.
type Ledger struct {
	mu   sync.Mutex
	seen map[uint64]entry

	path       string
	horizon    time.Duration
	maxEntries int
	saved      time.Time
	dirty      bool
}

// entry is the last value seen for one point identity, and the day it was last
// offered. The day is what lets the file be pruned: an item the collectors
// have stopped producing stops being remembered.
type entry struct {
	value uint64
	// day counts whole days since the Unix epoch. It is the same unsigned
	// four bytes the file holds, so the record round trips without a
	// conversion in either direction.
	day uint32
}

const (
	ledgerMagic   = "GHCLDG1\n"
	ledgerRecord  = 20 // 8 id + 8 value + 4 day
	defaultMaxLen = 4_000_000
)

// ledgerMode is stricter than the mode the dump beside it ends up with, and
// deliberately so.
//
// The dump is an interface: it exists so a shipper running as its own user can
// read it, so the mode there is the operator's to widen and the File sink
// carries whatever they set across restarts and rotations. The ledger is not
// an interface. Nothing but this process reads it, its records are opaque
// hashes rather than content, and it is still a fingerprint of one account's
// activity. So there is no grant to preserve here and no reason to leave room
// for one: every save writes a fresh temporary file at this mode and renames
// it into place, which means a chmod on the ledger does not survive, because
// it was a mistake rather than a decision.
//
// Windows keeps less of this than the constant says. Its file modes carry the
// owner's write bit alone, as the read-only attribute, and who may read the
// file is decided by the ACL it inherits from its directory. So there the
// ledger is created writable and readable by whoever the directory lets in,
// and the chmod that does not survive is the one bit a chmod can set there: a
// read-only attribute set by hand is cleared by the next save.
const ledgerMode os.FileMode = 0o600

// dayNumber is the whole days between the Unix epoch and t.
//
// Four bytes, unsigned, because that is the width of the field in the file, and
// every clock this can read lands five orders of magnitude inside the range. A
// clock set before 1970 yields day zero rather than a value that wraps into the
// far future, where the entry would outlive every horizon and never be pruned.
func dayNumber(t time.Time) uint32 {
	const secondsPerDay = 86400
	days := t.Unix() / secondsPerDay
	if days < 0 {
		return 0
	}
	if days > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(days)
}

// LoadLedger reads the ledger at path, or returns an empty one when it is
// missing or unreadable. A ledger that cannot be read is a first run, not an
// error: the worst it costs is one sweep's worth of rewriting.
//
// horizon is how long an entry survives after the collectors stop offering it.
// Zero means thirty days. maxEntries bounds the entry count; zero means four
// million, which is about eighty megabytes of file and far more points than an
// account produces.
func LoadLedger(path string, horizon time.Duration, maxEntries int) *Ledger {
	if horizon <= 0 {
		horizon = 30 * 24 * time.Hour
	}
	if maxEntries <= 0 {
		maxEntries = defaultMaxLen
	}
	l := &Ledger{seen: map[uint64]entry{}, path: path, horizon: horizon, maxEntries: maxEntries, saved: time.Now()}
	if path == "" {
		return l
	}
	f, err := os.Open(path)
	if err != nil {
		return l
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1<<16)
	head := make([]byte, len(ledgerMagic))
	if _, err = io.ReadFull(r, head); err != nil || string(head) != ledgerMagic {
		return l
	}
	buf := make([]byte, ledgerRecord)
	for {
		if _, err = io.ReadFull(r, buf); err != nil {
			break // a truncated tail costs one rewrite, so it is not worth an error
		}
		l.seen[binary.LittleEndian.Uint64(buf[0:8])] = entry{
			value: binary.LittleEndian.Uint64(buf[8:16]),
			day:   binary.LittleEndian.Uint32(buf[16:20]),
		}
	}
	return l
}

// Len reports how many points the ledger remembers.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.seen)
}

// Reserve splits points into the ones that carry something new and a commit
// function that records them.
//
// The two are separate because a write can fail. Recording before the store
// has accepted the batch would drop those points from the next sweep too, and
// the observation would be lost until its values changed again.
func (l *Ledger) Reserve(sink string, points []Point) (keep []Point, commit func()) {
	if l == nil {
		return points, func() {
			// Nothing was remembered, so there is nothing to record. The
			// commit exists so a caller with no ledger need not branch.
		}
	}
	day := dayNumber(time.Now())
	type change struct {
		id uint64
		e  entry
	}
	changes := make([]change, 0, len(points))

	l.mu.Lock()
	for _, p := range points {
		id, value := digest(sink, p)
		prev, ok := l.seen[id]
		if ok && prev.value == value {
			if prev.day != day {
				// Still being offered, so keep it from being pruned. This is
				// not a change to write, only a change to remember.
				changes = append(changes, change{id, entry{value, day}})
			}
			continue
		}
		keep = append(keep, p)
		changes = append(changes, change{id, entry{value, day}})
	}
	l.mu.Unlock()

	return keep, func() {
		l.mu.Lock()
		for _, c := range changes {
			l.seen[c.id] = c.e
		}
		l.dirty = len(changes) > 0 || l.dirty
		l.mu.Unlock()
		l.maybeSave()
	}
}

// maybeSave writes the ledger at most every five minutes. The file is only
// there to survive a restart, so a lost minute of it costs one rewrite.
func (l *Ledger) maybeSave() {
	l.mu.Lock()
	due := l.dirty && time.Since(l.saved) >= 5*time.Minute
	l.mu.Unlock()
	if due {
		_ = l.Save()
	}
}

// Save writes the ledger through a temporary file, pruning entries nothing has
// offered for a horizon.
func (l *Ledger) Save() error {
	if l == nil || l.path == "" {
		return nil
	}
	l.mu.Lock()
	cutoff := dayNumber(time.Now().Add(-l.horizon))
	for id, e := range l.seen {
		if e.day < cutoff {
			delete(l.seen, id)
		}
	}
	if len(l.seen) > l.maxEntries {
		l.trimLocked()
	}
	out := make([]byte, 0, len(ledgerMagic)+len(l.seen)*ledgerRecord)
	out = append(out, ledgerMagic...)
	buf := make([]byte, ledgerRecord)
	for id, e := range l.seen {
		binary.LittleEndian.PutUint64(buf[0:8], id)
		binary.LittleEndian.PutUint64(buf[8:16], e.value)
		binary.LittleEndian.PutUint32(buf[16:20], e.day)
		out = append(out, buf...)
	}
	l.saved, l.dirty = time.Now(), false
	l.mu.Unlock()

	if dir := filepath.Dir(l.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, out, ledgerMode); err != nil {
		return err
	}
	// replaceFile rather than os.Rename, because on Windows a ledger somebody
	// marked read-only would otherwise refuse every save from then on.
	return replaceFile(tmp, l.path)
}

// trimLocked keeps the most recently offered entries and forgets the rest.
// Forgetting is always safe: it costs one rewrite of those points.
func (l *Ledger) trimLocked() {
	days := make([]uint32, 0, len(l.seen))
	for _, e := range l.seen {
		days = append(days, e.day)
	}
	sort.Slice(days, func(i, j int) bool { return days[i] > days[j] })
	cut := days[l.maxEntries-1]
	for id, e := range l.seen {
		if e.day < cut {
			delete(l.seen, id)
		}
	}
}

// digest hashes a point's identity and its value separately. Identity is what
// a store keys a row by, plus the sink, so two destinations never share an
// answer. Value is every field, in the form the line protocol would write, so
// a float that renders identically counts as unchanged.
func digest(sink string, p Point) (id, value uint64) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(sink))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(p.Measurement))
	keys := make([]string, 0, len(p.Tags))
	for k, v := range p.Tags {
		if v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{'='})
		_, _ = h.Write([]byte(p.Tags[k]))
	}
	var stamp [8]byte
	binary.LittleEndian.PutUint64(stamp[:], uint64(stampOf(p).UnixNano()))
	_, _ = h.Write(stamp[:])
	id = h.Sum64()

	v := fnv.New64a()
	fkeys := make([]string, 0, len(p.Fields))
	for k := range p.Fields {
		fkeys = append(fkeys, k)
	}
	sort.Strings(fkeys)
	for _, k := range fkeys {
		s, ok := formatField(p.Fields[k])
		if !ok {
			continue
		}
		_, _ = v.Write([]byte(k))
		_, _ = v.Write([]byte{'='})
		_, _ = v.Write([]byte(s))
		_, _ = v.Write([]byte{0})
	}
	return id, v.Sum64()
}

// Unchanged wraps a sink so it only sees points that carry something new.
type Unchanged struct {
	inner  Sink
	ledger *Ledger
	// Dropped counts the points not written, for the log line that says the
	// filter is doing something.
	dropped uint64
	mu      sync.Mutex
}

// OnlyChanged wraps a sink with a ledger. A nil ledger returns the sink
// untouched, so a caller can decide per sink without branching.
func OnlyChanged(s Sink, l *Ledger) Sink {
	if l == nil {
		return s
	}
	return &Unchanged{inner: s, ledger: l}
}

func (u *Unchanged) Name() string { return u.inner.Name() }

func (u *Unchanged) Write(ctx context.Context, points []Point) error {
	keep, commit := u.ledger.Reserve(u.inner.Name(), points)
	if spared := len(points) - len(keep); spared > 0 {
		u.mu.Lock()
		u.dropped += uint64(spared)
		u.mu.Unlock()
	}
	if len(keep) == 0 {
		commit()
		return nil
	}
	if err := u.inner.Write(ctx, keep); err != nil {
		// RejectedError and DroppedError both mean everything writable was written, so
		// the batch counts as delivered. Not committing here would offer the
		// same points again on every sweep for as long as the store kept
		// refusing that one line.
		var rejected *RejectedError
		var dropped *DroppedError
		if errors.As(err, &rejected) || errors.As(err, &dropped) {
			commit()
		}
		return err
	}
	commit()
	return nil
}

// Filtered reports what the wrapped sink dropped rather than wrote. The
// ledger sits in front of that sink, so without this the wrapper would hide
// the wrapped sink's own filtering from anything counting the pair: see
// Filtering. A wrapped sink that writes everything it is given reports
// nothing.
func (u *Unchanged) Filtered() uint64 {
	if f, ok := u.inner.(Filtering); ok {
		return f.Filtered()
	}
	return 0
}

// Dropped reports how many points this sink was spared since it started.
func (u *Unchanged) Dropped() uint64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.dropped
}

func (u *Unchanged) Close() error {
	return errors.Join(u.ledger.Save(), u.inner.Close())
}
