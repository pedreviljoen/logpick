package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pedreviljoen/logpick/internal/config"
)

// New builds the Transport backend a profile calls for, on host. It is the
// single place that maps profile.Persistent onto a concrete backend, so
// every call site - the TUI's newTransport factory and the scan command
// alike - agrees on which backend a profile gets rather than each one
// re-deciding (and, historically, always picking NewCommand and quietly
// ignoring the flag).
//
// profile.Persistent true selects NewPersistent, for wrappers that cannot
// take a remote command as an argv and instead expect an interactive shell
// they can write commands into (DESIGN.md 7.3). Amazon's ec2-ssh is the
// canonical example: its CLI is `ec2-ssh [options] <host>` with no slot for
// a command, so an exec template like ["ec2-ssh", "{host}", "{cmd}"] makes
// it parse the substituted find command as a second host address and fail
// with HostInfoUndefinedHosttypeException. A persistent profile spawns
// `ec2-ssh <host>` once and feeds commands to the shell it lands on.
//
// profile.Persistent false (the default) selects NewCommand, which spawns
// one process per command with {cmd} substituted into the argv - correct
// for ssh and any wrapper that forwards a trailing command, e.g.
// ["ssh", "{host}", "--", "{cmd}"].
func New(host string, profile config.Profile) Transport {
	if profile.Persistent {
		return NewPersistent(host, profile)
	}
	return NewCommand(host, profile)
}

// Transport runs a command on a remote host and streams its stdout back.
//
// That single capability is the whole contract. Discovery is find, preview is
// tail -n, follow is tail -f and fetch is cat, so a backend that implements
// Exec gets every feature for free.
type Transport interface {
	// Exec runs cmd on the remote host. Cancelling ctx kills the process.
	Exec(ctx context.Context, cmd string) (*Process, error)

	// Fetch copies a remote file locally. Backends without native copy
	// embed FallbackFetcher, which streams cat over Exec.
	Fetch(ctx context.Context, remote, local string, prog chan<- Progress) (int64, error)

	// Caps reports what this backend can do. Never blocks.
	Caps() Caps

	// Check is a cheap liveness probe used before long operations.
	Check(ctx context.Context) error

	// Close releases any persistent resources.
	Close() error
}

// Process is a command in flight on the remote host. The caller reads Stdout
// to completion and then calls Wait.
type Process struct {
	Stdout io.ReadCloser
	Stderr io.ReadCloser
	Wait   func() error // returns *ExitError on non-zero exit
}

// Caps is a backend's honest account of what it can do. Callers check it
// rather than attempting an operation and catching the failure.
type Caps struct {
	NativeCopy     bool
	Follow         bool  // can hold a long-lived stream
	ConcurrentExec bool  // more than one command in flight
	BinarySafe     bool  // stdout is not passing through a PTY
	GNUFind        *bool // nil until probed
}

// Progress is one update on an in-flight transfer. Bytes is the running count
// of bytes transferred. Total is the expected size, or zero when the sender
// has no way to know it.
type Progress struct {
	Bytes int64
	Total int64
}

// Conditions callers branch on. Wrap them with context and match them with
// errors.Is, never by comparing error strings.
var (
	// ErrAuthRequired means the transport could not authenticate. The caller
	// surfaces the profile's auth_hint rather than a stack trace.
	ErrAuthRequired = errors.New("authentication required")

	// ErrConnLost means an established session went away mid-operation.
	ErrConnLost = errors.New("connection lost")

	// ErrNotSupported means the backend's Caps do not cover the request.
	ErrNotSupported = errors.New("capability not supported")
)

// ExitError reports a remote command that exited non-zero. Stderr holds what
// the command wrote to its standard error, so the caller can show it.
type ExitError struct {
	Code   int
	Stderr string
}

// Error implements the error interface.
func (e *ExitError) Error() string {
	if e.Stderr != "" {
		return fmt.Sprintf("exit %d: %s", e.Code, strings.TrimSpace(e.Stderr))
	}
	return fmt.Sprintf("exit %d", e.Code)
}

// FallbackFetcher gives a backend with no native copy path a Fetch method by
// streaming cat over Exec. Go has no default interface methods, so backends
// embed this struct and override Fetch only when they can do better.
type FallbackFetcher struct{ T Transport }

// Fetch copies remote to local by running cat over T.Exec.
//
// The stream is staged through local+".part" and renamed onto local once the
// copy has completed, so a killed transfer never leaves a truncated file
// looking complete. If the transfer fails for any reason, including
// cancellation of ctx, the part file is removed and nothing is left at local.
//
// It returns the number of bytes written. Errors are wrapped with the remote
// path.
//
// When prog is non-nil, Fetch sends a Progress as the copy advances and the
// last message reports the completed size, so its Bytes equals the returned
// count. Total is zero on every message because the fallback path has no way
// to learn the file size, which the caller knows from discovery instead. A nil
// prog is legal and means the caller does not want updates.
func (f FallbackFetcher) Fetch(ctx context.Context, remote, local string,
	prog chan<- Progress) (int64, error) {
	proc, err := f.T.Exec(ctx, "cat "+remote)
	if err != nil {
		return 0, fmt.Errorf("fetching %s: %w", remote, err)
	}

	part := local + ".part"
	out, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // part is derived from the caller-chosen local destination, not untrusted input.
	if err != nil {
		_ = closeProcessStreams(proc)
		return 0, fmt.Errorf("fetching %s: %w", remote, err)
	}

	written, copyErr := copyProgress(ctx, out, proc.Stdout, prog)

	closeErr := out.Close()
	streamErr := closeProcessStreams(proc)
	waitErr := proc.Wait()

	if err := firstNonNil(copyErr, closeErr, streamErr, waitErr); err != nil {
		_ = os.Remove(part)
		return 0, fmt.Errorf("fetching %s: %w", remote, err)
	}

	if err := os.Rename(part, local); err != nil {
		_ = os.Remove(part)
		return 0, fmt.Errorf("fetching %s: %w", remote, err)
	}

	return written, nil
}

// copyProgress streams src into dst, reporting the running byte count on prog
// as it goes. It watches ctx throughout the copy, not just before it starts,
// so a cancellation mid-transfer stops the loop rather than running to
// completion. Every send on prog is guarded by a select on ctx.Done() so a
// cancelled transfer, or a channel nobody drains, cannot block forever; a nil
// prog is simply never sent on.
func copyProgress(ctx context.Context, dst io.Writer, src io.Reader, prog chan<- Progress) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}

		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			total += int64(nw)
			if werr != nil {
				return total, werr
			}
			if nw != nr {
				return total, io.ErrShortWrite
			}
			if prog != nil {
				select {
				case prog <- Progress{Bytes: total}:
				case <-ctx.Done():
					return total, ctx.Err()
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF { //nolint:errorlint // io.EOF is a documented sentinel returned exactly, never wrapped.
				return total, nil
			}
			return total, rerr
		}
	}
}

// closeProcessStreams closes both halves of a Process's output, joining any
// errors so neither Close is silently dropped.
func closeProcessStreams(p *Process) error {
	return errors.Join(p.Stdout.Close(), p.Stderr.Close())
}

// firstNonNil returns the first non-nil error in errs, in priority order, or
// nil if all are nil.
func firstNonNil(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
