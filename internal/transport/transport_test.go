package transport

// Test plan for T02, one test per mechanic (AGENTS.md sections 5 and 8, T02).
// FallbackFetcher is exercised against a stub Transport, never a real process.
//
//  1. A successful fetch writes the full stream to the destination and returns
//     the byte count.
//  2. A stream that errors mid-transfer leaves no file at the destination, and
//     the .part file is removed.
//  3. Cancelling the context stops the transfer and returns the context error.
//  4. Progress messages are emitted and the final one reports the total.
//  5. A nil progress channel is accepted and does not block the transfer.
//
// Three points in the mechanics needed resolving before the assertions could be
// written. They are recorded here because they are contract, not test detail,
// and they are stated in the doc comment on FallbackFetcher.Fetch too.
//
//   - "returns the context error" (mechanic 3) and the error of mechanic 2 are
//     both asserted with errors.Is plus a strings.Contains for the remote path,
//     per AGENTS.md section 5. That means every error out of Fetch wraps the
//     underlying cause with %w and names the remote path.
//   - "the final one reports the total" (mechanic 4) means the last Progress
//     carries the completed size in Bytes, equal to the count Fetch returns.
//     Progress.Total stays zero: Fetch is given no size and the fallback path
//     cannot learn one without a second round trip, so zero is the honest
//     value and the caller fills the total in from discovery.
//   - Every case asserts that Exec was called exactly once with a cat of the
//     remote path, since streaming cat over Exec is what FallbackFetcher is.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// errStreamBroken is the failure the stub injects mid-transfer. Tests match it
// with errors.Is, so Fetch has to wrap rather than replace it.
var errStreamBroken = errors.New("stub: remote read failed, disk on fire")

// stubTransport is a Transport whose Exec hands back a Process the test built,
// which is how the copy loop is driven without spawning anything. AGENTS.md
// section 5 permits a small stub here.
type stubTransport struct {
	exec func(ctx context.Context, cmd string) (*Process, error)

	mu   sync.Mutex
	cmds []string
}

var _ Transport = (*stubTransport)(nil)

func (s *stubTransport) Exec(ctx context.Context, cmd string) (*Process, error) {
	s.mu.Lock()
	s.cmds = append(s.cmds, cmd)
	s.mu.Unlock()
	return s.exec(ctx, cmd)
}

// Fetch is never called: the tests call FallbackFetcher.Fetch directly, which
// is the unit under test.
func (s *stubTransport) Fetch(context.Context, string, string, chan<- Progress) (int64, error) {
	return 0, errors.New("stub: Fetch must not be called")
}

func (s *stubTransport) Caps() Caps {
	return Caps{Follow: true, ConcurrentExec: true, BinarySafe: true}
}

func (s *stubTransport) Check(context.Context) error { return nil }

func (s *stubTransport) Close() error { return nil }

// commands returns the commands Exec has been asked to run.
func (s *stubTransport) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.cmds...)
}

// scriptedReader stands in for the stdout of a remote cat. It serves its data
// in fixed size chunks so the copy loop makes several passes, it can fail part
// way through, and it can run a hook once the first chunk is out. That hook is
// how a test sequences cancellation against an in-flight transfer without a
// sleep.
//
// It holds the fetch context's done channel rather than the context itself,
// and reports cancellation the way a real backend does: the process dies and
// the next read off its stdout fails.
type scriptedReader struct {
	done      <-chan struct{} // closed when the fetch context is cancelled
	cancelled func() error    // the context's error, once done is closed
	data      []byte
	chunk     int    // bytes per Read; zero fills the caller's buffer
	failAt    int    // bytes served before the stream fails; zero never fails
	failErr   error  // the failure served at failAt
	afterRead func() // runs once, after the first chunk is served

	pos  int
	once sync.Once
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	select {
	case <-r.done:
		return 0, r.cancelled()
	default:
	}
	if r.failAt > 0 && r.pos >= r.failAt {
		return 0, r.failErr
	}
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}

	n := r.chunk
	if n <= 0 || n > len(p) {
		n = len(p)
	}
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}
	if r.failAt > 0 && r.pos+n > r.failAt {
		n = r.failAt - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	if r.afterRead != nil {
		r.once.Do(r.afterRead)
	}
	return n, nil
}

func TestFallbackFetcherFetch(t *testing.T) {
	const (
		remote = "/var/log/logpick/app.log"
		line   = "logpick fallback fetch line\n"
	)

	tests := []struct {
		name         string
		content      string
		chunk        int
		failAt       int
		cancelMidway bool
		nilProgress  bool
		wantErr      error
		wantErrHas   []string
		wantProgress bool
	}{
		{
			name:    "a successful fetch writes the full stream and returns the byte count",
			content: strings.Repeat(line, 8),
		},
		{
			name:       "a stream that errors mid-transfer leaves no file at the destination",
			content:    strings.Repeat(line, 512),
			chunk:      1024,
			failAt:     4096,
			wantErr:    errStreamBroken,
			wantErrHas: []string{remote, "disk on fire"},
		},
		{
			name:         "cancelling the context stops the transfer and returns the context error",
			content:      strings.Repeat(line, 2048),
			chunk:        4096,
			cancelMidway: true,
			wantErr:      context.Canceled,
			wantErrHas:   []string{remote},
		},
		{
			name:         "progress messages are emitted and the final one reports the total",
			content:      strings.Repeat(line, 4096),
			chunk:        4096,
			wantProgress: true,
		},
		{
			name:        "a nil progress channel is accepted and does not block the transfer",
			content:     strings.Repeat(line, 2048),
			chunk:       4096,
			nilProgress: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var afterRead func()
			if tc.cancelMidway {
				afterRead = cancel
			}

			stub := &stubTransport{
				exec: func(execCtx context.Context, _ string) (*Process, error) {
					return &Process{
						Stdout: io.NopCloser(&scriptedReader{
							done:      execCtx.Done(),
							cancelled: execCtx.Err,
							data:      []byte(tc.content),
							chunk:     tc.chunk,
							failAt:    tc.failAt,
							failErr:   errStreamBroken,
							afterRead: afterRead,
						}),
						Stderr: io.NopCloser(strings.NewReader("")),
						Wait:   func() error { return nil },
					}, nil
				},
			}

			dest := filepath.Join(t.TempDir(), "app.log")

			// A buffered channel drained by a goroutine, so neither a blocking
			// send nor a dropped non-blocking send can make this flaky.
			var prog chan Progress
			collected := make(chan []Progress, 1)
			if !tc.nilProgress {
				prog = make(chan Progress, 64)
				go func() {
					var got []Progress
					for p := range prog {
						got = append(got, p)
					}
					collected <- got
				}()
			}

			n, err := FallbackFetcher{T: stub}.Fetch(ctx, remote, dest, prog)
			if prog != nil {
				close(prog)
			}

			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("Fetch returned %d bytes and no error, want an error matching %v", n, tc.wantErr)
				}
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("Fetch error = %v, want errors.Is(err, %v)", err, tc.wantErr)
				}
				for _, want := range tc.wantErrHas {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Fetch error = %q, want it to mention %q", err.Error(), want)
					}
				}
				assertNotExist(t, dest)
			} else {
				if err != nil {
					t.Fatalf("Fetch returned unexpected error: %v", err)
				}
				if want := int64(len(tc.content)); n != want {
					t.Errorf("Fetch returned %d bytes, want %d", n, want)
				}
				assertContent(t, dest, tc.content)
			}
			assertNotExist(t, dest+".part")

			cmds := stub.commands()
			if len(cmds) != 1 {
				t.Fatalf("stub ran %d commands (%q), want exactly 1", len(cmds), cmds)
			}
			if !strings.HasPrefix(cmds[0], "cat ") || !strings.Contains(cmds[0], remote) {
				t.Errorf("stub ran %q, want a cat of %q", cmds[0], remote)
			}

			if tc.nilProgress {
				return
			}
			got := <-collected
			if !tc.wantProgress {
				return
			}
			if len(got) < 2 {
				t.Fatalf("got %d progress messages over %d bytes, want at least 2", len(got), len(tc.content))
			}
			var prev int64
			for i, p := range got {
				if p.Bytes < prev {
					t.Errorf("progress message %d went backwards: %d bytes after %d", i, p.Bytes, prev)
				}
				prev = p.Bytes
			}
			want := Progress{Bytes: int64(len(tc.content))}
			if diff := cmp.Diff(want, got[len(got)-1]); diff != "" {
				t.Errorf("final progress message mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// assertContent fails unless path holds exactly want.
func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("%s holds %d bytes which differ from the %d streamed bytes", path, len(got), len(want))
	}
}

// assertNotExist fails unless nothing is at path.
func assertNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected nothing at %s, os.Stat returned error %v", path, err)
	}
}
