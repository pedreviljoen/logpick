package state

// Test plan for T08 (AGENTS.md sections 2, 3, 4, 5 and 9; DESIGN.md sections
// 6.3 and 11). Copied verbatim from the task before any test below was
// written.
//
// Mechanics:
//  1. A save followed by a load returns equivalent state.
//  2. A missing state file loads as empty state without error.
//  3. A corrupt state file is moved to .bak and loading returns empty state.
//  4. Prune removes entries older than the cutoff and never removes pinned
//     ones.
//  5. Prune with dryRun returns the same list but writes nothing to disk.
//  6. Two concurrent read-modify-write cycles both land, verified under
//     -race.
//
// Notes on how these were resolved into assertions:
//
//   - Every time.Time compared below goes through timeEqual, a cmp.Comparer
//     built on time.Time.Equal. A value that has round-tripped through TOML
//     carries a *time.Location tied to the offset the file spelled out (UTC
//     for a "Z" suffix), which need not be the same *time.Location value
//     that went in, even when the instant is identical. Comparing with
//     reflect-based equality (the cmp default) would pass only on a machine
//     whose local zone happens to line up; Equal compares the instant, which
//     is what "equivalent state" means here. All fixture times are
//     constructed at second granularity, since that is what state.toml's
//     RFC 3339 timestamps round-trip at (DESIGN.md section 6.3).
//   - Mechanic 3 asserts on both halves of the contract: Load returns an
//     empty State with a nil error, and the ".bak" file holds the original,
//     unparseable bytes byte for byte. Checking only one half would not
//     catch a Load that silently discards the corrupt file instead of
//     preserving it.
//   - Mechanics 4 and 5 seed the state file directly with
//     github.com/pelletier/go-toml/v2, bypassing Store.Save, since the two
//     tests are about Prune's own read-modify-write behaviour and must not
//     depend on Save already working. Mechanic 5's "writes nothing to disk"
//     is checked by comparing the file's raw bytes before and after the dry
//     run, which catches a dry run that reformats the file even if the
//     hosts it contains are unchanged.
//   - Mechanic 6 drives two goroutines through Store.RecordConnect, a real
//     read-modify-write operation, on two distinct hosts, released together
//     from a closed channel rather than a sleep. "Both land" is checked by
//     listing afterwards and requiring both hosts to be present: a lost
//     update would silently drop one of the two.

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/pelletier/go-toml/v2"
)

// timeEqual compares time.Time values by instant rather than by
// representation. See the package comment block above for why.
var timeEqual = cmp.Comparer(func(a, b time.Time) bool {
	return a.Equal(b)
})

// byHostName orders Host slices by name, for comparing sets produced by
// concurrent operations where insertion order is not defined.
var byHostName = cmpopts.SortSlices(func(a, b Host) bool { return a.Name < b.Name })

// date builds a UTC time.Time at second granularity, matching what
// state.toml's timestamps round-trip at (DESIGN.md section 6.3).
func date(year, month, day, hour, min, sec int) time.Time {
	return time.Date(year, time.Month(month), day, hour, min, sec, 0, time.UTC)
}

// newStore returns a Store bound to a state file under a fresh t.TempDir,
// and the path it is bound to. Tests must never touch the real XDG paths.
func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.toml")
	return New(path), path
}

// writeRawState marshals st with go-toml directly and writes it to path,
// bypassing Store.Save so that Prune tests do not depend on Save. It
// returns the bytes written, for tests that need to assert the file is
// later left untouched.
func writeRawState(t *testing.T, path string, st *State) []byte {
	t.Helper()

	data, err := toml.Marshal(st)
	if err != nil {
		t.Fatalf("marshaling seed state: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating state dir: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing seed state file: %v", err)
	}
	return data
}

func TestSetTheme(t *testing.T) {
	s, _ := newStore(t)
	if err := s.SetTheme("#112233", "#ffaa00"); err != nil {
		t.Fatalf("SetTheme: %v", err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Theme{Configured: true, Primary: "#112233", Secondary: "#ffaa00"}
	if got.Theme != want {
		t.Fatalf("theme = %+v, want %+v", got.Theme, want)
	}
}

func TestStore(t *testing.T) {
	t.Run("a save followed by a load returns equivalent state", func(t *testing.T) {
		s, _ := newStore(t)

		want := &State{
			Hosts: []Host{
				{
					Name:         "jenkins-01.prod.internal",
					Profile:      "corp",
					FirstSeen:    date(2026, 2, 11, 8, 14, 22),
					LastSeen:     date(2026, 8, 14, 16, 2, 51),
					ConnectCount: 47,
					Pinned:       false,
					Caps: Caps{
						GNUFind:    true,
						NativeCopy: true,
						ProbedAt:   date(2026, 2, 11, 8, 14, 25),
					},
					Cache: Cache{
						ScannedAt: date(2026, 8, 14, 16, 2, 53),
						Entries: []CacheEntry{
							{Path: "/var/lib/jenkins/logs/jenkins.log", Size: 88213441, Mtime: 1755180171},
						},
					},
				},
				{
					Name:         "pinned-host",
					Profile:      "default",
					FirstSeen:    date(2026, 1, 1, 0, 0, 0),
					LastSeen:     date(2026, 1, 2, 0, 0, 0),
					ConnectCount: 1,
					Pinned:       true,
				},
			},
			Fetches: []Fetch{
				{
					Host:   "jenkins-01.prod.internal",
					Remote: "/var/lib/jenkins/logs/jenkins.log",
					Local:  "jenkins-01.prod.internal/2026-08-14T160251Z/var/lib/jenkins/logs/jenkins.log",
					Bytes:  88213441,
					At:     date(2026, 8, 14, 16, 3, 7),
				},
			},
		}

		if err := s.Save(want); err != nil {
			t.Fatalf("Save returned an unexpected error: %v", err)
		}

		got, err := s.Load()
		if err != nil {
			t.Fatalf("Load returned an unexpected error: %v", err)
		}

		if diff := cmp.Diff(want, got, timeEqual); diff != "" {
			t.Errorf("Load after Save mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("a missing state file loads as empty state without error", func(t *testing.T) {
		s, _ := newStore(t)

		got, err := s.Load()
		if err != nil {
			t.Fatalf("Load returned an unexpected error for a missing file: %v", err)
		}

		if diff := cmp.Diff(&State{}, got, timeEqual); diff != "" {
			t.Errorf("Load of missing file mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("a corrupt state file is moved to .bak and loading returns empty state", func(t *testing.T) {
		s, path := newStore(t)

		corrupt := []byte("this is not valid toml at all {{{ [[[ \x00\x01 garbage\n")
		if err := os.WriteFile(path, corrupt, 0o600); err != nil {
			t.Fatalf("seeding corrupt state file: %v", err)
		}

		got, err := s.Load()
		if err != nil {
			t.Fatalf("Load returned an unexpected error for a corrupt file: %v", err)
		}

		if diff := cmp.Diff(&State{}, got, timeEqual); diff != "" {
			t.Errorf("Load of corrupt file mismatch (-want +got):\n%s", diff)
		}

		bak, err := os.ReadFile(path + ".bak")
		if err != nil {
			t.Fatalf("reading .bak file: %v", err)
		}
		if !bytes.Equal(bak, corrupt) {
			t.Errorf(".bak contents = %q, want %q", bak, corrupt)
		}

		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("state file still present at %s after being moved to .bak, stat err = %v", path, err)
		}
	})

	t.Run("Prune removes entries older than the cutoff and never removes pinned ones", func(t *testing.T) {
		s, path := newStore(t)

		cutoff := date(2026, 6, 1, 0, 0, 0)

		oldUnpinned := Host{
			Name: "old-unpinned", Profile: "default",
			FirstSeen: date(2026, 1, 1, 0, 0, 0), LastSeen: date(2026, 5, 1, 0, 0, 0),
			Pinned: false,
		}
		oldPinned := Host{
			Name: "old-pinned", Profile: "default",
			FirstSeen: date(2026, 1, 1, 0, 0, 0), LastSeen: date(2026, 5, 1, 0, 0, 0),
			Pinned: true,
		}
		exactlyCutoff := Host{
			Name: "exactly-cutoff", Profile: "default",
			FirstSeen: date(2026, 1, 1, 0, 0, 0), LastSeen: cutoff,
			Pinned: false,
		}
		recent := Host{
			Name: "recent", Profile: "default",
			FirstSeen: date(2026, 7, 1, 0, 0, 0), LastSeen: date(2026, 8, 1, 0, 0, 0),
			Pinned: false,
		}

		writeRawState(t, path, &State{Hosts: []Host{oldUnpinned, oldPinned, exactlyCutoff, recent}})

		removed, err := s.Prune(cutoff, false)
		if err != nil {
			t.Fatalf("Prune returned an unexpected error: %v", err)
		}

		wantRemoved := []Host{oldUnpinned}
		if diff := cmp.Diff(wantRemoved, removed, timeEqual); diff != "" {
			t.Errorf("Prune removed set mismatch (-want +got):\n%s", diff)
		}

		remaining, err := s.List()
		if err != nil {
			t.Fatalf("List returned an unexpected error: %v", err)
		}

		wantRemaining := []Host{oldPinned, exactlyCutoff, recent}
		if diff := cmp.Diff(wantRemaining, remaining, timeEqual, byHostName); diff != "" {
			t.Errorf("List after Prune mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("Prune with dryRun returns the same list but writes nothing to disk", func(t *testing.T) {
		s, path := newStore(t)

		cutoff := date(2026, 6, 1, 0, 0, 0)

		oldUnpinned := Host{
			Name: "old-unpinned", Profile: "default",
			FirstSeen: date(2026, 1, 1, 0, 0, 0), LastSeen: date(2026, 5, 1, 0, 0, 0),
			Pinned: false,
		}
		recent := Host{
			Name: "recent", Profile: "default",
			FirstSeen: date(2026, 7, 1, 0, 0, 0), LastSeen: date(2026, 8, 1, 0, 0, 0),
			Pinned: false,
		}

		before := writeRawState(t, path, &State{Hosts: []Host{oldUnpinned, recent}})

		removed, err := s.Prune(cutoff, true)
		if err != nil {
			t.Fatalf("Prune returned an unexpected error: %v", err)
		}

		wantRemoved := []Host{oldUnpinned}
		if diff := cmp.Diff(wantRemoved, removed, timeEqual); diff != "" {
			t.Errorf("dry run Prune removed set mismatch (-want +got):\n%s", diff)
		}

		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading state file after dry run: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Errorf("dry run Prune modified the state file on disk")
		}
	})

	t.Run("two concurrent read-modify-write cycles both land", func(t *testing.T) {
		s, _ := newStore(t)

		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			<-start
			errs <- s.RecordConnect("host-a", "default", date(2026, 8, 15, 9, 0, 0))
		}()
		go func() {
			defer wg.Done()
			<-start
			errs <- s.RecordConnect("host-b", "default", date(2026, 8, 15, 9, 0, 1))
		}()

		close(start)
		wg.Wait()
		close(errs)

		for err := range errs {
			if err != nil {
				t.Fatalf("RecordConnect returned an unexpected error: %v", err)
			}
		}

		got, err := s.List()
		if err != nil {
			t.Fatalf("List returned an unexpected error: %v", err)
		}

		want := []Host{
			{
				Name: "host-a", Profile: "default",
				FirstSeen: date(2026, 8, 15, 9, 0, 0), LastSeen: date(2026, 8, 15, 9, 0, 0),
				ConnectCount: 1,
			},
			{
				Name: "host-b", Profile: "default",
				FirstSeen: date(2026, 8, 15, 9, 0, 1), LastSeen: date(2026, 8, 15, 9, 0, 1),
				ConnectCount: 1,
			},
		}

		if diff := cmp.Diff(want, got, timeEqual, byHostName); diff != "" {
			t.Errorf("List after concurrent RecordConnect mismatch (-want +got):\n%s", diff)
		}
	})
}
