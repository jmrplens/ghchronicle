// Package run schedules the collectors and moves their points to the sinks.
package run

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/jmrplens/ghchronicle/v2/internal/collect"
)

// State is what a sweep has to remember between runs.
//
// Nine things, and deleting the file costs a different thing for each of
// them. Seven of the nine cost only rate limit, because what is collected
// again overwrites what is already stored. last_head is the one that loses
// something: the dependency changes between the head it held and the next one
// are read from a range that nothing can name once the head is gone. stores
// is the one that costs a question nobody can answer afterwards: which
// release first wrote a store that cannot be asked what shape its rows are in.
//
//   - last_run: when each family last ran, so a restart does not re-collect
//     everything at once.
//   - first_saw: when each repository's one full walk of the stargazer list
//     came back, so that walk happens once instead of every sweep. Written
//     after the walk and only when it returned no error, so a walk a 502 cut
//     short is done again by the next sweep.
//   - history_read: when each repository's daily star history was last read
//     back to the week it was created, so that walk happens once and a sweep
//     reads only its newest page. Absent reads as never, which is what makes
//     the first sweep after an upgrade read every history whole.
//   - last_head: the commit each repository was on when the dependency diff
//     last ran. Without it the next sweep has only the photograph, no diff.
//   - last_full: when each family that normally reads what changed last read a
//     whole page. Absent reads as due, so the next sweep takes them all.
//   - last_notified: where the inbox window was cut. Zero asks for the whole
//     inbox.
//   - last_event: the newest event the feed had. Empty reads the whole feed.
//   - coauthored: the Pair Extraordinaire count and the last day it covers,
//     so the achievements family walks the pull requests merged since instead
//     of the account's whole history. Absent walks the whole history.
//   - stores: per configured store, where it points, which release first
//     wrote it and which last did, and the migrations applied to it. See
//     StoreRecord.
type State struct {
	path     string
	LastRun  map[string]time.Time `json:"last_run"`
	FirstSaw map[string]time.Time `json:"first_saw"`
	// HistoryRead is when each repository's daily star history was last read
	// whole. Written only after a walk that reached the end, which is a
	// stricter test than first_saw's: a history cut short by a 502, and one
	// cut short by a later page saying there is nothing here, is then read
	// whole again on the next sweep rather than staying partial until
	// somebody runs a backfill.
	HistoryRead map[string]time.Time `json:"history_read"`
	// LastHead is the commit each repository was on when the dependency diff
	// last ran, which is what makes the next diff a range rather than a guess.
	LastHead map[string]string `json:"last_head"`
	// LastFull is when each family that normally reads what changed last read
	// a whole page instead. Absent in an older state file, which reads as
	// due, so the first sweep after an upgrade takes the whole page once.
	LastFull map[string]time.Time `json:"last_full"`
	// LastNotified is the latest updated_at the inbox has answered with, which
	// is where the next sweep's `since` window is cut from. Zero, which is
	// what an older state file reads as, asks for the whole inbox.
	LastNotified time.Time `json:"last_notified,omitzero"`
	// LastEvent is the id of the newest event the feed had, which is the page
	// the next sweep stops at. Empty reads the whole feed.
	LastEvent string `json:"last_event,omitempty"`
	// Coauthored is the co-authored pull request count as far as the
	// achievements family has settled it. Zero, which is what an older state
	// file reads as, is a count made by no rule, and the next pass walks the
	// whole history for it.
	Coauthored collect.CoauthoredTally `json:"coauthored,omitzero"`
	// Stores is what this file remembers about each store it has written,
	// keyed by the sink's name in the configuration. Absent in a state file
	// written before 2.6.2, which is itself an answer: every store it served
	// was first written by a release that kept no record.
	Stores map[string]*StoreRecord `json:"stores,omitempty"`
}

func LoadState(path string) *State {
	s := &State{
		path: path, LastRun: map[string]time.Time{}, FirstSaw: map[string]time.Time{},
		HistoryRead: map[string]time.Time{}, LastHead: map[string]string{}, LastFull: map[string]time.Time{},
		Stores: map[string]*StoreRecord{},
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s // a missing state file is a first run, not an error
	}
	_ = json.Unmarshal(b, s)
	if s.LastRun == nil {
		s.LastRun = map[string]time.Time{}
	}
	if s.FirstSaw == nil {
		s.FirstSaw = map[string]time.Time{}
	}
	if s.HistoryRead == nil {
		s.HistoryRead = map[string]time.Time{}
	}
	if s.LastHead == nil {
		s.LastHead = map[string]string{}
	}
	if s.LastFull == nil {
		s.LastFull = map[string]time.Time{}
	}
	if s.Stores == nil {
		s.Stores = map[string]*StoreRecord{}
	}
	for name, rec := range s.Stores {
		if rec == nil {
			delete(s.Stores, name)
		}
	}
	s.path = path
	return s
}

// Save writes through a temporary file so a crash mid-write cannot leave a
// truncated state that would trigger a full re-collection.
func (s *State) Save() error {
	if s.path == "" {
		return nil
	}
	if dir := filepath.Dir(s.path); dir != "." {
		// 0750, matching the 0600 of the file itself: the only thing this
		// directory is created to hold is the state, and nothing outside the
		// owner and its group could read that anyway.
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return replaceFile(s.path, b)
}

// replaceFile writes b beside path and renames it over path, so a reader sees
// either the old file whole or the new one whole and never a half.
//
// The rename is what makes that true for a reader. It is not what makes it
// true for a machine that loses power: a rename can reach the disk before the
// contents it renames, and what comes back is then a name with nothing under
// it. So the contents are flushed before the rename and the directory after
// it, which is the pair that turns "never a half" into something that survives
// the plug being pulled. The backfill checkpoint is saved this way after every
// repository and is read back by a walk that will skip whatever it names, so a
// zero length file there is not a cosmetic problem; the sweep's state pays one
// flush a sweep for the same guarantee.
func replaceFile(path string, b []byte) error {
	return replaceFileWith(path, func(w io.Writer) error {
		_, err := w.Write(b)
		return err
	})
}

// replaceFileWith is replaceFile for contents written as they are produced
// rather than held whole first, which the cache file is: tens of megabytes
// that would otherwise be in memory twice for the length of every save.
func replaceFileWith(path string, write func(io.Writer) error) error {
	tmp := path + ".tmp"
	if err := writeWhole(tmp, write); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// writeWhole writes path through write and flushes it, so what the rename
// above puts in place is the contents and not just the name of them.
func writeWhole(path string, write func(io.Writer) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	buffered := bufio.NewWriterSize(f, 1<<16)
	if err = write(buffered); err == nil {
		err = buffered.Flush()
	}
	if err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// syncDir flushes the directory entry the rename above created.
//
// Its failure is not reported, and that is deliberate rather than lazy: a
// directory cannot be opened for this on every platform the binary is built
// for, Windows among them, and the file's own contents have already been
// flushed. A saved state under a name that has not reached the disk yet is the
// small half of the problem; refusing a save that worked would be the larger
// one.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// Due reports whether a family should run now.
func (s *State) Due(family string, every time.Duration, now time.Time) bool {
	last, ok := s.LastRun[family]
	return !ok || now.Sub(last) >= every
}

func (s *State) Mark(family string, now time.Time) { s.LastRun[family] = now }

// FullDue reports whether a family's whole-page read is due: never done, or
// done longer ago than every.
func (s *State) FullDue(family string, every time.Duration, now time.Time) bool {
	last, ok := s.LastFull[family]
	return !ok || now.Sub(last) >= every
}

func (s *State) MarkFull(family string, now time.Time) { s.LastFull[family] = now }

// FirstSight reports whether a repository still owes its one full walk of the
// stargazer list. It records nothing: MarkSeen does, once the walk came back.
func (s *State) FirstSight(full string) bool {
	_, seen := s.FirstSaw[full]
	return !seen
}

// MarkSeen records that a repository's full walk of the stargazer list came
// back. A list GitHub hides from the token comes back too, as a 404 the
// collector reads as nothing to collect, and walking it again would find the
// same.
//
// Only a walk that returned no error calls it. Recorded before the walk, as it
// used to be, a first walk that a 502 cut short retired the repository all the
// same: every later sweep read its newest hundred through the batch, and the
// older stars stayed unread until somebody ran a backfill, which since 2.5.1
// walks every stargazer list whole and before it did not.
func (s *State) MarkSeen(full string, now time.Time) { s.FirstSaw[full] = now }

// HistoryDue reports whether a repository's daily star history has never been
// read whole, which a repository seen for the first time and every repository
// of a state file older than the field both answer yes to.
func (s *State) HistoryDue(full string) bool {
	_, read := s.HistoryRead[full]
	return !read
}

// MarkHistory records that a repository's daily star history was read back to
// its first week. Only a walk that got there calls it.
func (s *State) MarkHistory(full string, now time.Time) { s.HistoryRead[full] = now }
