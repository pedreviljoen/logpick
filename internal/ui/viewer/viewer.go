// Package viewer is the local file viewer and in-file search screen.
package viewer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/pedreviljoen/logpick/internal/ui"
)

// SearchFunc runs an in-file search over the file this Model was
// constructed with and reports its hits. Model calls it only from inside
// the tea.Cmd Update returns when a search query is submitted (enter while
// the search input is focused), never from Update itself, which is what
// keeps Update from blocking on I/O (AGENTS.md section 3, invariant 4).
//
// It is injected into New rather than Model holding a local.Searcher
// directly, which is what keeps this package free of a dependency on
// internal/local, the same way browser.PreviewFunc keeps the browser
// package free of a dependency on internal/transport. A test supplies its
// own SearchFunc rather than reaching into Model's unexported state
// (AGENTS.md section 5).
//
// query and regex mirror local.Query's fields; the returned matches mirror
// ui.SearchMatch (internal/ui/msg.go) directly, since that is exactly the
// payload ui.SearchResultsMsg carries.
type SearchFunc func(ctx context.Context, path, query string, regex bool) ([]ui.SearchMatch, error)

// ContextFunc builds the context.Context a single search request runs
// under. New's default is context.Background(), the shape production code
// always uses.
//
// WithContextFunc exists so a test can substitute a context it can observe
// or control, the same reason browser.ContextFunc exists, without reaching
// into Model's unexported state.
type ContextFunc func() context.Context

// Option configures a Model constructed by New.
type Option func(*Model)

// WithContextFunc overrides the ContextFunc a Model uses for every search
// request it issues. New's default is a plain context.Background().
func WithContextFunc(fn ContextFunc) Option {
	return func(m *Model) {
		m.ctxFunc = fn
	}
}

// WithTheme applies the configured interactive palette.
func WithTheme(theme ui.Theme) Option {
	return func(m *Model) { m.theme = theme }
}

// Model is the viewer screen: a local file in a bubbles/viewport, with
// in-file search (DESIGN.md 9.1). It implements ui.ScreenModel.
//
// # Loading
//
// Unlike the library screen, Model owns loading its own file directly:
// reading a single already-known local path needs no injected dependency
// the way reading state.toml or scanning a remote host does, so Init
// returns the tea.Cmd that reads path (the argument New was constructed
// with) and reports it as ui.FileLoadedMsg, or ui.ErrorMsg on failure
// (mechanic 2).
//
// # Reading strategy (mechanic 3)
//
// The load command reads the whole file with os.ReadFile rather than a
// bufio.Scanner, so there is no per-line token limit to size at all: a
// single line, however large, is simply a longer string once the bytes are
// in memory. This sidesteps AGENTS.md section 9's bufio.Scanner trap
// entirely rather than picking a fixed buffer large enough to survive it —
// deliberately so, because DESIGN.md 10.3's suggested 1MB floor is not
// itself large enough for mechanic 3's 5MB single line, and any other fixed
// number is just a smaller version of the same bet. The file content is
// split into lines by trimming one trailing "\n" (so a file that ends with
// a newline does not report one extra, empty trailing line) and splitting
// the remainder on "\n"; an empty file reports zero lines.
//
// # Search and match stepping
//
// "/" focuses the query input; while it is focused every other key,
// including "n", "N" and another "/", is forwarded to the input rather than
// interpreted as a viewer command. enter submits the input's current value
// as the tea.Cmd documented on SearchFunc and unfocuses the input; esc
// unfocuses it without searching. An empty query on enter is a no-op: no
// Cmd, input stays cleared.
//
// A ui.SearchResultsMsg, however it arrives, replaces Matches and sets
// CurrentMatchIndex to 0 if the new Matches is non-empty or -1 if it is
// empty — a fresh search always lands on its own first hit, never on
// wherever the previous search's cursor happened to be.
//
// While the query input is not focused, "n" and "N" step CurrentMatchIndex
// forward and backward through Matches and wrap at the ends (mechanic 4):
// "n" past the last match (index len(Matches)-1) moves to the first (index
// 0); "N" before the first (index 0) moves to the last (index
// len(Matches)-1). Both are a no-op on an empty Matches. Every other key
// while unfocused is forwarded to the embedded viewport, for scrolling.
//
// # Value semantics
//
// Model follows the value-model style used throughout internal/ui: Init,
// Update, View and Resize never mutate the receiver, so a caller such as
// ui.App can hold a Model as a plain field with no aliasing surprises.
//
// The zero value is not useful. Use New.
type Model struct {
	// theme styles the panel, title and controls.
	theme ui.Theme

	// path is the local file this Model was constructed to view. It never
	// changes after New.
	path string

	// search runs one in-file search. See SearchFunc.
	search SearchFunc

	// ctxFunc builds the context each search request runs under. See
	// ContextFunc.
	ctxFunc ContextFunc

	// width and height are the last dimensions Resize was called with.
	width  int
	height int

	// viewport renders lines with scrolling. Its content is set from lines
	// whenever ui.FileLoadedMsg is applied.
	viewport viewport.Model

	// lines is the file content, one entry per line, from the last applied
	// ui.FileLoadedMsg. It is nil until loading completes. LineCount
	// reports its length.
	lines []string

	// loaded reports whether a ui.FileLoadedMsg for path has been applied.
	loaded bool

	// searching reports whether the query input is focused: "/" sets it
	// true, enter and esc set it false. While true, every key other than
	// what the input itself consumes is not interpreted as a viewer
	// command (see Model's "Search and match stepping" section).
	searching bool

	// query is the search input's own text field.
	query textinput.Model

	// matches is the hit set from the last applied ui.SearchResultsMsg.
	// MatchCount reports its length.
	matches []ui.SearchMatch

	// matchIdx indexes into matches: the currently stepped-to hit. -1 means
	// no search has produced a non-empty match set yet. CurrentMatchIndex
	// reports it directly.
	matchIdx int

	// searchGen is incremented on every submitted search. A result whose
	// Gen does not match is for a search this model has moved on from.
	searchGen uint64
}

// New returns a Model that will load path when Init's command runs, using
// search to perform every in-file search this Model issues, and
// context.Background() for every search request unless overridden by
// WithContextFunc.
func New(path string, search SearchFunc, opts ...Option) Model {
	m := Model{
		theme:    ui.DefaultTheme(),
		path:     path,
		search:   search,
		ctxFunc:  func() context.Context { return context.Background() },
		viewport: viewport.New(0, 0),
		query:    textinput.New(),
		matchIdx: -1,
	}
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

// Init returns the tea.Cmd that reads path and reports it as
// ui.FileLoadedMsg, or ui.ErrorMsg on failure (mechanic 2), using the
// reading strategy documented on Model.
func (m Model) Init() tea.Cmd {
	path := m.path
	return func() tea.Msg {
		lines, err := readLines(path)
		if err != nil {
			return ui.ErrorMsg{Err: fmt.Errorf("reading %s: %w", path, err)}
		}
		return ui.FileLoadedMsg{Path: path, Lines: lines}
	}
}

// readLines reads path whole with os.ReadFile rather than a bufio.Scanner
// and splits it into lines, per the "Reading strategy" section on Model.
func readLines(path string) ([]string, error) {
	//nolint:gosec // path is the local file this Model was constructed to
	// view (a fetch already landed on disk, or an already-validated local
	// path), not attacker-controlled input to this func.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := strings.TrimSuffix(string(data), "\n")
	if content == "" {
		return []string{}, nil
	}
	return strings.Split(content, "\n"), nil
}

// Update handles one message and returns the updated Model and, when the
// message requires further work, the tea.Cmd that performs it. It never
// mutates the receiver and it never blocks on I/O.
//
// Messages handled:
//
//   - ui.FileLoadedMsg for this Model's path fills lines, sets loaded and
//     loads the content into the embedded viewport. One for a different
//     path is ignored.
//   - ui.SearchResultsMsg replaces matches and resets matchIdx, per the
//     "Search and match stepping" section on Model.
//   - tea.KeyMsg is handled per that same section: "/" to focus the query
//     input, enter and esc to submit or cancel it while focused, "n"/"N" to
//     step matches while unfocused (mechanic 4), and every other key
//     forwarded to whichever of the query input or the viewport is
//     currently active.
//
// Every other message, while the query input is focused, is forwarded to
// it unchanged (so its own asynchronous behaviour, such as cursor blink,
// keeps working); while unfocused it is forwarded to the viewport
// unchanged.
func (m Model) Update(msg tea.Msg) (ui.ScreenModel, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.FileLoadedMsg:
		if msg.Path != m.path {
			return m, nil
		}
		next := m
		next.lines = append([]string(nil), msg.Lines...)
		next.loaded = true
		next.viewport.SetContent(strings.Join(next.lines, "\n"))
		return next, nil

	case ui.SearchResultsMsg:
		if (msg.Path != "" && msg.Path != m.path) || (msg.Gen != 0 && msg.Gen != m.searchGen) {
			return m, nil
		}
		next := m
		next.matches = append([]ui.SearchMatch(nil), msg.Matches...)
		if len(next.matches) > 0 {
			next.matchIdx = 0
		} else {
			next.matchIdx = -1
		}
		return next, nil

	case ui.ThemeChangedMsg:
		next := m
		next.theme = ui.DefaultTheme().WithColors(msg.Primary, msg.Secondary)
		return next, nil

	case tea.KeyMsg:
		return m.updateKey(msg)

	default:
		if m.searching {
			next := m
			var cmd tea.Cmd
			next.query, cmd = next.query.Update(msg)
			return next, cmd
		}
		next := m
		var cmd tea.Cmd
		next.viewport, cmd = next.viewport.Update(msg)
		return next, cmd
	}
}

// keyRune reports the single rune msg represents, and true, when msg is a
// tea.KeyRunes key carrying exactly one rune.
func keyRune(msg tea.KeyMsg) (rune, bool) {
	if msg.Type != tea.KeyRunes || len(msg.Runes) != 1 {
		return 0, false
	}
	return msg.Runes[0], true
}

// updateKey handles a tea.KeyMsg per the "Search and match stepping"
// section on Model.
func (m Model) updateKey(msg tea.KeyMsg) (ui.ScreenModel, tea.Cmd) {
	if m.searching {
		switch msg.Type {
		case tea.KeyEnter:
			return m.submitSearch()
		case tea.KeyEsc:
			next := m
			next.searching = false
			next.query.Blur()
			return next, nil
		default:
			next := m
			var cmd tea.Cmd
			next.query, cmd = next.query.Update(msg)
			return next, cmd
		}
	}

	if msg.Type == tea.KeyEsc {
		return m, func() tea.Msg { return ui.BackMsg{} }
	}

	if r, ok := keyRune(msg); ok {
		switch r {
		case '/':
			next := m
			next.searching = true
			cmd := next.query.Focus()
			return next, cmd
		case 'n':
			next := m
			next.matchIdx = stepMatch(m.matchIdx, len(m.matches), 1)
			return next, nil
		case 'N':
			next := m
			next.matchIdx = stepMatch(m.matchIdx, len(m.matches), -1)
			return next, nil
		}
	}

	next := m
	var cmd tea.Cmd
	next.viewport, cmd = next.viewport.Update(msg)
	return next, cmd
}

// stepMatch advances idx by delta (1 or -1) through a match set of size n,
// wrapping at either end, or returns -1 when n is 0 (mechanic 4).
func stepMatch(idx, n, delta int) int {
	if n == 0 {
		return -1
	}
	idx += delta
	switch {
	case idx >= n:
		idx = 0
	case idx < 0:
		idx = n - 1
	}
	return idx
}

// submitSearch handles enter while the query input is focused: an empty
// query is a no-op, otherwise it returns the tea.Cmd documented on
// SearchFunc and unfocuses the input.
func (m Model) submitSearch() (ui.ScreenModel, tea.Cmd) {
	query := m.query.Value()
	if query == "" {
		return m, nil
	}

	next := m
	next.searching = false
	next.query.Blur()
	next.searchGen++

	search := m.search
	ctxFunc := m.ctxFunc
	path := m.path
	gen := next.searchGen

	cmd := func() tea.Msg {
		ctx := ctxFunc()
		matches, err := search(ctx, path, query, false)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return ui.ErrorMsg{Err: fmt.Errorf("search %s: %w", path, err)}
		}
		return ui.SearchResultsMsg{Path: path, Gen: gen, Query: query, Regex: false, Matches: matches}
	}
	return next, cmd
}

// View renders the viewport followed by the query input, when focused, or
// the match count status line otherwise, sized to the area Resize was last
// called with.
func (m Model) View() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	height := m.height
	if height <= 0 {
		height = 24
	}
	innerWidth := max(1, width-4)
	innerHeight := max(1, height-2)
	textWidth := max(1, innerWidth-2)

	title := "Viewer"
	if m.path != "" {
		title += " — " + m.path
	}

	var b strings.Builder
	b.WriteString(m.theme.Title.Render(fitLine(title, textWidth)))
	b.WriteString("\n\n")
	b.WriteString(m.viewport.View())
	b.WriteByte('\n')
	if m.searching {
		b.WriteString(m.query.View())
	} else {
		status := fmt.Sprintf("%d/%d matches  •  / search  •  n/N step  •  esc library", m.matchIdx+1, len(m.matches))
		if !m.loaded {
			status = "Loading…"
		}
		b.WriteString(m.theme.Dim.Render(fitLine(status, textWidth)))
	}

	body := clipLines(b.String(), innerHeight)
	return m.theme.PaneActive.
		Width(innerWidth).
		Height(innerHeight).
		Padding(0, 1).
		Render(body)
}

// Resize records the area the screen has to render into and returns the
// updated Model, propagating the size to the embedded viewport less chrome
// for the title and status/query line.
func (m Model) Resize(width, height int) ui.ScreenModel {
	next := m
	next.width = width
	next.height = height

	innerWidth := max(1, width-6)
	vh := height - 6
	if vh < 0 {
		vh = 0
	}
	next.viewport.Width = innerWidth
	next.viewport.Height = vh
	next.query.Width = max(1, innerWidth-4)

	return next
}

func fitLine(value string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

func clipLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// Path returns the local file path this Model was constructed with.
func (m Model) Path() string {
	return m.path
}

// Lines returns the file content loaded by the last applied
// ui.FileLoadedMsg, one entry per line. It is nil until loading completes.
// The returned slice is owned by Model; a caller must not mutate it.
func (m Model) Lines() []string {
	return m.lines
}

// LineCount returns len(Lines): the file's line count as of the last
// applied ui.FileLoadedMsg, 0 before loading completes (mechanic 2).
func (m Model) LineCount() int {
	return len(m.lines)
}

// Loaded reports whether a ui.FileLoadedMsg for this Model's path has been
// applied.
func (m Model) Loaded() bool {
	return m.loaded
}

// Searching reports whether the query input is currently focused.
func (m Model) Searching() bool {
	return m.searching
}

// Matches returns the hit set from the last applied ui.SearchResultsMsg, in
// file order. It is nil until a search has completed. The returned slice is
// owned by Model; a caller must not mutate it.
func (m Model) Matches() []ui.SearchMatch {
	return m.matches
}

// MatchCount returns len(Matches).
func (m Model) MatchCount() int {
	return len(m.matches)
}

// CurrentMatchIndex returns the index into Matches that "n" and "N" step
// from: the hit currently considered current, wrapping at the ends per the
// "Search and match stepping" section on Model (mechanic 4). It is -1 when
// Matches is empty.
func (m Model) CurrentMatchIndex() int {
	return m.matchIdx
}

// CurrentMatch returns Matches()[CurrentMatchIndex()] and true, or the zero
// value and false when Matches is empty.
func (m Model) CurrentMatch() (ui.SearchMatch, bool) {
	if m.matchIdx < 0 || m.matchIdx >= len(m.matches) {
		return ui.SearchMatch{}, false
	}
	return m.matches[m.matchIdx], true
}
