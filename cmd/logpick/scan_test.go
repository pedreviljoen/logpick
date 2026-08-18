package main

// Test plan for T11 (AGENTS.md sections 2, 3, 4, 4.1, 5; DESIGN.md sections
// 8.2, 8.3, 6.4, 7.5). Copied verbatim from the task before any test below
// was written.
//
// Mechanics:
//  1. Given the mock transport, the rendered table lists entries sorted by
//     mtime descending.
//  2. The capability probe runs once and the result is written to state.
//  3. A cached capability value skips the probe.
//  4. A transport error returning ErrAuthRequired prints the profile's
//     auth_hint.
//
// Notes on how these were resolved into assertions:
//
//   - No test asserts on exact column padding of the rendered table: that
//     is implementation detail of the renderer, not part of runScan's
//     contract, and asserting on it would be exactly the brittleness
//     AGENTS.md section 5 warns against. Every test instead asserts on the
//     ordering of identifying substrings (a path) within the rendered
//     output, and on the presence of other identifying substrings (an
//     auth_hint).
//
//   - Mechanic 1 needs entries with distinct, known mtimes to assert an
//     order on, but the committed fixture files under
//     internal/transport/testdata/fixtures all land on disk within
//     milliseconds of each other at checkout time, which is not a
//     dependable source of ordering across machines or filesystems.
//     copyFixtures copies the fixture corpus into a t.TempDir() so the
//     test can set explicit, minute-spaced mtimes with os.Chtimes without
//     touching the committed fixtures, and without racing any other test
//     that reads them concurrently. The mock is built over that copy, not
//     over the committed directory, in every test below, for the same
//     "own temp copy" reason even where mtime is not the point of the
//     test.
//
//   - Mechanic 1 also pre-seeds the state Store with cached GNUFind caps
//     before calling runScan, so the test's assertions are about sorting
//     only and are not entangled with the capability-probe mechanics
//     covered separately by mechanics 2 and 3. GNUFind true is required
//     for this: the BSD-format scan output carries no mtime at all (T10),
//     so only the GNU form's real per-file mtimes make an order assertion
//     meaningful.
//
//   - Mechanics 2 and 3 observe the capability probe's behaviour, not any
//     unexported field: both read back state.Store.Load after calling
//     runScan and inspect the persisted Host.Caps. Mechanic 2 asserts
//     ProbedAt is now set and equals the value the injected Now clock
//     returned, which only happens if a probe actually ran and wrote it.
//     Mechanic 3 pre-seeds Caps with one clock value, calls runScan with
//     Now wired to a distinguishably different value, and asserts
//     ProbedAt is still the pre-seeded value: had a probe run despite the
//     cache, UpsertCaps would have overwritten it with the second clock's
//     value (state.Store.UpsertCaps's documented behaviour), so an
//     unchanged ProbedAt is direct evidence the probe was skipped.
//
//   - Mechanic 4 also pre-seeds cached caps, for the same reason as
//     mechanic 1: it isolates the assertion (the auth_hint appears in the
//     output, and the returned error wraps ErrAuthRequired) from the
//     probe mechanics already covered elsewhere. The injected failure
//     therefore lands on the scan's own Exec call, exactly the case the
//     mechanic describes.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pedreviljoen/logpick/internal/config"
	"github.com/pedreviljoen/logpick/internal/state"
	"github.com/pedreviljoen/logpick/internal/transport"
)

// fixturesDir is the fixture corpus every test below copies from. See
// internal/transport/mock.go's doc comment for what it contains and the
// command shapes transport.Mock recognises against it.
const fixturesDir = "../../internal/transport/testdata/fixtures"

// copyFixtures copies fixturesDir into a fresh directory under
// t.TempDir() and returns that directory's path. See the mechanic 1 note
// above for why: tests that need controlled mtimes must not mutate the
// committed fixtures, and must not race other tests reading them
// concurrently.
func copyFixtures(t *testing.T) string {
	t.Helper()

	dst := filepath.Join(t.TempDir(), "fixtures")
	err := filepath.WalkDir(fixturesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(fixturesDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		data, err := os.ReadFile(path) //nolint:gosec // path comes from WalkDir over the committed, trusted fixturesDir.
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o600)
	})
	if err != nil {
		t.Fatalf("copying fixtures from %s: %v", fixturesDir, err)
	}
	return dst
}

// newStore returns a *state.Store backed by a fresh state.toml under
// t.TempDir(), so every test starts from an empty, on-disk state file
// with no network and no shared state between tests.
func newStore(t *testing.T) *state.Store {
	t.Helper()
	return state.New(filepath.Join(t.TempDir(), "state.toml"))
}

// fixedClock returns a deps.Now func that always reports at, so a test
// controls every timestamp runScan writes without sleeping (AGENTS.md
// section 5).
func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// resolveDefault resolves host against a nil *config.Config, which yields
// config.DefaultProfile: exec over ssh, and a scan spec covering
// /var/log with the include/exclude patterns DESIGN.md 6.1 models
// [profile.corp.scan] on. That is enough to exercise the fixture corpus
// under internal/transport/testdata/fixtures/var/log without a
// hand-written config.Config in every test.
func resolveDefault(t *testing.T, host string) config.ResolvedHost {
	t.Helper()
	resolved, err := config.Resolve(nil, host, config.Overrides{})
	if err != nil {
		t.Fatalf("resolving host %q: %v", host, err)
	}
	return resolved
}

// loadCaps reads back host's Caps from store, failing the test if store
// has no entry for host at all.
func loadCaps(t *testing.T, store *state.Store, host string) state.Caps {
	t.Helper()
	st, err := store.Load()
	if err != nil {
		t.Fatalf("loading state: %v", err)
	}
	for _, h := range st.Hosts {
		if h.Name == host {
			return h.Caps
		}
	}
	t.Fatalf("no state entry recorded for host %q", host)
	return state.Caps{}
}

func strPtr(s string) *string { return &s }

func TestProbeGNUFindDoesNotDeadlockOnCapturedStderr(t *testing.T) {
	profile := config.Profile{Exec: []string{"/bin/sh", "-c", `printf warning >&2`}}
	tp := transport.NewCommand("test-host", profile)
	defer func() { _ = tp.Close() }()

	done := make(chan bool, 1)
	go func() {
		done <- probeGNUFind(context.Background(), tp)
	}()

	select {
	case got := <-done:
		if !got {
			t.Fatal("probeGNUFind = false, want successful command to report GNU find")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("probeGNUFind deadlocked draining stderr before Process.Wait")
	}
}

func TestRunScan(t *testing.T) {
	t.Run("entries sorted by mtime descending", func(t *testing.T) {
		dir := copyFixtures(t)

		// Newest first: the order runScan's rendered table must
		// reproduce.
		wantOrder := []string{
			"/var/log/messages",
			"/var/log/syslog",
			"/var/log/auth.log",
			"/var/log/nginx/access.log",
			"/var/log/nginx/error.log",
			"/var/log/app name/app.log",
		}
		base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		for i, remote := range wantOrder {
			local := filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(remote, "/")))
			mtime := base.Add(-time.Duration(i) * time.Minute)
			if err := os.Chtimes(local, mtime, mtime); err != nil {
				t.Fatalf("setting mtime on %s: %v", local, err)
			}
		}

		host := "sorted-host"
		store := newStore(t)
		if err := store.UpsertCaps(host, state.Caps{GNUFind: true}, base); err != nil {
			t.Fatalf("seeding caps: %v", err)
		}

		d := deps{
			Transport: transport.NewMock(dir),
			Store:     store,
			Host:      resolveDefault(t, host),
			Now:       fixedClock(base),
		}

		var buf bytes.Buffer
		if err := runScan(context.Background(), d, host, &buf); err != nil {
			t.Fatalf("runScan: %v", err)
		}

		out := buf.String()
		lastIdx := -1
		for _, remote := range wantOrder {
			idx := strings.Index(out, remote)
			if idx == -1 {
				t.Fatalf("output missing entry %q; got:\n%s", remote, out)
			}
			if idx <= lastIdx {
				t.Fatalf("entry %q appears out of mtime-descending order; got:\n%s", remote, out)
			}
			lastIdx = idx
		}
	})

	t.Run("capability probe runs once and is cached", func(t *testing.T) {
		dir := copyFixtures(t)
		host := "probe-host"
		store := newStore(t)

		probeTime := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		d := deps{
			Transport: transport.NewMock(dir),
			Store:     store,
			Host:      resolveDefault(t, host),
			Now:       fixedClock(probeTime),
		}

		var buf bytes.Buffer
		if err := runScan(context.Background(), d, host, &buf); err != nil {
			t.Fatalf("runScan: %v", err)
		}

		got := loadCaps(t, store, host)
		if got.ProbedAt.IsZero() {
			t.Fatalf("Caps.ProbedAt is zero after runScan; the capability probe should have run and recorded it")
		}
		if !got.ProbedAt.Equal(probeTime) {
			t.Fatalf("Caps.ProbedAt = %v, want %v (the injected clock, proving the probe wrote through Now)", got.ProbedAt, probeTime)
		}
		if !got.GNUFind {
			t.Fatalf("Caps.GNUFind = false, want true: the mock's default find --version succeeds")
		}
	})

	t.Run("a cached capability value skips the probe", func(t *testing.T) {
		dir := copyFixtures(t)
		host := "cached-host"
		store := newStore(t)

		seededAt := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
		if err := store.UpsertCaps(host, state.Caps{GNUFind: true}, seededAt); err != nil {
			t.Fatalf("seeding caps: %v", err)
		}

		// A clock value clearly distinct from seededAt: if the probe
		// runs despite the cache, UpsertCaps stamps ProbedAt with
		// this instead, per state.Store.UpsertCaps's documented
		// behaviour.
		laterTime := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		d := deps{
			Transport: transport.NewMock(dir),
			Store:     store,
			Host:      resolveDefault(t, host),
			Now:       fixedClock(laterTime),
		}

		var buf bytes.Buffer
		if err := runScan(context.Background(), d, host, &buf); err != nil {
			t.Fatalf("runScan: %v", err)
		}

		got := loadCaps(t, store, host)
		if !got.ProbedAt.Equal(seededAt) {
			t.Fatalf("Caps.ProbedAt = %v, want unchanged seeded value %v: a cached capability should skip a new probe", got.ProbedAt, seededAt)
		}
	})

	t.Run("ErrAuthRequired prints the profile's auth_hint", func(t *testing.T) {
		dir := copyFixtures(t)
		host := "auth-host"
		store := newStore(t)

		const authHint = "corp-bastion login"
		cfg := &config.Config{
			Profiles: map[string]config.Profile{
				"corp": {
					Exec:     []string{"ssh", "{host}", "--", "{cmd}"},
					AuthHint: authHint,
					Scan:     config.DefaultProfile.Scan,
				},
			},
		}
		resolved, err := config.Resolve(cfg, host, config.Overrides{Profile: strPtr("corp")})
		if err != nil {
			t.Fatalf("resolving host: %v", err)
		}
		if resolved.Profile.AuthHint != authHint {
			t.Fatalf("resolved AuthHint = %q, want %q", resolved.Profile.AuthHint, authHint)
		}

		// Cache caps up front so the injected failure below lands on
		// the scan's own Exec call rather than the probe's.
		seededAt := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
		if serr := store.UpsertCaps(host, state.Caps{GNUFind: true}, seededAt); serr != nil {
			t.Fatalf("seeding caps: %v", serr)
		}

		injected := fmt.Errorf("dial %s: %w", host, transport.ErrAuthRequired)
		mock := transport.NewMock(dir, transport.WithFailure(injected))

		d := deps{
			Transport: mock,
			Store:     store,
			Host:      resolved,
			Now:       fixedClock(seededAt),
		}

		var buf bytes.Buffer
		err = runScan(context.Background(), d, host, &buf)
		if err == nil {
			t.Fatalf("runScan: want a non-nil error, got nil")
		}
		if !errors.Is(err, transport.ErrAuthRequired) {
			t.Fatalf("runScan error = %v, want one wrapping transport.ErrAuthRequired", err)
		}
		if !strings.Contains(buf.String(), authHint) {
			t.Fatalf("output does not contain auth_hint %q; got:\n%s", authHint, buf.String())
		}
	})
}
