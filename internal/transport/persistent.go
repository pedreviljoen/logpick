package transport

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/pedreviljoen/logpick/internal/config"
)

// Persistent is the backend for wrappers that cannot multiplex a fresh
// process per command cheaply (DESIGN.md 7.3). Instead of spawning one
// process per Exec the way Command does, it spawns the profile's Exec
// template exactly once, with {cmd} substituted to "exec bash --noprofile
// --norc", and holds that shell open across calls, writing each command's
// text to its stdin and reading its stdout back up to a sentinel that
// marks the command's end.
//
// # Session setup
//
// Once the wrapped shell is running, the first bytes written to its stdin
// are:
//
//	exec bash --noprofile --norc
//	stty -echo 2>/dev/null || true
//	export PS1= PS2= LC_ALL=C
//
// The first line is the {cmd} the outer profile.Exec template spawned
// (see above); the remaining two lines are written to that shell's stdin
// once it is running, disabling local echo where the wrapper provides a
// pty and clearing prompt variables so nothing but command output and the
// sentinel ever appears on stdout.
//
// # Per-command protocol
//
// Every subsequent command sent is wrapped as:
//
//	{cmd} 2>/tmp/.logpick.$$.err; printf '\x1e%s %d\x1e\n' "$SENTINEL" "$?"
//
// SENTINEL is a fresh random token generated once per session when the
// shell is spawned, so nothing the remote command could ever write to its
// own stdout can forge it. The reader recognises only the full frame
// \x1e<SENTINEL> <exit>\x1e as the end of a command's output, never a bare
// 0x1e byte occurring in the command's own output (DESIGN.md 7.3) — see
// sentinelSplitter below, which is what actually implements that framing
// and is written to be tested directly, byte at a time, with no process
// involved.
//
// stderr is not part of the interactive stdout stream Exec reads: one
// shell gives one interleaved combined stream, so the per-command line
// above redirects stderr to a temp file on the remote host instead, read
// back on demand once the command completes. Creating that one temp file
// is the single documented exception to AGENTS.md invariant 2 ("never
// write to a remote host"); DESIGN.md 7.3 names it explicitly for that
// reason, and nothing else in this backend, or anywhere else in the
// codebase, writes to the remote.
//
// Only one command is ever in flight on a given session: Exec serialises
// internally so a second concurrent call waits for the first to finish
// reading its sentinel before proceeding, which is what makes
// Caps().ConcurrentExec false. Because the whole protocol is a text
// framing layered over one shared stdout stream, Caps().BinarySafe is also
// false; Fetch (see its doc comment) avoids that limitation entirely
// rather than working around it.
//
// A session that has died out from under Exec — the wrapped process
// exited, or its pipes closed unexpectedly — is reported by wrapping
// ErrConnLost, never a bare I/O error, so callers can tell "the remote
// command failed" apart from "the connection is gone and needs
// reconnecting."
//
// Genuinely hacky, as DESIGN.md 7.3 says outright. It exists only for
// profiles with persistent = true.
type Persistent struct {
	FallbackFetcher

	// host fills the {host} placeholder in profile.Exec, exactly as it
	// does for Command. See NewPersistent's doc comment for why
	// NewPersistent takes it as an explicit parameter.
	host string

	// profile is the resolved profile this session runs against.
	profile config.Profile

	// fetcher is a one-shot *Command for the same host and profile,
	// constructed by NewPersistent and used by Fetch instead of the
	// shared interactive session. See Fetch's doc comment for why.
	fetcher *Command

	// mu serialises everything that touches session: opening it, running a
	// command through it and tearing it down. This is what makes
	// Caps().ConcurrentExec false (Exec's doc comment).
	mu sync.Mutex

	// session is the live shell session, or nil if none has been opened
	// yet (NewPersistent does not open one) or the last one was torn down
	// after being lost. Guarded by mu.
	session *persistentSession
}

var _ Transport = (*Persistent)(nil)

// persistentSession is the live state behind one spawned shell: the
// process itself, its stdin for writing commands, its stdout wrapped in a
// bufio.Scanner driven by a sentinelSplitter bound to this session's fresh
// random token, and the splitter itself (so its exitCode field can be read
// once a Scan succeeds).
type persistentSession struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	scanner  *bufio.Scanner
	splitter *sentinelSplitter
}

// NewPersistent builds a Persistent backend that runs commands on host
// through one long-lived interactive shell spawned from profile.Exec,
// using the sentinel protocol documented on the Persistent type above.
//
// NewPersistent takes host as an explicit second parameter for the same
// reason NewCommand does (command.go's NewCommand doc comment):
// config.Profile (DESIGN.md 6.2) has no Host field, since a Profile is
// shared across every host a match rule or [host.<name>] entry assigns it
// to, so {host} must still be supplied separately to fill profile.Exec's
// template. This widens the NewPersistent(profile config.Profile)
// *Persistent signature AGENTS.md's T19 contract line names, matching
// T04's identical precedent and for the identical reason.
//
// NewPersistent does not spawn the underlying shell itself: no process
// exists, and no I/O has happened, until whichever method first needs a
// live session (Exec or Check) opens one. Close tears down a session if
// one was ever opened, and is always safe to call.
//
// NewPersistent also constructs an unexported one-shot *Command for the
// same host and profile, held in the returned Persistent and used only by
// Fetch, because Persistent's own Caps().BinarySafe is false: the
// interactive session's sentinel protocol is a text framing over a shared
// stdout stream and cannot safely carry arbitrary binary bytes. See
// Fetch's doc comment.
func NewPersistent(host string, profile config.Profile) *Persistent {
	p := &Persistent{host: host, profile: profile}
	p.fetcher = NewCommand(host, profile)
	p.FallbackFetcher = FallbackFetcher{T: p}
	return p
}

// Exec runs cmd on host through the persistent session's shell, per the
// protocol documented on the Persistent type. It opens the session first
// if none is open yet.
//
// The returned Process streams cmd's own stdout as it arrives; the
// sentinel frame that marks the end of that output is recognised and
// stripped by the session's reader (see sentinelSplitter) and never
// appears in Process.Stdout. Process.Stderr is read from the per-command
// temp file the shell redirected stderr into, not from the interactive
// stream, since a single shell only gives one interleaved combined
// stream. Process.Wait returns the *ExitError DESIGN.md 7.5 documents,
// built from the exit code carried in the sentinel frame, once the
// caller has finished reading Process.Stdout; a Wait for exit code 255
// additionally satisfies errors.Is(err, ErrAuthRequired), via the same
// ExitError.Unwrap command.go defines.
//
// Exec serialises internally: only one command is ever in flight on the
// shared session, so a second call blocks until the first has finished
// reading its sentinel rather than racing it on the same stream. This is
// what makes Caps().ConcurrentExec false.
//
// If the session has died — the underlying process exited, or a read or
// write against its pipes failed unexpectedly — Exec returns an error
// wrapping ErrConnLost rather than silently opening a new session, since a
// caller mid a sequence of related commands needs to know the shell state
// was lost rather than have it masked by a fresh one.
func (c *Persistent) Exec(ctx context.Context, cmd string) (*Process, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	sess, err := c.ensureSessionLocked()
	if err != nil {
		return nil, fmt.Errorf("exec on %s: %w", c.host, err)
	}

	out, exitCode, err := c.runLocked(ctx, sess, cmd)
	if err != nil {
		c.destroySessionLocked()
		return nil, fmt.Errorf("exec on %s: %s: %w", c.host, err, ErrConnLost)
	}

	// stderr was redirected by the shell into a per-command temp file on
	// the far side (DESIGN.md 7.3); read it back now, through the same
	// session, since the command has already finished and closed it. The
	// read command's own stderr is sent to /dev/null rather than the
	// shared temp file, so reading it can never truncate the very file
	// it is reading.
	stderrOut, _, serr := c.runRawLocked(ctx, sess, "cat /tmp/.logpick.$$.err 2>/dev/null")
	if serr != nil {
		c.destroySessionLocked()
		return nil, fmt.Errorf("exec on %s: %s: %w", c.host, serr, ErrConnLost)
	}

	stderrStr := string(stderrOut)
	proc := &Process{
		Stdout: io.NopCloser(bytes.NewReader(out)),
		Stderr: io.NopCloser(bytes.NewReader(stderrOut)),
	}
	proc.Wait = func() error {
		if exitCode != 0 {
			return &ExitError{Code: exitCode, Stderr: stderrStr}
		}
		return nil
	}

	return proc, nil
}

// ensureSessionLocked returns the live session, opening one if none exists
// yet. Callers must hold c.mu.
func (c *Persistent) ensureSessionLocked() (*persistentSession, error) {
	if c.session != nil {
		return c.session, nil
	}
	sess, err := c.openSession()
	if err != nil {
		return nil, err
	}
	c.session = sess
	return sess, nil
}

// openSession spawns profile.Exec with {cmd} substituted for "exec bash
// --noprofile --norc", then writes the remaining session-setup lines
// documented on the Persistent type to its stdin. It generates a fresh
// random SENTINEL token (crypto/rand) for this session and exports it into
// the shell's environment so the per-command protocol line can reference
// it as "$SENTINEL".
func (c *Persistent) openSession() (*persistentSession, error) {
	token, err := randomSentinelToken()
	if err != nil {
		return nil, fmt.Errorf("opening session: %w", err)
	}

	argv, err := Substitute(c.profile.Exec, map[string]string{"host": c.host, "cmd": "exec bash --noprofile --norc"})
	if err != nil {
		return nil, fmt.Errorf("opening session: %w", err)
	}

	// The session outlives any single Exec call's context: it is torn
	// down explicitly (Close, or a lost-session error), never by one
	// command's ctx expiring.
	osCmd := newCmd(context.Background(), argv)

	stdin, err := osCmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("opening session: %w", err)
	}
	stdout, err := osCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("opening session: %w", err)
	}

	if err := osCmd.Start(); err != nil {
		return nil, fmt.Errorf("opening session: %w", err)
	}

	splitter := newSentinelSplitter(token)
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	scanner.Split(splitter.split)

	sess := &persistentSession{
		cmd:      osCmd,
		stdin:    stdin,
		stdout:   stdout,
		scanner:  scanner,
		splitter: splitter,
	}

	setup := "stty -echo 2>/dev/null || true\n" +
		"export PS1= PS2= LC_ALL=C\n" +
		"export SENTINEL=" + token + "\n"
	if _, err := io.WriteString(stdin, setup); err != nil {
		c.killSession(sess)
		return nil, fmt.Errorf("opening session: %w", err)
	}

	return sess, nil
}

// runLocked runs cmd through sess using the per-command protocol line
// documented on the Persistent type, redirecting cmd's stderr into the
// session's shared temp file. Callers must hold c.mu.
//
// cmd runs inside a "( ... )" subshell rather than directly at the
// session's top level. Without that, a cmd built around a shell builtin
// like "exit" or "return" would terminate the persistent session itself
// instead of just that one command, and never reach the ";" that prints
// the sentinel; wrapping isolates cmd's exit (and any cd or environment
// change it makes) from the long-lived shell the rest of the session
// depends on.
func (c *Persistent) runLocked(ctx context.Context, sess *persistentSession, cmd string) ([]byte, int, error) {
	return c.runRawLocked(ctx, sess, "( "+cmd+" ) 2>/tmp/.logpick.$$.err")
}

// runRawLocked writes line, with the sentinel-printing suffix appended,
// to sess's stdin and reads sess's stdout up to and including the next
// sentinel frame, returning everything written before it and the exit
// code carried in the frame. Callers must hold c.mu.
//
// It respects ctx: if ctx is cancelled before the sentinel arrives, the
// session is torn down (which unblocks the pending read) and ctx.Err() is
// returned, since there is no way to abandon just this one command on a
// shared, serialised stream without losing the session's state.
func (c *Persistent) runRawLocked(ctx context.Context, sess *persistentSession, line string) ([]byte, int, error) {
	full := line + "; printf '\\x1e%s %d\\x1e\\n' \"$SENTINEL\" \"$?\"\n"
	if _, err := io.WriteString(sess.stdin, full); err != nil {
		return nil, 0, fmt.Errorf("writing command: %w", err)
	}

	done := make(chan struct{})
	var scanOK bool
	go func() {
		scanOK = sess.scanner.Scan()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		c.destroySessionLocked()
		<-done
		return nil, 0, ctx.Err()
	}

	if !scanOK {
		err := sess.scanner.Err()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, 0, err
	}

	out := append([]byte(nil), sess.scanner.Bytes()...)
	return out, sess.splitter.exitCode, nil
}

// destroySessionLocked tears down and forgets c.session, if one is open.
// Callers must hold c.mu. Safe to call when c.session is already nil.
func (c *Persistent) destroySessionLocked() {
	sess := c.session
	if sess == nil {
		return
	}
	c.session = nil
	c.killSession(sess)
}

// killSession terminates sess's process group and releases its pipes. It
// does not touch c.session; callers that need to forget the session too
// use destroySessionLocked instead.
func (c *Persistent) killSession(sess *persistentSession) {
	if sess.cmd.Cancel != nil {
		_ = sess.cmd.Cancel()
	}
	_ = sess.stdin.Close()
	_ = sess.stdout.Close()
	_ = sess.cmd.Wait()
}

// randomSentinelToken generates a fresh per-session SENTINEL value using
// crypto/rand, per DESIGN.md 7.3, so nothing a remote command writes to its
// own stdout can forge it.
func randomSentinelToken() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating sentinel token: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// Fetch copies remote to local without ever streaming it through the
// interactive session. Persistent.Fetch delegates outright to the
// one-shot *Command NewPersistent built alongside it: that Command
// already implements exactly the right choice on its own — a native copy
// spawn when profile.Copy is defined, otherwise one cat process per fetch
// through its own embedded FallbackFetcher — so Persistent.Fetch neither
// streams cat through the shared, non-binary-safe shell (Caps().BinarySafe
// is false; DESIGN.md 7.3: "Binary fetch falls back to a one-shot command
// spawn") nor re-derives Command's native-copy-or-fallback logic here.
//
// The FallbackFetcher embedded in Persistent above exists for structural
// parity with Command, which also embeds one, but it is never the method
// that actually runs: Fetch, defined here, always takes precedence over
// the promoted FallbackFetcher.Fetch.
func (c *Persistent) Fetch(ctx context.Context, remote, local string, prog chan<- Progress) (int64, error) {
	return c.fetcher.Fetch(ctx, remote, local, prog)
}

// Caps reports what Persistent can do, per DESIGN.md 7.3:
//
//   - Follow is false. The session is framed one command per sentinel, so
//     Exec cannot return until the frame closes. A tail -f would never
//     produce one and would block forever. Invariant 7 requires Caps to be
//     honest, so callers check this and refuse a follow rather than hanging
//     on it; follow over a persistent profile spawns a one-shot Command
//     instead, the same escape hatch Fetch uses for not being binary safe.
//   - ConcurrentExec is false: commands serialise behind the session, as
//     Exec's doc comment describes.
//   - BinarySafe is false: the sentinel protocol is a text framing over a
//     shared stdout stream, so it cannot safely carry arbitrary bytes.
//     Fetch avoids this by never running through the session at all.
//   - NativeCopy follows the same rule Command's Caps does: true only when
//     profile.Copy is non-empty, since that is what Fetch's delegated
//     one-shot Command actually uses.
//   - GNUFind is nil until a caller probes it, exactly as for Command;
//     Persistent never probes on its own.
func (c *Persistent) Caps() Caps {
	return Caps{
		NativeCopy:     len(c.profile.Copy) > 0,
		Follow:         false,
		ConcurrentExec: false,
		BinarySafe:     false,
		GNUFind:        nil,
	}
}

// Check is a cheap liveness probe run before a long operation, per the
// Transport contract in transport.go. For Persistent this doubles as the
// natural way to notice a session that is no longer usable: an error from
// Check wraps ErrConnLost when the underlying shell is gone.
func (c *Persistent) Check(ctx context.Context) error {
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

// Close tears down the underlying shell session, if one was ever opened,
// and releases anything it holds open (including the remote stderr temp
// file's local read side). It is safe to call more than once and safe to
// call when Exec and Check were never called.
func (c *Persistent) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	sess := c.session
	if sess == nil {
		return nil
	}

	// Best-effort remote cleanup of the stderr temp file, bounded so a
	// wedged session can never make Close hang. Errors are deliberately
	// ignored: Close is documented safe to call unconditionally, and the
	// session is being torn down regardless of whether this succeeds.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _, _ = c.runRawLocked(cleanupCtx, sess, "rm -f /tmp/.logpick.$$.err 2>/dev/null")
	cancel()

	c.destroySessionLocked()
	return nil
}

// sentinelSplitter locates one command's sentinel frame,
// \x1e<token> <exit>\x1e, followed by the newline the shell's printf
// writes after it, within a persistent session's combined stdout stream,
// for a fixed per-session token. It exists as its own type, independent of
// any process or session, specifically so the framing logic can be
// exercised directly against an arbitrary io.Reader — including one that
// hands back a single byte per Read, via testing/iotest.OneByteReader — with no
// bash spawned, which is what makes mechanic 2 ("a sentinel split across
// Read boundaries is detected") a real test of the framing rather than a
// test of a live process.
//
// split has the shape bufio.SplitFunc requires and is meant to be handed
// to a *bufio.Scanner via Scanner.Split(sp.split): the token
// (*bufio.Scanner).Bytes/Text returns after a successful Scan is
// everything the command wrote before its sentinel frame, with the frame
// itself and its trailing newline consumed but never included in that
// token. Using bufio.Scanner this way, rather than any form of naive
// string matching over an accumulated buffer, is what DESIGN.md 7.3 means
// by "custom bufio.SplitFunc, not naive string matching": bufio.Scanner
// already knows how to hold back a decision and ask for more data when
// what it has so far is not enough to tell, which is exactly the behaviour
// a sentinel split across two separate Read calls needs, including the
// worst case where every Read returns exactly one byte.
//
// A 0x1e byte occurring in the command's own output that is not
// immediately followed by exactly this session's token, a space, decimal
// digits and a closing 0x1e is ordinary data: split must not treat it as
// the start of a frame, and must include it verbatim in the eventual
// token, which is what stops log content containing the record separator
// byte from ever forging or falsely triggering early termination
// (mechanic 3). Only a byte-for-byte match of the complete frame for this
// session's exact token counts.
//
// exitCode is set to the integer exit code parsed out of the frame once
// split has located one and Scan has returned true for it; it is
// unspecified before then.
type sentinelSplitter struct {
	token    string
	exitCode int
}

// newSentinelSplitter returns a sentinelSplitter that recognises sentinel
// frames for the given per-session token. token is compared literally,
// never as a pattern: it is exactly the random SENTINEL value DESIGN.md
// 7.3 says a session generates fresh for itself, and this splitter never
// generates or knows how to generate one itself.
func newSentinelSplitter(token string) *sentinelSplitter {
	return &sentinelSplitter{token: token}
}

// sentinelByte is the ASCII record separator (0x1e) DESIGN.md 7.3's
// printf frame uses to bracket the sentinel.
const sentinelByte = 0x1e

// split is the bufio.SplitFunc bound to s. See the sentinelSplitter doc
// comment for the framing rule it implements.
func (s *sentinelSplitter) split(data []byte, atEOF bool) (advance int, token []byte, err error) {
	prefix := "\x1e" + s.token + " "

	searchFrom := 0
	for {
		idx := bytes.IndexByte(data[searchFrom:], sentinelByte)
		if idx < 0 {
			break
		}
		absIdx := searchFrom + idx
		rest := data[absIdx:]

		if len(rest) < len(prefix) {
			if !atEOF {
				// Not enough data yet to tell whether this 0x1e begins
				// this session's frame; ask bufio.Scanner for more.
				return 0, nil, nil
			}
			// No more data is ever coming, so this candidate can never
			// complete: it is ordinary content, not a frame start.
			searchFrom = absIdx + 1
			continue
		}
		if string(rest[:len(prefix)]) != prefix {
			searchFrom = absIdx + 1
			continue
		}

		digitsStart := len(prefix)
		j := digitsStart
		for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
			j++
		}
		if j == len(rest) {
			if !atEOF {
				return 0, nil, nil
			}
			searchFrom = absIdx + 1
			continue
		}
		if j == digitsStart {
			// No exit-code digits at all: not a valid frame.
			searchFrom = absIdx + 1
			continue
		}
		if rest[j] != sentinelByte {
			searchFrom = absIdx + 1
			continue
		}
		if j+1 == len(rest) {
			if !atEOF {
				return 0, nil, nil
			}
			searchFrom = absIdx + 1
			continue
		}
		if rest[j+1] != '\n' {
			searchFrom = absIdx + 1
			continue
		}

		code, convErr := strconv.Atoi(string(rest[digitsStart:j]))
		if convErr != nil {
			searchFrom = absIdx + 1
			continue
		}

		s.exitCode = code
		frameEnd := absIdx + j + 2 // past the closing 0x1e and the trailing \n
		return frameEnd, data[:absIdx], nil
	}

	if atEOF {
		if len(data) == 0 {
			return 0, nil, nil
		}
		// The stream ended without ever producing this session's
		// sentinel frame: the session is gone mid-command.
		return 0, nil, io.ErrUnexpectedEOF
	}
	return 0, nil, nil
}
