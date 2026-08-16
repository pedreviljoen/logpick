//go:build unix

package state

import (
	"fmt"
	"os"
	"syscall"
)

// lockSuffix names the advisory lock file kept beside the state file. It is
// a separate file from the state file itself, rather than an flock on
// state.toml directly, so that Save's atomic Rename of a brand new inode
// into place never disturbs a lock held on the still-open lock descriptor.
const lockSuffix = ".lock"

// acquireLock takes a blocking exclusive flock on the lock file beside
// path, creating it if necessary, and returns a function that releases it.
//
// flock is advisory and scoped to one open file descriptor: it serialises
// this process against other processes (and against itself if it opened
// the file twice), but not two goroutines sharing one already-open
// descriptor. That is why Store also holds a sync.Mutex for the duration
// of every operation; the two are not redundant (DESIGN.md section 11,
// AGENTS.md section 3 invariant 5).
//
// v1 has no Windows support (DESIGN.md section 3), so a syscall.Flock
// implementation gated on the "unix" build constraint is the whole story;
// there is no cross-platform fallback to maintain.
func acquireLock(path string) (release func() error, err error) {
	f, err := os.OpenFile(path+lockSuffix, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path is the caller-chosen state file location (XDG default), not untrusted input.
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // Fd() is a small unix file descriptor; it always fits in an int.
		_ = f.Close()
		return nil, fmt.Errorf("locking state file: %w", err)
	}

	return func() error {
		unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // Fd() is a small unix file descriptor; it always fits in an int.
		closeErr := f.Close()
		if unlockErr != nil {
			return fmt.Errorf("unlocking state file: %w", unlockErr)
		}
		if closeErr != nil {
			return fmt.Errorf("closing lock file: %w", closeErr)
		}
		return nil
	}, nil
}
