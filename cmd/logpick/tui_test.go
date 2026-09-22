package main

// Test plan for F02, the TUI composition layer (DESIGN.md 9.2, 9.3;
// AGENTS.md sections 3, 4.1, 5, 6, 7, 9). Copied verbatim from the task
// before any test below was written.
//
// Mechanics:
//  1. newApp returns an App whose four screens are all non-nil and whose
//     active screen is Hosts.
//  2. The scan drain turns entries arriving on the channel into batched
//     ui.ScanEntriesMsg and finishes with ui.ScanDoneMsg.
//  3. A transport whose Caps().Follow is false makes the follow action
//     surface an error rather than starting a stream.
//  4. --mock <dir> builds an app backed by the fixture directory and no
//     real transport.
//
// Notes on how these were resolved into assertions:
//
//   - Every deps value below is built the same way scan_test.go's TestRunScan
//     builds deps: transport.NewMock over a t.TempDir() copy of
//     internal/transport/testdata/fixtures (copyFixtures, reused from
//     scan_test.go, same package), state.New and local.New over t.TempDir(),
//     and a fixed clock (fixedClock, also reused from scan_test.go). No test
//     here touches a network, spawns a process, or reads a real XDG path.
//
//   - newApp, scanRun, scanDrainCmd and appDeps.followCmd are all
//     panic("not implemented") at this stage (Phase A/B: contract and tests
//     only). Every test below therefore currently fails via that panic
//     rather than a graceful assertion failure - that is expected, and is
//     what distinguishes "not implemented yet" from a broken test file.
//
//   - Mechanic 2 asserts on the exact ui.ScanEntriesMsg and ui.ScanDoneMsg
//     values scanDrainCmd's returned tea.Cmd would produce, driven by
//     channels the test owns and feeds directly - no sleep, no real scan, no
//     transport involved at all, since scanDrainCmd's contract is drain
//     mechanics only.
//
//   - Mechanic 3 uses transport.NewMock(dir, transport.WithCaps(...)) with
//     Follow: false and asserts with errors.Is against
//     transport.ErrNotSupported, per remote.Follow's own documented
//     contract that appDeps.followCmd is required to pass through
//     unchanged.
//
//   - Mechanic 4 asserts two things: that transport.NewMock(dir) is what
//     backs appDeps.Transport (a type assertion to *transport.Mock, proving
//     no real backend is ever constructed for --mock), and that newApp
//     still produces a fully-populated App from deps built that way, same
//     as mechanic 1.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/go-cmp/cmp"

	"github.com/pedreviljoen/logpick/internal/config"
	"github.com/pedreviljoen/logpick/internal/local"
	"github.com/pedreviljoen/logpick/internal/remote"
	"github.com/pedreviljoen/logpick/internal/transport"
	"github.com/pedreviljoen/logpick/internal/ui"
)

// testDeps returns an appDeps for host, built entirely over a fresh
// t.TempDir() copy of the fixture corpus and fresh, empty state and local
// stores: transport.NewMock (no network), state.New and local.New over
// t.TempDir() (no real XDG path), local.NewNative() (no rg subprocess) and
// a fixed clock (no sleep). It reuses copyFixtures, newStore, resolveDefault
// and fixedClock from scan_test.go, which build deps the identical way for
// TestRunScan.
func testDeps(t *testing.T, host string) appDeps {
	t.Helper()

	dir := copyFixtures(t)
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	return appDeps{
		Transport: transport.NewMock(dir),
		State:     newStore(t),
		Local:     local.New(t.TempDir()),
		Search:    local.NewNative(),
		Host:      resolveDefault(t, host),
		Now:       fixedClock(clock),
	}
}

type previewRetryTransport struct {
	calls []string
}

func (t *previewRetryTransport) Exec(_ context.Context, cmd string) (*transport.Process, error) {
	t.calls = append(t.calls, cmd)
	if len(t.calls) == 1 {
		return &transport.Process{
			Stdout: io.NopCloser(strings.NewReader("")),
			Stderr: io.NopCloser(strings.NewReader("")),
			Wait: func() error {
				return &transport.ExitError{Code: 1, Stderr: "tail: Permission denied"}
			},
		}, nil
	}
	return &transport.Process{
		Stdout: io.NopCloser(strings.NewReader("booted\nready\n")),
		Stderr: io.NopCloser(strings.NewReader("")),
		Wait:   func() error { return nil },
	}, nil
}

func (t *previewRetryTransport) Fetch(context.Context, string, string, chan<- transport.Progress) (int64, error) {
	return 0, transport.ErrNotSupported
}
func (t *previewRetryTransport) Caps() transport.Caps        { return transport.Caps{} }
func (t *previewRetryTransport) Check(context.Context) error { return nil }
func (t *previewRetryTransport) Close() error                { return nil }

func TestPreviewRetriesPermissionDeniedWithSudo(t *testing.T) {
	tp := &previewRetryTransport{}
	preview := previewFunc(appDeps{Transport: tp})
	lines, err := preview(context.Background(), "ec2-user@example.com", "/var/log/boot.log")
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if diff := cmp.Diff([]string{"booted", "ready"}, lines); diff != "" {
		t.Fatalf("preview lines mismatch (-want +got):\n%s", diff)
	}
	wantCalls := []string{
		"tail -n 100 '/var/log/boot.log'",
		"sudo -n tail -n 100 '/var/log/boot.log'",
	}
	if diff := cmp.Diff(wantCalls, tp.calls); diff != "" {
		t.Fatalf("preview commands mismatch (-want +got):\n%s", diff)
	}
}

func TestConnectionProbeError(t *testing.T) {
	noisy := &transport.ExitError{Code: 255, Stderr: "very long ssh diagnostic"}
	got := connectionProbeError(noisy)
	if !errors.Is(got, transport.ErrAuthRequired) {
		t.Fatalf("connectionProbeError = %v, want ErrAuthRequired", got)
	}
	if !strings.Contains(got.Error(), "ubuntu@host") || !strings.Contains(got.Error(), "ec2-user@host") {
		t.Fatalf("connectionProbeError is not actionable: %v", got)
	}
	if strings.Contains(got.Error(), noisy.Stderr) {
		t.Fatalf("connectionProbeError leaked noisy SSH stderr: %v", got)
	}
}

func TestHistoryProfileOverrides(t *testing.T) {
	t.Run("built-in default remains an implicit fallback", func(t *testing.T) {
		resolved, err := config.Resolve(nil, "example-host", historyProfileOverrides(config.DefaultProfileName))
		if err != nil {
			t.Fatalf("Resolve with stored default profile: %v", err)
		}
		if resolved.ProfileName != config.DefaultProfileName {
			t.Fatalf("profile = %q, want %q", resolved.ProfileName, config.DefaultProfileName)
		}
	})

	t.Run("a named profile remains an explicit override", func(t *testing.T) {
		cfg := &config.Config{Profiles: map[string]config.Profile{"corp": {}}}
		resolved, err := config.Resolve(cfg, "example-host", historyProfileOverrides("corp"))
		if err != nil {
			t.Fatalf("Resolve with stored named profile: %v", err)
		}
		if resolved.ProfileName != "corp" {
			t.Fatalf("profile = %q, want corp", resolved.ProfileName)
		}
	})
}

func TestNewApp(t *testing.T) {
	t.Run("assembles all four screens with hosts active", func(t *testing.T) {
		d := testDeps(t, "host-a")

		app, err := newApp(d)
		if err != nil {
			t.Fatalf("newApp: %v", err)
		}

		for i, screen := range app.Screens {
			if screen == nil {
				t.Fatalf("screen %d is nil, want every screen populated", i)
			}
		}

		if app.Active != ui.ScreenHosts {
			t.Fatalf("active screen = %v, want %v", app.Active, ui.ScreenHosts)
		}
	})

	t.Run("scan drain batches entries and finishes with ScanDoneMsg", func(t *testing.T) {
		entries := make(chan remote.Entry)
		report := make(chan remote.Report, 1)

		cmd := scanDrainCmd("host-a", entries, report)

		go func() {
			entries <- remote.Entry{Path: "/var/log/a.log", Size: 10}
			entries <- remote.Entry{Path: "/var/log/b.log", Size: 20}
			close(entries)
			report <- remote.Report{Count: 2, Skipped: 1, Truncated: false}
		}()

		gotMsg := cmd()
		gotEntries, ok := gotMsg.(ui.ScanEntriesMsg)
		if !ok {
			t.Fatalf("first message = %T, want ui.ScanEntriesMsg", gotMsg)
		}

		wantEntries := ui.ScanEntriesMsg{
			Host: "host-a",
			Entries: []ui.ScanEntry{
				{Path: "/var/log/a.log", Size: 10},
				{Path: "/var/log/b.log", Size: 20},
			},
		}
		if diff := cmp.Diff(wantEntries, gotEntries); diff != "" {
			t.Fatalf("ScanEntriesMsg mismatch (-want +got):\n%s", diff)
		}

		// The channel closed after the batch above, so the caller reissues
		// scanDrainCmd (per its own doc comment) to pick up the final
		// tally rather than another batch.
		doneCmd := scanDrainCmd("host-a", entries, report)
		gotDoneMsg := doneCmd()
		gotDone, ok := gotDoneMsg.(ui.ScanDoneMsg)
		if !ok {
			t.Fatalf("second message = %T, want ui.ScanDoneMsg", gotDoneMsg)
		}

		wantDone := ui.ScanDoneMsg{Host: "host-a", Count: 2, Skipped: 1, Truncated: false}
		if diff := cmp.Diff(wantDone, gotDone); diff != "" {
			t.Fatalf("ScanDoneMsg mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("follow surfaces ErrNotSupported when the transport cannot follow", func(t *testing.T) {
		dir := copyFixtures(t)
		tp := transport.NewMock(dir, transport.WithCaps(transport.Caps{Follow: false}))

		d := appDeps{
			Transport: tp,
			State:     newStore(t),
			Local:     local.New(t.TempDir()),
			Search:    local.NewNative(),
			Host:      resolveDefault(t, "host-a"),
			Now:       fixedClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)),
		}

		errc := make(chan error, 1)
		cmd, err := d.followCmd(context.Background(), errc, "host-a", "/var/log/syslog")
		if !errors.Is(err, transport.ErrNotSupported) {
			t.Fatalf("followCmd error = %v, want errors.Is transport.ErrNotSupported", err)
		}
		if cmd != nil {
			t.Fatalf("followCmd returned a non-nil tea.Cmd alongside an error, want nil: no stream should have started")
		}
	})

	t.Run("stopping a follow does not report the cancellation", func(t *testing.T) {
		dir := copyFixtures(t)
		d := appDeps{
			Transport: transport.NewMock(dir),
			State:     newStore(t),
			Local:     local.New(t.TempDir()),
			Search:    local.NewNative(),
			Host:      resolveDefault(t, "host-a"),
			Now:       fixedClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)),
		}

		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 4)
		_, ch, err := d.startFollow(ctx, errc, "host-a", "/var/log/syslog")
		if err != nil {
			t.Fatalf("startFollow: %v", err)
		}
		done := make(chan struct{})
		go func() {
			for range ch {
			}
			close(done)
		}()
		cancel()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("follow did not stop after cancel")
		}
		select {
		case err := <-errc:
			t.Fatalf("cancel reported %v, want silence", err)
		default:
		}
	})

	t.Run("--mock builds an app backed by the fixture directory, no real transport", func(t *testing.T) {
		dir := copyFixtures(t)
		tp := transport.NewMock(dir)

		if _, ok := any(tp).(*transport.Mock); !ok {
			t.Fatalf("transport.NewMock(dir) did not return a *transport.Mock, --mock must never reach a real backend")
		}

		d := appDeps{
			Transport: tp,
			State:     newStore(t),
			Local:     local.New(t.TempDir()),
			Search:    local.NewNative(),
			Host:      resolveDefault(t, "mock-host"),
			Now:       fixedClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)),
		}

		app, err := newApp(d)
		if err != nil {
			t.Fatalf("newApp: %v", err)
		}

		for i, screen := range app.Screens {
			if screen == nil {
				t.Fatalf("screen %d is nil, want every screen populated", i)
			}
		}

		if app.Active != ui.ScreenHosts {
			t.Fatalf("active screen = %v, want %v", app.Active, ui.ScreenHosts)
		}
	})
}

// TestScanPathListsEverythingUnderTheTypedPath is the escape hatch a user
// reaches for when the configured scan did not turn up the log they know is
// there (ui.PathScanRequestedMsg): the profile's include and exclude
// patterns are what hid it, so a one-off listing must drop them, and must
// list directories too, so the user can descend.
func TestScanPathListsEverythingUnderTheTypedPath(t *testing.T) {
	const host = "jenkins-01.prod.internal"

	deps := testDeps(t, host)
	// A profile narrow enough to hide every fixture but *.log, which is
	// exactly the situation the one-off listing exists to escape.
	deps.Host.Profile.Scan = config.ScanSpec{
		Paths:   []string{"/var/log"},
		Include: []string{"*.log"},
		Exclude: []string{"*.gz"},
	}

	m := tuiModel{active: deps, ctx: context.Background(), errc: make(chan error, 1)}
	next, cmd := m.scanPath(ui.PathScanRequestedMsg{Host: host, Path: "  /opt/app/logs  "})
	if cmd == nil {
		t.Fatal("scanPath returned nil Cmd")
	}
	if _, ok := next.(tuiModel); !ok {
		t.Fatalf("scanPath returned %T, want tuiModel", next)
	}

	// tea.Batch's message is not one of ours to inspect, so the probe and
	// start messages are exercised through the model's own Update: the
	// scanReadyMsg is what carries the spec the scan will actually run.
	ready := scanReadyMsg{}
	found := false
	for _, msg := range batchMessages(t, cmd) {
		switch v := msg.(type) {
		case ui.PathScanStartedMsg:
			want := ui.PathScanStartedMsg{Host: host, Path: "/opt/app/logs"}
			if diff := cmp.Diff(want, v); diff != "" {
				t.Errorf("PathScanStartedMsg mismatch (-want +got):\n%s", diff)
			}
		case scanReadyMsg:
			ready, found = v, true
		case ui.ErrorMsg:
			t.Fatalf("scanPath reported %v", v.Err)
		}
	}
	if !found {
		t.Fatal("scanPath never produced a scanReadyMsg")
	}
	if !ready.Browse {
		t.Error("scanReadyMsg.Browse = false, want true: a typed path is browsed, not scanned")
	}
	want := config.ScanSpec{Paths: []string{"/opt/app/logs"}}
	if diff := cmp.Diff(want, ready.Deps.Host.Profile.Scan); diff != "" {
		t.Errorf("one-off scan spec mismatch (-want +got):\n%s", diff)
	}

	// And the scan that spec drives lists directories alongside the files
	// the profile's patterns would have excluded.
	entries := make(chan remote.Entry)
	report := make(chan remote.Report, 1)
	go func() {
		if err := scanRun(ready.Deps, ready.GNUFind, ready.Browse, entries, report)(context.Background()); err != nil {
			t.Errorf("scanRun: %v", err)
		}
	}()

	dirs, files := map[string]bool{}, map[string]bool{}
	for e := range entries {
		if e.IsDir {
			dirs[e.Path] = true
			continue
		}
		files[e.Path] = true
	}
	if !dirs["/opt/app/logs/archive"] {
		t.Errorf("listing has no directories, got %v", dirs)
	}
	if !files["/opt/app/logs/gc.log.gz"] {
		t.Errorf("listing did not include the excluded gc.log.gz, got %v", files)
	}
	if rep := <-report; rep.Count != len(dirs)+len(files) {
		t.Errorf("report Count = %d, want %d", rep.Count, len(dirs)+len(files))
	}
}

// batchMessages runs cmd and returns every message it produced, flattening
// the tea.BatchMsg a tea.Batch returns into the individual messages its
// commands report. A nil message (a command whose only job is a side
// effect) is dropped.
func batchMessages(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	msg := cmd()
	batch, ok := msg.(tea.BatchMsg)
	if !ok {
		return []tea.Msg{msg}
	}
	out := make([]tea.Msg, 0, len(batch))
	for _, c := range batch {
		if c == nil {
			continue
		}
		if inner := c(); inner != nil {
			out = append(out, inner)
		}
	}
	return out
}
