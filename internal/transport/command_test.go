package transport

// Test plan for T04, one test per mechanic (AGENTS.md sections 5 and 8, T04).
// Command is exercised by spawning real local processes — echo, sh -c,
// sleep — the explicit exception AGENTS.md section 5 carves out for this
// package, because the unit under test is process handling itself.
//
//  1. A successful command streams stdout and Wait returns nil.
//  2. A non-zero exit returns *ExitError with the code and captured stderr.
//  3. Exit code 255 maps to ErrAuthRequired.
//  4. Cancelling the context terminates the whole process group, verified by
//     confirming a sleep child does not outlive cancellation.
//  5. Caps().NativeCopy is true only when the profile defines a copy
//     template.
//
// Several points needed resolving before the assertions could be written.
// They are recorded here, and in command.go's doc comments, because they
// are contract, not test detail:
//
//   - NewCommand takes host as a second parameter:
//     NewCommand(host string, profile config.Profile) *Command.
//     config.Profile (DESIGN.md 6.2) has no Host field — a Profile is
//     shared across every host a match rule or [host.<name>] entry assigns
//     it to — so the one-argument form the T04 task table quotes cannot
//     fill {host} on its own. See command.go's NewCommand doc comment.
//   - Wait's error for a 255 exit is a single value that is both an
//     *ExitError (errors.As succeeds, Code == 255) and matches
//     errors.Is(err, ErrAuthRequired). ExitError gains an Unwrap method for
//     this, added in command.go since transport.go is not modified by this
//     task.
//   - Mechanic 4 needs the child's whole process group killed, not just the
//     direct child (AGENTS.md section 9). The test profile's exec template
//     runs `sh -c 'sleep 30 & echo $!; wait'`: a non-interactive sh has no
//     job control, so the backgrounded sleep stays in the same process
//     group as sh unless something moves it, which is exactly what lets
//     this test tell a direct-child-only kill apart from a real group kill.
//     The test reads the grandchild's pid off stdout to synchronise instead
//     of sleeping, then polls syscall.Kill(pid, 0) for ESRCH inside a
//     context.WithTimeout-bounded loop rather than sleeping for a guessed
//     duration.
//   - Mechanic 1 also exercises {host} substitution and confirms a cmd
//     containing spaces arrives as a single argv element — Substitute fills
//     one template slot, exec.CommandContext never re-splits it — not just
//     that some output appeared.

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/pedreviljoen/logpick/internal/config"
)

func TestCommand(t *testing.T) {
	t.Run("a successful command streams stdout and Wait returns nil", func(t *testing.T) {
		profile := config.Profile{
			Exec: []string{"/bin/echo", "host={host}", "cmd={cmd}"},
		}
		c := NewCommand("example.internal", profile)

		proc, err := c.Exec(context.Background(), "tail -n 5 /var/log/app.log")
		if err != nil {
			t.Fatalf("Exec returned unexpected error: %v", err)
		}

		out, err := io.ReadAll(proc.Stdout)
		if err != nil {
			t.Fatalf("reading stdout: %v", err)
		}

		want := "host=example.internal cmd=tail -n 5 /var/log/app.log\n"
		if got := string(out); got != want {
			t.Errorf("stdout = %q, want %q", got, want)
		}

		if err := proc.Wait(); err != nil {
			t.Errorf("Wait() = %v, want nil", err)
		}
	})

	t.Run("a non-zero exit returns *ExitError with the code and captured stderr", func(t *testing.T) {
		profile := config.Profile{
			Exec: []string{"/bin/sh", "-c", "{cmd}"},
		}
		c := NewCommand("example.internal", profile)

		proc, err := c.Exec(context.Background(), `echo "boom" 1>&2; exit 7`)
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
		if !strings.Contains(exitErr.Stderr, "boom") {
			t.Errorf("ExitError.Stderr = %q, want it to contain %q", exitErr.Stderr, "boom")
		}
		if errors.Is(waitErr, ErrAuthRequired) {
			t.Error("Wait() unexpectedly matches errors.Is(err, ErrAuthRequired) for exit code 7")
		}
	})

	t.Run("exit code 255 maps to ErrAuthRequired", func(t *testing.T) {
		profile := config.Profile{
			Exec: []string{"/bin/sh", "-c", "{cmd}"},
		}
		c := NewCommand("example.internal", profile)

		proc, err := c.Exec(context.Background(), "exit 255")
		if err != nil {
			t.Fatalf("Exec returned unexpected error: %v", err)
		}
		if _, err := io.ReadAll(proc.Stdout); err != nil {
			t.Fatalf("reading stdout: %v", err)
		}

		waitErr := proc.Wait()
		if waitErr == nil {
			t.Fatal("Wait() = nil, want a non-nil error matching ErrAuthRequired")
		}
		if !errors.Is(waitErr, ErrAuthRequired) {
			t.Errorf("Wait() = %v, want errors.Is(err, ErrAuthRequired)", waitErr)
		}

		var exitErr *ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("Wait() = %v, want errors.As(err, *ExitError)", waitErr)
		}
		if exitErr.Code != 255 {
			t.Errorf("ExitError.Code = %d, want 255", exitErr.Code)
		}
	})

	t.Run("cancelling the context terminates the whole process group", func(t *testing.T) {
		profile := config.Profile{
			Exec: []string{"/bin/sh", "-c", "{cmd}"},
		}
		c := NewCommand("example.internal", profile)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		proc, err := c.Exec(ctx, "sleep 30 & echo $!; wait")
		if err != nil {
			t.Fatalf("Exec returned unexpected error: %v", err)
		}

		reader := bufio.NewReader(proc.Stdout)
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading grandchild pid off stdout: %v", err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil {
			t.Fatalf("parsing grandchild pid %q: %v", line, err)
		}

		// The grandchild must actually be running before cancellation is a
		// meaningful test of killing it.
		if err := syscall.Kill(pid, 0); err != nil {
			t.Fatalf("grandchild pid %d not alive before cancellation: %v", pid, err)
		}

		cancel()

		t.Cleanup(func() {
			_ = proc.Stdout.Close()
			_ = proc.Stderr.Close()
			_ = proc.Wait()
		})

		waitForProcessGone(t, pid)
	})

	t.Run("Caps().NativeCopy is true only when the profile defines a copy template", func(t *testing.T) {
		tests := []struct {
			name string
			copy []string
			want bool
		}{
			{name: "no copy template", copy: nil, want: false},
			{name: "copy template present", copy: []string{"cp", "{remote}", "{local}"}, want: true},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				profile := config.Profile{
					Exec: []string{"/bin/sh", "-c", "{cmd}"},
					Copy: tc.copy,
				}
				c := NewCommand("example.internal", profile)

				want := Caps{
					NativeCopy:     tc.want,
					Follow:         true,
					ConcurrentExec: true,
					BinarySafe:     true,
				}
				if diff := cmp.Diff(want, c.Caps()); diff != "" {
					t.Errorf("Caps() mismatch (-want +got):\n%s", diff)
				}
			})
		}
	})
}

// waitForProcessGone fails the test unless pid stops responding to signal 0
// within a bounded time budget. It polls rather than sleeping for a guessed
// duration (AGENTS.md section 9, "sleeps in tests"): the overall budget is a
// context.WithTimeout and each poll waits on a time.Ticker channel rather
// than calling time.Sleep.
func waitForProcessGone(t *testing.T, pid int) {
	t.Helper()

	const budget = 5 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("pid %d still alive %s after cancellation (kill(pid,0) = %v)", pid, budget, err)
		}
	}
}
