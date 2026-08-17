package transport

// Test plan for T19, one test per mechanic (AGENTS.md sections 5 and 8, T19).
//
//  1. Two sequential commands over one session both return correct output.
//  2. A sentinel split across Read boundaries is detected, verified by
//     feeding the reader one byte at a time.
//  3. Output containing the record separator byte does not falsely
//     terminate a command.
//  4. A non-zero exit yields the right ExitError.Code.
//  5. Caps reports BinarySafe and ConcurrentExec false.
//
// Several points needed resolving before the assertions could be written.
// They are recorded here, and in persistent.go's doc comments, because
// they are contract, not test detail:
//
//   - NewPersistent takes host as a second parameter, mirroring T04's
//     NewCommand(host string, profile config.Profile) *Command, for the
//     identical reason: config.Profile has no Host field to fill {host}
//     from. See persistent.go's NewPersistent doc comment.
//   - Mechanics 1, 3 and 4 describe Persistent.Exec's own behaviour, so
//     they spawn a real local bash through it, the sanctioned exception
//     AGENTS.md section 5 and 13 carve out for this package (alongside
//     T04). All three share the same profile shape command_test.go uses:
//     Exec: []string{"/bin/sh", "-c", "{cmd}"}, so the outer spawn is a
//     plain local shell and {cmd} is whatever text NewPersistent's
//     implementation substitutes in (DESIGN.md 7.3 names it literally:
//     "exec bash --noprofile --norc"). requireBash below skips these
//     three if no bash is resolvable on PATH, per this task's instruction
//     to prefer /bin/bash and skip cleanly rather than fail on a machine
//     that lacks it.
//   - Mechanic 2 is different in kind: it is a test of the sentinel
//     framing logic in isolation, not of Persistent.Exec, and the task is
//     explicit that it must not spawn a process. persistent.go exposes
//     that logic as sentinelSplitter, an unexported type independent of
//     any session or process, precisely so this is possible: the test
//     below wraps a bytes.Reader in testing/iotest.OneByteReader — a stdlib
//     io.Reader that returns exactly one byte per Read call — and drives
//     sentinelSplitter.split through a *bufio.Scanner over that reader
//     directly. If the split function correctly reassembles the sentinel
//     despite arriving one byte at a time, the boundary-crossing mechanic
//     is real, not theatre.
//   - Mechanic 3 (the subtle one) is tested twice, deliberately, at two
//     different levels: once against sentinelSplitter in isolation (same
//     technique as mechanic 2, proving the framing rule itself never
//     mistakes a bare 0x1e for a frame) and once through a real
//     Persistent.Exec call whose command's actual stdout contains a
//     literal 0x1e byte (proving the same guarantee holds end to end,
//     and that a second command on the same session afterwards still
//     gets correct output, which is the strongest evidence the bogus byte
//     never desynchronised the shell).
//   - go-cmp needs cmp.AllowUnexported(sentinelSplitter{}) wherever a test
//     compares a *sentinelSplitter directly, since both of its fields are
//     unexported and go-cmp panics on those regardless of equality
//     (AGENTS.md section 5).

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/google/go-cmp/cmp"

	"github.com/pedreviljoen/logpick/internal/config"
)

func TestPersistent(t *testing.T) {
	t.Run("two sequential commands over one session both return correct output", func(t *testing.T) {
		requireBash(t)

		profile := config.Profile{
			Exec:       []string{"/bin/sh", "-c", "{cmd}"},
			Persistent: true,
		}
		c := NewPersistent("example.internal", profile)
		t.Cleanup(func() { _ = c.Close() })

		proc1, err := c.Exec(context.Background(), "echo first")
		if err != nil {
			t.Fatalf("Exec (1st) returned unexpected error: %v", err)
		}
		out1, err := io.ReadAll(proc1.Stdout)
		if err != nil {
			t.Fatalf("reading 1st command's stdout: %v", err)
		}
		if werr := proc1.Wait(); werr != nil {
			t.Fatalf("Wait (1st) = %v, want nil", werr)
		}
		if want := "first\n"; string(out1) != want {
			t.Errorf("1st command output = %q, want %q", string(out1), want)
		}

		proc2, err := c.Exec(context.Background(), "echo second")
		if err != nil {
			t.Fatalf("Exec (2nd) returned unexpected error: %v", err)
		}
		out2, err := io.ReadAll(proc2.Stdout)
		if err != nil {
			t.Fatalf("reading 2nd command's stdout: %v", err)
		}
		if err := proc2.Wait(); err != nil {
			t.Fatalf("Wait (2nd) = %v, want nil", err)
		}
		if want := "second\n"; string(out2) != want {
			t.Errorf("2nd command output = %q, want %q", string(out2), want)
		}
	})

	t.Run("a sentinel split across Read boundaries is detected", func(t *testing.T) {
		const token = "deadbeef1234cafe"
		input := []byte("line one\nline two\n\x1e" + token + " 0\x1e\n")
		src := iotest.OneByteReader(bytes.NewReader(input))

		sp := newSentinelSplitter(token)
		sc := bufio.NewScanner(src)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		sc.Split(sp.split)

		if !sc.Scan() {
			t.Fatalf("Scan() = false, want true (err = %v)", sc.Err())
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("Scan() error = %v, want nil", err)
		}

		wantOutput := "line one\nline two\n"
		if got := sc.Text(); got != wantOutput {
			t.Errorf("token = %q, want %q", got, wantOutput)
		}

		want := &sentinelSplitter{token: token, exitCode: 0}
		if diff := cmp.Diff(want, sp, cmp.AllowUnexported(sentinelSplitter{})); diff != "" {
			t.Errorf("splitter state mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("output containing the record separator byte does not falsely terminate a command", func(t *testing.T) {
		t.Run("sentinel framing in isolation", func(t *testing.T) {
			const token = "cafef00dbeef9001"
			// A bare 0x1e with nothing matching the session token after it
			// must not be mistaken for the sentinel: only the full frame
			// for this exact token terminates the token bufio.Scanner
			// returns.
			input := []byte("before \x1e after\n\x1e" + token + " 3\x1e\n")
			src := iotest.OneByteReader(bytes.NewReader(input))

			sp := newSentinelSplitter(token)
			sc := bufio.NewScanner(src)
			sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
			sc.Split(sp.split)

			if !sc.Scan() {
				t.Fatalf("Scan() = false, want true (err = %v)", sc.Err())
			}
			if err := sc.Err(); err != nil {
				t.Fatalf("Scan() error = %v, want nil", err)
			}

			wantOutput := "before \x1e after\n"
			if got := sc.Text(); got != wantOutput {
				t.Errorf("token = %q, want %q", got, wantOutput)
			}

			want := &sentinelSplitter{token: token, exitCode: 3}
			if diff := cmp.Diff(want, sp, cmp.AllowUnexported(sentinelSplitter{})); diff != "" {
				t.Errorf("splitter state mismatch (-want +got):\n%s", diff)
			}
		})

		t.Run("through a real session", func(t *testing.T) {
			requireBash(t)

			profile := config.Profile{
				Exec:       []string{"/bin/sh", "-c", "{cmd}"},
				Persistent: true,
			}
			c := NewPersistent("example.internal", profile)
			t.Cleanup(func() { _ = c.Close() })

			proc, err := c.Exec(context.Background(), `printf 'a\x1eb\n'`)
			if err != nil {
				t.Fatalf("Exec returned unexpected error: %v", err)
			}
			out, err := io.ReadAll(proc.Stdout)
			if err != nil {
				t.Fatalf("reading stdout: %v", err)
			}
			if werr := proc.Wait(); werr != nil {
				t.Fatalf("Wait() = %v, want nil", werr)
			}
			if want := "a\x1eb\n"; string(out) != want {
				t.Errorf("output = %q, want %q", string(out), want)
			}

			// A second command on the same session proves the bogus 0x1e
			// byte in the first command's output did not desynchronise
			// the shell or get mistaken for (part of) a sentinel frame.
			proc2, err := c.Exec(context.Background(), "echo still-in-sync")
			if err != nil {
				t.Fatalf("Exec (2nd) returned unexpected error: %v", err)
			}
			out2, err := io.ReadAll(proc2.Stdout)
			if err != nil {
				t.Fatalf("reading 2nd command's stdout: %v", err)
			}
			if err := proc2.Wait(); err != nil {
				t.Fatalf("Wait (2nd) = %v, want nil", err)
			}
			if want := "still-in-sync\n"; string(out2) != want {
				t.Errorf("2nd command output = %q, want %q", string(out2), want)
			}
		})
	})

	t.Run("a non-zero exit yields the right ExitError.Code", func(t *testing.T) {
		requireBash(t)

		profile := config.Profile{
			Exec:       []string{"/bin/sh", "-c", "{cmd}"},
			Persistent: true,
		}
		c := NewPersistent("example.internal", profile)
		t.Cleanup(func() { _ = c.Close() })

		proc, err := c.Exec(context.Background(), "exit 7")
		if err != nil {
			t.Fatalf("Exec returned unexpected error: %v", err)
		}
		if _, err := io.ReadAll(proc.Stdout); err != nil {
			t.Fatalf("reading stdout: %v", err)
		}

		waitErr := proc.Wait()
		if waitErr == nil {
			t.Fatal("Wait() = nil, want a non-nil *ExitError")
		}

		var exitErr *ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("Wait() = %v, want errors.As(err, *ExitError)", waitErr)
		}
		if exitErr.Code != 7 {
			t.Errorf("ExitError.Code = %d, want 7", exitErr.Code)
		}
	})

	t.Run("Caps reports BinarySafe and ConcurrentExec false", func(t *testing.T) {
		profile := config.Profile{
			Exec: []string{"/bin/sh", "-c", "{cmd}"},
		}
		c := NewPersistent("example.internal", profile)
		t.Cleanup(func() { _ = c.Close() })

		caps := c.Caps()

		type capsSubset struct{ BinarySafe, ConcurrentExec bool }
		got := capsSubset{BinarySafe: caps.BinarySafe, ConcurrentExec: caps.ConcurrentExec}
		want := capsSubset{BinarySafe: false, ConcurrentExec: false}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Caps() BinarySafe/ConcurrentExec mismatch (-want +got):\n%s", diff)
		}
	})
}

// requireBash skips the calling test unless a bash binary is resolvable on
// PATH. Persistent's session-setup {cmd} is the literal "exec bash
// --noprofile --norc" from DESIGN.md 7.3, so any test that exercises a real
// session needs bash on PATH; on a machine without one this skips with a
// clear reason instead of failing for an unrelated cause, per this task's
// instruction to prefer /bin/bash and use exec.LookPath plus t.Skip where
// it may be absent.
func requireBash(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found on PATH, skipping: " + strings.TrimSpace(err.Error()))
	}
}
