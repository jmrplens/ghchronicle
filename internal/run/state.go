// Package run schedules the collectors and moves their points to the sinks.
package run

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// State is what a sweep has to remember between runs.
//
// Six things, and deleting the file costs a different thing for each of them.
// Five of the six cost only rate limit, because what is collected again
// overwrites what is already stored. last_head is the one that loses
// something: the dependency changes between the head it held and the next one
// are read from a range that nothing can name once the head is gone.
//
//   - last_run: when each family last ran, so a restart does not re-collect
//     everything at once.
//   - first_saw: when each repository was first seen, so the one-off full walk
//     of the star history happens once instead of every sweep.
//   - last_head: the commit each repository was on when the dependency diff
//     last ran. Without it the next sweep has only the photograph, no diff.
//   - last_full: when each family that normally reads what changed last read a
//     whole page. Absent reads as due, so the next sweep takes them all.
//   - last_notified: where the inbox window was cut. Zero asks for the whole
//     inbox.
//   - last_event: the newest event the feed had. Empty reads the whole feed.
type State struct {
	path     string
	LastRun  map[string]time.Time `json:"last_run"`
	FirstSaw map[string]time.Time `json:"first_saw"`
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
}

func LoadState(path string) *State {
	s := &State{
		path: path, LastRun: map[string]time.Time{}, FirstSaw: map[string]time.Time{},
		LastHead: map[string]string{}, LastFull: map[string]time.Time{},
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
	if s.LastHead == nil {
		s.LastHead = map[string]string{}
	}
	if s.LastFull == nil {
		s.LastFull = map[string]time.Time{}
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
func replaceFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

// FirstSight records a repository and reports whether this is the first time
// it has ever been collected.
func (s *State) FirstSight(full string, now time.Time) bool {
	if _, ok := s.FirstSaw[full]; ok {
		return false
	}
	s.FirstSaw[full] = now
	return true
}

// Head returns the commit a repository was on at the last dependency diff, and
// records the new one. An empty answer means there is no range yet, which is
// the first sweep and is not an error.
func (s *State) Head(full, head string) string {
	previous := s.LastHead[full]
	if head != "" {
		s.LastHead[full] = head
	}
	return previous
}
