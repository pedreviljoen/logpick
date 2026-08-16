package state

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic writes data to path atomically.
//
// A temp file is created in the same directory as path, written, fsynced
// and then renamed over path. Creating the temp file in the same directory
// rather than, say, os.TempDir matters: Rename is only atomic within one
// filesystem, and a temp file on a different mount could not be renamed
// atomically at all on many systems (AGENTS.md section 3 invariant 5).
//
// A crash or a full disk during the write therefore leaves either the old
// contents at path or the fully-written new ones, never a half-written
// file. If any step fails, the temp file is removed rather than left
// behind.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	closed := false

	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if chmodErr := os.Chmod(tmpPath, perm); chmodErr != nil { //nolint:gosec // tmpPath is the name CreateTemp just returned, not caller input.
		return fmt.Errorf("setting permissions on temp file: %w", chmodErr)
	}

	if _, writeErr := tmp.Write(data); writeErr != nil {
		return fmt.Errorf("writing temp file: %w", writeErr)
	}

	if syncErr := tmp.Sync(); syncErr != nil {
		return fmt.Errorf("syncing temp file: %w", syncErr)
	}

	if closeErr := tmp.Close(); closeErr != nil {
		closed = true
		return fmt.Errorf("closing temp file: %w", closeErr)
	}
	closed = true

	if renameErr := os.Rename(tmpPath, path); renameErr != nil { //nolint:gosec // tmpPath is the name CreateTemp just returned; path is the caller-chosen state file location (XDG default), not untrusted input.
		return fmt.Errorf("renaming temp file to %s: %w", path, renameErr)
	}

	return nil
}
