package remote

// Test plan for T20 (AGENTS.md sections 5 and 8, T20), copied verbatim
// before any test below was written:
//
//  1. Lines arriving slowly are flushed on the timer.
//  2. Lines arriving fast are flushed at the 200 line threshold, so message
//     count stays proportional to elapsed time rather than to line count.
//  3. The ring buffer discards oldest lines and never exceeds its cap.
//  4. Closing the source closes the output channel and emits
//     StreamClosedMsg.
//  5. Follow is refused when Caps().Follow is false.
//
// Notes on scope, recorded here rather than left implicit:
//
//   - Mechanics 1, 2 and 4 exercise Batch directly, over a plain in/out
//     channel pair, with no transport involved: Batch's contract does not
//     mention a transport at all, and testing it that way is what makes
//     the clock the only moving part.
//   - Mechanic 4's "and emits StreamClosedMsg" half is ui.WaitForLines's
//     own, already-committed contract (internal/ui/app.go): once a channel
//     closes, WaitForLines reports StreamClosedMsg unconditionally. That is
//     not this package's code to re-test. This suite's mechanic 4 proves
//     the half that is this package's code: Batch closes out when in
//     closes.
//   - Mechanic 5 needs a transport whose Caps().Follow is false. Per
//     AGENTS.md section 5 ("no mocks beyond transport.Mock"), that is
//     transport.Mock with WithCaps, not a bespoke stub.
//   - The clock seam is AfterFunc, shaped like time.After. fakeClock below
//     hands Batch a channel per call and lets the test fire it on demand,
//     so no test in this file sleeps or starts a real timer.
//   - Every receive that waits on the code under test is wrapped in a
//     bounded select against a generous timeout, exactly as
//     internal/transport/command_test.go's waitForProcessGone and
//     internal/transport/transport_test.go already do elsewhere in this
//     codebase. That timeout is a deadline guard against a hang in a
//     failing implementation, never a sequencing sleep: every real
//     synchronisation point in these tests is a channel operation.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/pedreviljoen/logpick/internal/transport"
)

// recvTimeout bounds every blocking receive in this file. It exists only so
// a broken implementation fails the test promptly instead of hanging the
// suite.
const recvTimeout = 2 * time.Second

// fixturesDir is the fixture root mechanic 5's transport.Mock serves,
// shared with internal/transport's own tests.
const fixturesDir = "../transport/testdata/fixtures"

// fakeClock is the AfterFunc double every test in this file drives instead
// of a real timer. Each call to after arms a new one-shot channel, buffered
// so Batch's internal goroutine never blocks arming a timer the test has no
// further interest in, and records it in arrivals so the test can wait for
// it and then fire it whenever the mechanic under test calls for a tick.
type fakeClock struct {
	arrivals chan chan time.Time
}

// newFakeClock returns a fakeClock ready to hand to Batch as its AfterFunc.
func newFakeClock() *fakeClock {
	// Buffered generously: no test in this file arms more timers than this
	// before it is done with the clock, so after never blocks.
	return &fakeClock{arrivals: make(chan chan time.Time, 64)}
}

// after implements AfterFunc.
func (c *fakeClock) after(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.arrivals <- ch
	return ch
}

// nextTimer blocks until Batch has armed a timer via after, and returns the
// channel that timer will fire on. t.Fatal if none arrives within
// recvTimeout.
func (c *fakeClock) nextTimer(t *testing.T) chan time.Time {
	t.Helper()
	select {
	case ch := <-c.arrivals:
		return ch
	case <-time.After(recvTimeout):
		t.Fatal("timed out waiting for Batch to arm a timer")
		return nil
	}
}

// send delivers line on in, bounded by recvTimeout so a Batch that has
// stopped reading in fails the test instead of hanging it.
func send(t *testing.T, in chan<- string, line string) {
	t.Helper()
	select {
	case in <- line:
	case <-time.After(recvTimeout):
		t.Fatalf("timed out sending %q on in", line)
	}
}

// recvBatch waits for the next batch on out, bounded by recvTimeout.
func recvBatch(t *testing.T, out <-chan []string) []string {
	t.Helper()
	select {
	case batch, ok := <-out:
		if !ok {
			t.Fatal("out closed while a batch was still expected")
		}
		return batch
	case <-time.After(recvTimeout):
		t.Fatal("timed out waiting for a batch on out")
		return nil
	}
}

// recvClosed waits for out to close, bounded by recvTimeout. It fails the
// test if a batch arrives instead.
func recvClosed(t *testing.T, out <-chan []string) {
	t.Helper()
	select {
	case batch, ok := <-out:
		if ok {
			t.Fatalf("out delivered %v, want it closed", batch)
		}
	case <-time.After(recvTimeout):
		t.Fatal("timed out waiting for out to close")
	}
}

// runBatch starts Batch in its own goroutine and returns a channel that
// receives its return value once Batch returns, so a test can assert on it
// without blocking the goroutine that is driving in and out.
func runBatch(ctx context.Context, in <-chan string, out chan<- []string, after AfterFunc) <-chan error {
	done := make(chan error, 1)
	go func() { done <- Batch(ctx, in, out, after) }()
	return done
}

// Mechanic 1.
func TestBatchFlushesSlowLinesOnTheTimer(t *testing.T) {
	t.Run("lines arriving slowly are flushed on the timer", func(t *testing.T) {
		in := make(chan string)
		out := make(chan []string)
		clock := newFakeClock()

		done := runBatch(context.Background(), in, out, clock.after)

		tick := clock.nextTimer(t)

		send(t, in, "one line, well inside the window")

		// Fire the tick. Nothing else pushed Batch over BatchSize, so this
		// is the only thing that can produce a flush.
		tick <- time.Now()

		got := recvBatch(t, out)
		want := []string{"one line, well inside the window"}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("batch mismatch (-want +got):\n%s", diff)
		}

		close(in)
		recvClosed(t, out)

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Batch() error = %v, want nil", err)
			}
		case <-time.After(recvTimeout):
			t.Fatal("timed out waiting for Batch to return")
		}
	})
}

// Mechanic 2.
func TestBatchFlushesFastLinesAtTheCountThreshold(t *testing.T) {
	t.Run("lines arriving fast are flushed at the 200 line threshold", func(t *testing.T) {
		const total = 1000 // an exact multiple of BatchSize, so the run
		// ends with nothing left over to flush.

		in := make(chan string)
		out := make(chan []string)
		clock := newFakeClock() // its ticks are never fired: every flush in
		// this test must come from the count threshold, never the timer.

		done := runBatch(context.Background(), in, out, clock.after)

		want := make([]string, total)
		for i := 0; i < total; i++ {
			want[i] = fmt.Sprintf("line-%04d", i)
		}

		go func() {
			for _, line := range want {
				send(t, in, line)
			}
			close(in)
		}()

		var got []string
		batches := 0
		for {
			select {
			case batch, ok := <-out:
				if !ok {
					goto drained
				}
				batches++
				if len(batch) != BatchSize {
					t.Errorf("batch %d has %d lines, want exactly BatchSize (%d)", batches, len(batch), BatchSize)
				}
				got = append(got, batch...)
			case <-time.After(recvTimeout):
				t.Fatal("timed out waiting for out to deliver or close")
			}
		}
	drained:

		wantBatches := total / BatchSize
		if batches != wantBatches {
			t.Errorf("got %d batches, want %d (message count must track elapsed volume via BatchSize, not 1 and not %d)", batches, wantBatches, total)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("flattened batch content mismatch (-want +got):\n%s", diff)
		}

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Batch() error = %v, want nil", err)
			}
		case <-time.After(recvTimeout):
			t.Fatal("timed out waiting for Batch to return")
		}
	})
}

// Mechanic 3.
func TestRingBufferDiscardsOldest(t *testing.T) {
	t.Run("the ring buffer discards oldest lines and never exceeds its cap", func(t *testing.T) {
		const overflow = 37 // pushed past RingCap, so exactly this many of
		// the oldest lines must be the ones gone.

		rb := NewRingBuffer(RingCap)
		for i := 0; i < RingCap+overflow; i++ {
			rb.Push(fmt.Sprintf("line-%05d", i))
		}

		if got := rb.Len(); got != RingCap {
			t.Errorf("Len() = %d, want RingCap (%d)", got, RingCap)
		}

		got := rb.Lines()
		if len(got) != RingCap {
			t.Fatalf("Lines() returned %d lines, want RingCap (%d)", len(got), RingCap)
		}

		want := make([]string, RingCap)
		for i := range want {
			want[i] = fmt.Sprintf("line-%05d", i+overflow)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Lines() mismatch (-want +got):\n%s", diff)
		}
	})
}

// Mechanic 4.
func TestBatchClosesOutWhenSourceCloses(t *testing.T) {
	t.Run("closing the source closes the output channel", func(t *testing.T) {
		in := make(chan string)
		out := make(chan []string)
		clock := newFakeClock()

		done := runBatch(context.Background(), in, out, clock.after)

		// Let Batch reach its steady state (timer armed, waiting on in)
		// before closing in, so this exercises the same running producer
		// mechanics 1 and 2 do, not a corner case where in closes before
		// Batch has done anything at all.
		clock.nextTimer(t)

		close(in)

		recvClosed(t, out)

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Batch() error = %v, want nil", err)
			}
		case <-time.After(recvTimeout):
			t.Fatal("timed out waiting for Batch to return")
		}
	})
}

// Mechanic 5.
func TestFollowRefusedWithoutCaps(t *testing.T) {
	t.Run("follow is refused when Caps().Follow is false", func(t *testing.T) {
		tr := transport.NewMock(fixturesDir, transport.WithCaps(transport.Caps{
			Follow: false,
		}))
		defer func() { _ = tr.Close() }()

		const path = "/var/log/syslog"

		ch, run, err := Follow(context.Background(), tr, path, time.After)

		if !errors.Is(err, transport.ErrNotSupported) {
			t.Fatalf("Follow() error = %v, want errors.Is(_, transport.ErrNotSupported)", err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("Follow() error = %q, want it to name %q", err.Error(), path)
		}
		if ch != nil {
			t.Errorf("Follow() channel = %v, want nil on refusal", ch)
		}
		if run != nil {
			t.Error("Follow() run func is non-nil, want nil on refusal")
		}
	})
}
