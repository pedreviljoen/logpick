package main

// tui.go is the composition layer no T01-T20 task covered: the place that
// assembles the four screen packages under internal/ui into a runnable
// application. DESIGN.md 9.2 is explicit that no screen owns its own I/O -
// each takes injected funcs (hosts.ListFunc, browser.PreviewFunc,
// library.DeleteFunc, viewer.SearchFunc, and the rest) and returns tea.Cmd
// values for its caller to run. Something has to be that caller: build the
// funcs, hold the real transport.Transport, *state.Store, *local.Store and
// local.Searcher those funcs close over, and own the streaming drains
// DESIGN.md 9.3 describes that no screen is allowed to own itself (see
// browser.Model's "Scope note" and remote/tail.go's package doc comment).
// This file is that place, mirroring scan.go's deps/runScan split
// (AGENTS.md section 4.1): appDeps carries the dependencies as explicit
// fields, and newApp plus the drain and follow helpers below are the
// testable core, with wireTUI as the thin cobra plumbing around them.
//
// # The composition gap newApp alone cannot close
//
// newApp builds one ui.App bound to one appDeps value: one Host, one
// Transport. But ui.App (internal/ui/app.go, committed and frozen) holds a
// fixed [4]ScreenModel and its own Update only ever flips which one is
// Active - it never rebuilds a screen, and it never reacts to
// ui.HostSelectedMsg at all (that message routes, like any other, straight
// to whichever screen is active). browser.New binds its host at
// construction. So the moment a user picks a different host at runtime,
// there is nowhere for that to land: no screen and no ui.App method can
// swap in a browser (or a transport.Transport) bound to the new host.
//
// tuiModel, defined near the bottom of this file, is the answer: a thin
// tea.Model that wraps a ui.App, intercepts ui.HostSelectedMsg to rebuild
// the browser screen and start its scan against a freshly resolved host,
// and otherwise delegates straight through to ui.App.Update. It lives here
// rather than in internal/ui because it is composition - deciding how
// config.Resolve, transport.NewCommand/NewMock and the screen packages fit
// together - which is exactly what cmd/logpick owns and internal/ui must
// not know about (DESIGN.md 9.2).
//
// wireTUI's RunE builds one appDeps and a small factory closure that
// rebuilds Host and Transport for an arbitrary hostname, calls newApp, and
// hands both to tuiModel before running it under tea.NewProgram.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/pedreviljoen/logpick/internal/config"
	"github.com/pedreviljoen/logpick/internal/local"
	"github.com/pedreviljoen/logpick/internal/remote"
	"github.com/pedreviljoen/logpick/internal/state"
	"github.com/pedreviljoen/logpick/internal/transport"
	"github.com/pedreviljoen/logpick/internal/ui"
	"github.com/pedreviljoen/logpick/internal/ui/browser"
	"github.com/pedreviljoen/logpick/internal/ui/hosts"
	"github.com/pedreviljoen/logpick/internal/ui/library"
	"github.com/pedreviljoen/logpick/internal/ui/viewer"
)

// appDeps carries newApp's dependencies as explicit fields instead of
// package-level state, exactly the pattern scan.go's deps uses for
// runScan (AGENTS.md section 4.1): it is what makes newApp and the
// streaming helpers below testable without a network, a spawned process or
// a real XDG path.
//
// A caller builds one appDeps per invocation. Tests construct it directly,
// wiring Transport to transport.NewMock over
// internal/transport/testdata/fixtures and State/Local to stores rooted
// under t.TempDir(). wireTUI's RunE instead builds the profile's real
// backend (transport.NewCommand, or --mock's transport.Mock) and stores
// rooted at the real XDG paths.
type appDeps struct {
	// NewTransport builds a host-bound transport for first-run probes. When
	// nil (primarily in focused unit tests), hostsProbeFunc falls back to
	// Transport.
	NewTransport func(host string, profile config.Profile) transport.Transport

	// Transport is the backend every screen's injected func issues its
	// Exec calls against: preview (tail -n) for the browser, the scan
	// itself (find, via BuildScan) for the browser's file list, follow
	// (tail -f) for the streaming preview, and fetch (cat, or a native
	// copy path) for the library. Never nil.
	Transport transport.Transport

	// State is where host history, capability probes, scan caches and
	// fetch records live (internal/state). The hosts screen's ListFunc and
	// the library screen's load path both read from it; the follow and
	// fetch actions write back to it. Never nil.
	State *state.Store

	// Local is where fetched files land on disk and where the library
	// screen's DeleteFunc removes them from (internal/local.Store). Never
	// nil.
	Local *local.Store

	// Search performs the viewer screen's in-file search
	// (internal/local.Searcher), chosen once via local.Choose before
	// newApp is called, so this layer does not re-decide ripgrep versus
	// native on every request.
	Search local.Searcher

	// Host is the fully resolved profile for the host the browser screen
	// is constructed against (config.Resolve's output, T07). The browser
	// screen is host-scoped at construction (browser.Model's host field),
	// so this is fixed for the life of the App newApp returns, the same
	// way scan.go's deps.Host is fixed for the life of one runScan call.
	Host config.ResolvedHost

	// Now returns the current time. Every timestamp this layer writes to
	// State - a capability probe, a recorded connect, a recorded fetch -
	// comes from calling Now, never time.Now directly, so a test controls
	// it and never sleeps (AGENTS.md section 5). wireTUI wires this to
	// time.Now; tests wire it to a fixed clock.
	Now func() time.Time

	// Config is the parsed config.toml, or nil for "no file, built-in
	// defaults only" (config.Resolve's own documented treatment of a nil
	// *config.Config). newApp passes it straight to hosts.New, which needs
	// it to recognise an already-routed hostname during the first-run flow
	// (hosts.Model's "The first-run flow" doc section). tuiModel also uses
	// it, via config.Resolve, to build a fresh appDeps for whatever host
	// ui.HostSelectedMsg names - the reason this field exists on appDeps
	// at all rather than being a wireTUI-local variable, since Config
	// (unlike Host and Transport) does not change across that rebuild.
	Config *config.Config

	// ThemePrimary and ThemeSecondary are the active palette values. Tool-owned
	// state overrides optional config defaults.
	ThemePrimary   string
	ThemeSecondary string

	// ConfigPath is where the hosts screen's first-run flow persists a
	// newly probed host's profile (hosts.WriteConfigFile's target). It is
	// wireTUI's --config flag when given, or a resolved XDG default
	// otherwise (defaultConfigPath below) - config.toml is never left
	// unwritable just because the user never named it explicitly.
	ConfigPath string
}

// scanDeps adapts d to scan.go's deps, the type resolveGNUFind (scan.go)
// is written against, so this file's HostSelectedMsg handling can call
// resolveGNUFind directly instead of re-deriving its probe-and-cache logic
// (AGENTS.md section 4.1's "do not duplicate" rule, and this task's own
// instruction to share resolveGNUFind rather than copy it). appDeps and
// scan.go's deps carry the same four fields resolveGNUFind needs; only the
// state field's name differs (State here, Store there).
func (d appDeps) scanDeps() deps {
	return deps{Transport: d.Transport, Store: d.State, Host: d.Host, Now: d.Now}
}

// newApp assembles the four screens with their real dependencies - each
// screen's injected funcs closing over d exactly as DESIGN.md 9.2
// requires, so no screen package here holds d.Transport, d.State, d.Local
// or d.Search itself - and returns a ui.App with the hosts screen active,
// ready to hand to tea.NewProgram.
//
// # What newApp wires
//
//   - hosts.New's ListFunc reads d.State's host history and converts it to
//     []ui.HostSummary; hosts.WithProber and hosts.WithWriter wire the
//     first-run liveness probe (over d.Transport) and config persistence.
//     hosts.WithRemover and hosts.WithPinner wire ctrl+d and ctrl+p to
//     d.State directly, since both are one-line calls with nothing to
//     inject beyond d.State itself.
//   - browser.New's PreviewFunc runs the equivalent of `tail -n 100` over
//     d.Transport for d.Host.Name.
//   - library.New's DeleteFunc removes a fetched file from disk via
//     d.Local and from d.State.
//   - viewer.New's SearchFunc runs d.Search.Search.
//
// # What newApp does not do
//
// newApp does not start a scan and does not drain any channel itself: see
// scanRun, scanDrainCmd and appDeps.followCmd below for the other half of
// the composition, the streaming drain DESIGN.md 9.3 describes. Those are
// started and reissued by whatever runs the tea.NewProgram loop this App
// is handed to, in response to messages such as ui.HostSelectedMsg and
// ui.ScanEntriesMsg - not by newApp, and not by ui.App itself, whose own
// Update handles only the four things its doc comment enumerates and
// routes everything else straight to the active screen. See this file's
// package doc comment and tuiModel below for the type that does that.
func newApp(d appDeps) (ui.App, error) {
	theme := ui.DefaultTheme().WithColors(d.ThemePrimary, d.ThemeSecondary)
	hostsScreen := hosts.New(d.Config, hostsListFunc(d),
		hosts.WithRemover(func(host string) error { return d.State.Remove(host) }),
		hosts.WithPinner(func(host string, pinned bool) error { return d.State.Pin(host, pinned) }),
		hosts.WithProber(hostsProbeFunc(d)),
		hosts.WithWriter(hosts.WriteConfigFile(d.ConfigPath)),
		hosts.WithTheme(theme),
		hosts.WithPalette(d.ThemePrimary, d.ThemeSecondary),
		hosts.WithThemeSaver(d.State.SetTheme),
	)

	browserScreen := browser.New(
		d.Host.Name,
		previewFunc(d),
		browser.WithSelector(selectLogFunc(d)),
		browser.WithSearch(browserSearchFunc(d)),
		browser.WithTheme(theme),
	)

	libraryScreen := library.New(libraryDeleteFunc(d))

	viewerScreen := viewer.New("", viewerSearchFunc(d))

	app := ui.New([4]ui.ScreenModel{
		ui.ScreenHosts:   hostsScreen,
		ui.ScreenBrowser: browserScreen,
		ui.ScreenLibrary: libraryScreen,
		ui.ScreenViewer:  viewerScreen,
	})
	app.Theme = theme
	app.ThemePrimary = d.ThemePrimary
	app.ThemeSecondary = d.ThemeSecondary

	return app, nil
}

// hostsListFunc returns the hosts.ListFunc newApp wires: it reads d.State's
// host history and converts each state.Host to a ui.HostSummary, looking
// its Label up from d.Config's [host.<name>] entry when one exists (state
// itself carries no label, only config.toml does).
func hostsListFunc(d appDeps) hosts.ListFunc {
	return func() ([]ui.HostSummary, error) {
		list, err := d.State.List()
		if err != nil {
			return nil, err
		}

		summaries := make([]ui.HostSummary, len(list))
		for i, h := range list {
			summaries[i] = ui.HostSummary{
				Name:         h.Name,
				Profile:      h.Profile,
				Label:        hostLabel(d.Config, h.Name),
				LastSeen:     h.LastSeen,
				ConnectCount: h.ConnectCount,
				Pinned:       h.Pinned,
			}
		}
		return summaries, nil
	}
}

// hostLabel returns cfg's [host.<name>] label for host, or "" when cfg is
// nil or has no entry for it.
func hostLabel(cfg *config.Config, host string) string {
	if cfg == nil {
		return ""
	}
	return cfg.Hosts[host].Label
}

// hostsProbeFunc returns the hosts.ProbeFunc newApp wires: the first-run
// liveness probe, run as d.Transport.Check per this func's own doc
// comment on newApp.
//
// When NewTransport is configured, the probe gets a fresh transport bound
// to the hostname the user actually typed and closes it afterward. Focused
// tests that omit the factory fall back to d.Transport.
func hostsProbeFunc(d appDeps) hosts.ProbeFunc {
	return func(ctx context.Context, host string, exec []string) error {
		if d.NewTransport == nil {
			return connectionProbeError(d.Transport.Check(ctx))
		}

		profile := config.DefaultProfile
		profile.Exec = append([]string(nil), exec...)
		tp := d.NewTransport(host, profile)
		defer func() { _ = tp.Close() }()
		return connectionProbeError(tp.Check(ctx))
	}
}

func connectionProbeError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, transport.ErrAuthRequired) {
		return fmt.Errorf(
			"%w; check the remote user and identity file (EC2 commonly uses ubuntu@host or ec2-user@host)",
			transport.ErrAuthRequired,
		)
	}
	return err
}

// previewFunc returns the browser.PreviewFunc newApp wires: the equivalent
// of `tail -n 100 <path>` over d.Transport, matching the exact "tail -n N
// <path>" shape transport.Mock recognises (mock.go) and any real `tail`
// binary accepts. host is not used to route the call - d.Transport is
// already bound to one host by construction - and is only ever the host
// this func's own Model was built for.
func previewFunc(d appDeps) browser.PreviewFunc {
	return func(ctx context.Context, host, path string) ([]string, error) {
		cmd := fmt.Sprintf("tail -n 100 %s", remote.Quote(path))

		if len(d.Host.Profile.SudoPrefix) > 0 {
			lines, err := execLines(ctx, d.Transport, prefixRemoteCommand(d.Host.Profile.SudoPrefix, cmd))
			if err != nil && isRemotePermissionDenied(err) {
				return nil, fmt.Errorf("permission denied reading %s with the configured sudo prefix", path)
			}
			return lines, err
		}

		lines, err := execLines(ctx, d.Transport, cmd)
		if err == nil {
			return lines, nil
		}
		if !isRemotePermissionDenied(err) {
			return nil, err
		}

		// Discovery can stat files that the login user cannot read. EC2's
		// standard users commonly have passwordless sudo, so retry read-only
		// preview access non-interactively; -n guarantees this never hangs on a
		// password prompt.
		lines, sudoErr := execLines(ctx, d.Transport, "sudo -n "+cmd)
		if sudoErr == nil {
			return lines, nil
		}
		return nil, fmt.Errorf(
			"permission denied reading %s; grant %s read access or allow passwordless sudo",
			path,
			host,
		)
	}
}

type prefixedTransport struct {
	transport.Transport
	prefix []string
}

func (t *prefixedTransport) Exec(ctx context.Context, cmd string) (*transport.Process, error) {
	return t.Transport.Exec(ctx, prefixRemoteCommand(t.prefix, cmd))
}

func (t *prefixedTransport) Fetch(ctx context.Context, remotePath, localPath string, progress chan<- transport.Progress) (int64, error) {
	return transport.FallbackFetcher{T: t}.Fetch(ctx, remotePath, localPath, progress)
}

func selectLogFunc(d appDeps) browser.SelectFunc {
	return func(ctx context.Context, host string, entry ui.ScanEntry) (string, []string, error) {
		at := d.Now()
		tp := d.Transport
		if len(d.Host.Profile.SudoPrefix) > 0 {
			tp = &prefixedTransport{Transport: d.Transport, prefix: d.Host.Profile.SudoPrefix}
		}

		localPath, _, err := d.Local.Fetch(ctx, tp, d.State, host, entry.Path, entry.Size, at, nil)
		if err != nil && len(d.Host.Profile.SudoPrefix) == 0 && isRemotePermissionDenied(err) {
			sudo := &prefixedTransport{Transport: d.Transport, prefix: []string{"sudo", "-n"}}
			localPath, _, err = d.Local.Fetch(ctx, sudo, d.State, host, entry.Path, entry.Size, at, nil)
		}
		if err != nil {
			var confirm *local.ConfirmRequiredError
			if errors.As(err, &confirm) {
				return "", nil, fmt.Errorf("log is %d MB; files at or above 500 MB require a bounded-tail fetch", confirm.Size/(1024*1024))
			}
			return "", nil, err
		}

		data, err := os.ReadFile(localPath) //nolint:gosec // localPath was created under logpick's local store by Fetch above.
		if err != nil {
			return "", nil, fmt.Errorf("reading local snapshot %s: %w", localPath, err)
		}
		content := strings.TrimSuffix(string(data), "\n")
		if content == "" {
			return localPath, []string{}, nil
		}
		return localPath, strings.Split(content, "\n"), nil
	}
}

func browserSearchFunc(d appDeps) browser.SearchFunc {
	return func(ctx context.Context, path, query string, regex bool) ([]ui.SearchMatch, error) {
		matches, err := d.Search.Search(ctx, path, local.Query{Pattern: query, Regex: regex})
		if err != nil {
			return nil, err
		}
		out := make([]ui.SearchMatch, len(matches))
		for i, match := range matches {
			out[i] = ui.SearchMatch{Line: match.Line, Start: match.Start, End: match.End, Text: match.Text}
		}
		return out, nil
	}
}

func prefixRemoteCommand(prefix []string, cmd string) string {
	quoted := make([]string, len(prefix))
	for i, part := range prefix {
		quoted[i] = remote.Quote(part)
	}
	return strings.Join(quoted, " ") + " " + cmd
}

func isRemotePermissionDenied(err error) bool {
	var exitErr *transport.ExitError
	return errors.As(err, &exitErr) && strings.Contains(strings.ToLower(exitErr.Stderr), "permission denied")
}

// libraryDeleteFunc returns the library.DeleteFunc newApp wires: it
// removes the fetched file at the absolute path local from disk via
// d.Local.Delete, then drops the matching state.Fetch record via
// d.State.Load/Save - the read-modify-write internal/state's own doc
// comment names as the legitimate use of those two exported methods
// outside an internal/state operation, since that package exposes no
// RemoveFetch of its own.
func libraryDeleteFunc(d appDeps) library.DeleteFunc {
	return func(ctx context.Context, local string) error {
		if err := d.Local.Delete(local); err != nil {
			return fmt.Errorf("delete %s: %w", local, err)
		}

		st, err := d.State.Load()
		if err != nil {
			return err
		}

		kept := make([]state.Fetch, 0, len(st.Fetches))
		for _, f := range st.Fetches {
			if d.Local.AbsPath(f.Local) != local {
				kept = append(kept, f)
			}
		}
		st.Fetches = kept

		return d.State.Save(st)
	}
}

// libraryListFunc reads d.State's fetch history and converts it to
// []ui.FetchedFile, most recent first (ui.LibraryLoadedMsg's documented
// order), resolving each root-relative state.Fetch.Local to the absolute
// path ui.FetchedFile.Local documents via d.Local.AbsPath.
//
// Unlike hosts.ListFunc and library.DeleteFunc, this is not injected into
// any screen constructor: library.Model's own doc comment ("Loading")
// leaves reading state.toml to whatever composes the running application,
// so tuiModel's Init calls this directly rather than newApp wiring it
// through an Option. It lives here, next to libraryDeleteFunc, because it
// is the same kind of d.State/d.Local translation.
func libraryListFunc(d appDeps) func() ([]ui.FetchedFile, error) {
	return func() ([]ui.FetchedFile, error) {
		st, err := d.State.Load()
		if err != nil {
			return nil, err
		}

		files := make([]ui.FetchedFile, len(st.Fetches))
		for i, f := range st.Fetches {
			files[len(st.Fetches)-1-i] = ui.FetchedFile{
				Host:   f.Host,
				Remote: f.Remote,
				Local:  d.Local.AbsPath(f.Local),
				Bytes:  f.Bytes,
				At:     f.At,
			}
		}
		return files, nil
	}
}

// viewerSearchFunc returns the viewer.SearchFunc newApp wires: it runs
// d.Search.Search and converts each local.Match to the field-identical
// ui.SearchMatch (msg.go documents the two as meant to become type
// aliases of one another; until they are, this is the conversion).
func viewerSearchFunc(d appDeps) viewer.SearchFunc {
	return func(ctx context.Context, path, query string, regex bool) ([]ui.SearchMatch, error) {
		matches, err := d.Search.Search(ctx, path, local.Query{Pattern: query, Regex: regex})
		if err != nil {
			return nil, err
		}

		out := make([]ui.SearchMatch, len(matches))
		for i, m := range matches {
			out[i] = ui.SearchMatch{Line: m.Line, Start: m.Start, End: m.End, Text: m.Text}
		}
		return out, nil
	}
}

// previewInitialBuf and previewMaxBuf size execLines' bufio.Scanner, the
// same "no default Scanner buffer" fix remote/tail.go's Follow applies to
// its own reader (AGENTS.md section 9), since a preview line can be just
// as long as a followed one.
const (
	previewInitialBuf = 64 * 1024
	previewMaxBuf     = 1024 * 1024
)

// execLines runs cmd over t and returns its stdout split into lines,
// reading Stdout to completion before calling Wait, per Process's
// documented contract (see scan.go's drainScan and remote/tail.go's Follow
// for the same read-then-close-then-wait sequence).
func execLines(ctx context.Context, t transport.Transport, cmd string) ([]string, error) {
	proc, err := t.Exec(ctx, cmd)
	if err != nil {
		return nil, err
	}

	var lines []string
	scanner := bufio.NewScanner(proc.Stdout)
	scanner.Buffer(make([]byte, previewInitialBuf), previewMaxBuf)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	scanErr := scanner.Err()

	stdoutErr := proc.Stdout.Close()
	stderrErr := proc.Stderr.Close()
	waitErr := proc.Wait()

	if scanErr != nil {
		return nil, scanErr
	}
	if waitErr != nil {
		return nil, waitErr
	}
	if stdoutErr != nil {
		return nil, stdoutErr
	}
	if stderrErr != nil {
		return nil, stderrErr
	}

	return lines, nil
}

// scanRun execs the find command remote.BuildScan builds for
// d.Host.Profile.Scan and gnuFind, and streams every remote.Entry
// remote.ParseScan produces on entries as it parses them, sending the
// final remote.Report on report exactly once before closing both channels.
//
// It is shaped for ui.SafeGo - func(context.Context) error - so the
// goroutine it runs in is cancellable via ctx and any panic inside it is
// recovered onto an error channel rather than taking the process down
// (AGENTS.md invariant 6), the identical shape remote.Follow's own run
// func uses (remote/tail.go's package doc comment) and for the same
// reason: this package cannot call ui.SafeGo without inverting the
// dependency direction of DESIGN.md section 5.
//
// remote.BuildScan returning "" - d.Host.Profile.Scan has no paths - is
// nothing to scan, per BuildScan's documented contract: scanRun must close
// both channels with a zero remote.Report and return nil without ever
// calling d.Transport.Exec, the same rule runScan (scan.go) already
// follows for the non-TUI path.
func scanRun(d appDeps, gnuFind bool, entries chan<- remote.Entry, report chan<- remote.Report) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		cmd := remote.BuildScan(d.Host.Profile.Scan, gnuFind)
		if cmd == "" {
			close(entries)
			report <- remote.Report{}
			close(report)
			return nil
		}

		proc, err := d.Transport.Exec(ctx, cmd)
		if err != nil {
			close(entries)
			report <- remote.Report{}
			close(report)
			return fmt.Errorf("scanning %s: %w", d.Host.Name, err)
		}

		// ParseScan owns entries as its only sender and closes it before
		// returning (ParseScan's own doc comment, "Streaming and the out
		// channel"), so nothing here closes entries itself.
		rep, parseErr := remote.ParseScan(proc.Stdout, gnuFind, entries)

		stdoutErr := proc.Stdout.Close()
		stderrErr := proc.Stderr.Close()
		waitErr := proc.Wait()

		report <- rep
		close(report)

		if parseErr != nil {
			return fmt.Errorf("scanning %s: %w", d.Host.Name, parseErr)
		}
		if waitErr != nil {
			return fmt.Errorf("scanning %s: %w", d.Host.Name, waitErr)
		}
		if stdoutErr != nil {
			return fmt.Errorf("scanning %s: %w", d.Host.Name, stdoutErr)
		}
		if stderrErr != nil {
			return fmt.Errorf("scanning %s: %w", d.Host.Name, stderrErr)
		}
		return nil
	}
}

// scanDrainCmd returns a command that drains entries for one batch of
// results already available - blocking for at least the first entry, then
// taking whatever more is immediately ready without blocking again - and
// reports the batch as a ui.ScanEntriesMsg for host. Once entries is
// closed, the next call instead takes the single, final tally off report
// and reports it as a ui.ScanDoneMsg; scanRun's contract guarantees
// exactly one value is sent on report by the time entries closes, so this
// never blocks waiting for it.
//
// A command returns exactly one message (DESIGN.md 9.3), so whatever
// composes the running application reissues scanDrainCmd on every
// ui.ScanEntriesMsg it receives to keep the drain going, and stops
// reissuing once the ui.ScanDoneMsg arrives - the same self-reissuing
// shape ui.WaitForLines documents for follow mode, applied here as its own
// function because ui.WaitForLines is typed for a channel of []string and
// a scan streams remote.Entry one at a time, not pre-batched lines.
func scanDrainCmd(host string, entries <-chan remote.Entry, report <-chan remote.Report) tea.Cmd {
	return func() tea.Msg {
		first, ok := <-entries
		if !ok {
			rep := <-report
			return ui.ScanDoneMsg{
				Host:      host,
				Count:     rep.Count,
				Skipped:   rep.Skipped,
				Truncated: rep.Truncated,
			}
		}

		batch := []ui.ScanEntry{toUIScanEntry(first)}
		for {
			select {
			case e, ok := <-entries:
				if !ok {
					return ui.ScanEntriesMsg{Host: host, Entries: batch}
				}
				batch = append(batch, toUIScanEntry(e))
			default:
				return ui.ScanEntriesMsg{Host: host, Entries: batch}
			}
		}
	}
}

// toUIScanEntry converts a remote.Entry to the field-identical ui.ScanEntry
// (msg.go documents the two as meant to become type aliases of one
// another; until they are, this is the conversion).
func toUIScanEntry(e remote.Entry) ui.ScanEntry {
	return ui.ScanEntry{Path: e.Path, Size: e.Size, ModTime: e.ModTime}
}

// followCmd starts a follow stream for path on host: it calls
// remote.Follow over d.Transport and, on success, hands the run func
// remote.Follow returns straight to ui.SafeGo - exactly the shape SafeGo
// takes, per remote/tail.go's package doc comment - so the goroutine that
// pumps the stream is cancellable via ctx and any panic inside it is
// recovered onto errc rather than taking the process down (AGENTS.md
// invariant 6). The returned tea.Cmd reports a ui.FollowStartedMsg;
// draining the channel remote.Follow returns from there on is
// ui.WaitForLines' job, reissued by the caller on every ui.LinesMsg it
// receives, the same way scanDrainCmd is reissued on every
// ui.ScanEntriesMsg.
//
// followCmd returns a non-nil error immediately, before ui.SafeGo runs
// anything, when d.Transport.Caps().Follow is false: remote.Follow itself
// refuses in that case, wrapping transport.ErrNotSupported (AGENTS.md
// invariant 7), and followCmd passes that error straight back rather than
// folding it into a tea.Cmd that would report it as an ordinary
// ui.ErrorMsg. The caller - not this func - decides how a refusal to even
// start is surfaced to the user, the same as any other error this layer
// returns directly instead of wrapping in a message.
func (d appDeps) followCmd(ctx context.Context, errc chan<- error, host, path string) (tea.Cmd, error) {
	_, run, err := remote.Follow(ctx, d.Transport, path, time.After)
	if err != nil {
		return nil, err
	}

	ui.SafeGo(ctx, errc, run)

	return func() tea.Msg {
		return ui.FollowStartedMsg{Host: host, Path: path}
	}, nil
}

// wireTUI attaches the root TUI command's flags and RunE to root, so
// invoking `logpick`, optionally with a host argument, launches the
// interactive application instead of leaving the root command bare. It
// mirrors newScanCmd's flag set (scan.go) - profile, path, mock, config
// and state - since both commands resolve a host's profile the same way;
// --mock in particular is required verbatim: serve a fixture directory
// through transport.NewMock instead of any real backend, no network.
//
// newRootCmd (main.go) calls this once, alongside AddCommand(newScanCmd()).
//
// RunE's body is the substantive orchestration: it resolves config,
// builds the initial appDeps (--mock's transport.Mock, or a real
// transport.NewCommand, bound to the optional host argument or "" when
// none is given), calls newApp, wraps the result in tuiModel (see this
// file's package doc comment) and runs it under tea.NewProgram. When a
// host argument is given, RunE also has tuiModel synthesize a
// ui.HostSelectedMsg for it on entry, so `logpick host` jumps straight
// into browsing that host instead of stopping at the picker.
func wireTUI(root *cobra.Command) {
	var (
		profileFlag string
		pathFlags   []string
		mockDir     string
		configPath  string
		statePath   string
	)

	root.Args = cobra.MaximumNArgs(1)
	root.RunE = func(cmd *cobra.Command, args []string) (err error) {
		var host string
		if len(args) == 1 {
			host = args[0]
		}

		var ov config.Overrides
		if cmd.Flags().Changed("profile") {
			ov.Profile = &profileFlag
		}
		if cmd.Flags().Changed("path") {
			ov.Paths = &pathFlags
		}

		writeConfigPath := configPath
		if writeConfigPath == "" {
			writeConfigPath = defaultConfigPath()
		}

		// Read the default config when it exists. The first-run flow writes
		// this path, so ignoring it on the next launch would make the generated
		// host profile unusable.
		var cfg *config.Config
		loaded, loadErr := config.Load(writeConfigPath)
		if loadErr == nil {
			cfg = loaded
		} else if configPath != "" || !errors.Is(loadErr, os.ErrNotExist) {
			return loadErr
		}

		resolved, err := config.Resolve(cfg, host, ov)
		if err != nil {
			return err
		}

		newTransport := func(host string, profile config.Profile) transport.Transport {
			if mockDir != "" {
				return transport.NewMock(mockDir)
			}
			return transport.NewCommand(host, profile)
		}

		stateStore := state.New(statePath)
		storedState, stateErr := stateStore.Load()
		if stateErr != nil {
			return stateErr
		}
		themePrimary, themeSecondary := "", ""
		if cfg != nil {
			themePrimary = cfg.Theme.Primary
			themeSecondary = cfg.Theme.Secondary
		}
		if storedState.Theme.Configured {
			themePrimary = storedState.Theme.Primary
			themeSecondary = storedState.Theme.Secondary
		}

		localRoot := defaultLocalRoot()
		if mkdirErr := os.MkdirAll(localRoot, 0o700); mkdirErr != nil {
			return fmt.Errorf("creating local log store %s: %w", localRoot, mkdirErr)
		}
		if chmodErr := os.Chmod(localRoot, 0o700); chmodErr != nil {
			return fmt.Errorf("securing local log store %s: %w", localRoot, chmodErr)
		}

		base := appDeps{
			NewTransport:   newTransport,
			Transport:      newTransport(host, resolved.Profile),
			State:          stateStore,
			Local:          local.New(localRoot),
			Search:         local.Choose(""),
			Host:           resolved,
			Now:            time.Now,
			Config:         cfg,
			ThemePrimary:   themePrimary,
			ThemeSecondary: themeSecondary,
			ConfigPath:     writeConfigPath,
		}

		app, err := newApp(base)
		if err != nil {
			return err
		}

		resolveHost := func(host, profileName string) (appDeps, error) {
			// Reload here because the first-run hosts flow may have just written
			// this file after the application was constructed.
			currentCfg := cfg
			loaded, loadErr := config.Load(writeConfigPath)
			if loadErr == nil {
				currentCfg = loaded
			} else if !errors.Is(loadErr, os.ErrNotExist) {
				return appDeps{}, loadErr
			}

			hov := historyProfileOverrides(profileName)
			hostResolved, resolveErr := config.Resolve(currentCfg, host, hov)
			if resolveErr != nil {
				return appDeps{}, resolveErr
			}

			nd := base
			nd.Config = currentCfg
			nd.Host = hostResolved
			nd.Transport = newTransport(host, hostResolved.Profile)
			return nd, nil
		}

		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		wrapped := newTUIModel(app, base, resolveHost, ctx)
		if host != "" {
			wrapped.initialHost = host
			wrapped.initialProfile = resolved.ProfileName
		}

		defer func() {
			if r := recover(); r != nil {
				// tea.Program.Run already restores the terminal before
				// returning or before a caught panic reaches here
				// (bubbletea's own recover, which this call never
				// disables via WithoutCatchPanics); this recover exists
				// so a panic that still escapes all of that - one in
				// RunE's own setup, not in the Update/View loop - is
				// re-raised through Go's normal panic machinery instead
				// of being silently absorbed by cobra (DESIGN.md 9.6,
				// AGENTS.md invariant 6).
				panic(r)
			}
		}()

		p := tea.NewProgram(wrapped, tea.WithContext(ctx), tea.WithAltScreen())
		finalModel, runErr := p.Run()
		if tm, ok := finalModel.(tuiModel); ok && tm.active.Transport != nil {
			_ = tm.active.Transport.Close()
		}
		return runErr
	}

	root.Flags().StringVar(&profileFlag, "profile", "", "override the resolved profile")
	root.Flags().StringArrayVar(&pathFlags, "path", nil, "override the resolved scan paths (repeatable)")
	root.Flags().StringVar(&mockDir, "mock", "", "serve this fixture directory instead of a real transport, no network")
	root.Flags().StringVar(&configPath, "config", "", "path to config.toml (default: built-in defaults only)")
	root.Flags().StringVar(&statePath, "state", defaultStatePath(), "path to state.toml")
}

// historyProfileOverrides translates a profile stored in host history back
// into resolution overrides. "default" must remain implicit: it may name the
// built-in fallback even when no [profile.default] exists in config.toml.
func historyProfileOverrides(profileName string) config.Overrides {
	if profileName == "" || profileName == config.DefaultProfileName {
		return config.Overrides{}
	}
	return config.Overrides{Profile: &profileName}
}

// defaultConfigPath returns $XDG_CONFIG_HOME/logpick/config.toml, falling
// back to ~/.config/logpick/config.toml per DESIGN.md section 6 when
// XDG_CONFIG_HOME is unset, mirroring defaultStatePath (scan.go) for the
// config file instead of the state file. It is what the hosts screen's
// first-run flow (hosts.WriteConfigFile) writes to when the user never
// passed --config: config.toml is not left unwritable just because it was
// never named explicitly, the same way --state already defaults instead
// of requiring the flag.
func defaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "logpick", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.toml"
	}
	return filepath.Join(home, ".config", "logpick", "config.toml")
}

// defaultLocalRoot returns $XDG_DATA_HOME/logpick/fetched, falling back to
// ~/.local/share/logpick/fetched per DESIGN.md 10.1 when XDG_DATA_HOME is
// unset, the local.Store root every fetch and the library screen's
// DeleteFunc operate under. There is no --local flag (wireTUI's flag set
// mirrors newScanCmd's exactly), so this is the only source for it.
func defaultLocalRoot() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "logpick", "fetched")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "fetched"
	}
	return filepath.Join(home, ".local", "share", "logpick", "fetched")
}

// tuiModel wraps ui.App to close the composition gap this file's package
// doc comment describes: ui.App cannot rebuild a screen in response to a
// message, and browser.Model binds its host at construction, so nothing in
// internal/ui has anywhere for ui.HostSelectedMsg - the user picking a
// host at runtime - to land. tuiModel is deliberately not part of
// internal/ui, which DESIGN.md 9.2 keeps free of transport.Transport and
// config.Resolve; it lives here because deciding how those fit together
// with the screen packages is composition, which cmd/logpick owns.
//
// It is not redundant with ui.App: ui.App still owns rendering, the four
// things its own Update handles (window size, screen transitions, the
// error banner, quit/help), and routing every other message to whichever
// screen is active. tuiModel only adds the one thing that requires
// transport and config knowledge - rebuilding the browser screen and
// starting its scan for a newly selected host - and otherwise passes
// every message straight through to the wrapped App.
//
// # No test covers this type
//
// tui_test.go tests newApp, scanRun, scanDrainCmd and appDeps.followCmd in
// isolation, over channels and deps a test owns directly; none of it
// drives a tea.Program or feeds tuiModel a message. tuiModel exists
// because the task is "the TUI is launchable", not because a test demands
// it - wireTUI's RunE is what actually runs it, and the coverage for it is
// exactly what `go build`, `go vet` and running the binary manually give:
// this file compiles, wireTUI wires it in, and `logpick --mock <dir>`
// launches a hosts screen that can pick a host and reach a scanning
// browser. See this task's final report for the exact list of what is and
// is not exercised by an automated test.
type tuiModel struct {
	// app is the wrapped root model. Every message not specially handled
	// below goes straight to app.Update, and app.View is exactly this
	// model's own View.
	app ui.App

	// resolve rebuilds appDeps for an arbitrary (host, profileName): a
	// fresh config.ResolvedHost and a transport.Transport bound to it
	// (transport.NewCommand, or transport.NewMock when --mock was given -
	// wireTUI's RunE closes over that choice). State, Local, Search,
	// Config and ConfigPath are carried over unchanged from the appDeps
	// newApp was originally built with; only Host and Transport differ
	// per call, since those are the two fields DESIGN.md's browser and
	// scan wiring bind to one host at construction.
	resolve func(host, profileName string) (appDeps, error)

	// active is the appDeps the currently installed browser screen and
	// any in-flight scan for it were built from. wireTUI's RunE closes
	// the transport it names once tea.Program.Run returns.
	active appDeps

	// ctx governs every goroutine this wrapper starts through ui.SafeGo
	// (a scan or a follow): it is the program's own context, cancelled by
	// wireTUI's RunE when tea.Program.Run returns, so nothing outlives
	// the running program.
	ctx context.Context

	// errc is drained by ui.WaitForError into ui.ErrorMsg, the channel
	// every ui.SafeGo call in this wrapper reports a panic or failure on.
	errc chan error

	// scanEntries, scanReport and scanCancel belong to the scan currently
	// backing the active browser screen, replaced together every time a
	// fresh ui.HostSelectedMsg starts a new one. scanCancel is nil when no
	// scan has ever been started.
	scanEntries chan remote.Entry
	scanReport  chan remote.Report
	scanCancel  context.CancelFunc

	// initialHost and initialProfile, when initialHost is non-empty, make
	// Init synthesize a ui.HostSelectedMsg for them, so a `logpick <host>`
	// invocation jumps straight into browsing that host instead of
	// stopping at the picker (wireTUI's RunE sets these after
	// newTUIModel, since they depend on the CLI's optional host
	// argument).
	initialHost    string
	initialProfile string
}

// newTUIModel returns a tuiModel wrapping app, with resolve and ctx wired
// as tuiModel's own fields document, and no scan or follow yet running.
// active starts as base, the same appDeps app itself was built from, so
// wireTUI's RunE has something to Close on exit even if the user never
// picks a host at all.
func newTUIModel(app ui.App, base appDeps, resolve func(host, profileName string) (appDeps, error), ctx context.Context) tuiModel {
	return tuiModel{
		app:     app,
		resolve: resolve,
		active:  base,
		ctx:     ctx,
		errc:    make(chan error, 16),
	}
}

// Init starts the wrapped App (which loads the host history, since the
// hosts screen is always the one newApp leaves active) alongside the
// library screen's load - library.Model's own doc comment leaves that to
// whatever composes the application, unlike hosts.Model, which loads
// itself - and the error-channel drain that keeps ui.SafeGo failures
// flowing into the event loop for the life of the program. When
// initialHost is set, it also synthesizes the ui.HostSelectedMsg that
// jumps straight into browsing it.
func (m tuiModel) Init() tea.Cmd {
	cmds := []tea.Cmd{m.app.Init(), loadLibraryCmd(m.active), ui.WaitForError(m.errc)}

	if m.initialHost != "" {
		host, profile := m.initialHost, m.initialProfile
		cmds = append(cmds, func() tea.Msg {
			return ui.HostSelectedMsg{Host: host, Profile: profile}
		})
	}

	return tea.Batch(cmds...)
}

// loadLibraryCmd returns the tea.Cmd that reads d's fetch history and
// reports it as a ui.LibraryLoadedMsg, or a ui.ErrorMsg on failure -
// exactly the shape hosts.Model.Init documents for its own ListFunc load,
// applied here for the library screen since library.Model does not do
// this itself (see that type's "Loading" doc section).
func loadLibraryCmd(d appDeps) tea.Cmd {
	list := libraryListFunc(d)
	return func() tea.Msg {
		files, err := list()
		if err != nil {
			return ui.ErrorMsg{Err: fmt.Errorf("loading fetch history: %w", err)}
		}
		return ui.LibraryLoadedMsg{Files: files}
	}
}

// Update handles ui.HostSelectedMsg, the one message this wrapper exists
// for, plus the bookkeeping needed to keep the scan drain and the error
// channel flowing (scanDrainCmd and ui.WaitForError are both self-
// reissuing commands, per their own doc comments, and something has to be
// the caller that reissues them). Every other message is routed straight
// to the wrapped App's own Update, unchanged.
func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.HostSelectedMsg:
		return m.selectHost(msg)

	case ui.ThemeChangedMsg:
		next, cmd := m.routeToApp(msg)
		next.active.ThemePrimary = msg.Primary
		next.active.ThemeSecondary = msg.Secondary
		return next, cmd

	case ui.PathScanRequestedMsg:
		return m.scanPath(msg)

	case scanReadyMsg:
		return m.startScan(msg)

	case ui.ScanEntriesMsg:
		next, cmd := m.routeToApp(msg)
		return next, tea.Batch(cmd, scanDrainCmd(msg.Host, m.scanEntries, m.scanReport))

	case ui.PreviewMsg:
		next, cmd := m.routeToApp(msg)
		next.app.Err = nil
		return next, cmd

	case ui.ErrorMsg:
		next, cmd := m.routeToApp(msg)
		return next, tea.Batch(cmd, ui.WaitForError(m.errc))

	default:
		return m.routeToApp(msg)
	}
}

// routeToApp forwards msg to the wrapped App's Update and folds the result
// back into a copy of m, the same value-model style ui.App and every
// screen package already follow.
func (m tuiModel) routeToApp(msg tea.Msg) (tuiModel, tea.Cmd) {
	updated, cmd := m.app.Update(msg)
	app, ok := updated.(ui.App)
	if !ok {
		// ui.App.Update is documented to always return an App value from
		// itself; this branch only guards against that contract changing
		// out from under this file without a compile break, so state is
		// never silently lost rather than merely returning wrong Views.
		return m, cmd
	}
	next := m
	next.app = app
	return next, cmd
}

// selectHost is the composition gap this whole file exists to close (see
// tuiModel's own doc comment): it resolves a fresh appDeps for msg.Host
// and msg.Profile, builds a browser.Model bound to it, installs that
// screen and makes it active, records the connect into state, loads
// whatever scan listing is already cached for the host so the browser has
// something to show immediately (DESIGN.md 8.3), and starts a fresh scan
// (resolving the host's find dialect via resolveGNUFind, shared with
// scan.go rather than duplicated) whose drain scanDrainCmd is reissued by
// the ui.ScanEntriesMsg case in Update above.
// scanReadyMsg is an internal message sent once the blocking
// resolveGNUFind probe completes, carrying everything selectHost needs
// to start the scan and its drain.  Moving the probe into a tea.Cmd
// keeps Update non-blocking so the TUI stays responsive while SSH
// connects.
type scanReadyMsg struct {
	Host    string
	Deps    appDeps
	GNUFind bool
}

func (m tuiModel) selectHost(msg ui.HostSelectedMsg) (tea.Model, tea.Cmd) {
	newDeps, err := m.resolve(msg.Host, msg.Profile)
	if err != nil {
		return m, errCmd(fmt.Errorf("resolving host %q: %w", msg.Host, err))
	}
	newDeps.ThemePrimary = m.app.ThemePrimary
	newDeps.ThemeSecondary = m.app.ThemeSecondary

	if m.scanCancel != nil {
		m.scanCancel()
	}

	browserScreen := browser.New(
		newDeps.Host.Name,
		previewFunc(newDeps),
		browser.WithSelector(selectLogFunc(newDeps)),
		browser.WithSearch(browserSearchFunc(newDeps)),
		browser.WithTheme(ui.DefaultTheme().WithColors(newDeps.ThemePrimary, newDeps.ThemeSecondary)),
		browser.WithContextFunc(func() (context.Context, context.CancelFunc) {
			return context.WithCancel(m.ctx)
		}),
	)

	next := m
	next.active = newDeps
	next.app.Screens[ui.ScreenBrowser] = browserScreen
	next.app.Active = ui.ScreenBrowser
	next.app.Err = nil

	// Resize the newly installed browser screen so it has dimensions.
	if next.app.Width > 0 && next.app.Height > 0 {
		next.app.Screens[ui.ScreenBrowser] = next.app.Screens[ui.ScreenBrowser].Resize(
			next.app.Width, ui.ContentHeight(next.app.Height),
		)
	}

	// resolveGNUFind does blocking I/O (state load + SSH probe), so run
	// it in a tea.Cmd rather than inline in Update.
	ctx := m.ctx
	host := msg.Host
	probeCmd := func() tea.Msg {
		gnuFind, err := resolveGNUFind(ctx, newDeps.scanDeps(), host)
		if err != nil {
			return ui.ErrorMsg{Err: fmt.Errorf("probing %q: %w", host, err)}
		}
		return scanReadyMsg{Host: host, Deps: newDeps, GNUFind: gnuFind}
	}

	connectAt := newDeps.Now()
	cmds := []tea.Cmd{
		cachedScanCmd(newDeps, host),
		func() tea.Msg { return ui.ScanStartedMsg{Host: host} },
		probeCmd,
		func() tea.Msg {
			if err := newDeps.State.RecordConnect(host, newDeps.Host.ProfileName, connectAt); err != nil {
				return ui.ErrorMsg{Err: fmt.Errorf("recording connect to %q: %w", host, err)}
			}
			return nil
		},
	}

	return next, tea.Batch(cmds...)
}

func (m tuiModel) scanPath(msg ui.PathScanRequestedMsg) (tea.Model, tea.Cmd) {
	if msg.Host != m.active.Host.Name || strings.TrimSpace(msg.Path) == "" {
		return m, nil
	}
	if m.scanCancel != nil {
		m.scanCancel()
	}

	deps := m.active
	deps.Host.Profile.Scan.Paths = []string{strings.TrimSpace(msg.Path)}
	host := msg.Host
	ctx := m.ctx
	probeCmd := func() tea.Msg {
		gnuFind, err := resolveGNUFind(ctx, deps.scanDeps(), host)
		if err != nil {
			return ui.ErrorMsg{Err: fmt.Errorf("probing %q: %w", host, err)}
		}
		return scanReadyMsg{Host: host, Deps: deps, GNUFind: gnuFind}
	}

	next := m
	next.app.Err = nil
	return next, tea.Batch(
		func() tea.Msg { return ui.PathScanStartedMsg{Host: host, Path: msg.Path} },
		probeCmd,
	)
}

// startScan handles scanReadyMsg: now that the blocking probe has
// completed asynchronously, it creates the scan channels, launches the
// scan goroutine, and starts draining entries into the browser.
func (m tuiModel) startScan(msg scanReadyMsg) (tea.Model, tea.Cmd) {
	// A slower probe for a previously selected host may finish after the user
	// has selected another one. Never let that stale result replace the active
	// host's scan channels.
	if msg.Host != m.active.Host.Name {
		return m, nil
	}

	if m.scanCancel != nil {
		m.scanCancel()
	}

	scanCtx, cancel := context.WithCancel(m.ctx)

	next := m
	next.scanCancel = cancel

	entries := make(chan remote.Entry)
	report := make(chan remote.Report, 1)
	next.scanEntries = entries
	next.scanReport = report

	ui.SafeGo(scanCtx, next.errc, scanRun(msg.Deps, msg.GNUFind, entries, report))

	return next, scanDrainCmd(msg.Host, entries, report)
}

// errCmd returns a tea.Cmd that reports err as a ui.ErrorMsg.
func errCmd(err error) tea.Cmd {
	return func() tea.Msg { return ui.ErrorMsg{Err: err} }
}

// cachedScanCmd returns the tea.Cmd that loads whatever scan listing
// d.State already has cached for host and reports it as a
// ui.CachedScanMsg, so the browser shows something immediately instead of
// an empty pane while the fresh scan streams in (DESIGN.md 8.3;
// browser.Model's own "Scope note" leaves this to whatever composes the
// application). A host with no cached listing, or no history at all,
// reports an empty ui.CachedScanMsg rather than an error: an empty cache
// is not a failure.
func cachedScanCmd(d appDeps, host string) tea.Cmd {
	return func() tea.Msg {
		st, err := d.State.Load()
		if err != nil {
			return ui.ErrorMsg{Err: fmt.Errorf("loading cached scan for %q: %w", host, err)}
		}

		for _, h := range st.Hosts {
			if h.Name != host {
				continue
			}
			entries := make([]ui.ScanEntry, len(h.Cache.Entries))
			for i, e := range h.Cache.Entries {
				entries[i] = ui.ScanEntry{Path: e.Path, Size: e.Size, ModTime: time.Unix(e.Mtime, 0).UTC()}
			}
			return ui.CachedScanMsg{Host: host, Entries: entries, ScannedAt: h.Cache.ScannedAt}
		}

		return ui.CachedScanMsg{Host: host}
	}
}

// View renders the wrapped App unchanged; tuiModel adds no chrome of its
// own.
func (m tuiModel) View() string {
	return m.app.View()
}
