package viewer_test

// Test plan (AGENTS.md section 5 and T16's Contract, copied verbatim before
// any test below was written):
//
//  2. The viewer loads a file and reports its line count.
//  3. A file containing a 5MB single line loads without a scanner error.
//  4. n and N step forward and backward through matches and wrap at the
//     ends.
//
// (Mechanic 1 belongs to the library screen; see library_test.go.)
//
// Every test below feeds a tea.Msg to Update and asserts on the returned
// Model's exported accessors (Lines, LineCount, Loaded, Matches,
// MatchCount, CurrentMatchIndex) and on the tea.Cmd Init or Update emitted,
// never on View's rendered output (DESIGN.md section 13). Mechanics 2 and 3
// drive the load command directly rather than through a real bubbletea
// program, by calling the tea.Cmd Init returns and feeding its result back
// into Update, the same shape the browser and library tests already use for
// their own commands.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/go-cmp/cmp"

	"github.com/pedreviljoen/logpick/internal/ui"
	"github.com/pedreviljoen/logpick/internal/ui/viewer"
)

// noopSearch is the SearchFunc every test in this file constructs a Model
// with unless it needs to observe a search request specifically. It is
// never actually called: no test here submits a query through the input,
// since mechanics 2, 3 and 4 do not require one.
func noopSearch(ctx context.Context, path, query string, regex bool) ([]ui.SearchMatch, error) {
	return nil, nil
}

// update drives m through one Update call and type-asserts the returned
// ui.ScreenModel back to viewer.Model, which is what every test in this
// file needs to keep chaining calls the way real usage does.
func update(t *testing.T, m viewer.Model, msg tea.Msg) (viewer.Model, tea.Cmd) {
	t.Helper()
	screen, cmd := m.Update(msg)
	vm, ok := screen.(viewer.Model)
	if !ok {
		t.Fatalf("Update(%T) returned %T, want viewer.Model", msg, screen)
	}
	return vm, cmd
}

// runInit calls m.Init(), fails the test if it returned a nil Cmd, and
// returns the tea.Msg that Cmd produces.
func runInit(t *testing.T, m viewer.Model) tea.Msg {
	t.Helper()
	cmd := m.Init()
	if cmd == nil {
		t.Fatalf("Init() returned a nil Cmd; loading the file should be a returned tea.Cmd")
	}
	return cmd()
}

// writeFile writes content to a fresh file under t.TempDir() and returns
// its absolute path.
func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "log.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test fixture: %v", err)
	}
	return path
}

func TestModel_LoadsFileAndReportsLineCount(t *testing.T) {
	t.Run("the viewer loads a file and reports its line count", func(t *testing.T) {
		content := "2026-08-14 16:02:11 INFO  Started build\n" +
			"2026-08-14 16:02:12 WARN  Retry 1 of 3\n" +
			"2026-08-14 16:02:14 ERROR Connection reset\n"
		wantLines := []string{
			"2026-08-14 16:02:11 INFO  Started build",
			"2026-08-14 16:02:12 WARN  Retry 1 of 3",
			"2026-08-14 16:02:14 ERROR Connection reset",
		}
		path := writeFile(t, content)

		m := viewer.New(path, noopSearch)
		if m.Loaded() {
			t.Fatalf("Loaded() = true before Init's Cmd ran, want false")
		}

		msg := runInit(t, m)
		loaded, ok := msg.(ui.FileLoadedMsg)
		if !ok {
			t.Fatalf("Init's Cmd reported %T, want ui.FileLoadedMsg", msg)
		}
		if loaded.Path != path {
			t.Fatalf("FileLoadedMsg.Path = %q, want %q", loaded.Path, path)
		}

		m, cmd := update(t, m, loaded)
		if cmd != nil {
			t.Fatalf("Update(FileLoadedMsg) returned a non-nil Cmd, want nil")
		}
		if !m.Loaded() {
			t.Fatalf("Loaded() = false after FileLoadedMsg, want true")
		}
		if diff := cmp.Diff(wantLines, m.Lines()); diff != "" {
			t.Fatalf("Lines after FileLoadedMsg (-want +got):\n%s", diff)
		}
		if got, want := m.LineCount(), len(wantLines); got != want {
			t.Fatalf("LineCount() = %d, want %d", got, want)
		}
	})
}

func TestModel_LoadsFiveMegabyteSingleLine(t *testing.T) {
	t.Run("a file containing a 5MB single line loads without a scanner error", func(t *testing.T) {
		const fiveMB = 5 * 1024 * 1024
		bigLine := strings.Repeat("x", fiveMB)
		path := writeFile(t, bigLine)

		m := viewer.New(path, noopSearch)
		msg := runInit(t, m)

		if errMsg, ok := msg.(ui.ErrorMsg); ok {
			t.Fatalf("Init's Cmd reported ui.ErrorMsg for a 5MB single line: %v", errMsg.Err)
		}
		loaded, ok := msg.(ui.FileLoadedMsg)
		if !ok {
			t.Fatalf("Init's Cmd reported %T, want ui.FileLoadedMsg", msg)
		}

		m, _ = update(t, m, loaded)

		if got := m.LineCount(); got != 1 {
			t.Fatalf("LineCount() = %d, want 1", got)
		}
		lines := m.Lines()
		if len(lines) != 1 {
			t.Fatalf("len(Lines()) = %d, want 1", len(lines))
		}
		if len(lines[0]) != fiveMB {
			t.Fatalf("len(Lines()[0]) = %d, want %d", len(lines[0]), fiveMB)
		}
		if lines[0] != bigLine {
			t.Fatalf("Lines()[0] did not round-trip the 5MB line unchanged (length matched but content did not)")
		}
	})
}

// matchesAt builds a []ui.SearchMatch with n hits, each on a distinct line,
// so a wrap test can tell them apart by CurrentMatch().Line alone.
func matchesAt(n int) []ui.SearchMatch {
	matches := make([]ui.SearchMatch, n)
	for i := range matches {
		matches[i] = ui.SearchMatch{Line: i + 1, Start: 0, End: 3, Text: "hit"}
	}
	return matches
}

func TestModel_SearchResultForAnotherFileIsIgnored(t *testing.T) {
	path := writeFile(t, "only this file\n")
	m := viewer.New(path, noopSearch)
	m, _ = update(t, m, ui.SearchResultsMsg{
		Path:    path + ".other",
		Gen:     1,
		Query:   "other",
		Matches: matchesAt(2),
	})
	if m.MatchCount() != 0 {
		t.Fatalf("MatchCount() = %d, want 0 for a result from another file", m.MatchCount())
	}
}

func TestModel_EscapeGoesBackToLibrary(t *testing.T) {
	path := writeFile(t, "line\n")
	m := viewer.New(path, noopSearch)
	_, cmd := update(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("escape returned nil Cmd, want ui.BackMsg")
	}
	if _, ok := cmd().(ui.BackMsg); !ok {
		t.Fatalf("escape command returned %T, want ui.BackMsg", cmd())
	}
}

func pressRune(t *testing.T, m viewer.Model, r rune) viewer.Model {
	t.Helper()
	m, _ = update(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	return m
}

func TestModel_StepsAndWrapsMatches(t *testing.T) {
	t.Run("n and N step forward and backward through matches and wrap at the ends", func(t *testing.T) {
		path := writeFile(t, "irrelevant to this test\n")
		m := viewer.New(path, noopSearch)

		matches := matchesAt(3)
		m, _ = update(t, m, ui.SearchResultsMsg{Query: "hit", Matches: matches})

		if got := m.MatchCount(); got != 3 {
			t.Fatalf("MatchCount() = %d, want 3", got)
		}
		if got := m.CurrentMatchIndex(); got != 0 {
			t.Fatalf("CurrentMatchIndex() after SearchResultsMsg = %d, want 0", got)
		}

		// n steps forward: 0 -> 1 -> 2, then wraps past the last match back
		// to the first (index 0).
		m = pressRune(t, m, 'n')
		if got := m.CurrentMatchIndex(); got != 1 {
			t.Fatalf("CurrentMatchIndex() after one 'n' = %d, want 1", got)
		}
		m = pressRune(t, m, 'n')
		if got := m.CurrentMatchIndex(); got != 2 {
			t.Fatalf("CurrentMatchIndex() after two 'n' = %d, want 2", got)
		}
		m = pressRune(t, m, 'n')
		if got := m.CurrentMatchIndex(); got != 0 {
			t.Fatalf("CurrentMatchIndex() after 'n' past the last match = %d, want 0 (wrap to first)", got)
		}

		// N steps backward: from index 0, wraps before the first match to
		// the last (index 2), then continues backward: 2 -> 1.
		m = pressRune(t, m, 'N')
		if got := m.CurrentMatchIndex(); got != 2 {
			t.Fatalf("CurrentMatchIndex() after 'N' before the first match = %d, want 2 (wrap to last)", got)
		}
		m = pressRune(t, m, 'N')
		if got := m.CurrentMatchIndex(); got != 1 {
			t.Fatalf("CurrentMatchIndex() after a second 'N' = %d, want 1", got)
		}
	})

	t.Run("n and N on an empty match set are a no-op", func(t *testing.T) {
		path := writeFile(t, "irrelevant to this test\n")
		m := viewer.New(path, noopSearch)

		if got := m.CurrentMatchIndex(); got != -1 {
			t.Fatalf("CurrentMatchIndex() before any search = %d, want -1", got)
		}

		m = pressRune(t, m, 'n')
		if got := m.CurrentMatchIndex(); got != -1 {
			t.Fatalf("CurrentMatchIndex() after 'n' with no matches = %d, want -1", got)
		}
		m = pressRune(t, m, 'N')
		if got := m.CurrentMatchIndex(); got != -1 {
			t.Fatalf("CurrentMatchIndex() after 'N' with no matches = %d, want -1", got)
		}
	})
}
