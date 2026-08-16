package transport

// Test plan for T05, one test per mechanic (AGENTS.md sections 5 and 8, T05).
// Mock is the transport package's own contribution to the test seam every
// package above transport uses (DESIGN.md 7.4, 13), so these tests exercise
// Mock exactly as those packages will: through Transport's Exec, Caps and
// Fetch, never against an unexported field.
//
//  1. Each recognised command shape returns the expected content from the
//     fixture directory.
//  2. An unrecognised command returns an error naming the command.
//  3. The failure injection option makes the next Exec return the injected
//     error.
//  4. The Caps override is reflected by Caps().
//
// Several points needed resolving before the assertions could be written.
// They are recorded here, and in mock.go's doc comments, because they are
// contract, not test detail:
//
//   - Mechanic 1 is one TestMockCommandShapes with one t.Run per command
//     shape, per this task's instructions. tail -f's subtest also covers the
//     behaviour mock.go documents beyond "returns expected content" — the
//     stream staying open and Wait reporting ctx.Err() on cancellation —
//     rather than splitting that into a separate mechanic, since it is one
//     shape's complete behaviour, not an independent way Exec can break.
//   - The path-traversal guard (mock.go's "Path mapping" section) is a
//     distinct way a recognised shape can fail — resolving a path that
//     escapes dir — so it gets its own TestMockPathTraversal rather than
//     being folded into mechanic 1's "returns expected content" cases or
//     mechanic 2's "unrecognised command" cases: the command here is
//     syntactically recognised, only the path resolution fails.
//   - WithLatency's cancellability is not one of the four listed mechanics,
//     but AGENTS.md's "no sleeps, including for latency" rule requires it be
//     tested without ever calling time.Sleep or asserting on elapsed wall
//     time. TestMockLatencyCancellable does this by configuring an hour of
//     latency and passing an already-cancelled context: a correct
//     implementation (select on ctx.Done() against a timer) returns
//     immediately regardless of the configured delay; a bare time.Sleep
//     would hang the test for an hour, which the test's own timeout catches
//     without this file ever sleeping or measuring time itself.
//   - GNU vs BSD find --version (part of mechanic 1's command-shape
//     coverage) is driven by WithCaps(Caps{GNUFind: ...}), per mock.go's
//     "Recognised shapes" section: there is no separate option for it, so
//     that Caps() and the probe response can never disagree.
//   - Every expected size and modification time in the find-scan cases is
//     computed by statting the same fixture file the test reads content
//     from, rather than hard-coded, since a fixture file's mtime is
//     whatever the local checkout gives it and is not otherwise
//     deterministic across machines.
//   - The find-scan cases exercise the deeply nested fixture
//     (opt/app/logs/archive/2024/08/15/worker/deep/nested.log) and the empty
//     fixture (opt/app/logs/empty.log) together with an unquoted "! -name"
//     exclusion, and a second case exercises an unquoted glob root
//     (/var/log/nginx/*) separately, since roots quoted vs. glob-expanded
//     are two different code paths per mock.go's "Recognised shapes"
//     section. The tail, cat and tail -f cases exercise the path containing
//     a space (var/log/app name/app.log).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// fixturesDir is the corpus every Mock test in this file serves, relative to
// the package directory go test runs from.
const fixturesDir = "testdata/fixtures"

// newMock builds a Mock over fixturesDir with the given options.
func newMock(t *testing.T, opts ...MockOption) *Mock {
	t.Helper()
	return NewMock(fixturesDir, opts...)
}

// readFixture returns the content of the fixture file at rel, a path
// relative to fixturesDir using "/" separators.
func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading fixture %s: %v", rel, err)
	}
	return data
}

// statFixture stats the fixture file at rel, the same way Mock's find-scan
// output is expected to.
func statFixture(t *testing.T, rel string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(filepath.Join(fixturesDir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("stat fixture %s: %v", rel, err)
	}
	return info
}

// drain reads proc.Stdout to completion.
func drain(t *testing.T, proc *Process) []byte {
	t.Helper()
	data, err := io.ReadAll(proc.Stdout)
	if err != nil {
		t.Fatalf("reading Stdout: %v", err)
	}
	return data
}

// gnuScanLine builds the GNU -printf scan line mock.go documents for the
// fixture at rel, reporting as remote.
func gnuScanLine(t *testing.T, rel, remote string) string {
	t.Helper()
	info := statFixture(t, rel)
	mt := info.ModTime()
	return fmt.Sprintf("%d\t%d.%09d\t%s\n", info.Size(), mt.Unix(), mt.Nanosecond(), remote)
}

// bsdScanLine builds the BSD "-exec ls -ldn" scan line mock.go documents for
// the fixture at rel, reporting as remote.
func bsdScanLine(t *testing.T, rel, remote string) string {
	t.Helper()
	info := statFixture(t, rel)
	mt := info.ModTime()
	return fmt.Sprintf("-rw-r--r-- 1 501 20 %d %s %d %02d:%02d %s\n",
		info.Size(), mt.Format("Jan"), mt.Day(), mt.Hour(), mt.Minute(), remote)
}

// lastNLines reproduces GNU tail -n's output for the fixture at rel.
func lastNLines(t *testing.T, rel string, n int) string {
	t.Helper()
	data := readFixture(t, rel)
	if len(data) == 0 {
		return ""
	}
	text := string(data)
	trailingNL := strings.HasSuffix(text, "\n")
	if trailingNL {
		text = strings.TrimSuffix(text, "\n")
	}
	lines := strings.Split(text, "\n")
	if n < len(lines) {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if trailingNL {
		out += "\n"
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

// TestMockCommandShapes is mechanic 1: each recognised command shape
// returns the expected content from the fixture directory.
func TestMockCommandShapes(t *testing.T) {
	t.Run("find scan (gnu -printf)", func(t *testing.T) {
		m := newMock(t)
		cmd := `find '/opt/app/logs' -type f \( -name '*.log' -o -name '*.log.*' \) ! -name '*.gz' -printf '%s\t%T@\t%p\n' 2>/dev/null`

		proc, err := m.Exec(context.Background(), cmd)
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		got := drain(t, proc)
		if err := proc.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		want := gnuScanLine(t, "opt/app/logs/app.log", "/opt/app/logs/app.log") +
			gnuScanLine(t, "opt/app/logs/app.log.1", "/opt/app/logs/app.log.1") +
			gnuScanLine(t, "opt/app/logs/archive/2024/08/15/worker/deep/nested.log", "/opt/app/logs/archive/2024/08/15/worker/deep/nested.log") +
			gnuScanLine(t, "opt/app/logs/empty.log", "/opt/app/logs/empty.log")

		if diff := cmp.Diff(want, string(got)); diff != "" {
			t.Errorf("scan output mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("find scan (bsd ls -ldn)", func(t *testing.T) {
		m := newMock(t)
		cmd := `find '/opt/app/logs' -type f \( -name '*.log' -o -name '*.log.*' \) ! -name '*.gz' -exec ls -ldn -- {} + 2>/dev/null`

		proc, err := m.Exec(context.Background(), cmd)
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		got := drain(t, proc)
		if err := proc.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		want := bsdScanLine(t, "opt/app/logs/app.log", "/opt/app/logs/app.log") +
			bsdScanLine(t, "opt/app/logs/app.log.1", "/opt/app/logs/app.log.1") +
			bsdScanLine(t, "opt/app/logs/archive/2024/08/15/worker/deep/nested.log", "/opt/app/logs/archive/2024/08/15/worker/deep/nested.log") +
			bsdScanLine(t, "opt/app/logs/empty.log", "/opt/app/logs/empty.log")

		if diff := cmp.Diff(want, string(got)); diff != "" {
			t.Errorf("scan output mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("find scan (unquoted glob root)", func(t *testing.T) {
		m := newMock(t)
		cmd := `find /var/log/nginx/* -type f \( -name '*.log' \) -printf '%s\t%T@\t%p\n' 2>/dev/null`

		proc, err := m.Exec(context.Background(), cmd)
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		got := drain(t, proc)
		if err := proc.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		want := gnuScanLine(t, "var/log/nginx/access.log", "/var/log/nginx/access.log") +
			gnuScanLine(t, "var/log/nginx/error.log", "/var/log/nginx/error.log")

		if diff := cmp.Diff(want, string(got)); diff != "" {
			t.Errorf("scan output mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("tail -n N", func(t *testing.T) {
		cases := []struct {
			name   string
			remote string
			rel    string
			n      int
		}{
			{"fewer lines requested than the file has", "/var/log/messages", "var/log/messages", 1},
			{"n larger than the line count returns the whole (empty) file", "/opt/app/logs/empty.log", "opt/app/logs/empty.log", 5},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				m := newMock(t)
				proc, err := m.Exec(context.Background(), fmt.Sprintf("tail -n %d %s", tc.n, tc.remote))
				if err != nil {
					t.Fatalf("Exec: %v", err)
				}
				got := drain(t, proc)
				if err := proc.Wait(); err != nil {
					t.Fatalf("Wait: %v", err)
				}

				want := lastNLines(t, tc.rel, tc.n)
				if diff := cmp.Diff(want, string(got)); diff != "" {
					t.Errorf("tail -n output mismatch (-want +got):\n%s", diff)
				}
			})
		}
	})

	t.Run("cat", func(t *testing.T) {
		m := newMock(t)
		proc, err := m.Exec(context.Background(), "cat '/var/log/app name/app.log'")
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		got := drain(t, proc)
		if err := proc.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}

		want := readFixture(t, "var/log/app name/app.log")
		if diff := cmp.Diff(string(want), string(got)); diff != "" {
			t.Errorf("cat output mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("cat empty file", func(t *testing.T) {
		m := newMock(t)
		proc, err := m.Exec(context.Background(), "cat /opt/app/logs/empty.log")
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		got := drain(t, proc)
		if err := proc.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("cat of an empty file returned %d bytes, want 0", len(got))
		}
	})

	t.Run("tail -f", func(t *testing.T) {
		m := newMock(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		proc, err := m.Exec(ctx, "tail -f '/var/log/app name/app.log'")
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}

		content := readFixture(t, "var/log/app name/app.log")
		buf := make([]byte, len(content))
		if _, rerr := io.ReadFull(proc.Stdout, buf); rerr != nil {
			t.Fatalf("reading initial content: %v", rerr)
		}
		if diff := cmp.Diff(string(content), string(buf)); diff != "" {
			t.Errorf("initial content mismatch (-want +got):\n%s", diff)
		}

		// The fixture corpus is static: nothing more is ever written, so
		// the stream must stay open until ctx is cancelled, not report EOF
		// on its own.
		cancel()

		extra := make([]byte, 1)
		n, err := proc.Stdout.Read(extra)
		if n != 0 || !errors.Is(err, io.EOF) {
			t.Errorf("Stdout.Read after cancellation = (%d, %v), want (0, io.EOF)", n, err)
		}

		if waitErr := proc.Wait(); !errors.Is(waitErr, context.Canceled) {
			t.Errorf("Wait() = %v, want context.Canceled", waitErr)
		}
	})

	t.Run("find --version (gnu)", func(t *testing.T) {
		m := newMock(t) // default Caps reports GNUFind true.
		proc, err := m.Exec(context.Background(), "find --version")
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		_ = drain(t, proc)
		if err := proc.Wait(); err != nil {
			t.Errorf("Wait() = %v, want nil for a GNU find --version", err)
		}
	})

	t.Run("find --version (bsd)", func(t *testing.T) {
		m := newMock(t, WithCaps(Caps{
			Follow: true, ConcurrentExec: true, BinarySafe: true,
			GNUFind: boolPtr(false),
		}))
		proc, err := m.Exec(context.Background(), "find --version")
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		_ = drain(t, proc)
		if err := proc.Wait(); err == nil {
			t.Error("Wait() = nil, want a non-nil error for a BSD find --version")
		}
	})
}

// TestMockUnrecognisedCommand is mechanic 2: an unrecognised command returns
// an error naming the command.
func TestMockUnrecognisedCommand(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
	}{
		{"a command outside the recognised set entirely", "rm -rf /var/log"},
		{"a find missing every recognised clause", "find /var/log"},
		{"tail with neither -n nor -f", "tail /var/log/syslog"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMock(t)
			proc, err := m.Exec(context.Background(), tc.cmd)
			if proc != nil {
				t.Error("Exec returned a non-nil Process for an unrecognised command")
			}
			if !errors.Is(err, ErrUnrecognisedCommand) {
				t.Fatalf("Exec(%q) error = %v, want ErrUnrecognisedCommand", tc.cmd, err)
			}
			if !strings.Contains(err.Error(), tc.cmd) {
				t.Errorf("error %q does not name the offending command %q", err, tc.cmd)
			}
		})
	}
}

// TestMockFailureInjection is mechanic 3: the failure injection option makes
// the next Exec return the injected error, and the following Exec succeeds
// (the injection is consumed, not permanent).
func TestMockFailureInjection(t *testing.T) {
	boom := errors.New("boom: injected failure")
	m := newMock(t, WithFailure(boom))

	proc, err := m.Exec(context.Background(), "cat /var/log/syslog")
	if proc != nil {
		t.Error("Exec returned a non-nil Process on the injected failure")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("first Exec error = %v, want the injected failure", err)
	}

	proc, err = m.Exec(context.Background(), "cat /var/log/syslog")
	if err != nil {
		t.Fatalf("second Exec returned %v, want the injection to have been consumed by the first call", err)
	}
	got := drain(t, proc)
	if err := proc.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	want := readFixture(t, "var/log/syslog")
	if diff := cmp.Diff(string(want), string(got)); diff != "" {
		t.Errorf("content mismatch after the injection was consumed (-want +got):\n%s", diff)
	}
}

// TestMockCapsOverride is mechanic 4: the Caps override is reflected by
// Caps().
func TestMockCapsOverride(t *testing.T) {
	cases := []struct {
		name string
		opts []MockOption
		want Caps
	}{
		{
			name: "default caps report a GNU find with no native copy",
			opts: nil,
			want: Caps{NativeCopy: false, Follow: true, ConcurrentExec: true, BinarySafe: true, GNUFind: boolPtr(true)},
		},
		{
			name: "WithCaps replaces every field",
			opts: []MockOption{WithCaps(Caps{
				NativeCopy: true, Follow: false, ConcurrentExec: false, BinarySafe: false,
				GNUFind: boolPtr(false),
			})},
			want: Caps{NativeCopy: true, Follow: false, ConcurrentExec: false, BinarySafe: false, GNUFind: boolPtr(false)},
		},
		{
			name: "WithCaps with a nil GNUFind is reported as nil, not defaulted",
			opts: []MockOption{WithCaps(Caps{Follow: true, ConcurrentExec: true, BinarySafe: true})},
			want: Caps{Follow: true, ConcurrentExec: true, BinarySafe: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMock(t, tc.opts...)
			got := m.Caps()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Caps() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestMockPathTraversal covers mock.go's traversal guard: a path that, once
// mapped under the served directory and cleaned, would read outside it is
// an error, never a read outside the tree. This is a way a syntactically
// recognised shape's argument can be invalid, distinct from mechanic 2's
// wholly unrecognised commands, so it is not folded into that mechanic.
func TestMockPathTraversal(t *testing.T) {
	m := newMock(t)
	cmd := "cat /../secret.log"

	proc, err := m.Exec(context.Background(), cmd)
	if proc != nil {
		t.Error("Exec returned a non-nil Process for a path escaping the fixture root")
	}
	if !errors.Is(err, ErrPathOutsideRoot) {
		t.Fatalf("Exec(%q) error = %v, want ErrPathOutsideRoot", cmd, err)
	}
	if !strings.Contains(err.Error(), "/../secret.log") {
		t.Errorf("error %q does not name the offending path", err)
	}
}

// TestMockLatencyCancellable asserts WithLatency's delay is cancellable via
// ctx, never via a bare time.Sleep, without this test sleeping or measuring
// elapsed time itself: an already-cancelled context, with an hour of
// configured latency, must still make Exec return immediately with
// ctx.Err(). A select-on-ctx.Done()-against-a-timer implementation does so;
// a time.Sleep-based one would instead hang for the full hour, which this
// test's own timeout catches without any sleep appearing here.
func TestMockLatencyCancellable(t *testing.T) {
	m := newMock(t, WithLatency(time.Hour))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := m.Exec(ctx, "cat /var/log/syslog")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Exec with an already-cancelled ctx returned %v, want context.Canceled", err)
	}
}
