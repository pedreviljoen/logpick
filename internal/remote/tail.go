package remote

// tail.go is follow mode's contract (T20): a batching producer that turns a
// raw stream of remote lines into batched updates the ui layer drains with
// ui.WaitForLines (DESIGN.md 9.3), a fixed-capacity buffer of the lines a
// follow has produced so far (DESIGN.md 9.5), and the entry point that
// starts a follow, refusing one the backend cannot sustain (AGENTS.md
// invariant 7). DESIGN.md section 5 assigns tail.go both "tail -n and
// tail -f"; this task's contract covers the follow half.
//
// Dependency direction is strictly downward (DESIGN.md section 5): ui
// imports remote, remote imports transport, and nothing below ui knows a
// terminal exists or imports bubbletea. AGENTS.md invariant 6 requires
// every goroutine the app spawns to be cancellable via context and to
// recover from panics, via ui.SafeGo (internal/ui/safego.go) — but this
// package cannot import ui to call it without inverting that direction.
//
// The resolution: Batch and the run function Follow returns are both
// shaped exactly like the func(context.Context) error that ui.SafeGo
// already takes as the fn it runs. Neither spawns a goroutine or recovers
// a panic itself; each blocks its caller until it returns. A caller in the
// ui layer, which imports both packages, is the one that starts the
// goroutine, by passing the func straight to ui.SafeGo:
//
//	out := make(chan []string)
//	ch, run, err := remote.Follow(ctx, t, path, time.After)
//	if err != nil { ... }
//	ui.SafeGo(ctx, errc, run)
//	cmd := ui.WaitForLines(ch)
//
// That gives the goroutine ui.SafeGo's cancellation and panic recovery
// without remote importing ui, or ui reaching down to duplicate SafeGo's
// recovery logic here.

import (
	"bufio"
	"context"
	"fmt"
	"time"

	"github.com/pedreviljoen/logpick/internal/transport"
)

// scanInitialBuf and scanMaxBuf size the bufio.Scanner Follow's reader uses
// on a process's stdout. The default 64KB max token size (bufio.Scanner's
// stdlib default) is too small for a single long log line, so Follow raises
// it to 1MB (AGENTS.md's "no default Scanner buffer" trap).
const (
	scanInitialBuf = 64 * 1024
	scanMaxBuf     = 1024 * 1024
)

// BatchWindow is the maximum time Batch accumulates lines before flushing
// them (DESIGN.md 9.3).
const BatchWindow = 50 * time.Millisecond

// BatchSize is the maximum number of lines Batch accumulates before
// flushing them, regardless of elapsed time (DESIGN.md 9.3).
const BatchSize = 200

// RingCap is the number of most recent lines a RingBuffer holds before it
// starts discarding the oldest (DESIGN.md 9.5).
const RingCap = 5000

// AfterFunc has the shape of time.After: given a duration it returns a
// channel that receives the current time once the duration has elapsed.
//
// Batch takes one instead of calling time.After directly so a test can
// substitute a channel it controls and drive the batching timer itself,
// with no sleep and no real timer (AGENTS.md section 5, "no sleeps ...
// injected clocks"). Production callers, including Follow, pass time.After.
type AfterFunc func(d time.Duration) <-chan time.Time

// Batch reads lines off in and writes batches of them to out, flushing the
// batch it is accumulating whenever BatchWindow has elapsed since that
// batch began or the batch reaches BatchSize lines, whichever happens
// first (DESIGN.md 9.3):
//
//   - Lines arriving slower than the timer are flushed on the tick
//     (mechanic 1), one short batch per tick, so a slow trickle still shows
//     up promptly instead of waiting on a line count that may never come.
//   - Lines arriving faster than the timer are flushed the instant a batch
//     reaches BatchSize (mechanic 2), so the number of flushes over a run
//     stays proportional to elapsed time, not to line volume: 1,000 lines
//     arriving inside a single tick still produce five flushes of 200
//     lines each, never one flush of 1,000 and never 1,000 flushes of one.
//   - A tick that fires over an empty batch — nothing arrived since the
//     last flush — sends nothing. Batch never puts an empty slice on out.
//
// after is called once at the start of every new batch, including
// immediately on entry, to arm that batch's timer; each channel it returns
// is read from at most once, for that one batch only.
//
// Batch flushes any partial batch and returns nil once in is closed
// (mechanic 4), closing out before it returns so a caller draining out with
// ui.WaitForLines sees the channel close and gets a StreamClosedMsg. It
// returns ctx.Err() instead, without a final flush, if ctx is cancelled
// first, still closing out either way. Every send on out is guarded by a
// select on ctx.Done(), so a caller that stops draining out cannot leave
// this call blocked forever (AGENTS.md section 4.1, the buffered-channel-
// plus-select-on-ctx.Done pattern).
//
// Batch does not spawn a goroutine; it blocks its caller until it returns.
// Run it inside one — see this file's package doc comment for how Follow's
// callers do that through ui.SafeGo.
func Batch(ctx context.Context, in <-chan string, out chan<- []string, after AfterFunc) error {
	defer close(out)

	batch := make([]string, 0, BatchSize)
	tick := after(BatchWindow)

	// flush sends batch on out if it holds anything, guarded by ctx.Done()
	// so a caller that has stopped draining out cannot block this forever,
	// then resets batch for the next cycle. An empty batch is left alone:
	// this is what keeps Batch from ever putting an empty slice on out.
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		select {
		case out <- batch:
		case <-ctx.Done():
			return ctx.Err()
		}
		batch = make([]string, 0, BatchSize)
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case line, ok := <-in:
			if !ok {
				// mechanic 4: in closed. Flush whatever partial batch is
				// pending, then return; the deferred close(out) above
				// handles closing the output channel.
				return flush()
			}
			batch = append(batch, line)
			if len(batch) >= BatchSize {
				// mechanic 2: count threshold reached before the timer
				// fired. Flush now and arm a fresh timer for the next
				// cycle; the old tick channel is abandoned unread, as the
				// doc comment allows.
				if err := flush(); err != nil {
					return err
				}
				tick = after(BatchWindow)
			}

		case <-tick:
			// mechanic 1: the window elapsed. Flush whatever arrived (or
			// nothing, per flush's own empty-batch guard) and arm the next
			// cycle's timer.
			if err := flush(); err != nil {
				return err
			}
			tick = after(BatchWindow)
		}
	}
}

// RingBuffer holds the most recent lines a follow has produced, discarding
// the single oldest line held whenever a Push would grow it past its
// capacity (DESIGN.md 9.5, mechanic 3). The zero value is not useful: use
// NewRingBuffer.
//
// RingBuffer is not safe for concurrent use. The bubbletea Update loop is
// single-threaded by construction (DESIGN.md section 9.2), and a
// RingBuffer is meant to be owned by exactly one screen model and touched
// only from there, typically once per LinesMsg.
type RingBuffer struct {
	// buf is a fixed-size backing array of length cap. head is the index of
	// the oldest line currently held; n is how many slots from head
	// (wrapping) are in use. This avoids any slice-reslicing trick that
	// would otherwise pin the whole original backing array in memory for
	// the life of a long-running follow.
	buf  []string
	head int
	n    int
	cap  int
}

// NewRingBuffer returns an empty RingBuffer that holds at most cap lines.
// Production callers pass RingCap; a test may pass a smaller number to keep
// its assertions easy to read without weakening what it proves.
func NewRingBuffer(cap int) *RingBuffer {
	return &RingBuffer{buf: make([]string, cap), cap: cap}
}

// Push appends line, first discarding the single oldest line held if the
// buffer is already at its capacity. The buffer never exceeds its capacity.
func (r *RingBuffer) Push(line string) {
	if r.cap == 0 {
		return
	}
	if r.n < r.cap {
		r.buf[(r.head+r.n)%r.cap] = line
		r.n++
		return
	}
	r.buf[r.head] = line
	r.head = (r.head + 1) % r.cap
}

// Lines returns the buffer's current contents, oldest first. The returned
// slice is a copy: mutating it never affects the buffer.
func (r *RingBuffer) Lines() []string {
	out := make([]string, r.n)
	for i := 0; i < r.n; i++ {
		out[i] = r.buf[(r.head+i)%r.cap]
	}
	return out
}

// Len returns the number of lines currently held, at most the buffer's
// capacity.
func (r *RingBuffer) Len() int {
	return r.n
}

// Follow starts `tail -f path` on t and returns a channel of batched line
// updates ready for ui.WaitForLines, alongside a run function shaped for
// ui.SafeGo — see this file's package doc comment — that execs the command,
// scans its stdout line by line feeding Batch, and returns when the
// process's stdout closes or ctx is cancelled.
//
// Follow returns an error immediately, before execing anything, if
// t.Caps().Follow is false (AGENTS.md invariant 7, mechanic 5):
// transport.Persistent (T19) reports Follow: false deliberately, so this is
// a live, reachable path, not dead code. On that refusal ch and run are
// both nil, and the error wraps transport.ErrNotSupported — match it with
// errors.Is — with path in its message as the identifying detail, checked
// with strings.Contains.
func Follow(ctx context.Context, t transport.Transport, path string, after AfterFunc) (ch <-chan []string, run func(context.Context) error, err error) {
	if !t.Caps().Follow {
		return nil, nil, fmt.Errorf("follow %s: %w", path, transport.ErrNotSupported)
	}

	cmd := "tail -f " + Quote(path)
	out := make(chan []string)

	run = func(runCtx context.Context) error {
		proc, err := t.Exec(runCtx, cmd)
		if err != nil {
			close(out)
			return fmt.Errorf("following %s: %w", path, err)
		}

		lines := make(chan string)
		readDone := make(chan error, 1)

		go func() {
			defer close(lines)
			scanner := bufio.NewScanner(proc.Stdout)
			scanner.Buffer(make([]byte, scanInitialBuf), scanMaxBuf)
			for scanner.Scan() {
				select {
				case lines <- scanner.Text():
				case <-runCtx.Done():
					readDone <- runCtx.Err()
					return
				}
			}
			readDone <- scanner.Err()
		}()

		batchErr := Batch(runCtx, lines, out, after)

		// Close the process's streams unconditionally, whether Batch
		// stopped because in closed or because runCtx was cancelled. This
		// is also what reliably unblocks the reader goroutine above if it
		// is still parked in a Read call the transport's own cancellation
		// has not yet interrupted: closing the pipe out from under a
		// concurrent Read makes that Read return, rather than leaking the
		// goroutine (AGENTS.md 4.1's buffered-channel-plus-select pattern
		// covers the send side; this covers the read side the same way).
		stdoutErr := proc.Stdout.Close()
		stderrErr := proc.Stderr.Close()
		waitErr := proc.Wait()
		readErr := <-readDone

		if err := firstErr(batchErr, readErr, stdoutErr, stderrErr, waitErr); err != nil {
			return fmt.Errorf("following %s: %w", path, err)
		}
		return nil
	}

	return out, run, nil
}

// firstErr returns the first non-nil error in errs, in priority order, or
// nil if all are nil.
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
