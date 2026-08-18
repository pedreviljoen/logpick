package picker

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sahilm/fuzzy"
)

// Item is what a picker filters and displays. FilterValue is the string
// sahilm/fuzzy matches the query against. It need not be what the caller
// eventually renders for a row: a picker over log entries might filter on
// the full remote path while a screen renders only the basename.
type Item interface {
	FilterValue() string
}

// Model is a generic fuzzy-filtered list, the component shared by every
// screen that needs one (DESIGN.md 9.4): the host picker, the browser's file
// list, the library, and any in-viewer jump list.
//
// It is a component, not a screen. It does not implement ui.ScreenModel: it
// has no Init and no Resize(width, height) returning the interface the root
// stores. A screen embeds a Model in its own state and drives it directly.
//
// Model follows the value-model style used throughout internal/ui: Update
// returns the updated model rather than mutating the receiver, so a screen
// can hold a Model as a plain field with no aliasing surprises.
//
// The zero value is not useful. Use New.
//
// # Exported surface
//
// DESIGN.md 9.4 specifies the fields but they are unexported, so the
// contract a caller and a test actually program against is this behavioural
// surface:
//
//   - Update and View, the two tea-shaped entry points.
//   - Selected, the commit signal every screen ultimately wants.
//   - Matches, Cursor and Offset, which expose exactly enough of the
//     filtering and scrolling state to test mechanics 1, 2, 3 and 5 without
//     asserting on unexported fields (AGENTS.md section 5) or on rendered
//     output (DESIGN.md section 13).
//   - SetHeight, which gives Offset a viewport edge to scroll past; without
//     it mechanic 3 has nothing to test against, since an unbounded list
//     never needs to scroll.
//
// # Filtering
//
// Filtering is synchronous: every keystroke re-runs fuzzy.Find (or, for an
// empty query, a pass-through) inline in Update. DESIGN.md 9.4 sizes this
// deliberately: at a few thousand items sahilm/fuzzy completes well inside a
// frame, so there is no background matcher and no matching tea.Msg for a
// result arriving later.
//
// An empty query is not treated as "no matches". It shows every item in the
// original order given to New, each as a Match with an empty
// MatchedIndexes, so a caller does not need to special-case Matches() being
// empty at startup.
//
// Match highlight indices are never touched after fuzzy.Find returns them:
// Matches()[i].MatchedIndexes is exactly the slice fuzzy.Find produced for
// that item, unmodified, so a view can index into it directly to highlight
// characters.
type Model[T Item] struct {
	// all is every item the picker was constructed with, in the original
	// order. Match.Index for a non-empty query indexes into this slice.
	all []T

	// values is all[i].FilterValue() for every i, computed once in New. It
	// is the []string fuzzy.Find wants as its data slice, cached so it is
	// not rebuilt on every keystroke.
	values []string

	// matches is the current match set: the result of fuzzy.Find against
	// the live query, or a synthetic pass-through of all when the query is
	// empty. Preallocated to len(all) capacity (AGENTS.md 4.1), since it is
	// rebuilt on every keystroke.
	matches []fuzzy.Match

	// input is the query text field.
	input textinput.Model

	// cursor indexes into matches: the currently highlighted row.
	cursor int

	// offset indexes into matches: the first row the viewport renders.
	offset int

	// height and width bound the rendered match rows. Zero means the
	// corresponding dimension has not been configured.
	height int
	width  int

	// selectedIdx indexes into all: the item committed by the most recent
	// enter on a non-empty match set. -1 means nothing has been committed
	// yet.
	selectedIdx int
}

// New returns a picker over items with an empty query, so Matches initially
// reports every item in the order given, the cursor at 0, and nothing
// committed.
func New[T Item](items []T) Model[T] {
	all := make([]T, len(items))
	copy(all, items)

	values := make([]string, len(all))
	for i, it := range all {
		values[i] = it.FilterValue()
	}

	input := textinput.New()
	input.Focus()

	return Model[T]{
		all:         all,
		values:      values,
		matches:     passthroughMatches(all),
		input:       input,
		cursor:      0,
		offset:      0,
		height:      0,
		selectedIdx: -1,
	}
}

// passthroughMatches builds the synthetic pass-through match set used for an
// empty query: every item of all, in order, each as a Match with an empty
// MatchedIndexes.
func passthroughMatches[T Item](all []T) []fuzzy.Match {
	matches := make([]fuzzy.Match, len(all))
	for i, it := range all {
		matches[i] = fuzzy.Match{Str: it.FilterValue(), Index: i}
	}
	return matches
}

// clampView clamps cursor into [0, matchLen-1] (or 0 if matchLen is 0), then
// adjusts offset by the minimal amount needed to bring cursor back within
// the viewport [offset, offset+height-1], never scrolling past the point
// where fewer than height rows (or, if there are fewer matches than height,
// all remaining matches) would be shown. height <= 0 means no viewport has
// been configured yet, so offset is always 0.
func clampView(cursor, matchLen, offset, height int) (int, int) {
	if matchLen == 0 {
		return 0, 0
	}

	switch {
	case cursor < 0:
		cursor = 0
	case cursor > matchLen-1:
		cursor = matchLen - 1
	}

	if height <= 0 {
		return cursor, 0
	}

	maxOffset := matchLen - height
	if maxOffset < 0 {
		maxOffset = 0
	}
	switch {
	case offset > maxOffset:
		offset = maxOffset
	case offset < 0:
		offset = 0
	}

	switch {
	case cursor < offset:
		offset = cursor
	case cursor > offset+height-1:
		offset = cursor - height + 1
	}
	return cursor, offset
}

// Update advances the model by one message and returns the updated model,
// never mutating the receiver.
//
// A tea.KeyMsg is handled as follows, and nothing else is inspected:
//
//   - ctrl+n/down moves to the next match, ctrl+p/up to the previous one.
//     Both stop at the bounds of the current match set rather than wrapping.
//     Moving the cursor
//     past the edge of the viewport set by SetHeight advances Offset by
//     exactly enough to keep the cursor visible.
//   - enter commits the item under the cursor: it becomes the value Selected
//     returns. Enter on an empty match set is a no-op; it does not clear a
//     previously committed selection.
//   - Any other key, including a printable rune or backspace, is given to
//     the underlying query input. If the resulting query text differs from
//     before, the match set is recomputed synchronously and the cursor and
//     offset are clamped into the new set.
//
// Any non-key message is passed to the underlying query input unchanged, so
// its own asynchronous behaviour (such as cursor blink) keeps working.
//
// esc is not handled here. A caller that wants "cancel the picker" reacts to
// esc itself; Update treats it like any other unrecognised key and forwards
// it to the input.
func (m Model[T]) Update(msg tea.Msg) (Model[T], tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd
	}

	switch keyMsg.Type {
	case tea.KeyCtrlN, tea.KeyDown:
		m.cursor, m.offset = clampView(m.cursor+1, len(m.matches), m.offset, m.height)
		return m, nil

	case tea.KeyCtrlP, tea.KeyUp:
		m.cursor, m.offset = clampView(m.cursor-1, len(m.matches), m.offset, m.height)
		return m, nil

	case tea.KeyEnter:
		if len(m.matches) > 0 {
			m.selectedIdx = m.matches[m.cursor].Index
		}
		return m, nil
	}

	oldQuery := m.input.Value()
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(keyMsg)

	if newQuery := m.input.Value(); newQuery != oldQuery {
		if newQuery == "" {
			m.matches = passthroughMatches(m.all)
		} else {
			m.matches = fuzzy.Find(newQuery, m.values)
		}
		m.cursor, m.offset = clampView(m.cursor, len(m.matches), m.offset, m.height)
	}

	return m, cmd
}

// View renders the query line followed by up to Matches height rows,
// starting at Offset, with the row at Cursor marked as highlighted and each
// row's matched characters (per its MatchedIndexes) styled.
func (m Model[T]) View() string {
	var b strings.Builder
	b.WriteString(m.input.View())
	b.WriteByte('\n')

	end := len(m.matches)
	if m.height > 0 && m.offset+m.height < end {
		end = m.offset + m.height
	}

	for i := m.offset; i < end; i++ {
		match := m.matches[i]

		if i == m.cursor {
			b.WriteString("> ")
		} else {
			b.WriteString("  ")
		}

		display := truncateRow(match.Str, m.width-2)
		if len(match.MatchedIndexes) == 0 {
			b.WriteString(display)
			b.WriteByte('\n')
			continue
		}

		highlighted := make(map[int]bool, len(match.MatchedIndexes))
		for _, idx := range match.MatchedIndexes {
			highlighted[idx] = true
		}
		for j, r := range display {
			if highlighted[j] {
				b.WriteByte('[')
				b.WriteRune(r)
				b.WriteByte(']')
			} else {
				b.WriteRune(r)
			}
		}
		b.WriteByte('\n')
	}

	return b.String()
}

// Selected returns the item committed by the most recent enter on a
// non-empty match set, and true. It returns the zero value of T and false
// if no commit has happened yet in this model's history, including because
// every enter so far landed on an empty match set.
func (m Model[T]) Selected() (T, bool) {
	if m.selectedIdx < 0 {
		var zero T
		return zero, false
	}
	return m.all[m.selectedIdx], true
}

// Query returns the current text in the picker's filter input.
func (m Model[T]) Query() string {
	return m.input.Value()
}

// Matches returns the current match set, in the order View renders them:
// the order fuzzy.Find returns for a non-empty query, or all items in their
// original order for an empty query. Each element's MatchedIndexes is
// exactly what fuzzy.Find produced for that item, never post-processed.
//
// The returned slice is owned by Model; a caller must not mutate it.
func (m Model[T]) Matches() []fuzzy.Match {
	return m.matches
}

// Cursor returns the index into Matches of the highlighted row. It is
// always within [0, len(Matches())) when Matches is non-empty, and 0 when
// Matches is empty.
func (m Model[T]) Cursor() int {
	return m.cursor
}

// Offset returns the index into Matches of the first row the viewport
// renders. It is 0 until SetHeight is called and until Cursor first moves
// past the bottom of the configured viewport, after which it advances by
// exactly enough to keep Cursor visible, and never past the point where
// fewer than height rows (or, if there are fewer matches than height, all
// remaining matches) would be shown.
func (m Model[T]) Offset() int {
	return m.offset
}

// SetHeight sets the number of match rows the viewport renders and returns
// the updated model, recomputing Offset to keep Cursor visible within it.
// This is what gives Offset a viewport edge to scroll past; a picker never
// configured with SetHeight (height's zero value) has no such edge, and
// Offset stays 0.
func (m Model[T]) SetHeight(height int) Model[T] {
	m.height = height
	m.cursor, m.offset = clampView(m.cursor, len(m.matches), m.offset, height)
	return m
}

// SetWidth bounds each rendered row and the filter input to width cells.
func (m Model[T]) SetWidth(width int) Model[T] {
	m.width = width
	inputWidth := width - 2
	if inputWidth < 1 {
		inputWidth = 1
	}
	m.input.Width = inputWidth
	return m
}

func truncateRow(value string, width int) string {
	if width <= 0 {
		return value
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

// SetCursor returns the updated model with Cursor set to i, never mutating
// the receiver. i indexes into the current match set (Matches()), not the
// unfiltered item slice given to New — the two differ whenever a filter is
// active.
//
// An out-of-range i, including on an empty match set, is clamped into the
// valid range rather than panicking: the same clampView rule ctrl+n and
// ctrl+p movement uses, so Offset ends up exactly where repeatedly pressing
// ctrl+n (or ctrl+p) from the current position to reach index i would have
// left it. This exists so a caller that grows or replaces the item set
// mid-session (DESIGN.md 9.4's streaming scan case) can restore a
// previously highlighted row in O(1) instead of replaying synthetic key
// presses.
func (m Model[T]) SetCursor(i int) Model[T] {
	m.cursor, m.offset = clampView(i, len(m.matches), m.offset, m.height)
	return m
}
