package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// State is the whole contents of state.toml, the tool-owned record of which
// hosts have been used, what they can do, what was last seen on them and what
// has been fetched from them.
//
// Fields and TOML tags come verbatim from the schema in DESIGN.md section 6.3.
// The zero State is useful: it has no hosts and no fetches, every operation
// works on it, and it is exactly what Load returns for a state file that is
// missing or unreadable. State is disposable by design, so nothing here is
// ever the only copy of anything the user cares about.
type State struct {
	Hosts   []Host  `toml:"hosts"`
	Fetches []Fetch `toml:"fetches"`
	Theme   Theme   `toml:"theme"`
}

// Theme is the tool-owned interactive palette. Keeping UI preferences in
// state.toml preserves config.toml's read-only contract outside first run.
type Theme struct {
	Configured bool   `toml:"configured"`
	Primary    string `toml:"primary"`
	Secondary  string `toml:"secondary"`
}

// Host is one remembered host: how it is reached, how often and how recently
// it has been connected to, what a capability probe found, and the last
// listing scanned from it.
//
// FirstSeen and LastSeen are stamped by RecordConnect from the time passed in,
// never from the wall clock, so ordering and pruning are testable. Pinned
// hosts are excluded from Prune (DESIGN.md section 11).
type Host struct {
	Name         string    `toml:"name"`
	Profile      string    `toml:"profile"`
	FirstSeen    time.Time `toml:"first_seen"`
	LastSeen     time.Time `toml:"last_seen"`
	ConnectCount int       `toml:"connect_count"`
	Pinned       bool      `toml:"pinned"`
	Caps         Caps      `toml:"caps"`
	Cache        Cache     `toml:"cache"`
}

// Caps is the persisted result of a capability probe against one host.
//
// It is deliberately narrower than transport.Caps and is owned by this
// package. Only the two facts worth remembering across runs are stored: which
// find dialect the host speaks (DESIGN.md section 8.2) and whether the profile
// had a native copy path when the probe ran. Everything else in transport.Caps
// is a property of the backend, is known without asking the host, and would
// invert the dependency direction of DESIGN.md section 5 if imported here.
//
// The zero Caps means "never probed": ProbedAt is the zero time, so callers
// test with ProbedAt.IsZero() rather than treating false as an answer.
type Caps struct {
	GNUFind    bool      `toml:"gnu_find"`
	NativeCopy bool      `toml:"native_copy"`
	ProbedAt   time.Time `toml:"probed_at"`
}

// Cache is the last listing scanned from a host, kept so the browser can show
// something immediately on reconnect and swap in the fresh scan when it
// arrives (DESIGN.md section 8.3). It is a display convenience and may be
// arbitrarily stale.
type Cache struct {
	ScannedAt time.Time    `toml:"scanned_at"`
	Entries   []CacheEntry `toml:"entries"`
}

// CacheEntry is one log file as discovery reported it. Mtime is Unix seconds,
// not a time.Time, because that is what find returns for %T@ and what the ls
// fallback is parsed into; it stays in the units it arrived in.
type CacheEntry struct {
	Path  string `toml:"path"`
	Size  int64  `toml:"size"`
	Mtime int64  `toml:"mtime"`
}

// Fetch is one completed download. Local is relative to the fetch root of
// DESIGN.md section 10.1, so moving or renaming that root does not invalidate
// the record. At is stamped by RecordFetch from the time passed in.
type Fetch struct {
	Host   string    `toml:"host"`
	Remote string    `toml:"remote"`
	Local  string    `toml:"local"`
	Bytes  int64     `toml:"bytes"`
	At     time.Time `toml:"at"`
}

// Store is state.toml at one path, and the only supported way to change it.
//
// Every operation below is a complete read-modify-write cycle: it loads the
// current file, applies its change and writes the result back, holding both an
// in-process mutex and an advisory file lock for the whole cycle. That is what
// makes two concurrent sessions, or two goroutines in one session, both land
// rather than one silently overwriting the other (DESIGN.md section 11). The
// mutex is not redundant with the lock: flock is advisory and per file
// descriptor, so it does not serialise two goroutines sharing one process.
//
// Load and Save are exported because callers legitimately need to read the
// whole file and, in the first-run path, write one back. They each take the
// lock for their own duration only. A caller that hand-rolls load, mutate,
// save is racing; use an operation instead.
//
// No operation reads the wall clock. Every timestamp written comes from a
// time.Time parameter, so callers control it and tests do not sleep
// (AGENTS.md sections 5 and 9).
//
// A Store must be created with New. The zero Store has no path and is not
// usable; it is State, not Store, that is useful at its zero value.
type Store struct {
	path string
	mu   sync.Mutex
}

// New returns a Store bound to the state file at path. It touches no disk:
// the file is read on the first operation and created on the first write,
// along with any missing parent directory.
func New(path string) *Store {
	return &Store{path: path}
}

// withLock runs fn with the Store's mutex and an advisory file lock both
// held for fn's entire duration, so that whatever fn does to the state file
// is one atomic unit with respect to every other operation on this Store,
// in this process or another (AGENTS.md section 3 invariant 5; DESIGN.md
// section 11).
//
// The two locks are not redundant: flock is per file descriptor and does
// not serialise two goroutines in the same process, only two independently
// opened descriptors, so a bare flock would pass mechanic 6 by luck rather
// than by construction. The mutex covers the in-process case, the flock
// covers the cross-process one.
//
// It creates the state file's parent directory if missing before locking,
// since even a read needs somewhere to put the lock file.
func (s *Store) withLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	release, err := acquireLock(s.path)
	if err != nil {
		return err
	}
	defer func() {
		_ = release()
	}()

	return fn()
}

// findHost returns the index of the host named name in hosts, or -1.
func findHost(hosts []Host, name string) int {
	for i := range hosts {
		if hosts[i].Name == name {
			return i
		}
	}
	return -1
}

// Load reads and parses the state file.
//
// A missing file is not an error: state is rebuildable, and its absence is
// simply the state of a tool that has not been used yet, so Load returns an
// empty State and a nil error.
//
// A file that does not parse is not an error either. It is renamed to the same
// path with a ".bak" suffix, keeping its original bytes for anyone who wants
// to look, and Load returns an empty State and a nil error. Refusing to start
// because the tool's own scratch file is damaged would be the worse failure.
//
// Errors that remain are genuine I/O failures: an unreadable directory, a
// permission problem, a full disk during the rename.
func (s *Store) Load() (*State, error) {
	var st *State
	err := s.withLock(func() error {
		loaded, loadErr := s.readState()
		st = loaded
		return loadErr
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// readState reads and parses the state file. Callers must hold the lock;
// this is the piece every exported operation shares for the read half of
// its read-modify-write.
func (s *Store) readState() (*State, error) {
	data, err := os.ReadFile(s.path) //nolint:gosec // path is the caller-chosen state file location (XDG default), not untrusted input.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &State{}, nil
		}
		return nil, fmt.Errorf("reading state file %s: %w", s.path, err)
	}

	var st State
	if err := toml.Unmarshal(data, &st); err != nil {
		if renameErr := os.Rename(s.path, s.path+".bak"); renameErr != nil {
			return nil, fmt.Errorf("state file %s is corrupt and could not be moved aside: %w", s.path, renameErr)
		}
		return &State{}, nil
	}

	normalizeEmptySlices(&st)

	return &st, nil
}

// normalizeEmptySlices collapses zero-length slices produced by round
// tripping through TOML back to nil.
//
// TOML has no way to distinguish an absent array from an empty one: a zero
// value Cache marshals its Entries field as "entries = []" (go-toml does
// not honour omitempty on it, since it is a nested table rather than a
// top-level key), and unmarshaling that back in produces a non-nil,
// zero-length slice rather than the nil the same zero value started as.
// Left alone, that would make "save, then load" observably different from
// the identity function, which is what mechanic 1 checks for. A cleared
// cache (CacheScan with no entries) round trips the same way either side of
// this, which matches its own doc comment: "no entries" is meaningful, and
// nil and an empty slice both mean it.
func normalizeEmptySlices(st *State) {
	for i := range st.Hosts {
		if len(st.Hosts[i].Cache.Entries) == 0 {
			st.Hosts[i].Cache.Entries = nil
		}
	}
}

// Save writes st to the state file, replacing whatever is there.
//
// The write is atomic: a temp file in the same directory, Sync, then Rename,
// so a crash or a full disk leaves either the old file or the new one and
// never a half-written one (AGENTS.md section 3, invariant 5). The parent
// directory is created 0700 if missing and the file is written 0600, matching
// the posture of DESIGN.md section 12.
//
// Save replaces the file wholesale and does not merge. Two callers that both
// Load, mutate and Save will lose one of the two changes; that is what the
// operations below exist to prevent.
func (s *Store) Save(st *State) error {
	return s.withLock(func() error {
		return s.writeState(st)
	})
}

// writeState marshals st and writes it atomically. Callers must hold the
// lock and, if the parent directory might not exist, must have created it
// first; withLock does that for every exported entry point.
func (s *Store) writeState(st *State) error {
	data, err := toml.Marshal(st)
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	if err := writeFileAtomic(s.path, data, 0o600); err != nil {
		return fmt.Errorf("writing state file %s: %w", s.path, err)
	}

	return nil
}

// SetTheme atomically persists the interactive palette.
func (s *Store) SetTheme(primary, secondary string) error {
	return s.withLock(func() error {
		st, err := s.readState()
		if err != nil {
			return err
		}
		st.Theme = Theme{Configured: true, Primary: primary, Secondary: secondary}
		return s.writeState(st)
	})
}

// RecordConnect notes a successful connection to host at time at.
//
// For a host not seen before it appends an entry with FirstSeen and LastSeen
// both at and ConnectCount 1. For a known host it updates Profile and
// LastSeen, and increments ConnectCount, leaving FirstSeen, Pinned, Caps and
// Cache alone.
func (s *Store) RecordConnect(host, profile string, at time.Time) error {
	return s.withLock(func() error {
		st, err := s.readState()
		if err != nil {
			return err
		}

		if idx := findHost(st.Hosts, host); idx >= 0 {
			st.Hosts[idx].Profile = profile
			st.Hosts[idx].LastSeen = at
			st.Hosts[idx].ConnectCount++
		} else {
			st.Hosts = append(st.Hosts, Host{
				Name:         host,
				Profile:      profile,
				FirstSeen:    at,
				LastSeen:     at,
				ConnectCount: 1,
			})
		}

		return s.writeState(st)
	})
}

// UpsertCaps records the outcome of a capability probe against host, stamping
// ProbedAt with at.
//
// Any ProbedAt set on c is ignored and overwritten with at: the store owns
// that field so a caller cannot record a probe as having happened at a time it
// did not. Recording caps for a host that has not been connected to creates
// the entry, since T11 probes as part of the first scan.
func (s *Store) UpsertCaps(host string, c Caps, at time.Time) error {
	c.ProbedAt = at

	return s.withLock(func() error {
		st, err := s.readState()
		if err != nil {
			return err
		}

		if idx := findHost(st.Hosts, host); idx >= 0 {
			st.Hosts[idx].Caps = c
		} else {
			st.Hosts = append(st.Hosts, Host{
				Name:      host,
				FirstSeen: at,
				LastSeen:  at,
				Caps:      c,
			})
		}

		return s.writeState(st)
	})
}

// CacheScan replaces the cached listing for host with entries, stamping
// ScannedAt with at.
//
// The listing is replaced rather than merged: a scan is a complete picture of
// the configured paths at one moment, and a log file that has since been
// rotated away should disappear from the cache rather than linger. Passing no
// entries is meaningful and clears the cache.
func (s *Store) CacheScan(host string, entries []CacheEntry, at time.Time) error {
	return s.withLock(func() error {
		st, err := s.readState()
		if err != nil {
			return err
		}

		cache := Cache{ScannedAt: at, Entries: entries}

		if idx := findHost(st.Hosts, host); idx >= 0 {
			st.Hosts[idx].Cache = cache
		} else {
			st.Hosts = append(st.Hosts, Host{
				Name:      host,
				FirstSeen: at,
				LastSeen:  at,
				Cache:     cache,
			})
		}

		return s.writeState(st)
	})
}

// RecordFetch appends f to the fetch history, stamping At with at.
//
// Any At set on f is ignored and overwritten, as with UpsertCaps. Fetches are
// appended, never deduplicated: fetching the same remote path twice is how a
// user gets two snapshots to diff (DESIGN.md section 10.1), so both are real
// history.
func (s *Store) RecordFetch(f Fetch, at time.Time) error {
	f.At = at

	return s.withLock(func() error {
		st, err := s.readState()
		if err != nil {
			return err
		}

		st.Fetches = append(st.Fetches, f)

		return s.writeState(st)
	})
}

// Remove deletes the entry for host, whether or not it is pinned.
//
// It is declarative: removing a host that is not there leaves the file
// unchanged and returns nil, so a delete keystroke repeated on a stale list is
// not an error.
//
// Fetch records for the host are kept. The files they name are still on disk,
// and orphaning them from the library screen would be a surprising side effect
// of forgetting a hostname.
func (s *Store) Remove(host string) error {
	return s.withLock(func() error {
		st, err := s.readState()
		if err != nil {
			return err
		}

		idx := findHost(st.Hosts, host)
		if idx < 0 {
			return nil
		}

		st.Hosts = append(st.Hosts[:idx], st.Hosts[idx+1:]...)

		return s.writeState(st)
	})
}

// Pin sets the pinned flag on host. Pinned hosts sort first in the picker and
// are never removed by Prune.
//
// Like Remove it is declarative: pinning an already pinned host is a no-op
// rather than an error, and pinning a host with no entry does nothing.
func (s *Store) Pin(host string, pinned bool) error {
	return s.withLock(func() error {
		st, err := s.readState()
		if err != nil {
			return err
		}

		idx := findHost(st.Hosts, host)
		if idx < 0 {
			return nil
		}

		st.Hosts[idx].Pinned = pinned

		return s.writeState(st)
	})
}

// Prune removes every unpinned host whose LastSeen is before olderThan, and
// returns the entries it removed, in the order they appeared in the file.
//
// Pinned hosts are never pruned, no matter how old (DESIGN.md section 11). A
// host whose LastSeen is exactly olderThan is kept, so the cutoff is the
// oldest moment still considered recent.
//
// With dryRun true, Prune computes and returns exactly the same list and
// writes nothing at all: no temp file, no rename, no change to the state
// file's contents. That is what makes `logpick prune --dry-run` worth
// trusting.
//
// As with Remove, fetch records for pruned hosts are kept.
func (s *Store) Prune(olderThan time.Time, dryRun bool) ([]Host, error) {
	var removed []Host

	err := s.withLock(func() error {
		st, err := s.readState()
		if err != nil {
			return err
		}

		var kept []Host
		for _, h := range st.Hosts {
			if !h.Pinned && h.LastSeen.Before(olderThan) {
				removed = append(removed, h)
				continue
			}
			kept = append(kept, h)
		}

		if dryRun {
			return nil
		}

		st.Hosts = kept

		return s.writeState(st)
	})
	if err != nil {
		return nil, err
	}

	return removed, nil
}

// List returns the remembered hosts.
//
// The entries come back in the order they are stored, and the returned slice
// and its contents are the caller's to modify. Presentation order, pinned
// first and then by last seen (DESIGN.md section 9.1), belongs to the screen
// that renders them, not to the store.
func (s *Store) List() ([]Host, error) {
	var hosts []Host

	err := s.withLock(func() error {
		st, err := s.readState()
		if err != nil {
			return err
		}
		hosts = st.Hosts
		return nil
	})
	if err != nil {
		return nil, err
	}

	return hosts, nil
}
