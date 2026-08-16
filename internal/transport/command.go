package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/pedreviljoen/logpick/internal/config"
)

// commandWaitDelay bounds how long Wait spends, after cancellation or the
// process's own exit, waiting on I/O to drain before forcibly closing pipes
// (see exec.Cmd.WaitDelay). It exists so a stuck grandchild holding a pipe
// open cannot hang Wait forever even if the process-group kill somehow fails
// to reach it.
const commandWaitDelay = 5 * time.Second

// Command is the default Transport backend (DESIGN.md 7.2). It substitutes
// {host}, {cmd}, {remote} and {local} into the profile's Exec and Copy argv
// templates via Substitute and runs the resulting slice directly with
// exec.CommandContext.
//
// Command never builds a shell string and hands it to a local sh -c
// (AGENTS.md invariant 1). The []string Substitute returns is the argv
// exec.CommandContext receives; the only shell involved is whatever the
// profile's own template names as argv[0] (typically ssh or a corporate
// wrapper), and that shell runs on the far side of that command, not one
// this package constructs.
//
// Command embeds FallbackFetcher so it has a working Fetch (cat streamed
// over Exec) with no further code. Fetch below only takes the native-copy
// path when profile.Copy is non-empty; with no Copy template it defers to
// the embedded FallbackFetcher.Fetch entirely.
type Command struct {
	FallbackFetcher

	// host fills the {host} placeholder in profile.Exec and profile.Copy.
	// It is not part of config.Profile (DESIGN.md 6.2 has no Host field:
	// a Profile describes a class of hosts, not one host), so NewCommand
	// takes it as a separate parameter and Command stores it here.
	host string

	// profile is the resolved profile this Command executes against.
	profile config.Profile
}

var _ Transport = (*Command)(nil)

// NewCommand builds a Command backend that runs commands on host using
// profile's argv templates.
//
// NewCommand takes host as an explicit second parameter rather than
// expecting it inside profile, since config.Profile (DESIGN.md 6.2) has no
// Host field: a Profile is shared across every host a match rule or
// [host.<name>] entry assigns it to, and host is supplied by whichever of
// those resolved to this profile (DESIGN.md 6.4). This extends the
// one-argument form named in AGENTS.md's T04 contract line to two
// arguments; see the T04 handover notes for why.
//
// The returned *Command embeds a FallbackFetcher whose T is wired to that
// same *Command, not a copy: NewCommand constructs the Command first, takes
// its address, and only then sets FallbackFetcher.T to that pointer, so
// FallbackFetcher.Fetch's calls to T.Exec are calls to this Command's own
// Exec.
func NewCommand(host string, profile config.Profile) *Command {
	c := &Command{host: host, profile: profile}
	c.FallbackFetcher = FallbackFetcher{T: c}
	return c
}

// newCmd builds an *exec.Cmd for argv, wired for the process-group kill
// AGENTS.md section 9 requires: the child (and anything it backgrounds into
// the same group, such as a remote tail -f or, in tests, a backgrounded
// local child) is started in its own process group via
// SysProcAttr.Setpgid, and cancellation signals the negative PID so the
// whole group dies rather than just the direct child that
// exec.CommandContext's default Cancel would kill.
func newCmd(ctx context.Context, argv []string) *exec.Cmd {
	osCmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	osCmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	osCmd.WaitDelay = commandWaitDelay
	osCmd.Cancel = func() error {
		if osCmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-osCmd.Process.Pid, syscall.SIGKILL); err != nil {
			return fmt.Errorf("killing process group %d: %w", osCmd.Process.Pid, err)
		}
		return nil
	}
	return osCmd
}

// Exec runs cmd on host by substituting {host} and {cmd} into profile.Exec
// and running the resulting argv with exec.CommandContext. The substituted
// argv is passed to exec.CommandContext as-is; nothing here re-joins it
// into a string or reaches for a local shell (AGENTS.md invariant 1).
//
// The child is started in its own process group (SysProcAttr.Setpgid) and
// cancelling ctx must terminate that whole group by signalling the negative
// PID, not just the direct child. exec.CommandContext's own Cancel hook
// kills only the direct child by default, which is not enough: a remote
// tail -f (or, in a template that shells out locally the way tests do, a
// backgrounded local child) would outlive cancellation otherwise (AGENTS.md
// section 9, DESIGN.md 7.2).
//
// Wait's returned error is *ExitError for any non-zero exit, carrying the
// exit code and the captured stderr. When the code is 255, that same error
// also satisfies errors.Is(err, ErrAuthRequired): ExitError.Unwrap, defined
// in this file since transport.go is not modified by this task, returns
// ErrAuthRequired for exactly that code (DESIGN.md 7.5) and nil otherwise,
// so errors.As(err, &exitErr) and errors.Is(err, ErrAuthRequired) both
// succeed off the one value Wait returns.
//
// Stderr is captured by a stderrCapture, an in-memory sink assigned directly
// as osCmd.Stderr: os/exec spawns its own internal goroutine to copy the
// child's stderr into that sink and Wait (below, via osCmd.Wait) blocks
// until that copy finishes, so the capture is complete before Wait returns
// regardless of whether the caller ever reads Process.Stderr. Writes into
// the sink never block (it is a mutex-guarded buffer, not a pipe), so there
// is nothing for a caller who never drains Process.Stderr to deadlock on:
// the trap this doc comment calls out is exactly the one an io.Pipe-backed
// Process.Stderr would create for a caller who reads Stdout and Wait but
// never Stderr, as the non-zero-exit tests in command_test.go do.
func (c *Command) Exec(ctx context.Context, cmd string) (*Process, error) {
	argv, err := Substitute(c.profile.Exec, map[string]string{"host": c.host, "cmd": cmd})
	if err != nil {
		return nil, fmt.Errorf("exec on %s: %w", c.host, err)
	}

	osCmd := newCmd(ctx, argv)

	stdout, err := osCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("exec on %s: %w", c.host, err)
	}

	stderr := newStderrCapture()
	osCmd.Stderr = stderr

	if err := osCmd.Start(); err != nil {
		return nil, fmt.Errorf("exec on %s: %w", c.host, err)
	}

	proc := &Process{
		Stdout: stdout,
		Stderr: stderr.reader(),
	}
	proc.Wait = func() error {
		waitErr := osCmd.Wait()
		stderr.markDone()

		if waitErr == nil {
			return nil
		}

		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return &ExitError{Code: exitErr.ExitCode(), Stderr: stderr.String()}
		}
		return fmt.Errorf("exec on %s: %w", c.host, waitErr)
	}

	return proc, nil
}

// Fetch copies remote to local. When profile.Copy is non-empty, Fetch
// substitutes {host}, {remote} and {local} into it and runs the resulting
// argv directly, the same way Exec does, giving a true native copy path
// instead of streaming through cat. When profile.Copy is empty, Fetch
// defers to the embedded FallbackFetcher, so Command effectively overrides
// Fetch only when the profile actually defines a copy template.
func (c *Command) Fetch(ctx context.Context, remote, local string, prog chan<- Progress) (int64, error) {
	if len(c.profile.Copy) == 0 {
		return c.FallbackFetcher.Fetch(ctx, remote, local, prog)
	}

	argv, err := Substitute(c.profile.Copy, map[string]string{"host": c.host, "remote": remote, "local": local})
	if err != nil {
		return 0, fmt.Errorf("fetching %s: %w", remote, err)
	}

	osCmd := newCmd(ctx, argv)

	stderr := newStderrCapture()
	osCmd.Stderr = stderr

	runErr := osCmd.Run()
	stderr.markDone()

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return 0, fmt.Errorf("fetching %s: %w", remote, &ExitError{Code: exitErr.ExitCode(), Stderr: stderr.String()})
		}
		return 0, fmt.Errorf("fetching %s: %w", remote, runErr)
	}

	info, err := os.Stat(local)
	if err != nil {
		return 0, fmt.Errorf("fetching %s: %w", remote, err)
	}
	written := info.Size()

	if prog != nil {
		select {
		case prog <- Progress{Bytes: written}:
		case <-ctx.Done():
			return written, ctx.Err()
		}
	}

	return written, nil
}

// Caps reports NativeCopy true only when profile.Copy is non-empty, per
// DESIGN.md 7.2's "everything else true" for this backend. GNUFind stays
// nil: Command never probes for it itself, that is a caller-driven check
// layered on top.
func (c *Command) Caps() Caps {
	return Caps{
		NativeCopy:     len(c.profile.Copy) > 0,
		Follow:         true,
		ConcurrentExec: true,
		BinarySafe:     true,
	}
}

// Check is a cheap liveness probe run before a long operation, per the
// Transport contract in transport.go. It runs a trivial command over Exec
// and reports whatever error that produces, including ErrAuthRequired for
// an exit-255 authentication failure.
func (c *Command) Check(ctx context.Context) error {
	proc, err := c.Exec(ctx, "true")
	if err != nil {
		return fmt.Errorf("check %s: %w", c.host, err)
	}

	_, copyErr := io.Copy(io.Discard, proc.Stdout)
	closeErr := closeProcessStreams(proc)
	waitErr := proc.Wait()

	if err := firstNonNil(copyErr, closeErr, waitErr); err != nil {
		return fmt.Errorf("check %s: %w", c.host, err)
	}
	return nil
}

// Close releases any resources Command holds open between calls. Command
// spawns a fresh process per Exec or Fetch call and keeps no persistent
// session, so there is nothing to release here.
func (c *Command) Close() error {
	return nil
}

// Unwrap lets errors.Is(err, ErrAuthRequired) succeed for an *ExitError
// whose Code is 255 (DESIGN.md 7.5), without transport.go — committed, not
// modified by this task — needing to know about that sentinel. A method may
// be attached to a type from any file in its package, so this lives here
// instead, next to the one place in the package that constructs an
// *ExitError this way.
func (e *ExitError) Unwrap() error {
	if e.Code == 255 {
		return ErrAuthRequired
	}
	return nil
}

// stderrCapture is a mutex-guarded, growable buffer used as an *exec.Cmd's
// Stderr sink. Writes never block, so nothing os/exec's internal
// stderr-copying goroutine does can ever deadlock on a caller who does not
// read Process.Stderr: the copy always completes as soon as the child (and
// anything it backgrounded into the same process group) closes its stderr
// fd, independent of whether reader ever consumes the accumulated bytes.
//
// reader hands out a *stderrReader, a separate cursor over the same buffer,
// so Process.Stderr can still be read live by a caller who wants to; String
// reads the whole buffer for building an *ExitError once the process has
// exited, without disturbing any reader's cursor.
type stderrCapture struct {
	mu   sync.Mutex
	cond *sync.Cond
	buf  bytes.Buffer
	done bool
}

func newStderrCapture() *stderrCapture {
	c := &stderrCapture{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// Write implements io.Writer so a *stderrCapture can be assigned directly to
// exec.Cmd.Stderr.
func (c *stderrCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	n, err := c.buf.Write(p)
	c.mu.Unlock()
	c.cond.Broadcast()
	return n, err
}

// markDone records that no further writes will occur (the owning Cmd.Wait
// has returned) and wakes any reader blocked waiting for more data.
func (c *stderrCapture) markDone() {
	c.mu.Lock()
	c.done = true
	c.mu.Unlock()
	c.cond.Broadcast()
}

// String returns everything captured so far, without consuming it.
func (c *stderrCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *stderrCapture) reader() io.ReadCloser {
	return &stderrReader{c: c}
}

// stderrReader is a per-caller read cursor into a stderrCapture's buffer. It
// blocks for new data (via stderrCapture.cond) rather than returning a
// premature EOF, and only reports io.EOF once the capture is marked done and
// every byte written has been delivered.
type stderrReader struct {
	c      *stderrCapture
	offset int
}

func (r *stderrReader) Read(p []byte) (int, error) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	for {
		data := r.c.buf.Bytes()
		if r.offset < len(data) {
			n := copy(p, data[r.offset:])
			r.offset += n
			return n, nil
		}
		if r.c.done {
			return 0, io.EOF
		}
		r.c.cond.Wait()
	}
}

// Close is a no-op: stderrReader holds no OS resources of its own, only a
// cursor into the shared in-memory buffer.
func (r *stderrReader) Close() error {
	return nil
}
