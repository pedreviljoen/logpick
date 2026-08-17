package local

// Test plan for T15, one test per mechanic (AGENTS.md sections 5 and 8, T15).
//
//  1. PathFor produces the host, timestamp and preserved-remote-path
//     layout.
//  2. Two fetches of the same remote path at different times produce
//     distinct destinations.
//  3. Files are created 0600 inside 0700 directories.
//  4. A profile with a copy template uses it, one without falls back to
//     FallbackFetcher.
//  5. A file above the size threshold returns the confirmation-required
//     signal rather than starting the transfer.
//
// Several points needed resolving before the assertions could be written.
// They are recorded here, and in store.go's doc comments, because they are
// contract, not test detail:
//
//   - PathFor is a method on a Store bound to a root
//     (local.New(root) *Store), not a free function taking root as a third
//     parameter. Every test below builds a Store over t.TempDir(); nothing
//     here ever resolves or touches the real XDG data directory.
//   - The <utc-timestamp> segment uses layout "2006-01-02T150405Z" — the
//     exact shape of DESIGN.md 6.3 and 10.1's worked example,
//     "2026-08-14T160251Z": dashes kept in the date, colons stripped from
//     the time of day, a literal "Z" since PathFor always converts to UTC
//     first. Second resolution is what mechanic 2 depends on: its two `at`
//     values are a second apart, not merely `!=`.
//   - The confirmation-required signal is a typed error,
//     *ConfirmRequiredError, carrying Host, Remote and Size — the fields
//     ui.FetchConfirmMsg needs (internal/ui/msg.go) — with an Unwrap that
//     returns the sentinel ErrConfirmRequired, so a caller that only wants
//     to branch on the condition can still use
//     errors.Is(err, ErrConfirmRequired). This is the same two-piece
//     pattern transport.ExitError/ErrAuthRequired already uses
//     (internal/transport/command.go).
//   - Mechanic 4's observable seam is recordingTransport, below: a small
//     hand-rolled Transport (AGENTS.md section 5's "small stubs where a
//     task says so"), not transport.Mock. Mock cannot demonstrate the
//     native-copy side of this mechanic on its own — its Caps().NativeCopy
//     is always false and its Fetch is always the inherited
//     FallbackFetcher (see the Mock doc comment's "Caps" section,
//     internal/transport/mock.go) — so it can only ever take the fallback
//     path. recordingTransport can be switched between the two: with
//     native set, its Fetch writes the destination directly and never
//     calls Exec; with native unset, its Fetch defers to a FallbackFetcher
//     wired back to itself, so the resulting "cat <remote>" is recorded by
//     its own Exec. The test asserts on that recording directly, not on
//     Store.Fetch's return value, since both paths produce the same bytes
//     at the same destination and only the recording tells them apart.
//   - Mechanics 1, 2, 3 and 5 use transport.NewMock over
//     internal/transport/testdata/fixtures, per AGENTS.md section 5: no
//     process spawn, no network. Only mechanic 4 needs recordingTransport,
//     for the reason above.
//   - Store.Fetch also fills in Progress.Total (zero from FallbackFetcher
//     on its own) using the size parameter before forwarding to the
//     caller's channel — documented on Fetch's doc comment in store.go —
//     but that is not one of the five listed mechanics and has no
//     dedicated test here, per AGENTS.md section 5's instruction not to
//     invent a mechanic beyond the task's list.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/pedreviljoen/logpick/internal/state"
	"github.com/pedreviljoen/logpick/internal/transport"
)

// fixturesDir is the fixture corpus T05's mock transport serves
// (internal/transport/testdata/fixtures), reused here so mechanics 1, 2, 3
// and 5 exercise Store.Fetch against a real, static file tree rather than
// inventing a parallel fixture set.
const fixturesDir = "../transport/testdata/fixtures"

// newState returns a *state.Store bound to a fresh state.toml under its own
// t.TempDir(), never the real XDG state path.
func newState(t *testing.T) *state.Store {
	t.Helper()
	return state.New(filepath.Join(t.TempDir(), "state.toml"))
}

func TestPathFor(t *testing.T) {
	t.Run("PathFor produces the host, timestamp and preserved-remote-path layout", func(t *testing.T) {
		root := t.TempDir()
		s := New(root)

		// A non-UTC input confirms PathFor converts to UTC before
		// formatting, not just that it formats whatever zone it is given.
		loc := time.FixedZone("UTC-5", -5*60*60)
		at := time.Date(2026, 8, 14, 11, 2, 51, 0, loc) // 16:02:51 UTC

		got := s.PathFor("jenkins-01.prod.internal", "/var/lib/jenkins/logs/jenkins.log", at)

		want := filepath.Join(root,
			"jenkins-01.prod.internal",
			"2026-08-14T160251Z",
			"var", "lib", "jenkins", "logs", "jenkins.log",
		)
		if got != want {
			t.Errorf("PathFor() = %q, want %q", got, want)
		}
	})
}

func TestFetch(t *testing.T) {
	t.Run("two fetches of the same remote path at different times produce distinct destinations", func(t *testing.T) {
		root := t.TempDir()
		s := New(root)
		rec := newState(t)
		mt := transport.NewMock(fixturesDir)

		host := "host-a"
		remote := "/var/log/syslog"
		at1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		at2 := at1.Add(time.Hour)

		dest1, n1, err := s.Fetch(context.Background(), mt, rec, host, remote, 296, at1, nil)
		if err != nil {
			t.Fatalf("first Fetch: %v", err)
		}
		dest2, n2, err := s.Fetch(context.Background(), mt, rec, host, remote, 296, at2, nil)
		if err != nil {
			t.Fatalf("second Fetch: %v", err)
		}

		if dest1 == dest2 {
			t.Fatalf("two fetches at different times produced the same destination: %q", dest1)
		}
		if n1 != n2 {
			t.Errorf("byte counts differ between two fetches of the same file: %d vs %d", n1, n2)
		}

		hostDir := filepath.Join(root, host) + string(filepath.Separator)
		if !strings.HasPrefix(dest1, hostDir) {
			t.Errorf("dest1 %q is not under host directory %q", dest1, hostDir)
		}
		if !strings.HasPrefix(dest2, hostDir) {
			t.Errorf("dest2 %q is not under host directory %q", dest2, hostDir)
		}

		for _, dest := range []string{dest1, dest2} {
			got, err := os.ReadFile(dest) //nolint:gosec // dest is produced by the Store under test, inside t.TempDir().
			if err != nil {
				t.Fatalf("reading fetched file %s: %v", dest, err)
			}
			want, err := os.ReadFile(filepath.Join(fixturesDir, "var", "log", "syslog"))
			if err != nil {
				t.Fatalf("reading fixture: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("fetched content mismatch for %s", dest)
			}
		}
	})

	t.Run("files are created 0600 inside 0700 directories", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("POSIX permission bits are not meaningful on windows; CI is linux and macOS only (AGENTS.md section 6)")
		}

		root := t.TempDir()
		s := New(root)
		rec := newState(t)
		mt := transport.NewMock(fixturesDir)

		at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
		dest, _, err := s.Fetch(context.Background(), mt, rec, "host-a", "/var/log/syslog", 296, at, nil)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}

		fileInfo, err := os.Stat(dest)
		if err != nil {
			t.Fatalf("stat fetched file: %v", err)
		}
		if got, want := fileInfo.Mode().Perm(), os.FileMode(0o600); got != want {
			t.Errorf("fetched file mode = %v, want %v", got, want)
		}

		dirInfo, err := os.Stat(filepath.Dir(dest))
		if err != nil {
			t.Fatalf("stat fetched file's directory: %v", err)
		}
		if got, want := dirInfo.Mode().Perm(), os.FileMode(0o700); got != want {
			t.Errorf("fetched file's directory mode = %v, want %v", got, want)
		}
	})

	t.Run("a profile with a copy template uses it, one without falls back to FallbackFetcher", func(t *testing.T) {
		content := []byte("hello from a fetch\n")
		at := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

		t.Run("native copy template is used", func(t *testing.T) {
			root := t.TempDir()
			s := New(root)
			rec := newState(t)
			rt := &recordingTransport{native: true, content: content}

			dest, n, err := s.Fetch(context.Background(), rt, rec, "host-a", "/var/log/app.log", int64(len(content)), at, nil)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if n != int64(len(content)) {
				t.Errorf("bytes = %d, want %d", n, len(content))
			}
			if execs := rt.execCmds(); len(execs) != 0 {
				t.Errorf("native copy path ran Exec commands, want none: %v", execs)
			}
			got, err := os.ReadFile(dest) //nolint:gosec // dest is produced by the Store under test, inside t.TempDir().
			if err != nil {
				t.Fatalf("reading fetched file: %v", err)
			}
			if !bytes.Equal(got, content) {
				t.Errorf("fetched content = %q, want %q", got, content)
			}
		})

		t.Run("no copy template falls back to cat over Exec", func(t *testing.T) {
			root := t.TempDir()
			s := New(root)
			rec := newState(t)
			rt := &recordingTransport{native: false, content: content}

			dest, n, err := s.Fetch(context.Background(), rt, rec, "host-a", "/var/log/app.log", int64(len(content)), at, nil)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if n != int64(len(content)) {
				t.Errorf("bytes = %d, want %d", n, len(content))
			}
			execs := rt.execCmds()
			if len(execs) != 1 || !strings.HasPrefix(execs[0], "cat ") {
				t.Fatalf("fallback path Exec commands = %v, want exactly one starting with %q", execs, "cat ")
			}
			got, err := os.ReadFile(dest) //nolint:gosec // dest is produced by the Store under test, inside t.TempDir().
			if err != nil {
				t.Fatalf("reading fetched file: %v", err)
			}
			if !bytes.Equal(got, content) {
				t.Errorf("fetched content = %q, want %q", got, content)
			}
		})
	})

	t.Run("a file above the size threshold returns the confirmation-required signal rather than starting the transfer", func(t *testing.T) {
		root := t.TempDir()
		s := New(root)
		rec := newState(t)
		mt := transport.NewMock(fixturesDir)

		host := "host-a"
		remote := "/var/log/syslog"
		at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

		_, _, err := s.Fetch(context.Background(), mt, rec, host, remote, SizeThreshold, at, nil)
		if !errors.Is(err, ErrConfirmRequired) {
			t.Fatalf("Fetch error = %v, want errors.Is(err, ErrConfirmRequired)", err)
		}

		var confirmErr *ConfirmRequiredError
		if !errors.As(err, &confirmErr) {
			t.Fatalf("Fetch error does not unwrap to *ConfirmRequiredError: %v", err)
		}
		want := &ConfirmRequiredError{Host: host, Remote: remote, Size: SizeThreshold}
		if diff := cmp.Diff(want, confirmErr); diff != "" {
			t.Errorf("confirm error mismatch (-want +got):\n%s", diff)
		}

		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("reading root: %v", err)
		}
		if len(entries) != 0 {
			t.Errorf("Fetch created entries under root despite requiring confirmation: %v", entries)
		}

		hosts, err := rec.List()
		if err != nil {
			t.Fatalf("reading state: %v", err)
		}
		if len(hosts) != 0 {
			t.Errorf("Fetch touched state despite requiring confirmation: %v", hosts)
		}
	})
}

// recordingTransport is a small hand-rolled transport.Transport (AGENTS.md
// section 5's sanctioned exception, "small stubs where a task says so"),
// used only by the native-vs-fallback mechanic. See the test-plan comment
// block above for why transport.Mock cannot serve this purpose by itself.
//
// When native is true, Fetch simulates a profile with a copy template: it
// writes content to the destination directly and never calls Exec. When
// native is false, Fetch defers to a transport.FallbackFetcher wired back
// to this same stub — the same self-wiring transport.Command's NewCommand
// uses for its own embedded FallbackFetcher (internal/transport/command.go)
// — so the resulting "cat <remote>" runs through, and is recorded by, this
// stub's own Exec.
type recordingTransport struct {
	native  bool
	content []byte

	mu    sync.Mutex
	execs []string
}

var _ transport.Transport = (*recordingTransport)(nil)

func (r *recordingTransport) Exec(_ context.Context, cmd string) (*transport.Process, error) {
	r.mu.Lock()
	r.execs = append(r.execs, cmd)
	r.mu.Unlock()

	return &transport.Process{
		Stdout: io.NopCloser(bytes.NewReader(r.content)),
		Stderr: io.NopCloser(strings.NewReader("")),
		Wait:   func() error { return nil },
	}, nil
}

func (r *recordingTransport) Fetch(ctx context.Context, remote, local string, prog chan<- transport.Progress) (int64, error) {
	if r.native {
		if err := os.WriteFile(local, r.content, 0o600); err != nil { //nolint:gosec // local is the caller-chosen destination under a test's t.TempDir().
			return 0, err
		}
		return int64(len(r.content)), nil
	}
	return (transport.FallbackFetcher{T: r}).Fetch(ctx, remote, local, prog)
}

func (r *recordingTransport) Caps() transport.Caps {
	return transport.Caps{NativeCopy: r.native}
}

func (r *recordingTransport) Check(_ context.Context) error { return nil }

func (r *recordingTransport) Close() error { return nil }

// execCmds returns a snapshot of every command Exec has recorded so far.
func (r *recordingTransport) execCmds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.execs))
	copy(out, r.execs)
	return out
}
