package ui

import "time"

// This file is the whole message vocabulary of the application. Every tea.Msg
// the root model or any screen model exchanges is declared here, so a screen
// added later reuses a message rather than inventing a synonym for one that
// already exists.
//
// Two rules keep it that way:
//
//   - A message is a plain value. It carries data, never behaviour and never a
//     channel, a context or a func.
//   - A message names who sends it and who handles it in its doc comment.
//
// The payload structs below (ScanEntry, HostSummary, FetchedFile, SearchMatch)
// mirror types owned by packages that land after T12: remote.Entry, state.Host,
// state.Fetch and local.Match. They are declared here so this package compiles
// and so the ui layer never depends on a package it cannot see yet. When the
// owning package lands, each becomes a type alias for the real type, which
// keeps every message field below unchanged.

// ScanEntry is one log file found by a scan. It mirrors the entry type the
// discovery parser emits (T10).
type ScanEntry struct {
	// Path is the absolute path on the remote host.
	Path string
	// Size is the file size in bytes as reported by find.
	Size int64
	// ModTime is the last modification time, the default sort key.
	ModTime time.Time
	// IsDir reports whether the entry is a directory rather than a log
	// file. It is always false for a configured discovery scan, which
	// matches regular files only, and can be true only in the unfiltered
	// listing a PathScanRequestedMsg produces. A screen must not preview
	// or fetch an entry with IsDir set: it is somewhere to look, not
	// something to read.
	IsDir bool
}

// HostSummary is one row of the host history. It mirrors the per-host record in
// the state file (T08).
type HostSummary struct {
	// Name is the hostname as the user typed it.
	Name string
	// Profile is the name of the profile the host resolved to.
	Profile string
	// Label is the optional human label from the host entry.
	Label string
	// LastSeen is when the host was last connected to.
	LastSeen time.Time
	// ConnectCount is how many times the host has been connected to.
	ConnectCount int
	// Pinned reports whether the host is exempt from pruning and sorts first.
	Pinned bool
}

// FetchedFile is one locally fetched log. It mirrors the fetch record in the
// state file (T08) and the local store layout (T15).
type FetchedFile struct {
	// Host is the host the file came from.
	Host string
	// Remote is the path the file had on that host.
	Remote string
	// Local is the absolute path of the fetched copy.
	Local string
	// Bytes is the size of the fetched copy.
	Bytes int64
	// At is when the fetch completed.
	At time.Time
}

// SearchMatch is one hit from an in-file search. It mirrors the match type the
// searcher returns (T17).
type SearchMatch struct {
	// Line is the one-based line number of the hit.
	Line int
	// Start and End are byte offsets of the hit within the line.
	Start int
	End   int
	// Text is the full text of the matching line.
	Text string
}

// --- Navigation -------------------------------------------------------------

// ScreenTransitionMsg asks the root to make another screen active.
//
// Sent by a screen model when the user commits an action that leaves it, and by
// the root's own key handling. Handled by the root, which switches the active
// screen and leaves every inactive screen untouched.
type ScreenTransitionMsg struct {
	// To is the screen that becomes active.
	To Screen
}

// BackMsg asks the root to follow the back edge out of the active screen, the
// esc arrows in the screen graph: viewer to library, browser to hosts.
//
// Sent by a screen model that does not want to name its own predecessor.
// Handled by the root, which decides the target.
type BackMsg struct{}

// --- Status and errors ------------------------------------------------------

// ErrorMsg reports a failure to the user.
//
// Sent by any command or goroutine that fails, including the error channel
// drained by WaitForError. Handled by the root, which sets the error banner and
// does not change the active screen.
type ErrorMsg struct {
	// Err is the failure, already wrapped with the operation that failed.
	Err error
}

// ClearErrorMsg dismisses the error banner.
//
// Sent by the root's key handling when the user acknowledges the banner.
// Handled by the root.
type ClearErrorMsg struct{}

// StatusMsg sets the transient status text, for example the result of a fetch.
//
// Sent by any screen model. Handled by the root, which renders it in the status
// line until the next status or error.
type StatusMsg struct {
	// Text is the message to show. Empty clears the status line.
	Text string
}

// ThemeChangedMsg applies a live palette preview to the root and every screen.
type ThemeChangedMsg struct {
	Primary   string
	Secondary string
}

// --- Hosts and connection ---------------------------------------------------

// HostsLoadedMsg carries the host history read from the state file.
//
// Sent by the command the hosts screen issues on entry. Handled by the hosts
// screen, which fills its picker.
type HostsLoadedMsg struct {
	// Hosts is the history, already sorted pinned first then by last seen.
	Hosts []HostSummary
}

// HostSelectedMsg reports that the user committed a host.
//
// Sent by the hosts screen. Handled by the root, which starts the connection
// and moves to the browser.
type HostSelectedMsg struct {
	// Host is the hostname the user picked.
	Host string
	// Profile is the resolved profile name for that host.
	Profile string
}

// HostRemovedMsg reports that a host was deleted from the history.
//
// Sent by the command behind ctrl+d on the hosts screen. Handled by the hosts
// screen, which drops the row.
type HostRemovedMsg struct {
	// Host is the hostname that was removed.
	Host string
}

// HostPinnedMsg reports a change to a host's pinned flag.
//
// Sent by the command behind ctrl+p on the hosts screen. Handled by the hosts
// screen, which re-sorts the list.
type HostPinnedMsg struct {
	// Host is the hostname whose flag changed.
	Host string
	// Pinned is the new value of the flag.
	Pinned bool
}

// ConnectedMsg reports that the liveness probe for a host succeeded. It is also
// the success signal of the first-run probe, the only point at which a new
// profile may be written to the user's config.
//
// Sent by the connect command. Handled by the root, which records the connect
// in state, and by the hosts screen during the first-run flow.
type ConnectedMsg struct {
	// Host is the host that answered.
	Host string
	// Profile is the profile name that was used.
	Profile string
}

// CapsProbedMsg carries the result of the one-off remote capability probe.
//
// Sent by the probe command. Handled by the root, which caches it in state so
// later sessions skip the probe.
type CapsProbedMsg struct {
	// Host is the host that was probed.
	Host string
	// GNUFind reports whether the remote find supports -printf.
	GNUFind bool
}

// --- Discovery --------------------------------------------------------------

// ScanStartedMsg reports that a scan is now running.
//
// Sent by the scan command. Handled by the browser, which shows the scanning
// indicator while keeping any cached listing on screen.
type ScanStartedMsg struct {
	// Host is the host being scanned.
	Host string
}

// CachedScanMsg carries the listing cached in state from a previous session, so
// the browser has something to show immediately instead of an empty pane.
//
// Sent by the command the browser issues on entry. Handled by the browser,
// which is later replaced by the fresh scan.
type CachedScanMsg struct {
	// Host is the host the listing belongs to.
	Host string
	// Entries is the cached listing.
	Entries []ScanEntry
	// ScannedAt is when the cached listing was taken.
	ScannedAt time.Time
}

// ScanEntriesMsg carries a batch of entries from the running scan. Batched
// rather than one message per entry, because a scan can return ten thousand.
//
// Sent by the self-reissuing command draining the scan channel. Handled by the
// browser, which appends them and reissues that command.
type ScanEntriesMsg struct {
	// Host is the host being scanned.
	Host string
	// Entries is the batch, in the order the parser produced it.
	Entries []ScanEntry
}

// ScanDoneMsg reports that a scan finished. A scan that failed reports through
// ErrorMsg instead, so there is one error path.
//
// Sent by the scan command. Handled by the browser, which stops the indicator
// and shows the counts.
type ScanDoneMsg struct {
	// Host is the host that was scanned.
	Host string
	// Count is how many entries were produced.
	Count int
	// Skipped is how many malformed lines were dropped.
	Skipped int
	// Truncated reports whether the entry cap was hit.
	Truncated bool
}

// PathScanRequestedMsg asks the composition layer to list a one-off remote
// path: the escape hatch for a log the configured scan did not turn up.
//
// The listing it asks for is deliberately unfiltered - every file and every
// directory under Path, with the profile's include and exclude patterns
// ignored - so the user can fuzzy-find their way to the file by name. The
// profile's log patterns are what hid the file in the first place, so
// re-applying them here would reproduce the empty list the user is trying
// to escape.
//
// Sent by the browser: from Ctrl+S with a typed path, and from Enter on a
// directory in a listing, which is how the user descends into one. Handled
// by the composition layer, which replies with PathScanStartedMsg and then
// the usual ScanEntriesMsg/ScanDoneMsg stream.
type PathScanRequestedMsg struct {
	Host string
	Path string
}

// PathScanStartedMsg clears the browser list for a one-off path listing and
// switches it into browse mode, where entries may be directories.
type PathScanStartedMsg struct {
	Host string
	Path string
}

// --- Preview ----------------------------------------------------------------

// PreviewDebounceMsg is the debounce tick for the preview pane. Gen is the
// generation counter: the browser increments it on every selection change and
// ignores any tick whose generation is no longer current, which is what turns a
// burst of cursor movement into a single request.
//
// Sent by the debounce command the browser issues on every selection change.
// Handled by the browser.
type PreviewDebounceMsg struct {
	// Gen is the generation this tick was issued for.
	Gen uint64
	// Host and Path identify the selection that was current when it was issued.
	Host string
	Path string
}

// PreviewMsg carries the head of a preview, the output of tail -n. Gen is
// carried through from the request so a reply that arrives after the selection
// moved on is dropped rather than rendered.
//
// Sent by the preview command. Handled by the browser, which renders it and
// puts it in the preview cache.
type PreviewMsg struct {
	// Gen is the generation the request was issued for.
	Gen uint64
	// Host and Path identify the previewed file.
	Host string
	Path string
	// Lines is the preview content, one entry per line, no trailing newlines.
	Lines []string
}

// --- Streaming --------------------------------------------------------------

// FollowRequestedMsg asks the composition layer to start (or stop) a live
// tail of Path on Host. A second request for the same path is a toggle: the
// running follow is cancelled.
//
// Sent by the browser: F on a file, and esc while a follow is already
// running. Handled by the composition layer, which replies with
// FollowStartedMsg and then the usual LinesMsg/StreamClosedMsg stream, or
// StreamClosedMsg alone when the request was a stop.
type FollowRequestedMsg struct {
	Host string
	Path string
}

// FollowStartedMsg reports that a follow stream is open.
//
// Sent by the follow command. Handled by the browser, which switches the
// preview pane into follow mode.
type FollowStartedMsg struct {
	// Host and Path identify the followed file.
	Host string
	Path string
}

// LinesMsg carries a batch of streamed lines. The producer accumulates and
// flushes on a 50ms ticker or at 200 lines, whichever comes first, because one
// message per line drowns the event loop.
//
// Sent by the self-reissuing command draining the line channel. Handled by the
// browser, which appends to the ring buffer and reissues that command.
type LinesMsg struct {
	// Lines is the batch, in arrival order.
	Lines []string
}

// StreamClosedMsg reports that the line channel was closed, either because the
// remote command ended or because the follow was cancelled.
//
// Sent by the same command that sends LinesMsg. Handled by the browser, which
// leaves follow mode and stops reissuing.
type StreamClosedMsg struct{}

// --- Fetch ------------------------------------------------------------------

// FetchConfirmMsg reports that the selected file is above the size threshold,
// so the fetch needs the user to confirm before any transfer starts.
//
// Sent by the fetch action. Handled by the browser, which prompts and offers
// the last-N-megabytes alternative.
type FetchConfirmMsg struct {
	// Host and Remote identify the file.
	Host   string
	Remote string
	// Size is the file size in bytes as known from discovery.
	Size int64
}

// FetchStartedMsg reports that a transfer has begun.
//
// Sent by the fetch command. Handled by the browser, which shows the progress
// bar.
type FetchStartedMsg struct {
	// Host and Remote identify the source.
	Host   string
	Remote string
	// Local is the destination path.
	Local string
	// Total is the expected size in bytes, zero when unknown.
	Total int64
}

// FetchProgressMsg carries transfer progress. It mirrors transport.Progress.
//
// Sent by the command draining the progress channel. Handled by the browser.
type FetchProgressMsg struct {
	// Bytes is how much has been transferred.
	Bytes int64
	// Total is the expected size in bytes, zero when unknown.
	Total int64
}

// FetchDoneMsg reports a completed transfer.
//
// Sent by the fetch command. Handled by the root, which records the fetch in
// state and can move to the library.
type FetchDoneMsg struct {
	// Host and Remote identify the source.
	Host   string
	Remote string
	// Local is the destination path, now complete and renamed off .part.
	Local string
	// Bytes is how much was transferred.
	Bytes int64
}

// --- Library and viewer -----------------------------------------------------

// FileSelectedMsg reports that the user committed a fetched file in the
// library.
//
// Sent by the library screen on enter. Handled by the composition layer,
// which opens the viewer on File.Local.
type FileSelectedMsg struct {
	File FetchedFile
}

// LibraryLoadedMsg carries the fetched files read from the state file.
//
// Sent by the command the library screen issues on entry. Handled by the
// library screen.
type LibraryLoadedMsg struct {
	// Files is the fetch history, most recent first.
	Files []FetchedFile
}

// LibraryDeletedMsg reports that a fetched file was deleted from disk and from
// state.
//
// Sent by the delete command. Handled by the library screen, which drops the
// row.
type LibraryDeletedMsg struct {
	// Local is the path that was deleted.
	Local string
}

// FileLoadedMsg carries a local file read into memory for viewing.
//
// Sent by the load command the viewer issues on entry. Handled by the viewer,
// which fills the viewport and reports the line count.
type FileLoadedMsg struct {
	// Path is the local path that was read.
	Path string
	// Lines is the file content, one entry per line.
	Lines []string
}

// SearchResultsMsg carries the hits of an in-file search.
//
// Sent by the search command. Handled by the viewer, which highlights the hits
// and shows the match count.
type SearchResultsMsg struct {
	// Path is the local file the hits belong to. The viewer ignores a
	// result whose path is not the file it is showing, so a search still
	// in flight cannot paint onto the next file opened from the library.
	Path string
	// Gen is the search generation the request was issued for. Zero means
	// the sender did not stamp one.
	Gen uint64
	// Query is the query the hits are for.
	Query string
	// Regex reports whether the query was treated as a pattern.
	Regex bool
	// Matches is every hit, in file order.
	Matches []SearchMatch
}
