package browser_test

// Test plan (AGENTS.md section 5 and T14's Contract, copied verbatim before
// any test below was written):
//
//  1. Entries arriving on the scan channel populate the list progressively.
//  2. Rapid selection changes issue exactly one preview request after the
//     debounce window, not one per change.
//  3. A stale debounce message, one whose generation no longer matches, is
//     ignored.
//  4. Selecting a path already in the cache renders without issuing a
//     request.
//  5. Moving selection while a preview is in flight cancels the previous
//     context.
//
// Every test below feeds a tea.Msg to Update and asserts on the returned
// Model's exported accessors (Entries, Highlighted, PreviewLines,
// PreviewRequestCount, Gen) and on the tea.Cmd it emitted, never on View's
// rendered output (DESIGN.md section 13). Mechanic 5 is made observable
// through browser.WithContextFunc: the test supplies its own CancelFunc,
// which sets a plain local flag, rather than reaching into Model's
// unexported cancel field.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/google/go-cmp/cmp"

	"github.com/pedreviljoen/logpick/internal/ui"
	"github.com/pedreviljoen/logpick/internal/ui/browser"
)

const testHost = "jenkins-01.prod.internal"

// noopPreview is the PreviewFunc every test in this file constructs a Model
// with. It is never actually called: no test here executes the tea.Cmd
// Update returns for a preview request, since "a preview was requested" is
// observable through Model.PreviewRequestCount and the non-nil Cmd alone
// (see the package doc comment above).
func noopPreview(ctx context.Context, host, path string) ([]string, error) {
	return nil, nil
}

// entriesFor builds a []ui.ScanEntry, one per path given, each with a
// distinct Size and ModTime so two entries are never accidentally equal.
func entriesFor(paths ...string) []ui.ScanEntry {
	entries := make([]ui.ScanEntry, len(paths))
	for i, p := range paths {
		entries[i] = ui.ScanEntry{
			Path:    p,
			Size:    int64(1000 + i),
			ModTime: time.Date(2026, 8, 17, 12, 0, i, 0, time.UTC),
		}
	}
	return entries
}

// update drives m through one Update call and type-asserts the returned
// ui.ScreenModel back to browser.Model, which is what every test in this
// file needs to keep chaining calls the way real usage does.
func update(t *testing.T, m browser.Model, msg tea.Msg) (browser.Model, tea.Cmd) {
	t.Helper()
	screen, cmd := m.Update(msg)
	bm, ok := screen.(browser.Model)
	if !ok {
		t.Fatalf("Update(%T) returned %T, want browser.Model", msg, screen)
	}
	return bm, cmd
}

// pressKeys drives m through Update once per key type given, in order, the
// same way ctrl+n and ctrl+p arrive as bare tea.KeyMsg values with no Runes
// (picker_test.go uses the identical pattern for the component this screen
// embeds).
func pressKeys(t *testing.T, m browser.Model, keys ...tea.KeyType) browser.Model {
	t.Helper()
	for _, k := range keys {
		m, _ = update(t, m, tea.KeyMsg{Type: k})
	}
	return m
}

func TestModel_CtrlSRequestsOneOffPathScan(t *testing.T) {
	m := browser.New(testHost, noopPreview)
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyCtrlS})
	for _, r := range "/opt/app/logs" {
		m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("path scan Enter returned nil Cmd")
	}
	raw := cmd()
	msg, ok := raw.(ui.PathScanRequestedMsg)
	if !ok {
		t.Fatalf("path scan command returned %T, want ui.PathScanRequestedMsg", raw)
	}
	want := ui.PathScanRequestedMsg{Host: testHost, Path: "/opt/app/logs"}
	if diff := cmp.Diff(want, msg); diff != "" {
		t.Fatalf("path scan request mismatch (-want +got):\n%s", diff)
	}

	m, _ = update(t, m, ui.PathScanStartedMsg{Host: testHost, Path: msg.Path})
	if m.ActiveScanPath() != msg.Path || len(m.Entries()) != 0 {
		t.Fatalf("path scan start kept path=%q entries=%d", m.ActiveScanPath(), len(m.Entries()))
	}
}

func TestModel_SpaceFetchesLocalSnapshotAndSearchesIt(t *testing.T) {
	const remotePath = "/var/log/app.log"
	const localPath = "/tmp/logpick/app.log"
	selectedLines := make([]string, 20)
	for i := range selectedLines {
		selectedLines[i] = fmt.Sprintf("line %02d", i+1)
	}
	selectedLines[9] = "error happened"
	selector := func(_ context.Context, host string, entry ui.ScanEntry) (string, []string, error) {
		if host != testHost || entry.Path != remotePath {
			t.Fatalf("selector got %s:%s, want %s:%s", host, entry.Path, testHost, remotePath)
		}
		return localPath, selectedLines, nil
	}
	var searched []string
	searcher := func(_ context.Context, path, query string, regex bool) ([]ui.SearchMatch, error) {
		if path != localPath || regex {
			t.Fatalf("search got path=%q query=%q regex=%v", path, query, regex)
		}
		searched = append(searched, query)
		var matches []ui.SearchMatch
		for i, line := range selectedLines {
			if start := strings.Index(line, query); start >= 0 {
				matches = append(matches, ui.SearchMatch{Line: i + 1, Start: start, End: start + len(query), Text: line})
			}
		}
		return matches, nil
	}

	m := browser.New(testHost, noopPreview, browser.WithSelector(selector), browser.WithSearch(searcher))
	m = m.Resize(100, 12).(browser.Model)
	m, _ = update(t, m, ui.ScanEntriesMsg{Host: testHost, Entries: entriesFor(remotePath, "/var/log/other.log")})
	m, selectCmd := update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	if selectCmd == nil {
		t.Fatal("space returned nil Cmd, want local snapshot fetch")
	}
	m, _ = update(t, m, selectCmd())
	if m.SelectedPath() != remotePath || m.SelectedLocal() != localPath {
		t.Fatalf("selected = %q -> %q, want %q -> %q", m.SelectedPath(), m.SelectedLocal(), remotePath, localPath)
	}
	if !m.PreviewFocused() {
		t.Fatal("successful selection did not focus the right pane")
	}
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyDown})
	if m.SelectedPath() != remotePath {
		t.Fatalf("moving the left cursor replaced selected path with %q", m.SelectedPath())
	}
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyTab})

	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if !m.Searching() {
		t.Fatal("/ did not focus search for the selected local snapshot")
	}
	for _, r := range "error" {
		var liveCmd tea.Cmd
		m, liveCmd = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		if liveCmd == nil {
			t.Fatalf("typing %q returned nil live-search Cmd", r)
		}
		m, _ = update(t, m, liveCmd())
	}
	if diff := cmp.Diff([]string{"e", "er", "err", "erro", "error"}, searched); diff != "" {
		t.Fatalf("live queries mismatch (-want +got):\n%s", diff)
	}
	if m.SearchMatchCount() != 1 || m.CurrentSearchMatchIndex() != 0 {
		t.Fatalf("search state = match %d of %d, want 0 of 1", m.CurrentSearchMatchIndex(), m.SearchMatchCount())
	}
	if got := m.FilteredLineCount(); got != 1 {
		t.Fatalf("filtered line count = %d, want only the matching line", got)
	}
	if got := m.PreviewOffset(); got != 0 {
		t.Fatalf("preview offset = %d, want filtered result at offset 0", got)
	}
	m, enterCmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if enterCmd != nil || m.Searching() {
		t.Fatal("enter should close live search without issuing another search")
	}
}

func TestModel_PreviewScrollsIndependently(t *testing.T) {
	m := browser.New(testHost, noopPreview)
	resized := m.Resize(100, 12)
	m = resized.(browser.Model)
	paths := make([]string, 80)
	for i := range paths {
		paths[i] = fmt.Sprintf("/var/log/application/very-long-service-name-%02d.log", i)
	}
	entries := entriesFor(paths...)
	m, _ = update(t, m, ui.ScanEntriesMsg{Host: testHost, Entries: entries})

	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %02d", i)
	}
	lines[0] = strings.Repeat("long-log-line ", 50)
	m, _ = update(t, m, ui.PreviewMsg{Gen: m.Gen(), Host: testHost, Path: entries[0].Path, Lines: lines})
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyTab})
	if !m.PreviewFocused() {
		t.Fatal("tab did not focus the preview pane")
	}
	for range 8 {
		m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	if got := m.PreviewOffset(); got == 0 {
		t.Fatal("down keys did not scroll the focused preview viewport")
	}
	view := m.View()
	if got := lipgloss.Height(view); got > 12 {
		t.Fatalf("browser height = %d, want at most 12:\n%s", got, view)
	}
	if got := lipgloss.Width(view); got > 100 {
		t.Fatalf("browser width = %d, want at most 100", got)
	}
}

func TestModel_EscapeReturnsToHosts(t *testing.T) {
	m := browser.New(testHost, noopPreview)
	_, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("escape returned nil Cmd, want ui.BackMsg")
	}
	msg := cmd()
	if _, ok := msg.(ui.BackMsg); !ok {
		t.Fatalf("escape command returned %T, want ui.BackMsg", msg)
	}
}

func TestModel_FollowKeyRequestsLiveTail(t *testing.T) {
	m := browser.New(testHost, noopPreview)
	entries := entriesFor("/var/log/syslog", "/var/log/auth.log")
	m, _ = update(t, m, ui.ScanEntriesMsg{Host: testHost, Entries: entries})

	m, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}})
	if cmd == nil {
		t.Fatal("F returned nil Cmd, want ui.FollowRequestedMsg")
	}
	raw := cmd()
	got, ok := raw.(ui.FollowRequestedMsg)
	if !ok {
		t.Fatalf("F command returned %T, want ui.FollowRequestedMsg", raw)
	}
	want := ui.FollowRequestedMsg{Host: testHost, Path: "/var/log/syslog"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("follow request mismatch (-want +got):\n%s", diff)
	}

	m, _ = update(t, m, ui.FollowStartedMsg{Host: testHost, Path: got.Path})
	if !m.Following() {
		t.Fatal("Following() = false after FollowStartedMsg")
	}
	if m.FollowPath() != got.Path {
		t.Fatalf("FollowPath() = %q, want %q", m.FollowPath(), got.Path)
	}

	m, _ = update(t, m, ui.LinesMsg{Lines: []string{"line one", "line two"}})
	if m.FollowLineCount() != 2 {
		t.Fatalf("FollowLineCount() = %d, want 2", m.FollowLineCount())
	}

	m, escCmd := update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if escCmd == nil {
		t.Fatal("esc while following returned nil Cmd, want FollowRequestedMsg to stop")
	}
	if _, ok := escCmd().(ui.FollowRequestedMsg); !ok {
		t.Fatalf("esc while following returned %T, want ui.FollowRequestedMsg", escCmd())
	}

	m, _ = update(t, m, ui.StreamClosedMsg{})
	if m.Following() {
		t.Fatal("Following() = true after StreamClosedMsg")
	}
}

func TestModel_FollowIgnoredOnDirectory(t *testing.T) {
	m := browser.New(testHost, noopPreview)
	m, _ = update(t, m, ui.ScanEntriesMsg{Host: testHost, Entries: []ui.ScanEntry{
		{Path: "/opt/app", IsDir: true},
	}})
	_, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'F'}})
	if cmd != nil {
		t.Fatalf("F on a directory returned %T, want nil", cmd())
	}
}

func TestModel_EntriesPopulateProgressively(t *testing.T) {
	t.Run("entries arriving on the scan channel populate the list progressively", func(t *testing.T) {
		m := browser.New(testHost, noopPreview)

		first := entriesFor("/var/log/a.log", "/var/log/b.log")
		m, cmd1 := update(t, m, ui.ScanEntriesMsg{Host: testHost, Entries: first})

		if diff := cmp.Diff(first, m.Entries()); diff != "" {
			t.Fatalf("Entries after first batch (-want +got):\n%s", diff)
		}
		if cmd1 == nil {
			t.Fatalf("Update(first batch) returned a nil Cmd; the first entry becoming highlighted should schedule a debounce tick")
		}

		second := entriesFor("/var/log/c.log")
		m, cmd2 := update(t, m, ui.ScanEntriesMsg{Host: testHost, Entries: second})

		want := append(append([]ui.ScanEntry{}, first...), second...)
		if diff := cmp.Diff(want, m.Entries()); diff != "" {
			t.Fatalf("Entries after second batch (-want +got):\n%s", diff)
		}
		if cmd2 != nil {
			t.Fatalf("Update(second batch) returned a non-nil Cmd; appending entries after the cursor should not move the highlighted path")
		}
	})
}

func TestModel_RapidSelectionChangesIssueOnePreviewRequest(t *testing.T) {
	t.Run("rapid selection changes issue exactly one preview request after the debounce window, not one per change", func(t *testing.T) {
		m := browser.New(testHost, noopPreview)

		entries := entriesFor("/var/log/a.log", "/var/log/b.log", "/var/log/c.log")
		m, _ = update(t, m, ui.ScanEntriesMsg{Host: testHost, Entries: entries})

		if got := m.PreviewRequestCount(); got != 0 {
			t.Fatalf("PreviewRequestCount after entries arrive = %d, want 0", got)
		}

		m = pressKeys(t, m, tea.KeyCtrlN, tea.KeyCtrlN)

		if got := m.PreviewRequestCount(); got != 0 {
			t.Fatalf("PreviewRequestCount after rapid selection changes, before any debounce fires, = %d, want 0", got)
		}

		entry, ok := m.Highlighted()
		if !ok || entry.Path != entries[2].Path {
			t.Fatalf("Highlighted() = %+v, %v, want %q, true", entry, ok, entries[2].Path)
		}

		m, cmd := update(t, m, ui.PreviewDebounceMsg{Gen: m.Gen(), Host: testHost, Path: entry.Path})

		if got := m.PreviewRequestCount(); got != 1 {
			t.Fatalf("PreviewRequestCount after the settled debounce message = %d, want exactly 1", got)
		}
		if cmd == nil {
			t.Fatalf("Update(settled PreviewDebounceMsg) returned a nil Cmd, want the preview request command")
		}
	})
}

func TestModel_StaleDebounceMessageIgnored(t *testing.T) {
	t.Run("a stale debounce message, one whose generation no longer matches, is ignored", func(t *testing.T) {
		m := browser.New(testHost, noopPreview)

		entries := entriesFor("/var/log/a.log", "/var/log/b.log")
		m, _ = update(t, m, ui.ScanEntriesMsg{Host: testHost, Entries: entries})
		staleGen := m.Gen()

		m = pressKeys(t, m, tea.KeyCtrlN)
		if m.Gen() == staleGen {
			t.Fatalf("Gen() did not advance after a selection change; the rest of this test cannot exercise a stale message")
		}

		before := m
		m, cmd := update(t, m, ui.PreviewDebounceMsg{Gen: staleGen, Host: testHost, Path: entries[0].Path})

		if got := m.PreviewRequestCount(); got != 0 {
			t.Fatalf("PreviewRequestCount after a stale debounce message = %d, want 0", got)
		}
		if cmd != nil {
			t.Fatalf("Update(stale PreviewDebounceMsg) returned a non-nil Cmd, want nil")
		}
		if diff := cmp.Diff(before.PreviewLines(), m.PreviewLines()); diff != "" {
			t.Fatalf("PreviewLines changed from a stale debounce message (-before +after):\n%s", diff)
		}
		if before.Gen() != m.Gen() {
			t.Fatalf("Gen changed from a stale debounce message: before %d, after %d", before.Gen(), m.Gen())
		}
	})
}

func TestModel_CachedPathRendersWithoutRequest(t *testing.T) {
	t.Run("selecting a path already in the cache renders without issuing a request", func(t *testing.T) {
		m := browser.New(testHost, noopPreview)

		entries := entriesFor("/var/log/a.log", "/var/log/b.log")
		m, _ = update(t, m, ui.ScanEntriesMsg{Host: testHost, Entries: entries})

		a, _ := m.Highlighted()
		if a.Path != entries[0].Path {
			t.Fatalf("Highlighted() = %q, want %q", a.Path, entries[0].Path)
		}

		m, _ = update(t, m, ui.PreviewDebounceMsg{Gen: m.Gen(), Host: testHost, Path: a.Path})
		if got := m.PreviewRequestCount(); got != 1 {
			t.Fatalf("PreviewRequestCount after the first request = %d, want 1", got)
		}

		aLines := []string{"a line one", "a line two"}
		m, _ = update(t, m, ui.PreviewMsg{Gen: m.Gen(), Host: testHost, Path: a.Path, Lines: aLines})
		if diff := cmp.Diff(aLines, m.PreviewLines()); diff != "" {
			t.Fatalf("PreviewLines after the A reply (-want +got):\n%s", diff)
		}

		m = pressKeys(t, m, tea.KeyCtrlN)
		b, _ := m.Highlighted()
		if b.Path != entries[1].Path {
			t.Fatalf("Highlighted() after ctrl+n = %q, want %q", b.Path, entries[1].Path)
		}

		m, _ = update(t, m, ui.PreviewDebounceMsg{Gen: m.Gen(), Host: testHost, Path: b.Path})
		if got := m.PreviewRequestCount(); got != 2 {
			t.Fatalf("PreviewRequestCount after the second (miss) request = %d, want 2", got)
		}

		bLines := []string{"b line one"}
		m, _ = update(t, m, ui.PreviewMsg{Gen: m.Gen(), Host: testHost, Path: b.Path, Lines: bLines})

		m = pressKeys(t, m, tea.KeyCtrlP)
		backToA, _ := m.Highlighted()
		if backToA.Path != a.Path {
			t.Fatalf("Highlighted() after ctrl+p = %q, want %q", backToA.Path, a.Path)
		}

		m, cmd := update(t, m, ui.PreviewDebounceMsg{Gen: m.Gen(), Host: testHost, Path: a.Path})

		if diff := cmp.Diff(aLines, m.PreviewLines()); diff != "" {
			t.Fatalf("PreviewLines after re-selecting the cached path (-want +got):\n%s", diff)
		}
		if got := m.PreviewRequestCount(); got != 2 {
			t.Fatalf("PreviewRequestCount after re-selecting a cached path = %d, want 2 (unchanged, no new request)", got)
		}
		if cmd != nil {
			t.Fatalf("Update(debounce for a cached path) returned a non-nil Cmd, want nil")
		}
	})
}

func TestModel_MovingSelectionCancelsInFlightPreview(t *testing.T) {
	t.Run("moving selection while a preview is in flight cancels the previous context", func(t *testing.T) {
		var cancelled bool
		ctxFunc := func() (context.Context, context.CancelFunc) {
			return context.Background(), func() { cancelled = true }
		}

		m := browser.New(testHost, noopPreview, browser.WithContextFunc(ctxFunc))

		entries := entriesFor("/var/log/a.log", "/var/log/b.log")
		m, _ = update(t, m, ui.ScanEntriesMsg{Host: testHost, Entries: entries})

		a, _ := m.Highlighted()
		m, _ = update(t, m, ui.PreviewDebounceMsg{Gen: m.Gen(), Host: testHost, Path: a.Path})
		if got := m.PreviewRequestCount(); got != 1 {
			t.Fatalf("PreviewRequestCount after issuing the in-flight request = %d, want 1", got)
		}
		if cancelled {
			t.Fatalf("the injected CancelFunc was called before any selection change; nothing should have cancelled it yet")
		}

		m, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyCtrlN})

		if !cancelled {
			t.Fatalf("the injected CancelFunc was not called after the selection moved away from the in-flight request")
		}
		if cmd == nil {
			t.Fatalf("Update(selection change) returned a nil Cmd, want the new debounce tick for the newly highlighted entry")
		}

		b, _ := m.Highlighted()
		if b.Path != entries[1].Path {
			t.Fatalf("Highlighted() after the move = %q, want %q", b.Path, entries[1].Path)
		}
	})
}

// dirEntry builds a directory ui.ScanEntry, the kind only a one-off listing
// (ui.PathScanRequestedMsg) can produce.
func dirEntry(path string) ui.ScanEntry {
	return ui.ScanEntry{Path: path, Size: 4096, ModTime: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), IsDir: true}
}

// browsing drives m into browse mode showing entries, the state the browser
// is in after the user typed a path with ctrl+s and the listing came back.
func browsing(t *testing.T, m browser.Model, path string, entries []ui.ScanEntry) browser.Model {
	t.Helper()
	m, _ = update(t, m, ui.PathScanStartedMsg{Host: testHost, Path: path})
	m, _ = update(t, m, ui.ScanEntriesMsg{Host: testHost, Entries: entries})
	return m
}

// TestModel_ListingShowsDirectoriesWithATrailingSlash covers the one place a
// directory can be told apart in the list: picker renders the same string it
// filtered on, so the trailing slash is both the mark and the fuzzy-match
// text, while Highlighted still reports the plain path the remote knows.
func TestModel_ListingShowsDirectoriesWithATrailingSlash(t *testing.T) {
	entries := append([]ui.ScanEntry{dirEntry("/opt/app/archive")}, entriesFor("/opt/app/app.log")...)
	m := browsing(t, browser.New(testHost, noopPreview), "/opt/app", entries)

	got, ok := m.Highlighted()
	if !ok {
		t.Fatal("no entry highlighted after the listing arrived")
	}
	if got.Path != "/opt/app/archive" || !got.IsDir {
		t.Fatalf("highlighted %+v, want the directory /opt/app/archive", got)
	}

	view := m.View()
	if !strings.Contains(view, "/opt/app/archive/") {
		t.Errorf("view does not render the directory with a trailing slash:\n%s", view)
	}
	if !strings.Contains(view, "Browsing — /opt/app") {
		t.Errorf("view does not name the path being browsed:\n%s", view)
	}
}

// TestModel_NoPreviewRequestForADirectory: a directory has nothing to tail,
// so highlighting one must not burn a request or leave the pane claiming to
// be loading one.
func TestModel_NoPreviewRequestForADirectory(t *testing.T) {
	m := browser.New(testHost, noopPreview, browser.WithDebounce(time.Millisecond))
	m = browsing(t, m, "/opt/app", []ui.ScanEntry{dirEntry("/opt/app/archive")})

	if m.PreviewRequestCount() != 0 {
		t.Fatalf("PreviewRequestCount = %d, want 0", m.PreviewRequestCount())
	}
	// Even a debounce tick that somehow arrives for the directory must not
	// reach PreviewFunc: nothing issued one, so its generation is stale.
	before := m.PreviewRequestCount()
	m, _ = update(t, m, ui.PreviewDebounceMsg{Gen: m.Gen(), Host: testHost, Path: "/opt/app/archive/"})
	if m.PreviewRequestCount() != before {
		t.Errorf("PreviewRequestCount = %d after a directory debounce, want %d", m.PreviewRequestCount(), before)
	}
}

// TestModel_EnterOnADirectoryDescendsIntoIt is how a user walks the tree
// after a listing: enter on a directory is the same request ctrl+s makes,
// for the child path, so the browser can drill down without retyping it.
func TestModel_EnterOnADirectoryDescendsIntoIt(t *testing.T) {
	entries := []ui.ScanEntry{dirEntry("/opt/app/archive")}
	m := browsing(t, browser.New(testHost, noopPreview), "/opt/app", entries)

	for _, key := range []tea.KeyType{tea.KeyEnter, tea.KeySpace} {
		_, cmd := update(t, m, tea.KeyMsg{Type: key})
		if cmd == nil {
			t.Fatalf("%v on a directory returned nil Cmd", key)
		}
		raw := cmd()
		msg, ok := raw.(ui.PathScanRequestedMsg)
		if !ok {
			t.Fatalf("%v on a directory produced %T, want ui.PathScanRequestedMsg", key, raw)
		}
		// The remote is asked for the plain path: the trailing slash the
		// list renders is a display mark and must not leak into a command.
		want := ui.PathScanRequestedMsg{Host: testHost, Path: "/opt/app/archive"}
		if diff := cmp.Diff(want, msg); diff != "" {
			t.Errorf("%v request mismatch (-want +got):\n%s", key, diff)
		}
	}
}

// TestModel_EnterOnAFileFetchesItLikeSpace: enter commits the highlighted
// row, and for a log that means the same local snapshot Space fetches.
func TestModel_EnterOnAFileFetchesItLikeSpace(t *testing.T) {
	const remotePath = "/opt/app/app.log"
	selected := make(chan string, 1)
	m := browser.New(testHost, noopPreview, browser.WithSelector(
		func(_ context.Context, _ string, entry ui.ScanEntry) (string, []string, error) {
			selected <- entry.Path
			return "/tmp/logpick/app.log", []string{"line one"}, nil
		},
	))
	m = browsing(t, m, "/opt/app", entriesFor(remotePath))

	_, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("enter on a file returned nil Cmd")
	}
	cmd()
	select {
	case got := <-selected:
		if got != remotePath {
			t.Errorf("selector fetched %q, want %q", got, remotePath)
		}
	default:
		t.Fatal("enter on a file did not reach the selector")
	}
}

// TestModel_EmptyListingSaysSo: the whole reason a user types a path is that
// the configured scan found nothing, so a listing that also finds nothing
// has to read as an answer rather than as a list still loading.
func TestModel_EmptyListingSaysSo(t *testing.T) {
	m := browser.New(testHost, noopPreview).Resize(100, 30)
	bm, ok := m.(browser.Model)
	if !ok {
		t.Fatalf("Resize returned %T, want browser.Model", m)
	}
	bm, _ = update(t, bm, ui.PathScanStartedMsg{Host: testHost, Path: "/nope"})
	bm, _ = update(t, bm, ui.ScanDoneMsg{Host: testHost})

	if view := bm.View(); !strings.Contains(view, "Nothing readable here") {
		t.Errorf("empty listing view does not say the path was empty:\n%s", view)
	}
}

// TestModel_ViewFitsItsTerminalAtEverySize is the regression test for the
// screen coming apart once the file list outgrew its pane. Three separate
// defects each broke the frame the same way - by rendering content taller
// or wider than the box it was given, which lipgloss pads but never trims,
// so the two panes stopped being the same height and the borders wandered
// off the screen:
//
//   - picker highlighted matched characters after truncating the row, so a
//     filtered row overflowed its width and wrapped onto three lines;
//   - picker's filter input was budgeted for its prompt but not for its
//     cursor cell, leaving the query line one column wider than the rows;
//   - calculateLayout floored each pane's content at a comfortable minimum
//     rather than at what its share of the terminal could hold, so in the
//     stacked layout two panes stacked into a frame taller than the screen.
//
// The assertion is deliberately structural rather than a golden render
// (DESIGN.md section 13): whatever it draws, the browser occupies exactly
// the height it was resized to and never more than its width.
func TestModel_ViewFitsItsTerminalAtEverySize(t *testing.T) {
	entries := make([]ui.ScanEntry, 0, 200)
	for i := 0; i < 200; i++ {
		entries = append(entries, ui.ScanEntry{
			Path:    fmt.Sprintf("/var/log/service-%03d/application.log", i),
			IsDir:   i%7 == 0,
			ModTime: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
		})
	}

	// 20x6 is the smallest frame the split pane can hold: a bordered box is
	// three rows at minimum, and below 72 columns two of them are stacked.
	for _, width := range []int{20, 30, 71, 72, 100, 160} {
		for _, height := range []int{6, 10, 24, 40} {
			for _, query := range []string{"", "application", "0"} {
				screen := browser.New(testHost, noopPreview).Resize(width, height)
				m, ok := screen.(browser.Model)
				if !ok {
					t.Fatalf("Resize returned %T, want browser.Model", screen)
				}
				m, _ = update(t, m, ui.ScanEntriesMsg{Host: testHost, Entries: entries})
				m, _ = update(t, m, ui.ScanDoneMsg{Host: testHost, Count: len(entries)})
				for _, r := range query {
					m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
				}

				// Walk well past the first page: the break only showed
				// once the cursor pushed the list's offset forward.
				for step := 0; step < 40; step++ {
					m = pressKeys(t, m, tea.KeyDown)
					view := m.View()
					if got := lipgloss.Height(view); got != height {
						t.Fatalf("%dx%d query=%q step=%d: view is %d rows, want %d",
							width, height, query, step, got, height)
					}
					if got := lipgloss.Width(view); got > width {
						t.Fatalf("%dx%d query=%q step=%d: view is %d columns, want at most %d",
							width, height, query, step, got, width)
					}
				}
			}
		}
	}
}
