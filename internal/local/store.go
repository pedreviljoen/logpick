package local

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pedreviljoen/logpick/internal/state"
	"github.com/pedreviljoen/logpick/internal/transport"
)

// SizeThreshold is the file size, in bytes, at or above which Store.Fetch
// refuses to start a transfer and instead returns a *ConfirmRequiredError
// (DESIGN.md 10.2): 500MB, above which the user is usually better served by
// `tail -c` for the last N megabytes than by pulling the whole file.
const SizeThreshold int64 = 500 * 1024 * 1024

// timestampLayout is the Go reference-time layout for the <utc-timestamp>
// path segment of DESIGN.md 10.1's layout. It matches the worked example in
// DESIGN.md 6.3 and 10.1 exactly — "2026-08-14T160251Z": dashes in the
// date, no colons in the time of day, and a literal "Z" (PathFor always
// formats in UTC, so this is never a numeric zone offset). Colons are
// stripped because they are not valid in a Windows path component and are
// an unwelcome complication in a shell-quoted one on any platform; the date
// keeps its dashes, since nothing requires those stripped and the DESIGN.md
// example keeps them. The layout has second resolution, which is what
// makes mechanic 2 (two fetches, different destinations) depend on the two
// `at` values being at least a second apart rather than merely distinct.
const timestampLayout = "2006-01-02T150405Z"

// ErrConfirmRequired is the sentinel a caller matches with
// errors.Is(err, ErrConfirmRequired) to recognise a *ConfirmRequiredError
// returned by Store.Fetch, without needing to know about the concrete
// type. See ConfirmRequiredError's doc comment for why the error also
// carries fields, and command.go's ExitError/ErrAuthRequired
// (internal/transport/command.go) for the identical two-piece pattern this
// one is copied from.
var ErrConfirmRequired = errors.New("fetch: confirmation required")

// ConfirmRequiredError reports that remote's size is at or above
// SizeThreshold, so Store.Fetch has not started a transfer: no destination
// directory or file was created, and nothing was recorded in state
// (mechanic 5).
//
// Host, Remote and Size mirror ui.FetchConfirmMsg's fields exactly
// (internal/ui/msg.go), so the browser's fetch action can build that
// message directly from this error via errors.As, while a caller that only
// wants to branch on the condition uses errors.Is(err, ErrConfirmRequired),
// which Unwrap below satisfies.
type ConfirmRequiredError struct {
	// Host and Remote identify the file that needs confirmation.
	Host   string
	Remote string
	// Size is the file size in bytes, as supplied to Fetch from discovery.
	Size int64
}

// Error implements the error interface.
func (e *ConfirmRequiredError) Error() string {
	return fmt.Sprintf("fetching %s from %s: %d bytes is at or above the %d byte confirmation threshold", e.Remote, e.Host, e.Size, SizeThreshold)
}

// Unwrap makes errors.Is(err, ErrConfirmRequired) succeed for any
// *ConfirmRequiredError, the same way transport.ExitError.Unwrap does for
// ErrAuthRequired.
func (e *ConfirmRequiredError) Unwrap() error {
	return ErrConfirmRequired
}

// Store is DESIGN.md 10.1's fetch destination root:
// ${XDG_DATA_HOME}/logpick/fetched in the running tool, or a t.TempDir() in
// every test. A Store never resolves XDG itself — the caller (cmd/logpick
// in the running tool, a test in every test) resolves XDG_DATA_HOME, or
// picks a temp directory, exactly once and passes the result to New — so
// nothing in this package can accidentally write to a real user's data
// directory during a test.
//
// PathFor is a method on Store, not a free function taking root as a third
// parameter, so that every caller which already holds a Store — the fetch
// action, and later the library screen resolving a stored, root-relative
// state.Fetch.Local back to an absolute path — shares the one place root is
// threaded from, the same shape state.Store (internal/state/state.go)
// already uses for the state file's path via New(path). A Store must be
// created with New; the zero Store has no root and is not usable.
type Store struct {
	root string
}

// New returns a Store that fetches files under root. New touches no disk;
// root's directories are created lazily, as Fetch needs them.
func New(root string) *Store {
	return &Store{root: root}
}

// PathFor returns the absolute local destination for a file fetched from
// remote on host at time at, per DESIGN.md 10.1's layout:
//
//	<root>/<host>/<utc-timestamp>/<original/path>/<file>
//
// remote is the file's absolute POSIX path on the remote host, as
// discovery and every Transport in this codebase produce it, regardless of
// the OS this binary itself runs on. Its leading slash is dropped and the
// rest is preserved verbatim as path segments under the timestamp
// directory, so two hosts' identically named app.log files, or the same
// host's app.log fetched twice, never collide (DESIGN.md 10.1, mechanic 1).
//
// at is converted to UTC and formatted to second resolution using
// timestampLayout before being used as the timestamp segment, so two calls
// with the same host and remote but at least a second apart in at produce
// distinct destinations, both nested under the same <root>/<host>
// directory (mechanic 2).
//
// PathFor does no I/O: it is pure string and path construction, safe to
// call before deciding whether a transfer will actually happen. Fetch below
// calls it before its size-threshold check for exactly this reason — the
// destination a confirmation prompt would eventually use needs no
// filesystem access to compute.
func (s *Store) PathFor(host, remote string, at time.Time) string {
	ts := at.UTC().Format(timestampLayout)
	trimmed := strings.TrimPrefix(remote, "/")
	segments := strings.Split(trimmed, "/")

	parts := make([]string, 0, len(segments)+3)
	parts = append(parts, s.root, host, ts)
	parts = append(parts, segments...)

	return filepath.Join(parts...)
}

// createDestDir creates dir, and every missing ancestor of dir up to but not
// including s.root, at mode 0700, chmod'ing each one explicitly after
// creation.
//
// os.MkdirAll alone is not enough: the mode it is given is subject to the
// process umask, so a request for 0700 can land as something looser (or, on
// a restrictive umask, something that silently fails a strict-mode check
// later). Creating each missing path segment individually and chmod'ing it
// right after means the final mode is always exactly 0700 regardless of
// umask (DESIGN.md section 12; mechanic 3). s.root itself is left
// untouched: it is the caller's directory (a resolved XDG path or a
// t.TempDir() in tests), not one Fetch created, so Fetch has no business
// changing its mode.
func (s *Store) createDestDir(dir string) error {
	rel, err := filepath.Rel(s.root, dir)
	if err != nil {
		return fmt.Errorf("resolving %s relative to store root: %w", dir, err)
	}

	cur := s.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		if err := os.Mkdir(cur, 0o700); err != nil && !os.IsExist(err) {
			return fmt.Errorf("creating %s: %w", cur, err)
		}
		if err := os.Chmod(cur, 0o700); err != nil { //nolint:gosec // cur is a directory this Store just created under its own root, not untrusted input.
			return fmt.Errorf("setting permissions on %s: %w", cur, err)
		}
	}
	return nil
}

// Fetch downloads remote from host over t, landing it under s's root at the
// path PathFor computes for host, remote and at, and records the result
// into rec via state.Store.RecordFetch (internal/state/state.go) stamped
// with at.
//
// at is the one and only source of the time recorded: Fetch never calls
// time.Now(), so a caller controls it completely, including in tests, where
// two fetches issued back to back in wall-clock time still land at whatever
// distinct at values the test chose (mechanic 2).
//
// size is the file's size as already known from discovery (DESIGN.md
// 10.2); Fetch never stats the remote file itself to learn it. If size is
// at or above SizeThreshold, Fetch returns a *ConfirmRequiredError
// (matchable with errors.Is(err, ErrConfirmRequired), or errors.As for its
// Host/Remote/Size fields) before doing anything else: no directory is
// created, no file is written, and rec is not touched (mechanic 5).
//
// Otherwise Fetch creates the destination's parent directories 0700 and
// calls t.Fetch(ctx, remote, dest, prog). t chooses the transfer mechanism
// entirely on its own: a profile with a copy template takes the native
// path, one without falls back to the embedded FallbackFetcher's cat-over-
// Exec streaming (DESIGN.md 7.2; internal/transport/command.go's Fetch).
// Store.Fetch does not inspect t.Caps() and does not choose between them
// itself — it only calls t.Fetch and lets the backend decide (mechanic 4).
//
// prog, when non-nil, receives every transport.Progress t.Fetch sends, with
// Total overwritten to size on each one before it is forwarded:
// FallbackFetcher's Total is always zero (documented on
// internal/transport/transport.go's Fetch, since streaming cat has no way
// to learn the size on its own), but Store.Fetch does know it, from the
// same size parameter the threshold check uses, so a caller watching prog
// gets a real total even over the fallback path. A nil prog is legal and is
// never touched.
//
// On success the destination file is chmod'd 0600 regardless of what mode
// t.Fetch happened to leave it in (its parent directories are already 0700
// from creation; mechanic 3), the fetch is recorded into rec with Local set
// relative to s's root — state.Fetch.Local's documented contract
// (internal/state/state.go) — so relocating root does not invalidate the
// record, and Fetch returns the absolute destination path together with
// the byte count t.Fetch reported.
func (s *Store) Fetch(ctx context.Context, t transport.Transport, rec *state.Store, host, remote string, size int64, at time.Time, prog chan<- transport.Progress) (string, int64, error) {
	dest := s.PathFor(host, remote, at)

	if size >= SizeThreshold {
		return "", 0, &ConfirmRequiredError{Host: host, Remote: remote, Size: size}
	}

	if err := s.createDestDir(filepath.Dir(dest)); err != nil {
		return "", 0, fmt.Errorf("creating destination directory for %s: %w", dest, err)
	}

	// prog, when non-nil, is not handed to t.Fetch directly: Total needs
	// overwriting with size on every message (FallbackFetcher's Total is
	// always zero), so an inner channel is forwarded through a goroutine
	// that rewrites Total before passing each message on. The forwarding
	// goroutine is entirely owned here: it is started only when prog is
	// non-nil, its input channel is closed only after t.Fetch (a
	// synchronous call) has returned — so no further sends onto it can
	// happen — and Fetch waits for it to drain and exit before returning,
	// so nothing outlives this call. Every forwarded send is itself
	// guarded by a select on ctx.Done(), so a caller that stops reading
	// prog blocks only the forwarder, never t.Fetch's own transfer loop,
	// and unblocks as soon as ctx is cancelled.
	var inner chan transport.Progress
	var forwarderDone chan struct{}
	if prog != nil {
		inner = make(chan transport.Progress)
		forwarderDone = make(chan struct{})
		go func() {
			defer close(forwarderDone)
			for p := range inner {
				p.Total = size
				select {
				case prog <- p:
				case <-ctx.Done():
				}
			}
		}()
	}

	n, fetchErr := t.Fetch(ctx, remote, dest, inner)

	if inner != nil {
		close(inner)
		<-forwarderDone
	}

	if fetchErr != nil {
		return "", 0, fmt.Errorf("fetching %s from %s: %w", remote, host, fetchErr)
	}

	if err := os.Chmod(dest, 0o600); err != nil { //nolint:gosec // dest is the path this Store just fetched to, under its own root, not untrusted input.
		return "", 0, fmt.Errorf("setting permissions on %s: %w", dest, err)
	}

	local, err := filepath.Rel(s.root, dest)
	if err != nil {
		return "", 0, fmt.Errorf("computing state-relative path for %s: %w", dest, err)
	}

	if err := rec.RecordFetch(state.Fetch{
		Host:   host,
		Remote: remote,
		Local:  local,
		Bytes:  n,
	}, at); err != nil {
		return "", 0, fmt.Errorf("recording fetch of %s from %s: %w", remote, host, err)
	}

	return dest, n, nil
}
