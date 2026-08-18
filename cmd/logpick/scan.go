package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/pedreviljoen/logpick/internal/config"
	"github.com/pedreviljoen/logpick/internal/remote"
	"github.com/pedreviljoen/logpick/internal/state"
	"github.com/pedreviljoen/logpick/internal/transport"
)

// deps carries runScan's dependencies as explicit fields instead of
// package-level state. That is what makes runScan testable without a
// network or a spawned process, and AGENTS.md section 4.1 names runScan
// as this project's motivating example for the pattern.
//
// A caller builds one deps value per invocation. Tests construct it
// directly, wiring Transport to transport.NewMock over
// internal/transport/testdata/fixtures and Store to a *state.Store over
// t.TempDir(). newScanCmd (below) instead builds the profile's real
// backend (transport.NewCommand, or --mock's Mock) and a Store over a
// real state.toml path.
type deps struct {
	// Transport is the backend runScan issues every Exec call against:
	// the capability probe ("find --version") when the host's find
	// dialect is not yet cached, and the discovery scan itself
	// (remote.BuildScan's output). Never nil.
	Transport transport.Transport

	// Store is where the capability probe's result is cached
	// (state.Store.UpsertCaps) and read back on a later call against
	// the same host (T08). Never nil.
	Store *state.Store

	// Host is the fully resolved profile for the host being scanned
	// (config.Resolve's output, T07). Profile.Scan drives
	// remote.BuildScan, and Profile.AuthHint is what runScan writes to
	// w when the scan's Exec call returns an error wrapping
	// transport.ErrAuthRequired.
	Host config.ResolvedHost

	// Now returns the current time. Every timestamp runScan writes to
	// Store - the capability probe's ProbedAt - comes from calling
	// Now, never from time.Now directly, so a test controls it and
	// never sleeps (AGENTS.md section 5). newScanCmd wires this to
	// time.Now; tests wire it to a fixed clock.
	Now func() time.Time
}

// runScan scans host's configured paths (d.Host.Profile.Scan) over
// d.Transport and writes a table of what it found to w, sorted so the
// most recently written log is first. It is the implementation behind the
// non-TUI `logpick scan` subcommand (T11).
//
// Every I/O runScan performs goes through d.Transport, d.Store and
// d.Now, and ctx governs all of it: cancelling ctx must stop an
// in-flight Exec the same way it would for any other caller of
// transport.Transport.
//
// # Capability probe
//
// DESIGN.md 8.2: GNU find's -printf output (remote.BuildScan's "gnu"
// clause) is what makes size and mtime come back in one round trip, but
// -printf is GNU-only, so the host's dialect is probed once and cached
// rather than re-probed on every scan.
//
// runScan reads host's cached state.Caps from d.Store. Per state.Caps'
// doc comment, the zero Caps means "never probed": ProbedAt is the zero
// time.Time, so that is the signal runScan checks, not any boolean
// default.
//
//   - If ProbedAt is zero, runScan runs "find --version" over
//     d.Transport.Exec and treats the outcome exactly the way DESIGN.md
//     8.2's shell probe would: the command succeeding (Process.Wait
//     returns nil) means GNU, any non-nil error at all - including one
//     wrapping transport.ErrAuthRequired - means not GNU, mirroring
//     `find --version >/dev/null 2>&1 && echo gnu || echo other`. An
//     auth failure surfacing from this specific probe is therefore never
//     surfaced to the caller of runScan; it degenerates to "not GNU"
//     like any other probe failure, the same as the shell one-liner it
//     mirrors would. The result is written back with
//     d.Store.UpsertCaps(host, ..., d.Now()), so a later call against
//     the same host finds ProbedAt already set (mechanic 2).
//   - If ProbedAt is already non-zero, runScan uses the cached GNUFind
//     value and issues no "find --version" Exec call at all (mechanic
//     3).
//
// # Nothing to scan
//
// remote.BuildScan(d.Host.Profile.Scan, gnuFind) returns "" when the
// resolved scan spec has no paths (T09's documented contract). runScan
// must honour that exactly as BuildScan requires: skip
// d.Transport.Exec entirely rather than run a pathless find, write a
// line to w saying there is nothing configured to scan, and return nil.
//
// # Running the scan
//
// Otherwise runScan calls d.Transport.Exec(ctx, cmd) with BuildScan's
// output.
//
// An error from that call wrapping transport.ErrAuthRequired (matched
// with errors.Is, DESIGN.md 7.5) is not surfaced as a stack trace:
// runScan writes a line to w naming d.Host.Profile.AuthHint and returns
// an error that still wraps transport.ErrAuthRequired, so a caller can
// still match it while the text a user sees is the operator-meaningful
// hint from their own config (mechanic 4). If AuthHint is empty - a
// profile that never set one - runScan writes a fallback line stating
// that authentication is required and no auth_hint is configured for
// the profile, rather than writing nothing.
//
// Any other error from this Exec call is wrapped with d.Host.Name for
// context and returned; runScan writes nothing to w beyond what it may
// already have written for the probe.
//
// # Streaming the result
//
// remote.ParseScan reads Process.Stdout and sends entries on a channel
// as it parses them, closing that channel itself before it returns
// (T10); a non-nil channel that nobody drains deadlocks ParseScan the
// moment its internal buffering fills, since sending on it is a blocking
// operation. runScan must run ParseScan and the code draining its
// output channel concurrently - typically ParseScan in a goroutine, with
// another goroutine or the caller itself ranging over the channel to
// collect entries - never call ParseScan and only then read the
// channel. Process.Stdout must be read to completion, which draining the
// channel to close does, before Process.Wait is called.
//
// # Sorting
//
// Entries are sorted by ModTime descending - the newest log first, the
// "whole ergonomic win" DESIGN.md 8.1 describes - using a stable sort
// (sort.SliceStable or slices.SortStableFunc, never sort.Slice) so two
// entries with equal ModTime do not reorder arbitrarily between runs.
// T10 leaves ModTime as the zero time.Time for every BSD-parsed entry,
// since ls -ldn's date column has no year to recover a real timestamp
// from, so on a non-GNU host every entry ties at the zero time and needs
// a deterministic secondary order: runScan breaks a tie by Path
// ascending.
//
// # Rendering
//
// The table has one row per entry, at minimum the path, a
// human-readable size, and the modification time. DESIGN.md 8.3 is the
// reason the table renders sizes this way at all: a size such as
// 88213441 is shown as "84M", never the raw byte count. A BSD-parsed
// entry's zero ModTime is rendered as something visibly not a real date
// (see "Sorting" above), since it is not one.
//
// remote.Report's Truncated and Skipped fields are surfaced visibly
// once parsing finishes, not folded silently into the row count:
// Truncated true means the 10,000-row cap (DESIGN.md 8.3) was hit and
// the listing is incomplete, and Skipped > 0 means that many lines could
// not be parsed and were dropped (permission-denied noise and similar,
// per remote.ParseScan's doc comment). Both, when applicable, are
// written to w as visible warnings.
//
// # Errors
//
// runScan returns a non-nil error, wrapped with fmt.Errorf and
// d.Host.Name for context, for: an Exec failure other than
// ErrAuthRequired (handled separately above), a d.Store failure reading
// or writing capabilities, or a ParseScan read error. The
// ErrAuthRequired case is the one exception: it is returned wrapping
// transport.ErrAuthRequired specifically, so errors.Is still matches it.
func runScan(ctx context.Context, d deps, host string, w io.Writer) error {
	gnuFind, err := resolveGNUFind(ctx, d, host)
	if err != nil {
		return fmt.Errorf("%s: %w", d.Host.Name, err)
	}

	cmd := remote.BuildScan(d.Host.Profile.Scan, gnuFind)
	if cmd == "" {
		_, _ = fmt.Fprintf(w, "nothing configured to scan for %s\n", d.Host.Name)
		return nil
	}

	proc, err := d.Transport.Exec(ctx, cmd)
	if err != nil {
		if errors.Is(err, transport.ErrAuthRequired) {
			hint := d.Host.Profile.AuthHint
			if hint != "" {
				_, _ = fmt.Fprintf(w, "authentication required: %s\n", hint)
			} else {
				_, _ = fmt.Fprintln(w, "authentication is required and no auth_hint is configured for this profile")
			}
			return fmt.Errorf("%s: %w", d.Host.Name, err)
		}
		return fmt.Errorf("%s: %w", d.Host.Name, err)
	}

	entries, report, err := drainScan(proc, gnuFind)
	if err != nil {
		return fmt.Errorf("%s: %w", d.Host.Name, err)
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].ModTime.Equal(entries[j].ModTime) {
			return entries[i].ModTime.After(entries[j].ModTime)
		}
		return entries[i].Path < entries[j].Path
	})

	renderTable(w, entries)

	if report.Truncated {
		_, _ = fmt.Fprintln(w, "warning: scan truncated at 10000 entries; the listing is incomplete")
	}
	if report.Skipped > 0 {
		_, _ = fmt.Fprintf(w, "warning: %d line(s) could not be parsed and were skipped\n", report.Skipped)
	}

	return nil
}

// resolveGNUFind returns whether host's remote find is the GNU dialect,
// consulting d.Store's cached state.Caps first (state.Caps.ProbedAt.IsZero()
// is the "never probed" signal) and, only when nothing is cached yet,
// running the "find --version" probe once and caching the result via
// d.Store.UpsertCaps(host, ..., d.Now()). See runScan's "Capability probe"
// doc section for the exact contract this implements.
func resolveGNUFind(ctx context.Context, d deps, host string) (bool, error) {
	st, err := d.Store.Load()
	if err != nil {
		return false, err
	}

	var caps state.Caps
	for _, h := range st.Hosts {
		if h.Name == host {
			caps = h.Caps
			break
		}
	}

	if !caps.ProbedAt.IsZero() {
		return caps.GNUFind, nil
	}

	gnuFind := probeGNUFind(ctx, d.Transport)
	if err := d.Store.UpsertCaps(host, state.Caps{GNUFind: gnuFind}, d.Now()); err != nil {
		return false, err
	}
	return gnuFind, nil
}

// probeGNUFind runs "find --version" over t and reports whether it
// succeeded, mirroring the shell one-liner
// `find --version >/dev/null 2>&1 && echo gnu || echo other` from
// DESIGN.md 8.2: any error at all, whether from Exec itself or from
// Process.Wait, means "not GNU". It never returns an error itself, per
// runScan's documented behaviour that a probe failure - including one
// wrapping transport.ErrAuthRequired - degenerates to "not GNU" rather
// than surfacing to the caller.
func probeGNUFind(ctx context.Context, t transport.Transport) bool {
	proc, err := t.Exec(ctx, "find --version")
	if err != nil {
		return false
	}

	// Stderr must be drained concurrently with stdout, but not awaited before
	// Wait. Command's captured stderr reaches EOF only after Wait marks the
	// capture complete; waiting for stderr first deadlocks every real SSH
	// capability probe and leaves the TUI on "Scanning…" forever.
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, proc.Stderr)
		close(stderrDone)
	}()

	_, _ = io.Copy(io.Discard, proc.Stdout)
	_ = proc.Stdout.Close()
	waitErr := proc.Wait()
	<-stderrDone
	_ = proc.Stderr.Close()

	return waitErr == nil
}

// drainScan runs remote.ParseScan over proc.Stdout concurrently with
// draining its output channel, collecting every entry it sends before
// returning, so the two never deadlock on each other (ParseScan blocks
// sending on out until something reads it). Process.Stdout is read to
// completion - which ranging the channel to close does - before Wait is
// called, per Process's documented contract.
func drainScan(proc *transport.Process, gnuFind bool) ([]remote.Entry, remote.Report, error) {
	out := make(chan remote.Entry, 256)

	var report remote.Report
	var parseErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		report, parseErr = remote.ParseScan(proc.Stdout, gnuFind, out)
	}()

	var entries []remote.Entry
	for e := range out {
		entries = append(entries, e)
	}
	<-done

	stdoutErr := proc.Stdout.Close()
	stderrErr := proc.Stderr.Close()
	waitErr := proc.Wait()

	if parseErr != nil {
		return entries, report, parseErr
	}
	if waitErr != nil {
		return entries, report, waitErr
	}
	if stdoutErr != nil {
		return entries, report, stdoutErr
	}
	if stderrErr != nil {
		return entries, report, stderrErr
	}

	return entries, report, nil
}

// renderTable writes one row per entry to w: the path, a human-readable
// size (formatSize, never the raw byte count per DESIGN.md 8.3) and the
// modification time. An entry with a zero ModTime - every BSD-parsed entry,
// per remote.Entry.ModTime's doc comment - renders as "-" rather than a
// fabricated date.
func renderTable(w io.Writer, entries []remote.Entry) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "PATH\tSIZE\tMODIFIED")
	for _, e := range entries {
		modified := "-"
		if !e.ModTime.IsZero() {
			modified = e.ModTime.Format("2006-01-02 15:04")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", e.Path, formatSize(e.Size), modified)
	}
	_ = tw.Flush()
}

// formatSize renders n bytes human-readably, e.g. 88213441 as "84M", never
// the raw byte count (DESIGN.md 8.3).
func formatSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}

	div, exp := int64(unit), 0
	for r := n / unit; r >= unit; r /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.0f%c", float64(n)/float64(div), "KMGTPE"[exp])
}

// newScanCmd builds the `logpick scan <host>` subcommand: it resolves
// host's profile from config, builds the appropriate transport.Transport
// (a real backend, or a transport.Mock when --mock is given), and calls
// runScan against a real *state.Store. It is plumbing only - the
// discovery, probing, sorting and rendering behaviour all lives in
// runScan above, which this command calls unchanged.
func newScanCmd() *cobra.Command {
	var (
		profileFlag string
		pathFlags   []string
		mockDir     string
		configPath  string
		statePath   string
	)

	cmd := &cobra.Command{
		Use:   "scan <host>",
		Short: "Scan a host's configured paths and print the logs found there",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := args[0]

			var ov config.Overrides
			if cmd.Flags().Changed("profile") {
				ov.Profile = &profileFlag
			}
			if cmd.Flags().Changed("path") {
				ov.Paths = &pathFlags
			}

			var cfg *config.Config
			if configPath != "" {
				loaded, err := config.Load(configPath)
				if err != nil {
					return err
				}
				cfg = loaded
			}

			resolved, err := config.Resolve(cfg, host, ov)
			if err != nil {
				return err
			}

			var tp transport.Transport
			if mockDir != "" {
				tp = transport.NewMock(mockDir)
			} else {
				tp = transport.NewCommand(host, resolved.Profile)
			}
			defer func() { _ = tp.Close() }()

			d := deps{
				Transport: tp,
				Store:     state.New(statePath),
				Host:      resolved,
				Now:       time.Now,
			}

			return runScan(cmd.Context(), d, host, cmd.OutOrStdout())
		},
	}

	cmd.Flags().StringVar(&profileFlag, "profile", "", "override the resolved profile")
	cmd.Flags().StringArrayVar(&pathFlags, "path", nil, "override the resolved scan paths (repeatable)")
	cmd.Flags().StringVar(&mockDir, "mock", "", "serve this fixture directory instead of a real transport, no network")
	cmd.Flags().StringVar(&configPath, "config", "", "path to config.toml (default: built-in defaults only)")
	cmd.Flags().StringVar(&statePath, "state", defaultStatePath(), "path to state.toml")

	return cmd
}

// defaultStatePath returns $XDG_DATA_HOME/logpick/state.toml, falling
// back to ~/.local/share/logpick/state.toml per DESIGN.md section 6 when
// XDG_DATA_HOME is unset, and finally to a relative path if even the
// home directory cannot be determined.
func defaultStatePath() string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "logpick", "state.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "state.toml"
	}
	return filepath.Join(home, ".local", "share", "logpick", "state.toml")
}
