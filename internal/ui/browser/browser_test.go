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
